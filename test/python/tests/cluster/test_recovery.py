"""
Restart recovery tests for UniSearch cluster mode.
Verifies that data persists across cluster restarts (both meta and
shard nodes), with volumes preserved.
"""
import json
import time
import subprocess
import pytest
import requests

pytestmark = pytest.mark.cluster

PROJECT_ROOT = subprocess.run(
    ["git", "rev-parse", "--show-toplevel"], capture_output=True, text=True
).stdout.strip()
COMPOSE_FILE = f"{PROJECT_ROOT}/docker-compose.yml"


def _run(cmd, check=True, cwd=None):
    result = subprocess.run(cmd, shell=True, capture_output=True, text=True, cwd=cwd)
    if check and result.returncode != 0:
        raise RuntimeError(f"Command failed: {cmd}\n{result.stderr}")
    return result.stdout


def _wait_healthy(url, timeout=120):
    import urllib.request
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            req = urllib.request.Request(f"{url}/_cluster/health")
            with urllib.request.urlopen(req, timeout=5) as resp:
                if resp.status == 200:
                    return True
        except Exception:
            pass
        time.sleep(2)
    return False


def _wait_shards(meta_url, count=2, timeout=60):
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            r = requests.get(f"{meta_url}/_internal/routes", timeout=5)
            if r.status_code == 200:
                members = r.json().get("members", [])
                shard_members = [m for m in members if m.get("role") == "shard"]
                if len(shard_members) >= count:
                    return True
        except Exception:
            pass
        time.sleep(2)
    return False


def _find_leader(meta_urls, timeout=120):
    """Find the current Raft leader meta node using is_leader field."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        for i, url in enumerate(meta_urls):
            try:
                r = requests.get(f"{url}/_internal/routes", timeout=5)
                if r.status_code == 200:
                    data = r.json()
                    if data.get("is_leader"):
                        return url
            except Exception:
                pass
        time.sleep(3)
    return None


class TestRecovery:

    def test_meta_state_persistence(self, cluster_url):
        """Create index + write docs, restart cluster (keep volumes),
        verify index and docs survive."""
        # Write data.
        idx = "test-rec-1"
        requests.put(f"{cluster_url}/{idx}", json={
            "settings": {"number_of_shards": 2},
            "mappings": {"properties": {"title": {"type": "text"}}}
        })
        for i in range(10):
            requests.post(f"{cluster_url}/{idx}/_doc/p-{i:03d}?refresh=true",
                          json={"title": f"persistent {i}"})

        # Verify data exists via search (more reliable than count).
        r = requests.post(f"{cluster_url}/{idx}/_search",
                          json={"query": {"match_all": {}}, "size": 100})
        assert r.status_code == 200
        assert len(r.json()["hits"]["hits"]) >= 10, "Data not written before restart"

        # Restart cluster (keep volumes — no -v flag).
        _run(f"docker compose -f {COMPOSE_FILE} down", cwd=PROJECT_ROOT)
        _run(f"docker compose -f {COMPOSE_FILE} up -d", cwd=PROJECT_ROOT)

        # Wait for all meta nodes to be healthy.
        meta_urls = ["http://localhost:9201", "http://localhost:9202", "http://localhost:9203"]
        for url in meta_urls:
            assert _wait_healthy(url, timeout=120), f"{url} not healthy after restart"

        # Wait for Raft leader election (use is_leader field).
        leader_url = _find_leader(meta_urls, timeout=120)
        assert leader_url is not None, "No leader elected after restart"

        # Wait for shard nodes to register with the leader.
        assert _wait_shards(leader_url, count=2, timeout=120), "Shards not registered after restart"

        # Give extra time for gRPC connections to establish.
        time.sleep(5)

        # Verify index still exists.
        r2 = requests.get(f"{leader_url}/_cat/indices")
        assert r2.status_code == 200
        assert idx in r2.text, f"Index {idx} not found in _cat/indices after restart"

        # Verify docs are still accessible.
        r3 = requests.post(f"{leader_url}/{idx}/_search", json={
            "query": {"match_all": {}}, "size": 100
        })
        assert r3.status_code == 200
        assert len(r3.json()["hits"]["hits"]) >= 10, "Docs not accessible after restart"

        # Cleanup.
        requests.delete(f"{leader_url}/{idx}")

    def test_shard_data_persistence(self, cluster_url, shard_containers):
        """Write data, stop all shard nodes, restart them, verify data."""
        idx = "test-rec-2"
        requests.put(f"{cluster_url}/{idx}", json={
            "settings": {"number_of_shards": 2},
            "mappings": {"properties": {"title": {"type": "text"}}}
        })
        for i in range(20):
            requests.post(f"{cluster_url}/{idx}/_doc/sd-{i:03d}?refresh=true",
                          json={"title": f"shard persist {i}"})

        # Stop all shard nodes.
        for c in shard_containers:
            subprocess.run(["docker", "stop", c], capture_output=True, timeout=30)
        time.sleep(5)

        # Restart all shard nodes.
        for c in shard_containers:
            subprocess.run(["docker", "start", c], capture_output=True, timeout=30)

        # Wait for shard health.
        for port in [9211, 9212]:
            _wait_healthy(f"http://localhost:{port}", timeout=60)

        # Wait for re-registration.
        time.sleep(10)

        # Verify data is accessible.
        r = requests.post(f"{cluster_url}/{idx}/_search", json={
            "query": {"match_all": {}}, "size": 100
        })
        assert r.status_code == 200
        assert len(r.json()["hits"]["hits"]) >= 20

        # Cleanup.
        requests.delete(f"{cluster_url}/{idx}")
