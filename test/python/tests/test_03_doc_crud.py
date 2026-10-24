"""
Test 03: Document CRUD operations.
"""
import requests

class TestDocCRUD:
    def test_index_doc_auto_id(self, base_url, index_name):
        """POST /{index}/_doc indexes a document with auto-generated ID."""
        doc = {"title": "hello world", "price": 99.9}
        r = requests.post(f"{base_url}/{index_name}/_doc", json=doc)
        assert r.status_code in (200, 201)
        data = r.json()
        assert "_id" in data or "id" in data
        assert data.get("result") in ("created", "created_persisted") or data.get("_id")

    def test_index_doc_with_id(self, base_url, index_name):
        """PUT /{index}/_doc/{id} indexes with explicit ID."""
        doc = {"title": "explicit id", "price": 50.0}
        r = requests.put(f"{base_url}/{index_name}/_doc/1", json=doc)
        assert r.status_code in (200, 201)
        data = r.json()
        assert data.get("_id") == "1" or data.get("id") == "1"

    def test_get_doc(self, base_url, index_name):
        """GET /{index}/_doc/{id} retrieves a document."""
        doc = {"title": "get me", "price": 10.0}
        requests.put(f"{base_url}/{index_name}/_doc/42", json=doc)
        r = requests.get(f"{base_url}/{index_name}/_doc/42")
        assert r.status_code == 200
        data = r.json()
        assert data.get("_id") == "42" or data.get("id") == "42"
        src = data.get("_source", data.get("source", {}))
        assert src.get("title") == "get me"

    def test_get_doc_not_found(self, base_url, index_name):
        """GET /{index}/_doc/{id} returns 404 for missing doc."""
        r = requests.get(f"{base_url}/{index_name}/_doc/nonexistent")
        assert r.status_code == 404

    def test_delete_doc(self, base_url, index_name):
        """DELETE /{index}/_doc/{id} removes a document."""
        requests.put(f"{base_url}/{index_name}/_doc/del1", json={"title": "delete me"})
        r = requests.delete(f"{base_url}/{index_name}/_doc/del1")
        assert r.status_code in (200, 202)
        # Verify it's gone
        r2 = requests.get(f"{base_url}/{index_name}/_doc/del1")
        assert r2.status_code == 404

    def test_create_doc_op_type(self, base_url, index_name):
        """POST /{index}/_create/{id} uses op_type=create."""
        doc = {"title": "create only"}
        r = requests.post(f"{base_url}/{index_name}/_create/c1", json=doc)
        assert r.status_code in (200, 201)
        # Second create with same ID should fail
        r2 = requests.post(f"{base_url}/{index_name}/_create/c1", json=doc)
        assert r2.status_code in (400, 409)

    def test_update_by_id(self, base_url, index_name):
        """PUT same ID replaces the document (update)."""
        requests.put(f"{base_url}/{index_name}/_doc/u1", json={"title": "original", "v": 1})
        requests.put(f"{base_url}/{index_name}/_doc/u1", json={"title": "updated", "v": 2})
        r = requests.get(f"{base_url}/{index_name}/_doc/u1")
        assert r.status_code == 200
        data = r.json()
        src = data.get("_source", data.get("source", {}))
        assert src.get("title") == "updated"
        assert src.get("v") == 2
