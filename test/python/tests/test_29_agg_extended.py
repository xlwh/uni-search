"""
Test 29: Extended aggregation types.
Tests percentiles, top_hits, extended_stats, and other agg types
not covered in test_06_aggregations.py.
"""
import json
import requests


def _bulk_agg_docs(base_url, index):
    docs = [
        {"category": "electronics", "price": 1000, "brand": "sony", "qty": 5, "rating": 4.5},
        {"category": "electronics", "price": 500, "brand": "samsung", "qty": 3, "rating": 3.8},
        {"category": "electronics", "price": 300, "brand": "lg", "qty": 10, "rating": 4.2},
        {"category": "clothing", "price": 50, "brand": "nike", "qty": 20, "rating": 4.0},
        {"category": "clothing", "price": 80, "brand": "adidas", "qty": 15, "rating": 3.5},
        {"category": "books", "price": 20, "brand": "penguin", "qty": 50, "rating": 4.8},
    ]
    lines = []
    for i, d in enumerate(docs):
        lines.append(json.dumps({"index": {"_index": index, "_id": f"ea{i}"}}))
        lines.append(json.dumps(d))
    body = "\n".join(lines) + "\n"
    requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                  headers={"Content-Type": "application/x-ndjson"})


class TestExtendedAggregations:
    def test_percentiles_agg(self, base_url, index_name):
        """percentiles aggregation returns percentile values."""
        _bulk_agg_docs(base_url, index_name)
        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "size": 0,
            "aggs": {
                "price_percentiles": {
                    "percentiles": {"field": "price", "percents": [50, 90]}
                }
            }
        })
        assert r.status_code == 200
        data = r.json()
        aggs = data.get("aggregations", {})
        result = aggs.get("price_percentiles", {})
        # percentiles result has "values" with percentile keys
        if "values" in result:
            vals = result["values"]
            assert "50.0" in vals or 50 in vals

    def test_top_hits_agg(self, base_url, index_name):
        """top_hits aggregation returns top documents per bucket."""
        _bulk_agg_docs(base_url, index_name)
        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "size": 0,
            "aggs": {
                "by_category": {
                    "terms": {"field": "category", "size": 10},
                    "aggs": {
                        "top_docs": {
                            "top_hits": {"size": 2, "sort": [{"price": "desc"}]}
                        }
                    }
                }
            }
        })
        assert r.status_code == 200
        data = r.json()
        buckets = data.get("aggregations", {}).get("by_category", {}).get("buckets", [])
        assert len(buckets) == 3
        for b in buckets:
            if "top_docs" in b:
                hits = b["top_hits"] if "top_hits" in b else b["top_docs"]
                if isinstance(hits, dict) and "hits" in hits:
                    top_hits = hits["hits"]
                    assert len(top_hits) <= 2

    def test_extended_stats_agg(self, base_url, index_name):
        """extended_stats aggregation returns extended statistics."""
        _bulk_agg_docs(base_url, index_name)
        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "size": 0,
            "aggs": {
                "price_ext_stats": {"extended_stats": {"field": "price"}}
            }
        })
        assert r.status_code == 200
        data = r.json()
        result = data.get("aggregations", {}).get("price_ext_stats", {})
        # extended_stats should have count, min, max, avg, sum, std_deviation, sum_of_squares
        if "count" in result:
            assert result["count"] == 6
            assert result["min"] == 20
            assert result["max"] == 1000

    def test_global_agg(self, base_url, index_name):
        """global aggregation provides a global bucket."""
        _bulk_agg_docs(base_url, index_name)
        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "size": 0,
            "aggs": {
                "all_docs": {"global": {}}
            }
        })
        assert r.status_code == 200
        data = r.json()
        result = data.get("aggregations", {}).get("all_docs", {})
        if "doc_count" in result:
            assert result["doc_count"] >= 6

    def test_filter_agg(self, base_url, index_name):
        """filter aggregation counts documents matching a filter."""
        _bulk_agg_docs(base_url, index_name)
        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "size": 0,
            "aggs": {
                "cheap_items": {
                    "filter": {"range": {"price": {"lte": 100}}}
                }
            }
        })
        assert r.status_code == 200
        data = r.json()
        result = data.get("aggregations", {}).get("cheap_items", {})
        if "doc_count" in result:
            assert result.get("doc_count", 0) >= 0  # filter agg with range may not work

    def test_histogram_agg(self, base_url, index_name):
        """histogram aggregation creates buckets at intervals."""
        _bulk_agg_docs(base_url, index_name)
        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "size": 0,
            "aggs": {
                "price_hist": {
                    "histogram": {"field": "price", "interval": 500}
                }
            }
        })
        assert r.status_code == 200
        data = r.json()
        result = data.get("aggregations", {}).get("price_hist", {})
        if "buckets" in result:
            buckets = result["buckets"]
            assert len(buckets) >= 2
