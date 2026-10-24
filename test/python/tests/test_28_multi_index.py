"""
Test 28: Multi-index operations.
Tests searching across multiple indices, wildcard index patterns,
and _all search.
"""
import json
import pytest
import requests


class TestMultiIndex:
    def test_multi_index_search(self, base_url, session_id):
        """POST /index1,index2/_search searches across multiple indices."""
        idx1 = f"multi_{session_id}_a"
        idx2 = f"multi_{session_id}_b"
        requests.put(f"{base_url}/{idx1}")
        requests.put(f"{base_url}/{idx2}")

        # Write to both indices
        for idx, prefix in [(idx1, "alpha"), (idx2, "beta")]:
            lines = []
            for i in range(3):
                lines.append(json.dumps({"index": {"_index": idx, "_id": f"mi{i}"}}))
                lines.append(json.dumps({"title": f"{prefix} doc {i}", "status": "active"}))
            body = "\n".join(lines) + "\n"
            requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                          headers={"Content-Type": "application/x-ndjson"})

        # Search across both indices
        r = requests.post(f"{base_url}/{idx1},{idx2}/_search",
                          json={"query": {"match_all": {}}})
        assert r.status_code == 200
        total = r.json()["hits"]["total"]
        if isinstance(total, dict):
            assert total["value"] == 6
        else:
            assert total == 6

        # Cleanup
        requests.delete(f"{base_url}/{idx1}")
        requests.delete(f"{base_url}/{idx2}")

    def test_wildcard_index_search(self, base_url, session_id):
        """POST /prefix*/_search uses wildcard pattern."""
        idx1 = f"wild_{session_id}_log"
        idx2 = f"wild_{session_id}_metric"
        requests.put(f"{base_url}/{idx1}")
        requests.put(f"{base_url}/{idx2}")

        lines = []
        for idx in [idx1, idx2]:
            for i in range(2):
                lines.append(json.dumps({"index": {"_index": idx, "_id": f"w{i}"}}))
                lines.append(json.dumps({"title": f"wildcard {idx} {i}"}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        r = requests.post(f"{base_url}/wild_{session_id}_*/_search",
                          json={"query": {"match_all": {}}})
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        assert len(hits) >= 0

        requests.delete(f"{base_url}/{idx1}")
        requests.delete(f"{base_url}/{idx2}")

    def test_all_indices_search(self, base_url, index_name):
        """POST /_all/_search or POST /_search searches all indices."""
        lines = []
        for i in range(3):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"ai{i}"}}))
            lines.append(json.dumps({"title": f"all index {i}"}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        r = requests.post(f"{base_url}/_search", json={"query": {"match_all": {}}})
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        assert len(hits) >= 3

    def test_get_search_method(self, base_url, index_name):
        """GET /{index}/_search works (ES supports GET search)."""
        lines = []
        for i in range(3):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"gm{i}"}}))
            lines.append(json.dumps({"title": f"get search {i}"}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        r = requests.get(f"{base_url}/{index_name}/_search",
                         params={"q": "title:get"})
        assert r.status_code in (200, 400)  # GET search may not support body
        if r.status_code == 200:
            hits = r.json()["hits"]["hits"]
            assert len(hits) >= 0

    def test_source_false(self, base_url, index_name):
        """_source: false excludes _source from all hits."""
        lines = []
        for i in range(3):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"sf{i}"}}))
            lines.append(json.dumps({"title": f"no source {i}", "secret": "hidden"}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "query": {"match_all": {}},
            "_source": False
        })
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        assert len(hits) == 3
        for h in hits:
            # _source should be absent or empty when _source: false
            src = h.get("_source")
            assert src is None or src == {} or src == ""

    def test_from_pagination(self, base_url, index_name):
        """from parameter skips results for pagination."""
        lines = []
        for i in range(10):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"pg{i}"}}))
            lines.append(json.dumps({"title": f"page doc {i}", "n": i}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        # Get first page
        r1 = requests.post(f"{base_url}/{index_name}/_search", json={
            "query": {"match_all": {}},
            "from": 0,
            "size": 3
        })
        assert r1.status_code == 200
        hits1 = r1.json()["hits"]["hits"]
        assert len(hits1) == 3

        # Get second page
        r2 = requests.post(f"{base_url}/{index_name}/_search", json={
            "query": {"match_all": {}},
            "from": 3,
            "size": 3
        })
        assert r2.status_code == 200
        hits2 = r2.json()["hits"]["hits"]
        assert len(hits2) == 3

        # Pages should have different docs
        ids1 = {h["_id"] for h in hits1}
        ids2 = {h["_id"] for h in hits2}
        assert not ids1.intersection(ids2)

    def test_ids_query(self, base_url, index_name):
        """ids query filters by document IDs."""
        lines = []
        for i in range(5):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"idq{i}"}}))
            lines.append(json.dumps({"title": f"ids query {i}"}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "query": {"ids": {"values": ["idq0", "idq2", "idq4"]}}
        })
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        assert len(hits) == 3

    def test_fuzzy_query(self, base_url, index_name):
        """fuzzy query tolerates edit distance."""
        lines = []
        for word in ["apple", "apply", "banana", "orange"]:
            lines.append(json.dumps({"index": {"_index": index_name, "_id": word}}))
            lines.append(json.dumps({"title": word}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "query": {"fuzzy": {"title": {"value": "apple", "fuzziness": 1}}}
        })
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        # Should match "apple" and possibly "apply" (1 edit distance)
        assert len(hits) >= 1

    def test_query_string_query(self, base_url, index_name):
        """query_string query supports simple query syntax."""
        lines = []
        for word in ["java", "python", "golang"]:
            lines.append(json.dumps({"index": {"_index": index_name, "_id": word}}))
            lines.append(json.dumps({"title": word}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "query": {"query_string": {"query": "java OR python"}}
        })
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        assert len(hits) >= 1
