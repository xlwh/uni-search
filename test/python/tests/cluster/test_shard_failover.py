"""
Shard node Failover tests for UniSearch cluster mode.
Verifies that killing a shard node degrades gracefully and data
recovers when the node restarts.
"""
import json
import time
import subprocess
import pytest
import requests

pytestmark = pytest.mark.cluster


def _stop_container(name):
    subprocess.run(["docker", "stop", name], capture_output=True, timeout=30)


def _start_container(name):
    subprocess.run(["docker", "start", name], capture_output=True, timeout=30)


def _wait_for_shard_healthy(url, timeout=60):
    """Wait for a shard node's health endpoint to return 200."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            r = requests.get(f"{url}/_cluster/health", timeout=5)
            if r.status_code == 200:
                return True
        except Exception:
            pass
        time.sleep(2)
    return False


class TestShardFailover:

    def test_cluster_survives_shard_node_kill(self, cluster_url, shard_containers, shard_urls):
        """Kill a shard node; verify meta still responds and partial data available."""
        # Create 2-shard index and write 20 docs.
        requests.put(f"{cluster_url}/test-sf-1", json={
            "settings": {"number_of_shards": 2},
            "mappings": {"properties": {"title": {"type": "text"}}}
        })
        for i in range(20):
            requests.post(f"{cluster_url}/test-sf-1/_doc/sf-{i:03d}?refresh=true",
                          json={"title": f"shard failover {i}"})

        # Kill shard-1.
        _stop_container("shard-1")
        time.sleep(3)

        # Meta should still respond.
        r = requests.get(f"{cluster_url}/_cluster/health")
        assert r.status_code == 200

        # Restart shard-1.
        _start_container("shard-1")
        assert _wait_for_shard_healthy("http://localhost:9211", timeout=60)

        # Wait for re-registration.
        time.sleep(10)

        # Verify all 20 docs are accessible again.
        r2 = requests.post(f"{cluster_url}/test-sf-1/_search", json={
            "query": {"match_all": {}}, "size": 100
        })
        assert r2.status_code == 200
        assert len(r2.json()["hits"]["hits"]) >= 20

        # Cleanup.
        requests.delete(f"{cluster_url}/test-sf-1")

    def test_writes_during_shard_downtime(self, cluster_url, shard_containers):
        """Write during shard downtime; verify surviving shard accepts writes."""
        requests.put(f"{cluster_url}/test-sf-2", json={
            "settings": {"number_of_shards": 2},
            "mappings": {"properties": {"title": {"type": "text"}}}
        })
        # Initial write.
        requests.post(f"{cluster_url}/test-sf-2/_doc/init-0?refresh=true",
                      json={"title": "initial"})

        # Kill shard-1.
        _stop_container("shard-1")
        time.sleep(3)

        # Try writing — some writes may fail (routed to dead shard),
        # some should succeed (routed to alive shard).
        success_count = 0
        for i in range(10):
            try:
                r = requests.post(f"{cluster_url}/test-sf-2/_doc/dt-{i:03d}",
                                  json={"title": f"downtime {i}"}, timeout=10)
                if r.status_code in (200, 201):
                    success_count += 1
            except Exception:
                pass

        # At least some writes should succeed (those routed to shard-2).
        assert success_count > 0, "No writes succeeded during shard downtime"

        # Restart shard-1.
        _start_container("shard-1")
        assert _wait_for_shard_healthy("http://localhost:9211", timeout=60)
        time.sleep(10)

        # Verify the surviving writes are readable.
        r = requests.get(f"{cluster_url}/test-sf-2/_count")
        assert r.status_code == 200
        assert r.json()["count"] >= 1

        # Cleanup.
        requests.delete(f"{cluster_url}/test-sf-2")

    def test_shard_restart_recovers_data(self, cluster_url, shard_containers):
        """Write data, restart a shard, verify data persists from local disk."""
        requests.put(f"{cluster_url}/test-sf-3", json={
            "settings": {"number_of_shards": 2},
            "mappings": {"properties": {"title": {"type": "text"}}}
        })
        # Write 50 docs.
        lines = []
        for i in range(50):
            lines.append(json.dumps({"index": {"_id": f"rc-{i:03d}"}}))
            lines.append(json.dumps({"title": f"recovery {i}"}))
        requests.post(f"{cluster_url}/test-sf-3/_bulk?refresh=true",
                      data="\n".join(lines) + "\n",
                      headers={"Content-Type": "application/x-ndjson"})

        # Verify count before restart.
        r = requests.post(f"{cluster_url}/test-sf-3/_search", json={"query": {"match_all": {}}, "size": 200})
        assert r.status_code == 200
        assert len(r.json()["hits"]["hits"]) >= 50

        # Restart shard-1.
        _stop_container("shard-1")
        time.sleep(5)
        _start_container("shard-1")
        assert _wait_for_shard_healthy("http://localhost:9211", timeout=60)
        time.sleep(10)

        # Verify data persists from local storage.
        r2 = requests.post(f"{cluster_url}/test-sf-3/_search", json={
            "query": {"match_all": {}}, "size": 100
        })
        assert r2.status_code == 200
        assert r2.json()["hits"]["total"]["value"] >= 50 or len(r2.json()["hits"]["hits"]) >= 50

        # Cleanup.
        requests.delete(f"{cluster_url}/test-sf-3")

    def test_get_specific_doc_after_shard_restart(self, cluster_url, shard_containers):
        """Write known IDs, restart shard, verify each doc is retrievable."""
        requests.put(f"{cluster_url}/test-sf-4", json={
            "settings": {"number_of_shards": 2},
            "mappings": {"properties": {"title": {"type": "text"}}}
        })
        doc_ids = [f"gd-{i:03d}" for i in range(10)]
        for did in doc_ids:
            requests.post(f"{cluster_url}/test-sf-4/_doc/{did}?refresh=true",
                          json={"title": f"doc {did}"})

        # Restart shard-1.
        _stop_container("shard-1")
        time.sleep(3)
        _start_container("shard-1")
        assert _wait_for_shard_healthy("http://localhost:9211", timeout=60)
        time.sleep(10)

        # Verify each doc is retrievable.
        found = 0
        for did in doc_ids:
            r = requests.get(f"{cluster_url}/test-sf-4/_doc/{did}")
            if r.status_code == 200 and r.json().get("found"):
                found += 1
        assert found >= 10, f"Only {found}/10 docs found after shard restart"

        # Cleanup.
        requests.delete(f"{cluster_url}/test-sf-4")
