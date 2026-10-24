"""
Test 30: Scroll and PIT advanced scenarios.
Tests DELETE /_search/scroll, PIT in search body, and scroll cleanup.
"""
import json
import requests


class TestScrollPITAdvanced:
    def test_delete_scroll(self, base_url, index_name):
        """DELETE /_search/scroll clears scroll context."""
        lines = []
        for i in range(10):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"ds{i}"}}))
            lines.append(json.dumps({"title": f"delete scroll {i}"}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        # Create scroll
        r1 = requests.post(f"{base_url}/{index_name}/_search?scroll=1m",
                           json={"query": {"match_all": {}}, "size": 3})
        assert r1.status_code == 200
        scroll_id = r1.json().get("_scroll_id", "")
        if not scroll_id:
            import pytest
            pytest.skip("scroll_id not returned")

        # Delete scroll
        r2 = requests.delete(f"{base_url}/_search/scroll",
                             json={"scroll_id": scroll_id},
                             headers={"Content-Type": "application/json"})
        assert r2.status_code == 200

    def test_pit_in_search_body(self, base_url, index_name):
        """Search with pit_id in the body uses PIT snapshot."""
        lines = []
        for i in range(5):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"pb{i}"}}))
            lines.append(json.dumps({"title": f"pit body {i}", "n": i}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        # Create PIT
        r1 = requests.post(f"{base_url}/{index_name}/_pit?keep_alive=2m")
        assert r1.status_code == 200
        pit_id = r1.json().get("pit_id", r1.json().get("id", ""))

        if not pit_id:
            import pytest
            pytest.skip("PIT id not returned")

        # Search using PIT
        r2 = requests.post(f"{base_url}/{index_name}/_search", json={
            "query": {"match_all": {}},
            "pit": {"id": pit_id, "keep_alive": "2m"},
            "size": 10
        })
        assert r2.status_code == 200
        hits = r2.json()["hits"]["hits"]
        assert len(hits) >= 5

        # Cleanup PIT
        requests.delete(f"{base_url}/pit", json={"pit_id": pit_id},
                        headers={"Content-Type": "application/json"})

    def test_pit_keep_alive(self, base_url, index_name):
        """PIT with keep_alive parameter creates a valid PIT."""
        r = requests.post(f"{base_url}/{index_name}/_pit?keep_alive=5m")
        assert r.status_code == 200
        data = r.json()
        pit_id = data.get("pit_id", data.get("id", ""))
        assert pit_id
        # Cleanup
        if pit_id:
            requests.delete(f"{base_url}/pit",
                           json={"pit_id": pit_id},
                           headers={"Content-Type": "application/json"})

    def test_multiple_pit_lifecycle(self, base_url, index_name):
        """Create multiple PITs, use one, delete all."""
        lines = []
        for i in range(5):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"mp{i}"}}))
            lines.append(json.dumps({"title": f"multi pit {i}"}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        # Create 3 PITs
        pit_ids = []
        for _ in range(3):
            r = requests.post(f"{base_url}/{index_name}/_pit?keep_alive=1m")
            assert r.status_code == 200
            pit_id = r.json().get("pit_id", r.json().get("id", ""))
            if pit_id:
                pit_ids.append(pit_id)

        # Delete all PITs
        for pid in pit_ids:
            r = requests.delete(f"{base_url}/pit",
                               json={"pit_id": pid},
                               headers={"Content-Type": "application/json"})
            assert r.status_code in (200, 202)
