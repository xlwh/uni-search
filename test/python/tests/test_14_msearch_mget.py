"""
Test 14: Multi-search and multi-get.
"""
import json
import requests

class TestMSearchMGet:
    def test_msearch(self, base_url, index_name):
        """POST /_msearch executes multiple searches."""
        # Index some docs
        lines = []
        for i in range(6):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"ms{i}"}}))
            lines.append(json.dumps({"title": f"doc {i}", "cat": "even" if i % 2 == 0 else "odd"}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        # msearch body: header + query pairs
        msearch_body = "\n".join([
            json.dumps({"index": index_name}),
            json.dumps({"query": {"match_all": {}}, "size": 3}),
            json.dumps({"index": index_name}),
            json.dumps({"query": {"term": {"cat": "even"}}}),
        ]) + "\n"
        r = requests.post(f"{base_url}/_msearch", data=msearch_body,
                          headers={"Content-Type": "application/x-ndjson"})
        assert r.status_code == 200
        data = r.json()
        assert "responses" in data
        assert len(data["responses"]) == 2
        # First search: 3 docs
        assert len(data["responses"][0]["hits"]["hits"]) == 3
        # Second search: even docs
        assert len(data["responses"][1]["hits"]["hits"]) == 3

    def test_mget(self, base_url, index_name):
        """POST /_mget retrieves multiple documents."""
        lines = []
        for i in range(5):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"mg{i}"}}))
            lines.append(json.dumps({"title": f"mget doc {i}"}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        r = requests.post(f"{base_url}/_mget", json={
            "docs": [
                {"_index": index_name, "_id": "mg0"},
                {"_index": index_name, "_id": "mg1"},
                {"_index": index_name, "_id": "mg2"},
            ]
        })
        assert r.status_code == 200
        data = r.json()
        docs = data.get("docs", [])
        assert len(docs) == 3
        assert all(d.get("found", False) for d in docs)

    def test_mget_missing(self, base_url, index_name):
        """POST /_mget handles missing documents."""
        r = requests.post(f"{base_url}/_mget", json={
            "docs": [
                {"_index": index_name, "_id": "missing1"},
                {"_index": index_name, "_id": "missing2"},
            ]
        })
        assert r.status_code == 200
        docs = r.json().get("docs", [])
        for d in docs:
            assert not d.get("found", True)
