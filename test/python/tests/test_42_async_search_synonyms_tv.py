"""
Test 42: Async search, synonyms, and term vectors (es-feature-gap-fill).
"""
import pytest
import requests
import json
import time

def _bulk_docs(base_url, index, docs):
    lines = []
    for i, d in enumerate(docs):
        lines.append('{"index": {"_index": "%s", "_id": "t%d"}}' % (index, i))
        lines.append(json.dumps(d))
    body = "\n".join(lines) + "\n"
    requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                  headers={"Content-Type": "application/x-ndjson"})


class TestAsyncSearch:
    def test_submit_async_search(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"title": "async test"}])
        r = requests.post(f"{base_url}/_async_search",
                          json={"query": {"match_all": {}}})
        assert r.status_code == 200
        data = r.json()
        assert "id" in data
        assert "is_running" in data

    def test_get_async_search_result(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"title": "get async"}])
        r1 = requests.post(f"{base_url}/_async_search",
                           json={"query": {"match_all": {}}})
        search_id = r1.json()["id"]
        time.sleep(1)
        r2 = requests.get(f"{base_url}/_async_search/{search_id}")
        assert r2.status_code == 200
        data = r2.json()
        assert "is_running" in data

    def test_get_async_search_status(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"title": "status test"}])
        r1 = requests.post(f"{base_url}/_async_search",
                           json={"query": {"match_all": {}}})
        search_id = r1.json()["id"]
        r2 = requests.get(f"{base_url}/_async_search/status/{search_id}")
        assert r2.status_code == 200
        assert "is_running" in r2.json()

    def test_delete_async_search(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"title": "delete async"}])
        r1 = requests.post(f"{base_url}/_async_search",
                           json={"query": {"match_all": {}}})
        search_id = r1.json()["id"]
        r2 = requests.delete(f"{base_url}/_async_search/{search_id}")
        assert r2.status_code == 200
        assert r2.json()["acknowledged"] is True


class TestSynonyms:
    def test_put_synonyms(self, base_url, index_name):
        r = requests.put(f"{base_url}/_synonyms/test-synonyms", json={
            "synonyms": ["happy, joy, glad", "big, large, huge"]
        })
        assert r.status_code == 200
        assert r.json()["acknowledged"] is True

    def test_get_synonyms(self, base_url, index_name):
        requests.put(f"{base_url}/_synonyms/get-synonyms", json={
            "synonyms": ["fast, quick, rapid"]
        })
        r = requests.get(f"{base_url}/_synonyms/get-synonyms")
        assert r.status_code == 200
        data = r.json()
        assert "synonyms" in data

    def test_list_synonyms(self, base_url, index_name):
        requests.put(f"{base_url}/_synonyms/list-synonyms", json={
            "synonyms": ["cold, chilly, freezing"]
        })
        r = requests.get(f"{base_url}/_synonyms")
        assert r.status_code == 200
        assert "count" in r.json()

    def test_delete_synonyms(self, base_url, index_name):
        requests.put(f"{base_url}/_synonyms/del-synonyms", json={
            "synonyms": ["old, ancient, vintage"]
        })
        r = requests.delete(f"{base_url}/_synonyms/del-synonyms")
        assert r.status_code == 200
        assert r.json()["acknowledged"] is True


class TestTermVectors:
    def test_termvectors(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [
            {"title": "hello world from unisearch", "content": "test document"}
        ])
        r = requests.get(f"{base_url}/{index_name}/_termvectors/t0")
        assert r.status_code == 200
        data = r.json()
        assert data["found"] is True
        assert "term_vectors" in data

    def test_termvectors_with_fields(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [
            {"title": "hello world", "content": "another field"}
        ])
        r = requests.get(f"{base_url}/{index_name}/_termvectors/t0?fields=title")
        assert r.status_code == 200
        data = r.json()
        assert "title" in data["term_vectors"]

    def test_termvectors_no_offsets(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"title": "offsets test here"}])
        r = requests.get(f"{base_url}/{index_name}/_termvectors/t0?offsets=false")
        assert r.status_code == 200
        tv = r.json()["term_vectors"]
        if "title" in tv:
            for term_info in tv["title"]["terms"].values():
                assert "start_offset" not in term_info

    def test_termvectors_no_positions(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"title": "positions test"}])
        r = requests.get(f"{base_url}/{index_name}/_termvectors/t0?positions=false")
        assert r.status_code == 200

    def test_termvectors_term_statistics(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"title": "stats test word"}])
        r = requests.get(f"{base_url}/{index_name}/_termvectors/t0?term_statistics=true")
        assert r.status_code == 200

    def test_termvectors_field_statistics(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"title": "field stats here"}])
        r = requests.get(f"{base_url}/{index_name}/_termvectors/t0?field_statistics=true")
        assert r.status_code == 200
        tv = r.json()["term_vectors"]
        if "title" in tv:
            assert "field_statistics" in tv["title"]

    def test_termvectors_not_found(self, base_url, index_name):
        r = requests.get(f"{base_url}/{index_name}/_termvectors/nonexistent-id")
        assert r.status_code == 200
        assert r.json()["found"] is False

    def test_mtermvectors(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [
            {"title": "doc one"}, {"title": "doc two"}
        ])
        r = requests.post(f"{base_url}/_mtermvectors", json={
            "docs": [
                {"_index": index_name, "_id": "t0"},
                {"_index": index_name, "_id": "t1"}
            ]
        })
        assert r.status_code == 200
        docs = r.json()["docs"]
        assert len(docs) == 2
