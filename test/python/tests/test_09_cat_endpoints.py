"""
Test 09: _cat endpoints.
"""
import requests

class TestCatEndpoints:
    def test_cat_indices(self, base_url, index_name):
        """GET /_cat/indices returns index list."""
        r = requests.get(f"{base_url}/_cat/indices?format=json")
        assert r.status_code == 200
        data = r.json()
        assert isinstance(data, list)
        assert len(data) > 0

    def test_cat_indices_for_specific(self, base_url, index_name):
        """GET /_cat/indices/{index} returns specific index."""
        r = requests.get(f"{base_url}/_cat/indices/{index_name}?format=json")
        assert r.status_code == 200
        data = r.json()
        assert len(data) >= 1

    def test_cat_indices_text(self, base_url):
        """GET /_cat/indices returns text format by default."""
        r = requests.get(f"{base_url}/_cat/indices")
        assert r.status_code == 200
        assert len(r.text) > 0

    def test_cat_shards(self, base_url, index_name):
        """GET /_cat/shards returns shard info."""
        r = requests.get(f"{base_url}/_cat/shards?format=json")
        assert r.status_code == 200
        data = r.json()
        assert isinstance(data, list)
        assert len(data) > 0

    def test_cat_nodes(self, base_url):
        """GET /_cat/nodes returns node info."""
        r = requests.get(f"{base_url}/_cat/nodes?format=json")
        assert r.status_code == 200
        data = r.json()
        assert isinstance(data, list)
        assert len(data) >= 1

    def test_cat_health(self, base_url):
        """GET /_cat/health returns health info."""
        r = requests.get(f"{base_url}/_cat/health")
        assert r.status_code == 200
        lines = r.text.strip().split("\n")
        assert len(lines) >= 1

    def test_cat_health_json(self, base_url):
        """GET /_cat/health?format=json returns JSON."""
        r = requests.get(f"{base_url}/_cat/health?format=json")
        assert r.status_code == 200
        data = r.json()
        assert isinstance(data, list)
        assert len(data) >= 1
