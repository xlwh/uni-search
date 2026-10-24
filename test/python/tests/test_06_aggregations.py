"""
Test 06: Aggregation queries.
"""
import json
import requests

def _bulk_agg_docs(base_url, index):
    docs = [
        {"category": "electronics", "price": 1000, "brand": "sony", "qty": 5},
        {"category": "electronics", "price": 500, "brand": "samsung", "qty": 3},
        {"category": "electronics", "price": 300, "brand": "lg", "qty": 10},
        {"category": "clothing", "price": 50, "brand": "nike", "qty": 20},
        {"category": "clothing", "price": 80, "brand": "adidas", "qty": 15},
        {"category": "books", "price": 20, "brand": "penguin", "qty": 50},
    ]
    lines = []
    for i, d in enumerate(docs):
        lines.append(json.dumps({"index": {"_index": index, "_id": f"agg{i}"}}))
        lines.append(json.dumps(d))
    body = "\n".join(lines) + "\n"
    requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                  headers={"Content-Type": "application/x-ndjson"})


class TestAggregations:
    def test_terms_agg(self, base_url, index_name):
        """terms aggregation groups by category."""
        _bulk_agg_docs(base_url, index_name)
        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "size": 0,
            "aggs": {"by_category": {"terms": {"field": "category"}}}
        })
        assert r.status_code == 200
        data = r.json()
        aggs = data.get("aggregations", {})
        buckets = aggs.get("by_category", {}).get("buckets", [])
        assert len(buckets) == 3
        names = [b["key"] for b in buckets]
        assert "electronics" in names
        assert "clothing" in names
        assert "books" in names

    def test_sum_agg(self, base_url, index_name):
        """sum aggregation."""
        _bulk_agg_docs(base_url, index_name)
        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "size": 0,
            "aggs": {"total_price": {"sum": {"field": "price"}}}
        })
        assert r.status_code == 200
        data = r.json()
        val = data["aggregations"]["total_price"]["value"]
        assert val == 1950

    def test_avg_agg(self, base_url, index_name):
        """avg aggregation."""
        _bulk_agg_docs(base_url, index_name)
        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "size": 0,
            "aggs": {"avg_price": {"avg": {"field": "price"}}}
        })
        assert r.status_code == 200
        val = r.json()["aggregations"]["avg_price"]["value"]
        assert abs(val - 325.0) < 0.1

    def test_min_max_agg(self, base_url, index_name):
        """min and max aggregation."""
        _bulk_agg_docs(base_url, index_name)
        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "size": 0,
            "aggs": {
                "min_price": {"min": {"field": "price"}},
                "max_price": {"max": {"field": "price"}},
            }
        })
        assert r.status_code == 200
        aggs = r.json()["aggregations"]
        assert aggs["min_price"]["value"] == 20
        assert aggs["max_price"]["value"] == 1000

    def test_stats_agg(self, base_url, index_name):
        """stats aggregation returns count, min, max, avg, sum."""
        _bulk_agg_docs(base_url, index_name)
        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "size": 0,
            "aggs": {"price_stats": {"stats": {"field": "price"}}}
        })
        assert r.status_code == 200
        stats = r.json()["aggregations"]["price_stats"]
        assert stats["count"] == 6
        assert stats["min"] == 20
        assert stats["max"] == 1000
        assert stats["sum"] == 1950

    def test_cardinality_agg(self, base_url, index_name):
        """cardinality (distinct count) aggregation."""
        _bulk_agg_docs(base_url, index_name)
        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "size": 0,
            "aggs": {"brands": {"cardinality": {"field": "brand"}}}
        })
        assert r.status_code == 200
        val = r.json()["aggregations"]["brands"]["value"]
        assert val == 6

    def test_value_count_agg(self, base_url, index_name):
        """value_count aggregation."""
        _bulk_agg_docs(base_url, index_name)
        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "size": 0,
            "aggs": {"price_count": {"value_count": {"field": "price"}}}
        })
        assert r.status_code == 200
        val = r.json()["aggregations"]["price_count"]["value"]
        assert val == 6

    def test_nested_agg(self, base_url, index_name):
        """Nested aggregation: terms → sub-aggregation."""
        _bulk_agg_docs(base_url, index_name)
        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "size": 0,
            "aggs": {
                "by_category": {
                    "terms": {"field": "category"},
                    "aggs": {
                        "avg_price": {"avg": {"field": "price"}}
                    }
                }
            }
        })
        assert r.status_code == 200
        buckets = r.json()["aggregations"]["by_category"]["buckets"]
        assert len(buckets) == 3
        for b in buckets:
            assert "avg_price" in b
            assert b["avg_price"]["value"] > 0
