"""
Test 20: Flush and force merge.
"""
import json
import requests

class TestFlushForceMerge:
    def test_refresh(self, base_url, index_name):
        """POST /{index}/_refresh refreshes the index."""
        # Write a doc without refresh
        requests.put(f"{base_url}/{index_name}/_doc/r1", json={"title": "refresh me"})
        # Explicit refresh
        r = requests.post(f"{base_url}/{index_name}/_refresh")
        assert r.status_code == 200

    def test_flush(self, base_url, index_name):
        """POST /{index}/_flush flushes the index."""
        lines = []
        for i in range(3):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"fl{i}"}}))
            lines.append(json.dumps({"title": f"flush doc {i}"}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        r = requests.post(f"{base_url}/{index_name}/_flush")
        assert r.status_code == 200

    def test_flush_all(self, base_url, index_name):
        """POST /_flush flushes all indices."""
        r = requests.post(f"{base_url}/_flush")
        assert r.status_code == 200

    def test_forcemerge(self, base_url, index_name):
        """POST /{index}/_forcemerge triggers force merge."""
        # Write some docs
        lines = []
        for i in range(10):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"fm{i}"}}))
            lines.append(json.dumps({"title": f"merge doc {i}"}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        r = requests.post(f"{base_url}/{index_name}/_forcemerge")
        assert r.status_code == 200

    def test_forcemerge_max_segments(self, base_url, index_name):
        """POST /{index}/_forcemerge with max_num_segments."""
        lines = []
        for i in range(5):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"fm2{i}"}}))
            lines.append(json.dumps({"title": f"merge2 doc {i}"}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        r = requests.post(f"{base_url}/{index_name}/_forcemerge?max_num_segments=1")
        assert r.status_code == 200
