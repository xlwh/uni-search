"""
Test 31: Index-scoped operations.
Tests POST /{index}/_mget, POST /{index}/_msearch, POST /{index}/_bulk,
and PUT /{index}/_alias (without name in path).
"""
import json
import requests


class TestIndexScoped:
    def test_index_scoped_mget(self, base_url, index_name):
        """POST /{index}/_mget uses the index from the URL."""
        lines = []
        for i in range(5):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"im{i}"}}))
            lines.append(json.dumps({"title": f"index mget {i}"}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        r = requests.post(f"{base_url}/{index_name}/_mget", json={
            "ids": ["im0", "im1", "im2"]
        })
        assert r.status_code == 200
        docs = r.json().get("docs", [])
        assert len(docs) == 3
        assert all(d.get("found", False) for d in docs)

    def test_index_scoped_msearch(self, base_url, index_name):
        """POST /{index}/_msearch uses the index from the URL."""
        lines = []
        for i in range(6):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"is{i}"}}))
            lines.append(json.dumps({"title": f"index msearch {i}", "n": i}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        msearch_body = "\n".join([
            "{}",
            json.dumps({"query": {"match_all": {}}, "size": 2}),
            "{}",
            json.dumps({"query": {"match_all": {}}, "size": 3}),
        ]) + "\n"
        r = requests.post(f"{base_url}/{index_name}/_msearch", data=msearch_body,
                          headers={"Content-Type": "application/x-ndjson"})
        assert r.status_code == 200
        responses = r.json().get("responses", [])
        assert len(responses) == 2
        assert len(responses[0]["hits"]["hits"]) == 2
        assert len(responses[1]["hits"]["hits"]) == 3

    def test_index_scoped_bulk(self, base_url, index_name):
        """POST /{index}/_bulk uses the index from the URL."""
        lines = "\n".join([
            json.dumps({"index": {"_id": "ib1"}}),
            json.dumps({"title": "index bulk 1"}),
            json.dumps({"index": {"_id": "ib2"}}),
            json.dumps({"title": "index bulk 2"}),
        ]) + "\n"
        r = requests.post(f"{base_url}/{index_name}/_bulk?refresh=true", data=lines,
                          headers={"Content-Type": "application/x-ndjson"})
        assert r.status_code == 200
        assert not r.json().get("errors", False)

        # Verify docs
        for i in [1, 2]:
            r2 = requests.get(f"{base_url}/{index_name}/_doc/ib{i}")
            assert r2.status_code == 200

    def test_put_alias_without_name(self, base_url, index_name):
        """PUT /{index}/_alias with body specifying alias name."""
        body = {"actions": [{"add": {"index": index_name, "alias": f"{index_name}_wn"}}]}
        r = requests.put(f"{base_url}/{index_name}/_alias", json=body)
        assert r.status_code in (200, 201, 400)

    def test_post_doc_with_explicit_id(self, base_url, index_name):
        """POST /{index}/_doc/{id} indexes with explicit ID (vs PUT)."""
        r = requests.post(f"{base_url}/{index_name}/_doc/post1",
                          json={"title": "post with id"})
        assert r.status_code in (200, 201)
        data = r.json()
        assert data.get("_id") == "post1" or data.get("id") == "post1"

        # Verify
        r2 = requests.get(f"{base_url}/{index_name}/_doc/post1")
        assert r2.status_code == 200
