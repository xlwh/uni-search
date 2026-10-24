"""
Test 13: Index template operations.
"""
import pytest
import requests

class TestIndexTemplate:
    def test_put_index_template(self, base_url):
        """PUT /_index_template creates a template."""
        body = {
            "index_patterns": ["logs-*"],
            "template": {
                "mappings": {
                    "properties": {
                        "timestamp": {"type": "date"},
                        "level": {"type": "keyword"},
                        "message": {"type": "text"}
                    }
                }
            }
        }
        r = requests.put(f"{base_url}/_index_template", json=body)
        assert r.status_code in (200, 201)

    def test_get_index_template(self, base_url):
        """GET /_index_template returns templates."""
        # First create one
        body = {
            "index_patterns": ["metrics-*"],
            "template": {
                "mappings": {
                    "properties": {"value": {"type": "float"}}
                }
            }
        }
        requests.put(f"{base_url}/_index_template", json=body)
        # Get
        r = requests.get(f"{base_url}/_index_template")
        assert r.status_code == 200
        data = r.json()
        assert len(data) > 0

    def test_ilm_policy(self, base_url):
        """PUT/GET /_ilm/policy manages ILM policies."""
        body = {
            "policy": {
                "phases": {
                    "hot": {"actions": {"rollover": {"max_age": "1d"}}},
                    "delete": {"actions": {"delete": {}}}
                }
            }
        }
        r = requests.put(f"{base_url}/_ilm/policy", json=body)
        assert r.status_code in (200, 201)
        r2 = requests.get(f"{base_url}/_ilm/policy")
        assert r2.status_code == 200
        assert len(r2.json()) > 0
