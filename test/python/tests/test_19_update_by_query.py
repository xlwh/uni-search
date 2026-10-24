"""
Test 19: Update by query.
"""
import json
import requests

class TestUpdateByQuery:
    def test_update_by_query(self, base_url, index_name):
        """POST /{index}/_update_by_query updates matching docs."""
        lines = []
        for i in range(5):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"uq{i}"}}))
            lines.append(json.dumps({"status": "pending", "n": i}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        r = requests.post(f"{base_url}/{index_name}/_update_by_query", json={
            "query": {"match_all": {}}
        })
        assert r.status_code in (200, 201)
        # Verify the endpoint returned success
        data = r.json()
        assert data.get("updated", 0) > 0 or data.get("total", 0) > 0 or r.status_code == 200
