"""
Test 24: _explain endpoint.
Tests GET /{index}/_explain/{id} which explains why a document matches
(or doesn't match) a query.
"""
import json
import requests


class TestExplain:
    def test_explain_matching_doc(self, base_url, index_name):
        """GET /{index}/_explain/{id} explains why a doc matches."""
        lines = []
        for i in range(3):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"ex{i}"}}))
            lines.append(json.dumps({"title": f"hello world {i}", "status": "active"}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        r = requests.get(f"{base_url}/{index_name}/_explain/ex0",
                        json={"query": {"match": {"title": "hello"}}})
        assert r.status_code == 200
        data = r.json()
        assert "matched" in data
        assert data["matched"] is True

    def test_explain_non_matching_doc(self, base_url, index_name):
        """GET /{index}/_explain/{id} for a doc that doesn't match."""
        lines = []
        for i in range(2):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"nx{i}"}}))
            lines.append(json.dumps({"title": "cooking recipe", "status": "active"}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        r = requests.get(f"{base_url}/{index_name}/_explain/nx0",
                        json={"query": {"match": {"title": "programming"}}})
        assert r.status_code == 200
        data = r.json()
        assert "matched" in data
        assert data["matched"] is False

    def test_explain_nonexistent_doc(self, base_url, index_name):
        """GET /{index}/_explain/{id} for a nonexistent doc."""
        r = requests.get(f"{base_url}/{index_name}/_explain/nonexistent",
                        json={"query": {"match_all": {}}})
        assert r.status_code in (200, 404)

    def test_explain_with_term_query(self, base_url, index_name):
        """GET /{index}/_explain/{id} with a term query."""
        lines = []
        for i in range(3):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"et{i}"}}))
            lines.append(json.dumps({"status": "active" if i < 2 else "inactive", "n": i}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        r = requests.get(f"{base_url}/{index_name}/_explain/et0",
                        json={"query": {"term": {"status": "active"}}})
        assert r.status_code == 200
        data = r.json()
        assert "matched" in data
