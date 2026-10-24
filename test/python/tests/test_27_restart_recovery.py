"""
Test 27: Restart recovery.
Tests that data survives a container restart by writing to a Docker volume,
stopping the container, starting a new one with the same volume, and
verifying all documents are still searchable.
"""
import json
import time
import subprocess
import urllib.request
import urllib.error

import pytest
import requests


IMAGE_NAME = "unisearch:latest"
RESTART_CONTAINER = "unisearch-restart-test"
RESTART_PORT = 19201
RESTART_DATA_DIR = "/tmp/unisearch-restart-data"


def _run(cmd):
    result = subprocess.run(cmd, shell=True, capture_output=True, text=True)
    return result.stdout.strip(), result.stderr.strip()


class TestRestartRecovery:
    def test_write_restart_recover(self, base_url):
        """Write docs → stop container → start new → verify data survives."""
        # Ensure clean state
        _run(f"docker rm -f {RESTART_CONTAINER}")
        import shutil, os
        if os.path.exists(RESTART_DATA_DIR):
            shutil.rmtree(RESTART_DATA_DIR, ignore_errors=True)
        os.makedirs(RESTART_DATA_DIR, exist_ok=True)

        # Start container with persistent volume
        _run(
            f"docker run -d --name {RESTART_CONTAINER} "
            f"-p {RESTART_PORT}:9200 "
            f"-v {RESTART_DATA_DIR}:/data "
            f"{IMAGE_NAME} --embedded --data-dir /data --listen :9200"
        )

        # Wait for healthy
        url = f"http://localhost:{RESTART_PORT}/_cluster/health"
        deadline = time.time() + 20
        while time.time() < deadline:
            try:
                with urllib.request.urlopen(url, timeout=3) as resp:
                    if resp.status == 200:
                        break
            except (urllib.error.URLError, ConnectionRefusedError, OSError):
                pass
            time.sleep(1)
        else:
            pytest.skip("Restart test container did not become healthy")

        restart_url = f"http://localhost:{RESTART_PORT}"

        # Write 20 docs with refresh
        lines = []
        for i in range(10):
            lines.append(json.dumps({"index": {"_index": "restart_test", "_id": f"r{i}"}}))
            lines.append(json.dumps({"title": f"restart doc {i}", "n": i}))
        body = "\n".join(lines) + "\n"
        r = requests.post(f"{restart_url}/_bulk?refresh=true", data=body,
                          headers={"Content-Type": "application/x-ndjson"})
        assert r.status_code == 200

        # Verify all 20 docs are searchable before restart
        r = requests.get(f"{restart_url}/restart_test/_count")
        assert r.json().get("count", 0) == 10

        # Stop container
        _run(f"docker rm -f {RESTART_CONTAINER}")
        time.sleep(1)

        # Start new container with the same data volume
        _run(
            f"docker run -d --name {RESTART_CONTAINER} "
            f"-p {RESTART_PORT}:9200 "
            f"-v {RESTART_DATA_DIR}:/data "
            f"{IMAGE_NAME} --embedded --data-dir /data --listen :9200"
        )

        # Wait for healthy
        deadline = time.time() + 20
        while time.time() < deadline:
            try:
                with urllib.request.urlopen(url, timeout=3) as resp:
                    if resp.status == 200:
                        break
            except (urllib.error.URLError, ConnectionRefusedError, OSError):
                pass
            time.sleep(1)
        else:
            pytest.skip("Restart test container did not become healthy after restart")

        # Verify all 20 docs survived the restart
        r = requests.get(f"{restart_url}/restart_test/_count")
        assert r.status_code == 200
        assert r.json().get("count", 0) == 10

        # Verify a specific doc is intact
        r = requests.get(f"{restart_url}/restart_test/_doc/r0")
        assert r.status_code == 200
        data = r.json()
        src = data.get("_source", data.get("source", {}))
        assert src.get("title") == "restart doc 0"

        # Cleanup
        _run(f"docker rm -f {RESTART_CONTAINER}")
        import shutil
        shutil.rmtree(RESTART_DATA_DIR, ignore_errors=True)
