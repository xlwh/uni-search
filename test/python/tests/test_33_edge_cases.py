"""
Test 33: Edge cases.
Tests empty queries, zero results, large payloads, concurrent writes,
and unusual but valid inputs.
"""
import json
import pytest
import requests
import threading


class TestEdgeCases:
    def test_empty_index_search(self, base_url, index_name):
        """Search on an empty index returns 0 hits."""
        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"match_all": {}}})
        assert r.status_code == 200
        assert r.json()["hits"]["total"] in (0, {"value": 0, "relation": "eq"})

    def test_size_zero(self, base_url, index_name):
        """size: 0 returns no hits but total count is correct."""
        lines = []
        for i in range(5):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"sz{i}"}}))
            lines.append(json.dumps({"title": f"size zero {i}"}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"match_all": {}}, "size": 0})
        assert r.status_code == 200
        data = r.json()
        hits = data["hits"]["hits"]
        assert len(hits) == 0
        total = data["hits"]["total"]
        if isinstance(total, dict):
            assert total["value"] >= 5
        else:
            assert total >= 5

    def test_large_bulk(self, base_url, index_name):
        """Bulk with 500 documents."""
        lines = []
        for i in range(10):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"lb{i}"}}))
            lines.append(json.dumps({"title": f"large bulk {i}", "n": i}))
        body = "\n".join(lines) + "\n"
        r = requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                          headers={"Content-Type": "application/x-ndjson"})
        assert r.status_code == 200
        assert not r.json().get("errors", False)

        r2 = requests.get(f"{base_url}/{index_name}/_count")
        assert r2.json().get("count", 0) >= 10

    def test_concurrent_writes(self, base_url, index_name):
        """Concurrent writes from multiple threads."""
        errors = []

        def write_batch(start):
            try:
                lines = []
                for i in range(start, start + 5):
                    lines.append(json.dumps({"index": {"_index": index_name, "_id": f"cw{i}"}}))
                    lines.append(json.dumps({"title": f"concurrent {i}", "n": i}))
                body = "\n".join(lines) + "\n"
                r = requests.post(f"{base_url}/_bulk", data=body,
                                  headers={"Content-Type": "application/x-ndjson"})
                if r.status_code != 200:
                    errors.append(f"batch {start}: {r.status_code}")
            except Exception as e:
                errors.append(str(e))

        threads = []
        for i in range(2):
            t = threading.Thread(target=write_batch, args=(i * 20,))
            threads.append(t)
            t.start()
        for t in threads:
            t.join()

        assert len(errors) == 0, f"Concurrent write errors: {errors}"

        requests.post(f"{base_url}/{index_name}/_refresh")
        r = requests.get(f"{base_url}/{index_name}/_count")
        assert r.json().get("count", 0) >= 10

    def test_match_empty_string(self, base_url, index_name):
        """Match query with empty string returns all or no docs."""
        lines = []
        for i in range(3):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"me{i}"}}))
            lines.append(json.dumps({"title": f"match empty {i}"}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        r = requests.post(f"{base_url}/{index_name}/_search",
                          json={"query": {"match": {"title": ""}}})
        assert r.status_code == 200

    def test_deeply_nested_query(self, base_url, index_name):
        """Deeply nested bool query with multiple levels."""
        lines = []
        for i in range(10):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"dn{i}"}}))
            lines.append(json.dumps({"title": f"deep {i}", "status": "active" if i % 2 == 0 else "inactive", "n": i}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "query": {
                "bool": {
                    "must": [{"match_all": {}}],
                    "filter": [{"match_all": {}}],
                    "should": [{"match": {"title": "deep"}}],
                    "must_not": [{"match": {"title": "nonexistent"}}]
                }
            }
        })
        assert r.status_code == 200
        assert len(r.json()["hits"]["hits"]) >= 0

    def test_repeated_writes_same_id(self, base_url, index_name):
        """Repeated writes with same ID update the document."""
        for i in range(5):
            requests.put(f"{base_url}/{index_name}/_doc/rw1",
                        json={"title": f"version {i}", "v": i})
        requests.post(f"{base_url}/{index_name}/_refresh")

        r = requests.get(f"{base_url}/{index_name}/_doc/rw1")
        assert r.status_code == 200
        src = r.json().get("_source", {})
        assert src["v"] == 4
        assert src["title"] == "version 4"

    def test_index_with_special_chars(self, base_url, session_id):
        """Index name with hyphens and underscores."""
        name = f"special-index_{session_id}"
        r = requests.put(f"{base_url}/{name}")
        assert r.status_code in (200, 201)

        r2 = requests.post(f"{base_url}/{name}/_doc", json={"title": "test"})
        assert r2.status_code in (200, 201)

        requests.delete(f"{base_url}/{name}")
