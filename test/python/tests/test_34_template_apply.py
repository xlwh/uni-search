"""
Test 34: Index template auto-application.
Tests that index templates are automatically applied when a new index
matching the template pattern is created.
"""
import json
import requests


class TestTemplateApply:
    def test_template_auto_apply_mapping(self, base_url, session_id):
        """PUT template → create matching index → verify mapping applied."""
        pattern = f"tpl_{session_id}_*"
        template_name = f"my_template_{session_id}"

        # Create index template
        template_body = {
            "index_patterns": [pattern],
            "template": {
                "mappings": {
                    "properties": {
                        "timestamp": {"type": "keyword"},
                        "level": {"type": "keyword"},
                        "message": {"type": "text"},
                        "host": {"type": "keyword"}
                    }
                }
            }
        }
        r = requests.put(f"{base_url}/_index_template", json=template_body)
        assert r.status_code in (200, 201)

        # Create a matching index
        idx = f"tpl_{session_id}_logs"
        r2 = requests.put(f"{base_url}/{idx}")
        assert r2.status_code in (200, 201)

        # Write a document
        doc = {"timestamp": "2026-01-01", "level": "info", "message": "hello", "host": "server1"}
        requests.put(f"{base_url}/{idx}/_doc/t1", json=doc)
        requests.post(f"{base_url}/{idx}/_refresh")

        # Verify the document is searchable
        r3 = requests.post(f"{base_url}/{idx}/_search",
                          json={"query": {"match_all": {}}})
        assert r3.status_code == 200
        assert len(r3.json()["hits"]["hits"]) == 1

        # Verify the mapping is available
        r4 = requests.get(f"{base_url}/{idx}/_mapping")
        assert r4.status_code == 200

        # Cleanup
        requests.delete(f"{base_url}/{idx}")

    def test_template_not_applied_to_non_matching(self, base_url, session_id):
        """Template is NOT applied to non-matching indices."""
        pattern = f"nomatch_{session_id}_*"
        template_body = {
            "index_patterns": [pattern],
            "template": {
                "mappings": {
                    "properties": {"special_field": {"type": "keyword"}}
                }
            }
        }
        requests.put(f"{base_url}/_index_template", json=template_body)

        # Create a non-matching index
        idx = f"other_{session_id}_index"
        requests.put(f"{base_url}/{idx}")

        # Write a doc
        requests.put(f"{base_url}/{idx}/_doc/n1", json={"title": "no template"})
        requests.post(f"{base_url}/{idx}/_refresh")

        # Verify doc is searchable
        r = requests.post(f"{base_url}/{idx}/_search",
                         json={"query": {"match_all": {}}})
        assert r.status_code == 200
        assert len(r.json()["hits"]["hits"]) == 1

        requests.delete(f"{base_url}/{idx}")

    def test_multiple_templates_different_patterns(self, base_url, session_id):
        """Multiple templates with different patterns coexist."""
        for suffix in ["app", "web", "api"]:
            pattern = f"mt_{session_id}_{suffix}_*"
            requests.put(f"{base_url}/_index_template", json={
                "index_patterns": [pattern],
                "template": {
                    "mappings": {
                        "properties": {f"{suffix}_field": {"type": "keyword"}}
                    }
                }
            })

        # Create matching indices
        for suffix in ["app", "web", "api"]:
            idx = f"mt_{session_id}_{suffix}_001"
            r = requests.put(f"{base_url}/{idx}")
            assert r.status_code in (200, 201)
            requests.put(f"{base_url}/{idx}/_doc/1", json={"title": "test"})
            requests.post(f"{base_url}/{idx}/_refresh")
            requests.delete(f"{base_url}/{idx}")
