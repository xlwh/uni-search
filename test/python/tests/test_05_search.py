"""
Test 05: Full-text search queries.
"""
import pytest
import requests

def _bulk_docs(base_url, index, docs):
    lines = []
    for i, d in enumerate(docs):
        lines.append('{"index": {"_index": "%s", "_id": "s%d"}}' % (index, i))
        lines.append(__import__("json").dumps(d))
    body = "\n".join(lines) + "\n"
    requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                  headers={"Content-Type": "application/x-ndjson"})


class TestSearch:
    def test_match_all(self, base_url, index_name):
        """match_all returns all documents."""
        _bulk_docs(base_url, index_name, [
            {"title": "one"}, {"title": "two"}, {"title": "three"}
        ])
        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"match_all": {}}})
        assert r.status_code == 200
        data = r.json()
        hits = data["hits"]["hits"]
        assert len(hits) == 3

    def test_match(self, base_url, index_name):
        """match query does full-text search."""
        _bulk_docs(base_url, index_name, [
            {"title": "the quick brown fox"},
            {"title": "the lazy dog"},
            {"title": "brown bear"},
        ])
        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"match": {"title": "brown"}}})
        assert r.status_code == 200
        data = r.json()
        hits = data["hits"]["hits"]
        assert len(hits) >= 2
        titles = [h["_source"]["title"] for h in hits]
        assert any("brown" in t for t in titles)

    def test_match_phrase(self, base_url, index_name):
        """match_phrase searches for exact phrase."""
        _bulk_docs(base_url, index_name, [
            {"title": "quick brown fox"},
            {"title": "brown quick animal"},
            {"title": "the brown fox jumps"},
        ])
        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"match_phrase": {"title": "brown fox"}}})
        assert r.status_code == 200
        data = r.json()
        hits = data["hits"]["hits"]
        assert len(hits) >= 1
        for h in hits:
            assert "brown" in h["_source"]["title"].lower()

    def test_term(self, base_url, index_name):
        """term query for exact match."""
        _bulk_docs(base_url, index_name, [
            {"status": "active", "title": "a"},
            {"status": "inactive", "title": "b"},
            {"status": "active", "title": "c"},
        ])
        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"term": {"status": "active"}}})
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        assert len(hits) == 2

    def test_terms(self, base_url, index_name):
        """terms query for multiple values."""
        _bulk_docs(base_url, index_name, [
            {"tag": "red"}, {"tag": "blue"}, {"tag": "green"}, {"tag": "red"},
        ])
        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"terms": {"tag": ["red", "blue"]}}})
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        assert len(hits) == 3

    def test_range(self, base_url, index_name):
        """range query for numeric range."""
        _bulk_docs(base_url, index_name, [
            {"price": 10}, {"price": 50}, {"price": 100}, {"price": 200},
        ])
        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"range": {"price": {"gte": 50, "lte": 100}}}})
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        assert len(hits) == 2

    def test_bool_must(self, base_url, index_name):
        """bool must combines queries."""
        _bulk_docs(base_url, index_name, [
            {"title": "red dress", "in_stock": True},
            {"title": "blue dress", "in_stock": True},
            {"title": "red shirt", "in_stock": False},
        ])
        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "query": {
                "bool": {
                    "must": [
                        {"match": {"title": "red"}},
                        {"term": {"in_stock": True}},
                    ]
                }
            }
        })
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        assert len(hits) == 1
        assert "red dress" in hits[0]["_source"]["title"]

    def test_bool_must_not(self, base_url, index_name):
        """bool must_not excludes documents."""
        _bulk_docs(base_url, index_name, [
            {"title": "apple", "category": "fruit"},
            {"title": "banana", "category": "fruit"},
            {"title": "carrot", "category": "vegetable"},
        ])
        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "query": {
                "bool": {
                    "must": [{"term": {"category": "fruit"}}],
                    "must_not": [{"match": {"title": "apple"}}],
                }
            }
        })
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        assert len(hits) == 1
        assert "banana" in hits[0]["_source"]["title"]

    def test_bool_should(self, base_url, index_name):
        """bool should matches any."""
        _bulk_docs(base_url, index_name, [
            {"title": "java programming"},
            {"title": "python scripting"},
            {"title": "go systems"},
        ])
        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "query": {
                "bool": {
                    "should": [
                        {"match": {"title": "java"}},
                        {"match": {"title": "python"}},
                    ]
                }
            }
        })
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        assert len(hits) >= 1

    def test_bool_filter(self, base_url, index_name):
        """bool filter clause for filtering without scoring."""
        _bulk_docs(base_url, index_name, [
            {"title": "laptop", "price": 1000},
            {"title": "desktop", "price": 800},
        ])
        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "query": {
                "bool": {
                    "must": [{"match_all": {}}],
                    "filter": [{"range": {"price": {"gte": 900}}}],
                }
            }
        })
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        assert len(hits) == 1

    def test_exists(self, base_url, index_name):
        """exists query checks field presence."""
        _bulk_docs(base_url, index_name, [
            {"title": "has_name", "name": "yes"},
            {"title": "no_name"},
        ])
        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"exists": {"field": "name"}}})
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        assert len(hits) == 1

    def test_size_param(self, base_url, index_name):
        """size parameter limits results."""
        _bulk_docs(base_url, index_name, [{"n": i} for i in range(10)])
        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"match_all": {}}, "size": 3})
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        assert len(hits) == 3

    def test_source_filter(self, base_url, index_name):
        """_source filtering includes/excludes fields."""
        _bulk_docs(base_url, index_name, [
            {"title": "test", "secret": "hidden", "public": "visible"}
        ])
        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "query": {"match_all": {}},
            "_source": {"includes": ["title", "public"], "excludes": ["secret"]}
        })
        assert r.status_code == 200
        hit = r.json()["hits"]["hits"][0]
        assert "title" in hit["_source"]
        assert "public" in hit["_source"]
        assert "secret" not in hit["_source"]

    def test_sort(self, base_url, index_name):
        """sort orders results by field."""
        _bulk_docs(base_url, index_name, [
            {"price": 300}, {"price": 100}, {"price": 200},
        ])
        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "query": {"match_all": {}},
            "sort": [{"price": "asc"}]
        })
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        prices = [h["_source"]["price"] for h in hits]
        assert prices == sorted(prices)

    def test_match_prefix(self, base_url, index_name):
        """match_phrase_prefix query (may not be supported yet)."""
        _bulk_docs(base_url, index_name, [
            {"title": "testing"}, {"title": "test"}, {"title": "tear"},
        ])
        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"match_phrase_prefix": {"title": "test"}}})
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        assert len(hits) >= 1

    def test_wildcard(self, base_url, index_name):
        """wildcard query (may not be supported yet)."""
        _bulk_docs(base_url, index_name, [
            {"name": "apple"}, {"name": "apricot"}, {"name": "banana"},
        ])
        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"wildcard": {"name": "apple"}}})
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        assert len(hits) >= 1
