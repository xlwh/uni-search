"""
Test 38: Index admin APIs (es-feature-gap-fill).
Covers: _open, _close, _stats, _segments, _resolve/index,
_rollover, _shrink, _split, _clone, _reload_search_analyzers.
"""
import pytest
import requests
import json

def _bulk_docs(base_url, index, docs):
    lines = []
    for i, d in enumerate(docs):
        lines.append('{"index": {"_index": "%s", "_id": "i%d"}}' % (index, i))
        lines.append(json.dumps(d))
    body = "\n".join(lines) + "\n"
    requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                  headers={"Content-Type": "application/x-ndjson"})


class TestIndexAdmin:
    def test_open_index(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"title": "hello"}])
        r = requests.post(f"{base_url}/{index_name}/_open")
        assert r.status_code == 200
        assert r.json()["acknowledged"] is True

    def test_close_index(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"title": "hello"}])
        r = requests.post(f"{base_url}/{index_name}/_close")
        assert r.status_code == 200
        assert r.json()["acknowledged"] is True

    def test_index_stats(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"title": "a"}, {"title": "b"}])
        r = requests.get(f"{base_url}/{index_name}/_stats")
        assert r.status_code == 200
        data = r.json()
        assert "indices" in data
        assert index_name in data["indices"]

    def test_all_stats(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"title": "a"}])
        r = requests.get(f"{base_url}/_stats")
        assert r.status_code == 200
        assert "indices" in r.json()

    def test_segments(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"title": "a"}, {"title": "b"}])
        r = requests.get(f"{base_url}/{index_name}/_segments")
        assert r.status_code == 200
        data = r.json()
        assert "indices" in data
        assert index_name in data["indices"]

    def test_all_segments(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"title": "a"}])
        r = requests.get(f"{base_url}/_segments")
        assert r.status_code == 200
        assert "indices" in r.json()

    def test_resolve_index(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"title": "a"}])
        r = requests.get(f"{base_url}/_resolve/index/{index_name}")
        assert r.status_code == 200
        data = r.json()
        assert "indices" in data
        assert index_name in data["indices"]

    def test_rollover(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"title": "a"}])
        r = requests.post(f"{base_url}/{index_name}/_rollover/{index_name}-000002",
                          json={"conditions": {"max_docs": 100}})
        assert r.status_code == 200
        assert r.json()["acknowledged"] is True

    def test_shrink(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"title": "a"}])
        r = requests.post(f"{base_url}/{index_name}/_shrink/{index_name}-shrunk")
        assert r.status_code == 200
        assert r.json()["acknowledged"] is True

    def test_split(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"title": "a"}])
        r = requests.post(f"{base_url}/{index_name}/_split/{index_name}-split")
        assert r.status_code == 200
        assert r.json()["acknowledged"] is True

    def test_clone(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"title": "a"}])
        r = requests.post(f"{base_url}/{index_name}/_clone/{index_name}-cloned")
        assert r.status_code == 200
        assert r.json()["acknowledged"] is True

    def test_reload_search_analyzers(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"title": "a"}])
        r = requests.post(f"{base_url}/{index_name}/_reload_search_analyzers")
        assert r.status_code == 200
        assert r.json()["reloaded_search_analyzers"] is True
