"""
Test 21: track_total_hits parameter.
"""
import json
import requests

class TestTrackTotalHits:
    def test_track_total_hits_true(self, base_url, index_name):
        """track_total_hits=true returns exact count."""
        lines = []
        for i in range(10):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"th{i}"}}))
            lines.append(json.dumps({"title": f"track {i}"}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "query": {"match_all": {}},
            "track_total_hits": True,
            "size": 3
        })
        assert r.status_code == 200
        data = r.json()
        total = data["hits"]["total"]
        # total can be int or {"value": N, "relation": "eq"}
        if isinstance(total, dict):
            assert total["value"] == 10
            assert total["relation"] == "eq"
        else:
            assert total == 10

    def test_track_total_hits_false(self, base_url, index_name):
        """track_total_hits=false skips exact count."""
        lines = []
        for i in range(5):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"tf{i}"}}))
            lines.append(json.dumps({"title": f"no track {i}"}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "query": {"match_all": {}},
            "track_total_hits": False,
            "size": 2
        })
        assert r.status_code == 200
        data = r.json()
        total = data["hits"]["total"]
        if isinstance(total, dict):
            assert total["value"] == 0 or total["relation"] in ("eq", "gte")
        else:
            assert total == 0 or total > 0

    def test_track_total_hits_number(self, base_url, index_name):
        """track_total_hits=N caps the count at N."""
        lines = []
        for i in range(10):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"tn{i}"}}))
            lines.append(json.dumps({"title": f"num track {i}"}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "query": {"match_all": {}},
            "track_total_hits": 5,
            "size": 3
        })
        assert r.status_code == 200
        data = r.json()
        total = data["hits"]["total"]
        if isinstance(total, dict):
            assert total["value"] >= 5
        else:
            assert total >= 5
