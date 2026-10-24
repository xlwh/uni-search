"""
Test 22: terminate_after parameter.
"""
import json
import requests

class TestTerminateAfter:
    def test_terminate_after(self, base_url, index_name):
        """terminate_after limits collected documents."""
        lines = []
        for i in range(20):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"ta{i}"}}))
            lines.append(json.dumps({"title": f"terminate doc {i}"}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "query": {"match_all": {}},
            "terminate_after": 5,
            "size": 10
        })
        assert r.status_code == 200
        data = r.json()
        hits = data["hits"]["hits"]
        assert len(hits) <= 5
        total = data["hits"]["total"]
        if isinstance(total, dict):
            assert total["value"] <= 5
        else:
            assert total <= 5

    def test_terminate_after_one(self, base_url, index_name):
        """terminate_after=1 returns at most 1 hit."""
        lines = []
        for i in range(5):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"to{i}"}}))
            lines.append(json.dumps({"title": f"one doc {i}"}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "query": {"match_all": {}},
            "terminate_after": 1,
            "size": 10
        })
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        assert len(hits) <= 1
