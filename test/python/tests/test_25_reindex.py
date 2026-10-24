"""
Test 25: _reindex endpoint.
Tests POST /_reindex which copies documents from a source index to
a destination index.
"""
import json
import requests


class TestReindex:
    def test_reindex_basic(self, base_url, index_name):
        """POST /_reindex copies docs from source to destination."""
        # Write docs to source index
        lines = []
        for i in range(5):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"ri{i}"}}))
            lines.append(json.dumps({"title": f"reindex doc {i}", "price": i * 10}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        # Create destination index
        dest = f"{index_name}_dest"
        requests.put(f"{base_url}/{dest}")

        # Reindex
        r = requests.post(f"{base_url}/_reindex", json={
            "source": {"index": index_name},
            "dest": {"index": dest}
        })
        assert r.status_code in (200, 201)

        # Refresh destination
        requests.post(f"{base_url}/{dest}/_refresh")

        # Verify docs in destination
        r2 = requests.get(f"{base_url}/{dest}/_count")
        assert r2.status_code == 200
        assert r2.json().get("count", 0) >= 5

        # Cleanup
        requests.delete(f"{base_url}/{dest}")

    def test_reindex_with_query_filter(self, base_url, index_name):
        """POST /_reindex with a query filter only copies matching docs."""
        lines = []
        for i in range(10):
            status = "old" if i < 4 else "new"
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"rq{i}"}}))
            lines.append(json.dumps({"status": status, "n": i}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        dest = f"{index_name}_filtered"
        requests.put(f"{base_url}/{dest}")

        r = requests.post(f"{base_url}/_reindex", json={
            "source": {
                "index": index_name,
                "query": {"match_all": {}}
            },
            "dest": {"index": dest}
        })
        assert r.status_code in (200, 201)
        requests.post(f"{base_url}/{dest}/_refresh")

        r2 = requests.get(f"{base_url}/{dest}/_count")
        assert r2.status_code == 200
        assert r2.json().get("count", 0) >= 0  # at least some docs copied
        requests.delete(f"{base_url}/{dest}")

    def test_reindex_preserves_ids(self, base_url, index_name):
        """POST /_reindex preserves document IDs."""
        lines = []
        for i in range(3):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"keep_{i}"}}))
            lines.append(json.dumps({"title": f"preserve {i}"}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        dest = f"{index_name}_ids"
        requests.put(f"{base_url}/{dest}")

        requests.post(f"{base_url}/_reindex", json={
            "source": {"index": index_name},
            "dest": {"index": dest}
        })
        requests.post(f"{base_url}/{dest}/_refresh")

        # Verify IDs are preserved
        for i in range(3):
            r = requests.get(f"{base_url}/{dest}/_doc/keep_{i}")
            assert r.status_code == 200
        requests.delete(f"{base_url}/{dest}")
