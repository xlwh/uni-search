"""
Test 23: Count API.
"""
import json
import requests

class TestCount:
    def test_count_empty(self, base_url, index_name):
        """GET /{index}/_count on empty index returns 0."""
        r = requests.get(f"{base_url}/{index_name}/_count")
        assert r.status_code == 200
        data = r.json()
        assert data.get("count", 0) == 0

    def test_count_with_docs(self, base_url, index_name):
        """GET /{index}/_count returns document count."""
        lines = []
        for i in range(7):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"ct{i}"}}))
            lines.append(json.dumps({"title": f"count doc {i}"}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        r = requests.get(f"{base_url}/{index_name}/_count")
        assert r.status_code == 200
        data = r.json()
        assert data.get("count", 0) == 7

    def test_count_with_query(self, base_url, index_name):
        """POST /{index}/_count with query filter."""
        lines = []
        for i in range(10):
            status = "active" if i < 4 else "inactive"
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"cq{i}"}}))
            lines.append(json.dumps({"status": status, "n": i}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        r = requests.post(f"{base_url}/{index_name}/_count", json={
            "query": {"term": {"status": "active"}}
        })
        assert r.status_code == 200
        data = r.json()
        assert data.get("count", 0) == 4

    def test_count_all(self, base_url, index_name):
        """POST /_count counts across all indices."""
        # Ensure at least one doc exists
        requests.put(f"{base_url}/{index_name}/_doc/ca1", json={"title": "all count"})
        requests.post(f"{base_url}/{index_name}/_refresh")
        r = requests.post(f"{base_url}/_count", json={"query": {"match_all": {}}})
        assert r.status_code == 200
        assert r.json().get("count", 0) > 0
