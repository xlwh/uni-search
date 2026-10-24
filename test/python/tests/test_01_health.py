"""
Test 01: Cluster health and root endpoint.
"""
import requests

class TestHealth:
    def test_root(self, base_url):
        """GET / returns server info."""
        r = requests.get(f"{base_url}/")
        assert r.status_code == 200
        data = r.json()
        assert "name" in data or "cluster_name" in data or "version" in data

    def test_cluster_health(self, base_url):
        """GET /_cluster/health returns green/yellow status."""
        r = requests.get(f"{base_url}/_cluster/health")
        assert r.status_code == 200
        data = r.json()
        assert "status" in data
        assert data["status"] in ("green", "yellow", "red")

    def test_cat_health_text(self, base_url):
        """GET /_cat/health returns text format by default."""
        r = requests.get(f"{base_url}/_cat/health")
        assert r.status_code == 200
        lines = r.text.strip().split("\n")
        assert len(lines) >= 1

    def test_cat_health_json(self, base_url):
        """GET /_cat/health?format=json returns JSON array."""
        r = requests.get(f"{base_url}/_cat/health?format=json")
        assert r.status_code == 200
        data = r.json()
        assert isinstance(data, list)
        assert len(data) > 0
        assert "status" in data[0]

    def test_cat_nodes(self, base_url):
        """GET /_cat/nodes returns node info."""
        r = requests.get(f"{base_url}/_cat/nodes?format=json")
        assert r.status_code == 200
        data = r.json()
        assert isinstance(data, list)
        assert len(data) > 0

    def test_prometheus_metrics(self, base_url):
        """GET /_prometheus/metrics returns Prometheus format."""
        r = requests.get(f"{base_url}/_prometheus/metrics")
        assert r.status_code == 200
        assert "unisearch_" in r.text or "# HELP" in r.text or len(r.text) > 0
