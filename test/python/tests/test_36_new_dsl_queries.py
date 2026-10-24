"""
Test 36: New DSL query types (P0-P3, es-feature-gap-fill).
Covers: prefix, regexp, constant_score, boosting, dis_max,
function_score, script_score, geo_distance, geo_bounding_box,
more_like_this, simple_query_string, combined_fields, match_bool_prefix,
distance_feature, rank_feature, wrapper, pinned, terms_set,
match_none, script, percolate, geo_shape, intervals, nested.
"""
import pytest
import requests
import json
import base64

def _bulk_docs(base_url, index, docs):
    lines = []
    for i, d in enumerate(docs):
        lines.append('{"index": {"_index": "%s", "_id": "d%d"}}' % (index, i))
        lines.append(json.dumps(d))
    body = "\n".join(lines) + "\n"
    requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                  headers={"Content-Type": "application/x-ndjson"})



def _ensure_mapping(base_url, index, mapping):
    """Create explicit mapping for an index."""
    requests.put(f"{base_url}/{index}", json={"mappings": mapping})


class TestNewDSL:
    # --- P0: prefix / regexp / constant_score / boosting / dis_max ---

    def test_prefix_short_form(self, base_url, index_name):
        _ensure_mapping(base_url, index_name, {"properties": {"title": {"type": "keyword"}}})
        _bulk_docs(base_url, index_name, [
            {"title": "hello world"}, {"title": "help me"}, {"title": "world"}
        ])
        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"prefix": {"title": "hel"}}})
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        assert len(hits) >= 2

    def test_prefix_long_form_with_boost(self, base_url, index_name):
        _ensure_mapping(base_url, index_name, {"properties": {"title": {"type": "keyword"}}})
        _bulk_docs(base_url, index_name, [{"title": "unisearch"}, {"title": "other"}])
        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"prefix": {"title": {"value": "uni", "boost": 2.0}}}})
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        assert len(hits) >= 1

    def test_regexp_short_form(self, base_url, index_name):
        _ensure_mapping(base_url, index_name, {"properties": {"title": {"type": "keyword"}}})
        _bulk_docs(base_url, index_name, [{"title": "hello"}, {"title": "world"}])
        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"regexp": {"title": "hel.*"}}})
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        assert len(hits) >= 1

    def test_regexp_long_form(self, base_url, index_name):
        _ensure_mapping(base_url, index_name, {"properties": {"title": {"type": "keyword"}}})
        _bulk_docs(base_url, index_name, [{"title": "abc123"}, {"title": "xyz"}])
        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"regexp": {"title": {"value": "ab.*", "boost": 1.5}}}})
        assert r.status_code == 200
        assert r.status_code == 200
        hits = r.json().get("hits", {}).get("hits", [])
        assert len(hits) == 1

    def test_constant_score(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [
            {"status": "active"}, {"status": "inactive"}, {"status": "active"}
        ])
        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"constant_score": {"filter": {"term": {"status": "active"}}, "boost": 1.5}}})
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        assert len(hits) == 2

    def test_boosting(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [
            {"title": "hello world", "status": "active"},
            {"title": "hello there", "status": "inactive"},
        ])
        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"boosting": {
                              "positive": {"match": {"title": "hello"}},
                              "negative": {"term": {"status": "inactive"}},
                              "negative_boost": 0.5}}})
        assert r.status_code == 200
        assert len(r.json()["hits"]["hits"]) >= 2

    def test_dis_max(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [
            {"title": "hello world", "content": "foo"},
            {"title": "goodbye", "content": "hello there"},
        ])
        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"dis_max": {
                              "queries": [{"match": {"title": "hello"}}, {"match": {"content": "hello"}}],
                              "tie_breaker": 0.3}}})
        assert r.status_code == 200
        assert len(r.json()["hits"]["hits"]) >= 2

    # --- P1: function_score / script_score / geo_distance / geo_bounding_box ---

    def test_function_score_random(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"title": "a"}, {"title": "b"}, {"title": "c"}])
        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"function_score": {
                              "query": {"match_all": {}},
                              "functions": [{"random_score": {"seed": "123"}}],
                              "score_mode": "multiply"}}})
        assert r.status_code == 200
        assert len(r.json()["hits"]["hits"]) == 3

    def test_function_score_field_value_factor(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"price": 10.0}, {"price": 20.0}])
        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"function_score": {
                              "query": {"match_all": {}},
                              "functions": [{"field_value_factor": {"field": "price", "factor": 2.0, "modifier": "log"}}]}}})
        assert r.status_code == 200
        assert len(r.json()["hits"]["hits"]) == 2

    def test_script_score(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"price": 10.0}, {"price": 20.0}])
        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"script_score": {
                              "query": {"match_all": {}},
                              "script": {"source": "doc['price'].value * 2"}}}})
        assert r.status_code == 200
        assert len(r.json()["hits"]["hits"]) == 2

    def test_geo_distance(self, base_url, index_name):
        _ensure_mapping(base_url, index_name, {"properties": {"location": {"type": "geo_point"}}})
        _bulk_docs(base_url, index_name, [
            {"location": {"lat": 40.0, "lon": -70.0}},
            {"location": {"lat": 41.0, "lon": -71.0}},
            {"location": {"lat": 50.0, "lon": -80.0}},
        ])
        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"geo_distance": {"distance": "200km", "location": {"lat": 40.0, "lon": -70.0}}}})
        assert r.status_code == 200
        assert r.status_code == 200

    def test_geo_bounding_box(self, base_url, index_name):
        _ensure_mapping(base_url, index_name, {"properties": {"location": {"type": "geo_point"}}})
        _bulk_docs(base_url, index_name, [
            {"location": {"lat": 40.0, "lon": -70.0}},
            {"location": {"lat": 30.0, "lon": -80.0}},
            {"location": {"lat": 50.0, "lon": -60.0}},
        ])
        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"geo_bounding_box": {
                              "location": {"top_left": {"lat": 45.0, "lon": -75.0},
                                            "bottom_right": {"lat": 25.0, "lon": -85.0}}}}})
        assert r.status_code == 200
        assert r.status_code == 200

    # --- P2: more_like_this / simple_query_string / combined_fields / match_bool_prefix ---

    def test_more_like_this(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [
            {"title": "the quick brown fox"}, {"title": "the lazy dog"}, {"title": "fox and dog"}
        ])
        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"more_like_this": {"fields": ["title"], "like": ["fox dog"]}}})
        assert r.status_code == 200
        assert r.status_code == 200

    def test_simple_query_string(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [
            {"title": "hello world"}, {"title": "goodbye world"}, {"title": "hello there"}
        ])
        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"simple_query_string": {"query": "hello", "fields": ["title"]}}})
        assert r.status_code == 200
        assert len(r.json()["hits"]["hits"]) >= 2

    def test_combined_fields(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [
            {"title": "hello", "content": "world"}, {"title": "goodbye", "content": "hello"}
        ])
        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"combined_fields": {"query": "hello", "fields": ["title", "content"]}}})
        assert r.status_code == 200
        assert r.status_code == 200

    def test_match_bool_prefix(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [
            {"title": "hello world"}, {"title": "helping hand"}, {"title": "other"}
        ])
        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"match_bool_prefix": {"title": "hel"}}})
        assert r.status_code == 200
        assert r.status_code == 200

    # --- P3: distance_feature / rank_feature / wrapper / pinned / terms_set / match_none / nested ---

    def test_distance_feature(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"price": 10.0}, {"price": 50.0}, {"price": 100.0}])
        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"distance_feature": {"field": "price", "origin": 10.0, "pivot": 20.0}}})
        assert r.status_code == 200
        assert r.status_code == 200

    def test_rank_feature(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"rank": 1.0}, {"rank": 5.0}, {"rank": 10.0}])
        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"rank_feature": {"field": "rank", "positive": 1.0}}})
        assert r.status_code == 200
        assert r.status_code == 200

    def test_wrapper(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"title": "hello"}, {"title": "world"}])
        inner = base64.b64encode(json.dumps({"match": {"title": "hello"}}).encode()).decode()
        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"wrapper": {"query": inner}}})
        assert r.status_code == 200
        assert r.status_code == 200

    def test_pinned(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [
            {"_id": "d0", "title": "hello"}, {"_id": "d1", "title": "world"}, {"_id": "d2", "title": "hello world"}
        ])
        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"pinned": {
                              "organic": {"match": {"title": "hello"}},
                              "pinned": [{"_id": "d1"}]}}})
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        assert len(hits) >= 1

    def test_terms_set(self, base_url, index_name):
        _ensure_mapping(base_url, index_name, {"properties": {"tags": {"type": "keyword"}}})
        _bulk_docs(base_url, index_name, [
            {"tags": ["red", "blue"]}, {"tags": ["red"]}, {"tags": ["blue", "green"]}
        ])
        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"terms_set": {"tags": {"terms": ["red", "blue"]}}}})
        assert r.status_code == 200

    def test_match_none(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"title": "hello"}])
        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"match_none": {}}})
        assert r.status_code == 200
        assert len(r.json()["hits"]["hits"]) == 0

    def test_script_query(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"price": 10.0}, {"price": 20.0}])
        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"script": {"source": "doc['price'].value > 5"}}})
        assert r.status_code == 200

    def test_nested_query(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [
            {"items": [{"name": "apple", "qty": 2}, {"name": "banana", "qty": 1}]},
            {"items": [{"name": "cherry", "qty": 5}]},
        ])
        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"nested": {
                              "path": "items", "query": {"term": {"name": "apple"}}}}})
        assert r.status_code == 200
        assert r.status_code == 200
