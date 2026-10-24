"""
Test 04: Bulk operations.
"""
import requests

class TestBulk:
    def test_bulk_index(self, base_url, index_name):
        """POST /_bulk indexes multiple documents."""
        lines = "\n".join([
            '{"index": {"_index": "%s", "_id": "b1"}}' % index_name,
            '{"title": "bulk doc 1", "price": 10}',
            '{"index": {"_index": "%s", "_id": "b2"}}' % index_name,
            '{"title": "bulk doc 2", "price": 20}',
            '{"index": {"_index": "%s", "_id": "b3"}}' % index_name,
            '{"title": "bulk doc 3", "price": 30}',
        ]) + "\n"
        r = requests.post(f"{base_url}/_bulk", data=lines,
                          headers={"Content-Type": "application/x-ndjson"})
        assert r.status_code == 200
        data = r.json()
        assert not data.get("errors", False)
        # Verify docs
        for i in range(1, 4):
            r2 = requests.get(f"{base_url}/{index_name}/_doc/b{i}")
            assert r2.status_code == 200

    def test_bulk_mixed_ops(self, base_url, index_name):
        """POST /_bulk with mixed index/delete operations."""
        # First index some docs
        lines1 = "\n".join([
            '{"index": {"_index": "%s", "_id": "m1"}}' % index_name,
            '{"title": "mixed 1"}',
            '{"index": {"_index": "%s", "_id": "m2"}}' % index_name,
            '{"title": "mixed 2"}',
        ]) + "\n"
        requests.post(f"{base_url}/_bulk", data=lines1,
                      headers={"Content-Type": "application/x-ndjson"})
        # Now bulk with delete + index
        lines2 = "\n".join([
            '{"delete": {"_index": "%s", "_id": "m1"}}' % index_name,
            '{"index": {"_index": "%s", "_id": "m3"}}' % index_name,
            '{"title": "mixed 3"}',
        ]) + "\n"
        r = requests.post(f"{base_url}/_bulk", data=lines2,
                          headers={"Content-Type": "application/x-ndjson"})
        assert r.status_code == 200
        # m1 deleted, m3 exists
        assert requests.get(f"{base_url}/{index_name}/_doc/m1").status_code == 404
        assert requests.get(f"{base_url}/{index_name}/_doc/m3").status_code == 200

    def test_bulk_with_refresh(self, base_url, index_name):
        """POST /_bulk?refresh=true makes docs immediately searchable."""
        lines = "\n".join([
            '{"index": {"_index": "%s", "_id": "r1"}}' % index_name,
            '{"title": "refresh test"}',
        ]) + "\n"
        r = requests.post(f"{base_url}/_bulk?refresh=true", data=lines,
                          headers={"Content-Type": "application/x-ndjson"})
        assert r.status_code == 200
        # Should be searchable immediately
        r2 = requests.post(f"{base_url}/{index_name}/_search",
                           json={"query": {"match_all": {}}})
        assert r2.status_code == 200
        data = r2.json()
        hits = data.get("hits", {}).get("hits", [])
        assert len(hits) >= 1

    def test_bulk_large(self, base_url, index_name):
        """POST /_bulk with 100 documents."""
        lines = []
        for i in range(10):
            lines.append('{"index": {"_index": "%s", "_id": "large%d"}}' % (index_name, i))
            lines.append('{"title": "doc %d", "price": %d}' % (i, i * 10))
        body = "\n".join(lines) + "\n"
        r = requests.post(f"{base_url}/_bulk", data=body,
                          headers={"Content-Type": "application/x-ndjson"})
        assert r.status_code == 200
        data = r.json()
        assert not data.get("errors", False)
        # Count
        r2 = requests.get(f"{base_url}/{index_name}/_count")
        assert r2.status_code == 200
        count_data = r2.json()
        assert count_data.get("count", 0) >= 10
