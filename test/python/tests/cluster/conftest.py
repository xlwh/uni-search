"""
conftest.py — pytest fixtures for UniSearch cluster (docker-compose) testing.

Manages docker-compose lifecycle: build image, start cluster, wait for
health and shard registration, provide cluster_url fixture, tear down.
Independent from the root conftest.py (single-node embedded mode).
"""
import os
import sys
import time
import json
import uuid
import subprocess
import urllib.request
import urllib.error

import pytest
import requests

# --- Configuration ---
PROJECT_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", "..", ".."))
COMPOSE_FILE = os.path.join(PROJECT_ROOT, "docker-compose.yml")
IMAGE_NAME = "unisearch:latest"

# Host ports (mapped from container port 9200)
META_PORTS = [9201, 9202, 9203]
SHARD_PORTS = [9211, 9212]
META_URLS = [f"http://localhost:{p}" for p in META_PORTS]
SHARD_URLS = [f"http://localhost:{p}" for p in SHARD_PORTS]
META_CONTAINERS = ["meta-1", "meta-2", "meta-3"]
SHARD_CONTAINERS = ["shard-1", "shard-2"]
STARTUP_TIMEOUT = 120  # seconds


def _run(cmd, check=True, capture=True, cwd=None):
    """Run a shell command and return (stdout, stderr)."""
    result = subprocess.run(
        cmd, shell=True, capture_output=capture, text=True, cwd=cwd
    )
    if check and result.returncode != 0:
        raise RuntimeError(f"Command failed: {cmd}\nstdout: {result.stdout}\nstderr: {result.stderr}")
    return result.stdout, result.stderr


def _http_get(url, timeout=5):
    """Simple HTTP GET returning (status_code, body_json)."""
    try:
        req = urllib.request.Request(url)
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, json.loads(resp.read())
    except Exception:
        return 0, None


def _wait_for_healthy(url, timeout=60):
    """Poll URL/_cluster/health until it returns 200."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        status, _ = _http_get(f"{url}/_cluster/health")
        if status == 200:
            return True
        time.sleep(2)
    return False


def _wait_for_shards_registered(meta_url, expected_count=2, timeout=60):
    """Poll /_internal/routes until all shard nodes are registered."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            req = urllib.request.Request(f"{meta_url}/_internal/routes")
            with urllib.request.urlopen(req, timeout=5) as resp:
                data = json.loads(resp.read())
                members = data.get("members", [])
                shard_members = [m for m in members if m.get("role") == "shard"]
                if len(shard_members) >= expected_count:
                    return True
        except Exception:
            pass
        time.sleep(2)
    return False


def _get_leader(meta_urls):
    """Find the current leader meta node URL using is_leader field."""
    for i, url in enumerate(meta_urls):
        try:
            r = requests.get(f"{url}/_internal/routes", timeout=5)
            if r.status_code == 200:
                data = r.json()
                if data.get("is_leader"):
                    return url, META_CONTAINERS[i]
        except Exception:
            pass
    # Fallback: try creating a test index on each meta node
    for i, url in enumerate(meta_urls):
        try:
            r = requests.put(f"{url}/_leader-test",
                             json={"mappings": {"properties": {"t": {"type": "text"}}}},
                             timeout=5)
            if r.status_code == 200:
                requests.delete(f"{url}/_leader-test")
                return url, META_CONTAINERS[i]
        except Exception:
            pass
    return None, None


def _stop_container(name):
    """Stop a Docker container."""
    _run(f"docker stop {name}", check=False)


def _start_container(name):
    """Start a Docker container."""
    _run(f"docker start {name}", check=False)


# --- Pytest fixtures ---

@pytest.fixture(scope="session")
def session_id():
    """Unique session ID for test isolation."""
    return uuid.uuid4().hex[:8]


@pytest.fixture(scope="session")
def cluster_up():
    """Build image, start docker-compose cluster, wait for health."""
    # Step 1: Build the Docker image (includes Go cross-compilation).
    print("\n[cluster] Building Docker image...")
    goos = "linux"
    goarch = "arm64"
    # Detect architecture
    out, _ = _run("uname -m", check=False)
    if "x86_64" in out:
        goarch = "amd64"
    
    _run(
        f"CGO_ENABLED=0 GOOS={goos} GOARCH={goarch} go build -o unisearch-linux ./cmd/unisearch",
        cwd=PROJECT_ROOT, check=True
    )
    _run(f"docker compose -f {COMPOSE_FILE} build", cwd=PROJECT_ROOT, check=True)

    # Step 2: Start the cluster.
    print("[cluster] Starting docker-compose cluster...")
    _run(f"docker compose -f {COMPOSE_FILE} up -d", cwd=PROJECT_ROOT, check=True)

    # Step 3: Wait for all meta nodes to be healthy.
    print("[cluster] Waiting for meta nodes...")
    all_healthy = True
    for url in META_URLS:
        if not _wait_for_healthy(url, timeout=STARTUP_TIMEOUT):
            print(f"[cluster] WARNING: {url} not healthy")
            all_healthy = False

    # Step 4: Wait for shard nodes to register with meta.
    print("[cluster] Waiting for shard registration...")
    # Try all meta URLs for shard registration.
    registered = False
    for url in META_URLS:
        if _wait_for_shards_registered(url, expected_count=2, timeout=60):
            registered = True
            break
    if not registered:
        print("[cluster] WARNING: shard nodes may not be registered")

    # Give gRPC servers extra time to fully initialize.
    time.sleep(5)

    # Step 5: Find the leader meta node.
    leader_url, leader_container = _get_leader(META_URLS)
    print(f"[cluster] Leader: {leader_url} ({leader_container})")

    yield True

    # Teardown: stop and remove containers and volumes.
    print("\n[cluster] Tearing down cluster...")
    _run(f"docker compose -f {COMPOSE_FILE} down -v", cwd=PROJECT_ROOT, check=False)


@pytest.fixture(autouse=True, scope="function")
def ensure_containers(cluster_up, meta_containers, shard_containers):
    """Ensure all containers are running before each test (recover from
    failover test container kills)."""
    for c in meta_containers + shard_containers:
        _start_container(c)
    time.sleep(5)


@pytest.fixture(scope="function")
def cluster_url(ensure_containers):
    """Return the leader meta node HTTP URL for ES API requests.
    Function-scoped so it re-finds the leader after failover."""
    leader_url, _ = _get_leader(META_URLS)
    if leader_url:
        return leader_url
    # Fallback to meta-1
    return META_URLS[0]


@pytest.fixture(scope="session")
def meta_urls(cluster_up):
    """Return all meta node HTTP URLs."""
    return META_URLS


@pytest.fixture(scope="session")
def shard_urls(cluster_up):
    """Return all shard node HTTP URLs."""
    return SHARD_URLS


@pytest.fixture(scope="session")
def meta_containers(cluster_up):
    """Return meta container names."""
    return META_CONTAINERS


@pytest.fixture(scope="session")
def shard_containers(cluster_up):
    """Return shard container names."""
    return SHARD_CONTAINERS


@pytest.fixture
def index_name(cluster_url, session_id):
    """Create a unique index name for each test function."""
    name = f"tc_{session_id}_{uuid.uuid4().hex[:8]}"
    yield name
    # Cleanup: delete the index.
    import requests
    try:
        requests.delete(f"{cluster_url}/{name}")
    except Exception:
        pass
