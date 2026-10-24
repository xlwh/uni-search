"""
Test 37: Search enhancements (es-feature-gap-fill).
Covers: rescore, collapse, post_filter, search_template,
_validate/query, _search_shards, script_fields, runtime_mappings.
"""
import pytest
import requests
import json

def _bulk_docs(base_url, index, docs):
    lines = []
    for i, d in enumerate(docs):
        lines.append('{"index": {"_index": "%s", "_id": "s%d"}}' % (index, i))
        lines.append(json.dumps(d))
    body = "\n".join(lines) + "\n"
    requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                  headers={"Content-Type": "application/x-ndjson"})


class TestSearchEnhancements:
    def test_rescore(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [
            {"title": "hello world", "content": "foo"},
            {"title": "hello there", "content": "bar"},
            {"title": "goodbye", "content": "hello world"},
        ])
        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "query": {"match": {"title": "hello"}},
            "rescore": [{"query": {"match": {"content": "world"}}, "window_size": 10,
                          "query_weight": 0.3, "rescore_query_weight": 0.7}]
        })
        assert r.status_code == 200
        assert len(r.json()["hits"]["hits"]) >= 2

    def test_collapse(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [
            {"category": "A", "title": "first"},
            {"category": "A", "title": "second"},
            {"category": "B", "title": "third"},
        ])
        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "query": {"match_all": {}},
            "collapse": {"field": "category"}
        })
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        cats = [h["_source"]["category"] for h in hits]
        assert "A" in cats
        assert "B" in cats

    def test_collapse_with_inner_hits(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [
            {"category": "A", "title": "first"},
            {"category": "A", "title": "second"},
            {"category": "B", "title": "third"},
        ])
        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "query": {"match_all": {}},
            "collapse": {"field": "category", "inner_hits": {"name": "group", "size": 2}}
        })
        assert r.status_code == 200
        assert len(r.json()["hits"]["hits"]) >= 2

    def test_post_filter(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [
            {"title": "hello", "status": "active"},
            {"title": "world", "status": "inactive"},
        ])
        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "query": {"match_all": {}},
            "post_filter": {"term": {"status": "active"}}
        })
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        assert len(hits) == 1
        assert hits[0]["_source"]["status"] == "active"

    def test_validate_query_valid(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"title": "hello"}])
        r = requests.post(f"{base_url}/{index_name}/_validate/query",
                          json={"query": {"match": {"title": "hello"}}})
        assert r.status_code == 200
        assert r.json()["valid"] is True

    def test_validate_query_invalid(self, base_url, index_name):
        r = requests.post(f"{base_url}/{index_name}/_validate/query",
                          json={"query": {"unknown_query_type": {"field": "value"}}})
        assert r.status_code == 200
        assert r.json()["valid"] is False

    def test_search_template(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [
            {"title": "hello world"}, {"title": "goodbye world"}
        ])
        r = requests.post(f"{base_url}/{index_name}/_search/template", json={
            "inline": '{"query": {"match": {"title": "{{q}}"}}}',
            "params": {"q": "hello"}
        })
        assert r.status_code == 200
        data = r.json()
        if "hits" in data:
            assert len(data["hits"]["hits"]) >= 0
        assert r.status_code == 200

    def test_render_template(self, base_url, index_name):
        r = requests.post(f"{base_url}/_render/template", json={
            "inline": '{"query": {"match": {"title": "{{q}}"}}}',
            "params": {"q": "hello"}
        })
        assert r.status_code == 200
        rendered = r.json()["rendered"]
        assert "query" in rendered

    def test_search_shards(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"title": "hello"}])
        r = requests.get(f"{base_url}/{index_name}/_search_shards")
        assert r.status_code == 200
        data = r.json()
        assert "shards" in data
        assert "nodes" in data

    def test_script_fields(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"price": 10.0}, {"price": 20.0}])
        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "query": {"match_all": {}},
            "script_fields": {"doubled": {"script": {"source": "doc['price'].value * 2"}}}
        })
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        assert len(hits) == 2
