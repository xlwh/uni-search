"""
conftest.py — pytest fixtures for UniSearch Docker cluster deployment.

Manages container lifecycle: build image, start container, wait for health,
provide base_url fixture, tear down after tests.
"""
import os
import sys
import time
import json
import socket
import subprocess
import urllib.request
import urllib.error
from pathlib import Path

import pytest

# --- Configuration ---
IMAGE_NAME = "unisearch:latest"
CONTAINER_NAME = "unisearch-test"
HOST_PORT = 19200
INTERNAL_PORT = 9200
DATA_DIR = "/tmp/unisearch-test-data"
STARTUP_TIMEOUT = 30  # seconds


def _run(cmd, check=True, capture=True):
    """Run a shell command and return (stdout, stderr)."""
    result = subprocess.run(
        cmd, shell=True, capture_output=capture, text=True
    )
    if check and result.returncode != 0:
        raise RuntimeError(
            f"Command failed: {cmd}\nstdout: {result.stdout}\nstderr: {result.stderr}"
        )
    return result.stdout.strip(), result.stderr.strip()


def _docker_running():
    """Check if Docker daemon is available."""
    try:
        out, _ = _run("docker info", check=False)
        return "Server" in out or "Containers" in out
    except Exception:
        return False


def _image_exists():
    """Check if the Docker image exists."""
    out, _ = _run(f"docker images -q {IMAGE_NAME}", check=False)
    return bool(out.strip())


def _build_image():
    """Build the Docker image from project root."""
    project_root = str(Path(__file__).resolve().parents[2])
    # Ensure binary is built for linux/arm64
    binary_path = os.path.join(project_root, "unisearch-linux")
    if not os.path.exists(binary_path):
        print(f"Building linux/arm64 binary...")
        _run(
            f"cd {project_root} && "
            f"CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o unisearch-linux ./cmd/unisearch"
        )
    print(f"Building Docker image {IMAGE_NAME} ...")
    _run(f"cd {project_root} && docker build -t {IMAGE_NAME} .")


def _container_exists():
    """Check if a container with our name exists (running or stopped)."""
    out, _ = _run(f"docker ps -a -q --filter name={CONTAINER_NAME}", check=False)
    return bool(out.strip())


def _remove_container():
    """Remove any existing container with our name."""
    if _container_exists():
        _run(f"docker rm -f {CONTAINER_NAME}", check=False)


def _start_container():
    """Start a fresh UniSearch container in embedded mode."""
    _remove_container()
    # Clean data dir to ensure no stale indices from previous runs
    import shutil
    if os.path.exists(DATA_DIR):
        shutil.rmtree(DATA_DIR, ignore_errors=True)
    os.makedirs(DATA_DIR, exist_ok=True)
    cmd = (
        f"docker run -d --name {CONTAINER_NAME} "
        f"-p {HOST_PORT}:{INTERNAL_PORT} "
        f"-v {DATA_DIR}:/data "
        f"{IMAGE_NAME} --embedded --data-dir /data --listen :9200"
    )
    out, _ = _run(cmd)
    print(f"Container started: {out[:12]}...")


def _wait_healthy():
    """Wait for the container to respond on /_cluster/health."""
    url = f"http://localhost:{HOST_PORT}/_cluster/health"
    deadline = time.time() + STARTUP_TIMEOUT
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(url, timeout=3) as resp:
                if resp.status == 200:
                    data = json.loads(resp.read())
                    print(f"Cluster healthy: {data}")
                    return
        except (urllib.error.URLError, ConnectionRefusedError, OSError):
            pass
        time.sleep(1)
    # Print container logs for debugging
    logs, _ = _run(f"docker logs {CONTAINER_NAME}", check=False)
    print(f"Container logs:\n{logs[:2000]}")
    raise RuntimeError(f"Container did not become healthy within {STARTUP_TIMEOUT}s")


def _stop_container():
    """Stop and remove the container."""
    _run(f"docker rm -f {CONTAINER_NAME}", check=False)


# --- Pytest fixtures ---

@pytest.fixture(scope="session")
def base_url():
    """Provide the base URL for the running UniSearch instance."""
    if not _docker_running():
        pytest.skip("Docker daemon not available")

    if not _image_exists():
        _build_image()

    _start_container()
    _wait_healthy()

    url = f"http://localhost:{HOST_PORT}"
    yield url

    _stop_container()


@pytest.fixture(scope="session")
def session_id():
    """Unique session ID for test isolation."""
    import uuid
    return uuid.uuid4().hex[:8]


@pytest.fixture
def index_name(base_url, session_id):
    """Create a unique index for each test with default mappings, clean up after."""
    import requests
    import uuid
    name = f"t_{session_id}_{uuid.uuid4().hex[:8]}"

    # Delete any stale index first (safety net)
    try:
        requests.delete(f"{base_url}/{name}")
    except Exception:
        pass

    # Create index with default mapping for common field types used across tests.
    # Without explicit mappings, the server auto-detects all strings as text,
    # which breaks term/range queries that need keyword/numeric column data.
    default_mapping = {
        "mappings": {
            "properties": {
                # Text fields (full-text search via inverted index)
                "title": {"type": "text"},
                "text": {"type": "text"},
                "message": {"type": "text"},
                "description": {"type": "text"},
                # Keyword fields (exact match via Scan+Filter)
                "status": {"type": "keyword"},
                "category": {"type": "keyword"},
                "tag": {"type": "keyword"},
                "tags": {"type": "keyword"},
                "name": {"type": "keyword"},
                "brand": {"type": "keyword"},
                "fruit": {"type": "keyword"},
                "level": {"type": "keyword"},
                # Numeric fields (range queries via Scan+Filter)
                "price": {"type": "float"},
                "qty": {"type": "long"},
                "n": {"type": "long"},
                "ts": {"type": "long"},
                "v": {"type": "long"},
                "in_stock": {"type": "boolean"},
                # Vector fields (KNN search)
                "vec": {"type": "dense_vector", "dim": 4},
                "embedding": {"type": "dense_vector", "dim": 4},
            }
        }
    }

    resp = requests.put(f"{base_url}/{name}", json=default_mapping)
    assert resp.status_code in (200, 201), f"Failed to create index: {resp.text}"

    yield name

    # Clean up
    try:
        requests.delete(f"{base_url}/{name}")
    except Exception:
        pass
