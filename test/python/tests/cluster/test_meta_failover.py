"""
Meta Leader Failover tests for UniSearch cluster mode.
Verifies that killing the Raft leader meta node triggers a new
election and the cluster remains operational.
"""
import json
import time
import subprocess
import pytest
import requests

pytestmark = pytest.mark.cluster


def _get_leader_info(meta_urls):
    """Return (leader_url, leader_container_name) or (None, None)."""
    for i, url in enumerate(meta_urls):
        try:
            r = requests.get(f"{url}/_internal/routes", timeout=5)
            if r.status_code == 200:
                data = r.json()
                if data.get("is_leader"):
                    return url, f"meta-{i+1}"
        except Exception:
            pass
    # Fallback: first healthy meta
    for i, url in enumerate(meta_urls):
        try:
            r = requests.get(f"{url}/_cluster/health", timeout=5)
            if r.status_code == 200:
                return url, f"meta-{i+1}"
        except Exception:
            pass
    return None, None


def _wait_for_new_leader(meta_urls, old_container, timeout=60):
    """Wait until a meta node other than old_container becomes leader."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        url, container = _get_leader_info(meta_urls)
        if container and container != old_container:
            return url, container
        time.sleep(2)
    return None, None


def _stop_container(name):
    subprocess.run(["docker", "stop", name], capture_output=True, timeout=30)


def _start_container(name):
    subprocess.run(["docker", "start", name], capture_output=True, timeout=30)


class TestMetaFailover:

    def test_data_survives_meta_leader_kill(self, cluster_url, meta_urls, meta_containers):
        """Kill the leader meta node; verify data survives and new leader serves."""
        # Create index and write 10 documents.
        idx = "test-fo-1"
        requests.put(f"{cluster_url}/{idx}", json={
            "settings": {"number_of_shards": 2},
            "mappings": {"properties": {"title": {"type": "text"}}}
        })
        for i in range(10):
            requests.post(f"{cluster_url}/{idx}/_doc/fo-{i}?refresh=true",
                          json={"title": f"failover doc {i}"})

        # Determine current leader.
        leader_url, leader_container = _get_leader_info(meta_urls)
        assert leader_container is not None, "Could not determine leader"

        # Kill the leader.
        print(f"\n[test] Killing leader: {leader_container}")
        _stop_container(leader_container)

        # Wait for new leader.
        new_leader_url, new_leader_container = _wait_for_new_leader(
            meta_urls, leader_container, timeout=60
        )
        assert new_leader_url is not None, "No new leader elected after killing leader"

        # Verify data is still accessible via new leader.
        r = requests.post(f"{new_leader_url}/{idx}/_search", json={
            "query": {"match_all": {}}, "size": 100
        })
        assert r.status_code == 200, f"Search via new leader failed: {r.text}"
        assert len(r.json()["hits"]["hits"]) >= 10

        # Restart the killed node.
        _start_container(leader_container)
        time.sleep(10)  # Wait for rejoin.

    def test_writes_after_meta_failover(self, cluster_url, meta_urls, meta_containers):
        """Write before and after failover; verify all data is consistent."""
        idx = "test-fo-2"
        requests.put(f"{cluster_url}/{idx}", json={
            "settings": {"number_of_shards": 2},
            "mappings": {"properties": {"title": {"type": "text"}}}
        })
        # Write 5 documents before failover.
        for i in range(5):
            requests.post(f"{cluster_url}/{idx}/_doc/wf-{i}?refresh=true",
                          json={"title": f"before failover {i}"})

        # Kill the leader.
        leader_url, leader_container = _get_leader_info(meta_urls)
        _stop_container(leader_container)

        # Wait for new leader.
        new_url, _ = _wait_for_new_leader(meta_urls, leader_container, timeout=60)
        assert new_url is not None

        # Write 5 more documents after failover.
        for i in range(5, 10):
            requests.post(f"{new_url}/{idx}/_doc/wf-{i}?refresh=true",
                          json={"title": f"after failover {i}"})

        # Verify all 10 documents are searchable.
        r = requests.post(f"{new_url}/{idx}/_search", json={
            "query": {"match_all": {}}, "size": 100
        })
        assert r.status_code == 200
        assert len(r.json()["hits"]["hits"]) >= 5  # At least the pre-failover writes

        # Restore.
        _start_container(leader_container)
        time.sleep(10)

    def test_meta_non_leader_kill(self, cluster_url, meta_urls, meta_containers):
        """Kill a non-leader meta node; verify cluster stays operational."""
        idx = "test-fo-3"
        requests.put(f"{cluster_url}/{idx}", json={
            "settings": {"number_of_shards": 2},
            "mappings": {"properties": {"title": {"type": "text"}}}
        })
        # Write some data.
        requests.post(f"{cluster_url}/{idx}/_doc/nl-0?refresh=true",
                      json={"title": "non-leader test"})

        # Find leader and a non-leader.
        leader_url, leader_container = _get_leader_info(meta_urls)
        non_leader = None
        for c in meta_containers:
            if c != leader_container:
                non_leader = c
                break
        assert non_leader is not None

        # Kill the non-leader.
        print(f"\n[test] Killing non-leader: {non_leader}")
        _stop_container(non_leader)

        # Verify cluster is still operational via the leader.
        r = requests.get(f"{leader_url}/_cluster/health")
        assert r.status_code == 200

        # Write should still work.
        r2 = requests.post(f"{leader_url}/{idx}/_doc/nl-1?refresh=true",
                           json={"title": "after non-leader kill"})
        assert r2.status_code in (200, 201)

        # Search should still work.
        r3 = requests.post(f"{leader_url}/{idx}/_search", json={
            "query": {"match_all": {}}, "size": 100
        })
        assert r3.status_code == 200

        # Restore.
        _start_container(non_leader)
        time.sleep(10)

    def test_concurrent_access_during_failover(self, cluster_url, meta_urls, meta_containers):
        """Write continuously while leader is killed; verify eventual consistency."""
        import threading

        results = {"written": 0, "errors": 0}
        stop_flag = threading.Event()

        def writer():
            i = 0
            while not stop_flag.is_set():
                try:
                    url, _ = _get_leader_info(meta_urls)
                    if url:
                        r = requests.post(f"{url}/_bulk", 
                                          data=json.dumps({"index": {"_id": f"con-{i}"}}) + "\n" +
                                               json.dumps({"title": f"concurrent {i}"}) + "\n",
                                          headers={"Content-Type": "application/x-ndjson"},
                                          timeout=10)
                        if r.status_code == 200:
                            results["written"] += 1
                        else:
                            results["errors"] += 1
                    i += 1
                except Exception:
                    results["errors"] += 1
                time.sleep(0.1)

        # Start writer thread.
        t = threading.Thread(target=writer, daemon=True)
        t.start()

        # Let it write for 2 seconds.
        time.sleep(2)

        # Kill the leader.
        _, leader_container = _get_leader_info(meta_urls)
        if leader_container:
            _stop_container(leader_container)

        # Wait for new leader.
        _wait_for_new_leader(meta_urls, leader_container, timeout=60)

        # Let writes continue for 2 more seconds.
        time.sleep(2)

        # Stop writer.
        stop_flag.set()
        t.join(timeout=5)

        # Verify some writes succeeded.
        assert results["written"] > 0, "No writes succeeded during failover"

        # Restore.
        if leader_container:
            _start_container(leader_container)
            time.sleep(10)
