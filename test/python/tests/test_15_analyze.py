"""
Test 15: Analyze API.
"""
import requests

class TestAnalyze:
    def test_analyze_default(self, base_url):
        """POST /_analyze tokenizes text."""
        r = requests.post(f"{base_url}/_analyze", json={
            "text": "hello world test"
        })
        assert r.status_code == 200
        data = r.json()
        tokens = data.get("tokens", [])
        assert len(tokens) >= 1

    def test_analyze_with_analyzer(self, base_url):
        """POST /_analyze with explicit analyzer."""
        r = requests.post(f"{base_url}/_analyze", json={
            "text": "The Quick Brown Fox",
            "analyzer": "standard"
        })
        assert r.status_code == 200
        tokens = r.json().get("tokens", [])
        assert len(tokens) >= 1

    def test_analyze_with_field(self, base_url, index_name):
        """POST /{index}/_analyze with field mapping."""
        requests.put(f"{base_url}/{index_name}", json={
            "mappings": {"properties": {"text": {"type": "text"}}}
        })
        r = requests.post(f"{base_url}/{index_name}/_analyze", json={
            "text": "some text to analyze",
            "field": "text"
        })
        assert r.status_code == 200
        tokens = r.json().get("tokens", [])
        assert len(tokens) >= 1
