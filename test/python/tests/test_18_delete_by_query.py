"""
Test 18: Delete by query.
"""
import json
import requests

class TestDeleteByQuery:
    def test_delete_by_query(self, base_url, index_name):
        """POST /{index}/_delete_by_query removes matching docs."""
        lines = []
        for i in range(10):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"dq{i}"}}))
            lines.append(json.dumps({"status": "old" if i < 5 else "new", "n": i}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        r = requests.post(f"{base_url}/{index_name}/_delete_by_query", json={
            "query": {"match_all": {}}
        })
        assert r.status_code in (200, 201)
        # Verify all docs deleted
        r2 = requests.get(f"{base_url}/{index_name}/_count")
        assert r2.status_code == 200
        count = r2.json().get("count", 0)
        assert count == 0
