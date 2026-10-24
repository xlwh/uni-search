"""
Test 02: Index CRUD operations.
"""
import requests

class TestIndexCRUD:
    def test_create_index(self, base_url, session_id):
        """PUT /{index} creates an index."""
        name = f"crud_{session_id}_create"
        r = requests.put(f"{base_url}/{name}")
        assert r.status_code in (200, 201)
        # Verify it appears in _cat/indices
        r2 = requests.get(f"{base_url}/_cat/indices?format=json")
        assert r2.status_code == 200
        indices = r2.json()
        assert any(i.get("index") == name or i.get("Index") == name or i.get("index_name") == name for i in indices), \
            f"Index {name} not found in _cat/indices"
        # Clean up
        requests.delete(f"{base_url}/{name}")

    def test_create_index_with_mappings(self, base_url, session_id):
        """PUT /{index} with explicit mappings."""
        name = f"crud_{session_id}_mapped"
        body = {
            "mappings": {
                "properties": {
                    "title": {"type": "text"},
                    "price": {"type": "float"},
                    "tags": {"type": "keyword"},
                    "embedding": {"type": "knn_vector", "dim": 4}
                }
            }
        }
        r = requests.put(f"{base_url}/{name}", json=body)
        assert r.status_code in (200, 201)
        # Verify mapping
        r2 = requests.get(f"{base_url}/{name}/_mapping")
        assert r2.status_code == 200
        data = r2.json()
        assert name in data or len(data) > 0
        requests.delete(f"{base_url}/{name}")

    def test_delete_index(self, base_url, session_id):
        """DELETE /{index} removes an index."""
        name = f"crud_{session_id}_delete"
        requests.put(f"{base_url}/{name}")
        r = requests.delete(f"{base_url}/{name}")
        assert r.status_code in (200, 202)
        # Verify it's gone
        r2 = requests.get(f"{base_url}/_cat/indices?format=json")
        indices = r2.json()
        assert not any(i.get("index") == name for i in indices)

    def test_put_mapping(self, base_url, session_id):
        """PUT /{index}/_mapping adds fields."""
        name = f"crud_{session_id}_putmap"
        requests.put(f"{base_url}/{name}")
        body = {
            "properties": {
                "new_field": {"type": "keyword"}
            }
        }
        r = requests.put(f"{base_url}/{name}/_mapping", json=body)
        assert r.status_code in (200, 201)
        r2 = requests.get(f"{base_url}/{name}/_mapping")
        assert r2.status_code == 200
        requests.delete(f"{base_url}/{name}")

    def test_get_settings(self, base_url, session_id):
        """GET /{index}/_settings returns index settings."""
        name = f"crud_{session_id}_settings"
        requests.put(f"{base_url}/{name}")
        r = requests.get(f"{base_url}/{name}/_settings")
        assert r.status_code == 200
        data = r.json()
        assert len(data) > 0
        requests.delete(f"{base_url}/{name}")

    def test_put_settings(self, base_url, session_id):
        """PUT /{index}/_settings updates settings."""
        name = f"crud_{session_id}_putset"
        requests.put(f"{base_url}/{name}")
        body = {"index": {"number_of_replicas": 0}}
        r = requests.put(f"{base_url}/{name}/_settings", json=body)
        assert r.status_code in (200, 201)
        requests.delete(f"{base_url}/{name}")
