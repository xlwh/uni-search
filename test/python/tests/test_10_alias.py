"""
Test 10: Alias operations.
"""
import json
import pytest
import requests

class TestAlias:
    def test_create_alias(self, base_url, index_name):
        """PUT /{index}/_alias/{name} creates an alias."""
        alias = f"{index_name}_alias"
        r = requests.put(f"{base_url}/{index_name}/_alias/{alias}")
        assert r.status_code in (200, 201)
        # Search via alias
        r2 = requests.post(f"{base_url}/{alias}/_search",
                          json={"query": {"match_all": {}}})
        assert r2.status_code == 200

    def test_get_aliases(self, base_url, index_name):
        """GET /{index}/_alias returns alias list."""
        alias = f"{index_name}_get"
        requests.put(f"{base_url}/{index_name}/_alias/{alias}")
        r = requests.get(f"{base_url}/{index_name}/_alias")
        assert r.status_code == 200
        data = r.json()
        assert len(data) > 0

    def test_post_aliases_actions(self, base_url, index_name):
        """POST /_aliases with add/remove actions."""
        alias = f"{index_name}_post"
        body = {"actions": [{"add": {"index": index_name, "alias": alias}}]}
        r = requests.post(f"{base_url}/_aliases", json=body)
        assert r.status_code in (200, 201)
        # Verify
        r2 = requests.post(f"{base_url}/{alias}/_search",
                           json={"query": {"match_all": {}}})
        assert r2.status_code == 200

    def test_remove_alias(self, base_url, index_name):
        """Remove alias via POST /_aliases remove action."""
        alias = f"{index_name}_rem"
        requests.put(f"{base_url}/{index_name}/_alias/{alias}")
        body = {"actions": [{"remove": {"index": index_name, "alias": alias}}]}
        r = requests.post(f"{base_url}/_aliases", json=body)
        assert r.status_code in (200, 201)
        # Alias should no longer resolve
        r2 = requests.post(f"{base_url}/{alias}/_search",
                           json={"query": {"match_all": {}}})
        assert r2.status_code in (404, 400)

    def test_alias_routing(self, base_url, index_name):
        """Alias with routing filter."""
        # Create second index
        idx2 = f"{index_name}_2"
        requests.put(f"{base_url}/{idx2}")
        alias = f"{index_name}_multi"
        body = {"actions": [
            {"add": {"index": index_name, "alias": alias}},
            {"add": {"index": idx2, "alias": alias}},
        ]}
        requests.post(f"{base_url}/_aliases", json=body)
        # Search via multi-index alias
        r = requests.post(f"{base_url}/{alias}/_search",
                          json={"query": {"match_all": {}}})
        assert r.status_code == 200
        requests.delete(f"{base_url}/{idx2}")
