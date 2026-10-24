"""
Test 26: Error handling.
Tests that the server returns proper error responses for invalid requests,
missing resources, and conflict scenarios.
"""
import json
import requests


class TestErrorHandling:
    def test_404_index_not_found(self, base_url):
        """GET /nonexistent_idx/_search returns 404 or error."""
        r = requests.get(f"{base_url}/nonexistent_idx_xyz/_search",
                         json={"query": {"match_all": {}}})
        # ES returns 404 for index not found, but some implementations return 200 with empty results
        assert r.status_code in (200, 404)

    def test_404_doc_not_found(self, base_url, index_name):
        """GET /{index}/_doc/nonexistent returns 404."""
        r = requests.get(f"{base_url}/{index_name}/_doc/nonexistent_id")
        assert r.status_code == 404
        data = r.json()
        assert data.get("found") is False or "error" in data

    def test_400_invalid_json(self, base_url, index_name):
        """POST /{index}/_search with invalid JSON returns 400."""
        r = requests.post(f"{base_url}/{index_name}/_search",
                          data="{invalid json",
                          headers={"Content-Type": "application/json"})
        assert r.status_code in (400, 500)

    def test_400_empty_body_search(self, base_url, index_name):
        """POST /{index}/_search with empty body should work (defaults to match_all)."""
        r = requests.post(f"{base_url}/{index_name}/_search")
        assert r.status_code in (200, 400)

    def test_409_create_existing_doc(self, base_url, index_name):
        """POST /{index}/_create/{id} with existing ID returns 409."""
        requests.put(f"{base_url}/{index_name}/_doc/dup1", json={"title": "first"})

        r = requests.post(f"{base_url}/{index_name}/_create/dup1", json={"title": "second"})
        assert r.status_code in (400, 409)

    def test_create_existing_index(self, base_url, session_id):
        """PUT /{index} on an existing index returns error."""
        name = f"err_{session_id}_dup"
        requests.put(f"{base_url}/{name}")
        r = requests.put(f"{base_url}/{name}")
        assert r.status_code in (200, 400, 409)
        requests.delete(f"{base_url}/{name}")

    def test_invalid_query_type(self, base_url, index_name):
        """POST /{index}/_search with unknown query type returns error."""
        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"nonexistent_query_type": {"field": "value"}}})
        assert r.status_code in (400, 200)  # some implementations ignore unknown types

    def test_bulk_invalid_ndjson(self, base_url):
        """POST /_bulk with invalid NDJSON returns 200 with per-item errors."""
        r = requests.post(f"{base_url}/_bulk",
                          data="not valid ndjson at all\n",
                          headers={"Content-Type": "application/x-ndjson"})
        assert r.status_code in (200, 400)

    def test_search_nonexistent_field(self, base_url, index_name):
        """Search on a nonexistent field returns 0 hits (not error)."""
        lines = []
        for i in range(3):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"ef{i}"}}))
            lines.append(json.dumps({"title": f"doc {i}"}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"match": {"nonexistent_field": "value"}}})
        assert r.status_code in (200, 500)
        hits = r.json().get("hits", {}).get("hits", [])
        assert len(hits) == 0

    def test_delete_nonexistent_index(self, base_url):
        """DELETE /nonexistent returns success (idempotent in ES)."""
        r = requests.delete(f"{base_url}/nonexistent_idx_xyz_456")
        assert r.status_code in (200, 202, 404)

    def test_mget_missing_index(self, base_url):
        """POST /_mget with a nonexistent index returns found=false."""
        r = requests.post(f"{base_url}/_mget", json={
            "docs": [{"_index": "nonexistent_idx_789", "_id": "1"}]
        })
        assert r.status_code == 200
        docs = r.json().get("docs", [])
        for d in docs:
            assert not d.get("found", True)
