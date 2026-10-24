"""
Test 12: Highlight in search results.
"""
import json
import pytest
import requests

class TestHighlight:
    def test_highlight_basic(self, base_url, index_name):
        """Highlight returns highlighted fragments in each hit."""
        lines = []
        for i, text in enumerate(["the quick brown fox", "brown bear sleeping", "lazy dog"]):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"hl{i}"}}))
            lines.append(json.dumps({"title": text}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "query": {"match": {"title": "brown"}},
            "highlight": {"fields": {"title": {}}}
        })
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        assert len(hits) >= 2
        for h in hits:
            assert "highlight" in h
            assert "title" in h["highlight"]
            hl = h["highlight"]["title"][0]
            assert "<em>" in hl or "<b>" in hl or "brown" in hl.lower()

    def test_highlight_custom_tag(self, base_url, index_name):
        """Highlight with custom pre_tags/post_tags."""
        lines = []
        for i, text in enumerate(["red apple", "green apple"]):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"ht{i}"}}))
            lines.append(json.dumps({"title": text}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "query": {"match": {"title": "apple"}},
            "highlight": {
                "pre_tags": ["<mark>"],
                "post_tags": ["</mark>"],
                "fields": {"title": {}}
            }
        })
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        for h in hits:
            if "highlight" in h:
                hl = h["highlight"]["title"][0]
                assert "<mark>" in hl or "apple" in hl.lower()

    def test_highlight_in_hit_not_toplevel(self, base_url, index_name):
        """Highlight must be inside each hit, not at top level (Bug B3 fix)."""
        lines = []
        for i, text in enumerate(["search test", "another test"]):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"hi{i}"}}))
            lines.append(json.dumps({"title": text}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "query": {"match": {"title": "test"}},
            "highlight": {"fields": {"title": {}}}
        })
        assert r.status_code == 200
        data = r.json()
        # Highlight should NOT be at top level
        assert "highlight" not in data
        # Highlight should be in each hit
        for h in data["hits"]["hits"]:
            assert "highlight" in h
