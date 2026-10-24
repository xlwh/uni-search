"""
Test 11: PIT (Point In Time) and scroll.
"""
import json
import pytest
import requests

class TestPITScroll:
    def test_create_pit(self, base_url, index_name):
        """POST /{index}/_pit creates a PIT."""
        r = requests.post(f"{base_url}/{index_name}/_pit?keep_alive=1m")
        assert r.status_code == 200
        data = r.json()
        assert "pit_id" in data or "id" in data

    def test_delete_pit(self, base_url, index_name):
        """DELETE /pit deletes a PIT."""
        r1 = requests.post(f"{base_url}/{index_name}/_pit?keep_alive=1m")
        pit_id = r1.json().get("pit_id", r1.json().get("id", ""))
        r2 = requests.delete(f"{base_url}/pit", json={"pit_id": pit_id})
        assert r2.status_code in (200, 202)

    def test_scroll_search(self, base_url, index_name):
        """POST /_search/scroll paginates through results."""
        # Index 20 docs
        lines = []
        for i in range(20):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"sc{i}"}}))
            lines.append(json.dumps({"title": f"scroll doc {i}"}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})
        # Initial search with scroll
        r = requests.post(f"{base_url}/{index_name}/_search?scroll=1m",
                          json={"query": {"match_all": {}}, "size": 5})
        assert r.status_code == 200
        data = r.json()
        scroll_id = data.get("_scroll_id", data.get("scroll_id", ""))
        assert scroll_id
        assert len(data["hits"]["hits"]) == 5
        # Next page
        if scroll_id:
            r2 = requests.post(f"{base_url}/_search/scroll",
                                json={"scroll": "1m", "scroll_id": scroll_id})
            assert r2.status_code == 200
            data2 = r2.json()
            assert len(data2["hits"]["hits"]) == 5

    def test_search_after(self, base_url, index_name):
        """search_after pagination."""
        lines = []
        for i in range(10):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"sa{i}"}}))
            lines.append(json.dumps({"title": f"after {i}", "ts": i}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})
        # First page
        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "query": {"match_all": {}},
            "size": 3,
            "sort": [{"ts": "asc"}]
        })
        assert r.status_code == 200
        data = r.json()
        assert len(data["hits"]["hits"]) == 3
        # Get sort values for search_after
        last = data["hits"]["hits"][-1]
        sort_vals = last.get("sort", [])
        if sort_vals:
            r2 = requests.post(f"{base_url}/{index_name}/_search", json={
                "query": {"match_all": {}},
                "size": 3,
                "sort": [{"ts": "asc"}],
                "search_after": sort_vals
            })
            assert r2.status_code == 200
            data2 = r2.json()
            assert len(data2["hits"]["hits"]) > 0
