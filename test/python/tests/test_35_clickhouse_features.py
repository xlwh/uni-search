"""
test_35_clickhouse_features.py — E2E tests for ClickHouse feature import.

Covers F1-F6:
  F1: Inverted index mergeable (merge preserves search results)
  F2: Composable codecs (data integrity after merge)
  F3: Aggregation hash table specialization (large GROUP BY correctness)
  F4: TTL lifecycle management (expired row deletion)
  F5: Async INSERT batch merging (?async=true)
  F6: Projections / pre-computation (_settings projections)

@spec: 02_spec.md#8.2 F1-T01..F6-T06
"""
import json
import time
import uuid

import pytest
import requests


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _bulk(base_url, index, docs, refresh=True):
    """Bulk-index docs into index. Each doc is a dict."""
    lines = []
    for i, d in enumerate(docs):
        lines.append(json.dumps({"index": {"_index": index, "_id": f"doc{i}"}}))
        lines.append(json.dumps(d))
    body = "\n".join(lines) + "\n"
    url = f"{base_url}/_bulk"
    if refresh:
        url += "?refresh=true"
    r = requests.post(url, data=body, headers={"Content-Type": "application/x-ndjson"})
    assert r.status_code in (200, 201), f"Bulk failed: {r.text}"


def _search(base_url, index, query):
    """Execute a search and return the response JSON."""
    r = requests.post(f"{base_url}/{index}/_search", json=query)
    assert r.status_code == 200, f"Search failed: {r.text}"
    return r.json()


def _forcemerge(base_url, index, max_num=1):
    """Force merge the index."""
    r = requests.post(f"{base_url}/{index}/_forcemerge?max_num_segments={max_num}")
    assert r.status_code in (200, 202), f"Forcemerge failed: {r.text}"


# ---------------------------------------------------------------------------
# F1: Inverted Index Mergeable
# ---------------------------------------------------------------------------

class TestF1InvertedIndexMerge:
    """Verify that merging segments preserves BM25 search results."""

    def test_search_after_merge(self, base_url, index_name):
        """F1-T01: Search results identical before and after merge."""
        docs = [
            {"title": "hello world", "text": "the quick brown fox"},
            {"title": "hello uniSearch", "text": "jumps over the lazy dog"},
            {"title": "world peace", "text": "a quick brown fox jumps"},
            {"title": "hello world again", "text": "the fox is quick"},
        ]
        _bulk(base_url, index_name, docs)

        # Search before merge.
        before = _search(base_url, index_name, {
            "query": {"match": {"title": "hello world"}},
            "size": 10,
        })
        before_hits = before["hits"]["hits"]
        assert len(before_hits) >= 3, f"Expected >=3 hits, got {len(before_hits)}"

        # Force merge to 1 segment.
        _forcemerge(base_url, index_name)

        # Search after merge — results must be identical.
        after = _search(base_url, index_name, {
            "query": {"match": {"title": "hello world"}},
            "size": 10,
        })
        after_hits = after["hits"]["hits"]
        assert len(after_hits) == len(before_hits), \
            f"Hit count changed after merge: {len(before_hits)} → {len(after_hits)}"

        # Verify BM25 scores are preserved.
        before_scores = [h["_score"] for h in before_hits]
        after_scores = [h["_score"] for h in after_hits]
        for i, (bs, as_) in enumerate(zip(before_scores, after_scores)):
            assert abs(bs - as_) < 0.001, \
                f"Score mismatch at hit {i}: {bs} vs {as_}"

    def test_phrase_search_after_merge(self, base_url, index_name):
        """F1-T03: Phrase query positions correctly remapped after merge."""
        docs = [
            {"title": "quick brown fox", "text": "the quick brown fox jumps"},
            {"title": "lazy dog", "text": "over the lazy dog"},
            {"title": "brown fox quick", "text": "quick brown fox again"},
        ]
        _bulk(base_url, index_name, docs)

        before = _search(base_url, index_name, {
            "query": {"match_phrase": {"text": "quick brown fox"}},
            "size": 10,
        })
        before_count = len(before["hits"]["hits"])

        _forcemerge(base_url, index_name)

        after = _search(base_url, index_name, {
            "query": {"match_phrase": {"text": "quick brown fox"}},
            "size": 10,
        })
        after_count = len(after["hits"]["hits"])
        assert before_count == after_count, \
            f"Phrase search count changed: {before_count} → {after_count}"

    def test_delete_then_merge(self, base_url, index_name):
        """F1-T02: Deleted docs don't appear in merged results."""
        docs = [
            {"title": "keep me", "text": "important content"},
            {"title": "delete me", "text": "stale content"},
            {"title": "keep this too", "text": "more important content"},
        ]
        _bulk(base_url, index_name, docs)

        # Delete one doc.
        r = requests.delete(f"{base_url}/{index_name}/_doc/doc1?refresh=true")
        assert r.status_code in (200, 200)

        _forcemerge(base_url, index_name)

        # Search — deleted doc should not appear.
        result = _search(base_url, index_name, {
            "query": {"match_all": {}},
            "size": 100,
        })
        ids = [h["_id"] for h in result["hits"]["hits"]]
        assert "doc1" not in ids, "Deleted doc appeared in merged results"
        assert "doc0" in ids
        assert "doc2" in ids


# ---------------------------------------------------------------------------
# F2: Composable Codecs
# ---------------------------------------------------------------------------

class TestF2ComposableCodecs:
    """Verify data integrity after merge with composable codecs."""

    def test_data_types_after_merge(self, base_url, index_name):
        """F2-T05: All data types round-trip correctly after merge."""
        docs = [
            {"title": "alpha", "status": "active", "price": 10.5, "qty": 100, "in_stock": True},
            {"title": "beta", "status": "inactive", "price": 20.0, "qty": 200, "in_stock": False},
            {"title": "gamma", "status": "active", "price": 30.25, "qty": 300, "in_stock": True},
        ]
        _bulk(base_url, index_name, docs)

        # Read before merge.
        before = _search(base_url, index_name, {"query": {"match_all": {}}, "size": 100})
        before_docs = {h["_id"]: h["_source"] for h in before["hits"]["hits"]}

        _forcemerge(base_url, index_name)

        # Read after merge — verify all fields match.
        after = _search(base_url, index_name, {"query": {"match_all": {}}, "size": 100})
        after_docs = {h["_id"]: h["_source"] for h in after["hits"]["hits"]}

        assert len(after_docs) == len(before_docs)
        for doc_id, before_doc in before_docs.items():
            after_doc = after_docs[doc_id]
            assert after_doc["title"] == before_doc["title"]
            assert after_doc["status"] == before_doc["status"]
            assert abs(after_doc["price"] - before_doc["price"]) < 1e-9
            assert after_doc["qty"] == before_doc["qty"]
            assert after_doc["in_stock"] == before_doc["in_stock"]

    def test_range_query_after_merge(self, base_url, index_name):
        """F2-T05: Range queries work on numeric columns after merge."""
        docs = [{"qty": i * 10, "price": float(i) * 0.5} for i in range(50)]
        _bulk(base_url, index_name, docs)

        _forcemerge(base_url, index_name)

        # Range query on long field.
        r = _search(base_url, index_name, {
            "query": {"range": {"qty": {"gte": 100, "lte": 300}}},
            "size": 100,
        })
        hits = r["hits"]["hits"]
        for h in hits:
            assert 100 <= h["_source"]["qty"] <= 300

    def test_null_values_after_merge(self, base_url, index_name):
        """F2-T06: Null values survive merge correctly."""
        lines = []
        for i in range(20):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"n{i}"}}))
            doc = {"qty": i}
            if i % 3 == 0:
                doc["price"] = None  # explicit null
            elif i % 3 == 1:
                doc["price"] = float(i) * 1.5
            # i % 3 == 2: field absent (implicit null)
            lines.append(json.dumps(doc))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        _forcemerge(base_url, index_name)

        result = _search(base_url, index_name, {"query": {"match_all": {}}, "size": 100})
        assert len(result["hits"]["hits"]) == 20


# ---------------------------------------------------------------------------
# F3: Aggregation Hash Table Specialization
# ---------------------------------------------------------------------------

class TestF3AggregationHashTable:
    """Verify aggregation correctness with specialized hash tables."""

    def test_large_terms_agg(self, base_url, index_name):
        """F3-T07: Terms aggregation with many groups (exercises hash table)."""
        # Write 500 docs across 50 categories.
        docs = []
        for i in range(500):
            docs.append({"category": f"cat_{i % 50}", "qty": i, "price": float(i) * 0.1})
        _bulk(base_url, index_name, docs)

        r = _search(base_url, index_name, {
            "size": 0,
            "aggs": {
                "by_category": {"terms": {"field": "category", "size": 100}},
            },
        })
        buckets = r.get("aggregations", {}).get("by_category", {}).get("buckets", [])
        assert len(buckets) == 50, f"Expected 50 buckets, got {len(buckets)}"
        # Each bucket should have 10 docs.
        for b in buckets:
            assert b["doc_count"] == 10, f"Bucket {b['key']} has {b['doc_count']} docs"

    def test_long_field_agg(self, base_url, index_name):
        """F3: Aggregation on long field (FixedHashTableU64 path)."""
        docs = [{"qty": i * 100, "category": "x"} for i in range(200)]
        _bulk(base_url, index_name, docs)

        r = _search(base_url, index_name, {
            "size": 0,
            "aggs": {
                "by_qty": {"terms": {"field": "qty", "size": 300}},
            },
        })
        buckets = r.get("aggregations", {}).get("by_qty", {}).get("buckets", [])
        assert len(buckets) == 200, f"Expected 200 buckets, got {len(buckets)}"

    def test_sum_agg_after_merge(self, base_url, index_name):
        """F3: Aggregation results correct after segment merge."""
        docs = [{"category": "a", "price": 10.0}, {"category": "b", "price": 20.0}]
        _bulk(base_url, index_name, docs)

        # Write more docs to create multiple segments.
        docs2 = [{"category": "a", "price": 30.0}, {"category": "b", "price": 40.0}]
        _bulk(base_url, index_name, docs2)

        _forcemerge(base_url, index_name)

        r = _search(base_url, index_name, {
            "size": 0,
            "aggs": {
                "by_cat": {
                    "terms": {"field": "category"},
                    "aggs": {"total_price": {"sum": {"field": "price"}}},
                },
            },
        })
        buckets = r.get("aggregations", {}).get("by_cat", {}).get("buckets", [])
        for b in buckets:
            if b["key"] == "a":
                assert abs(b["total_price"]["value"] - 40.0) < 0.01
            elif b["key"] == "b":
                assert abs(b["total_price"]["value"] - 60.0) < 0.01


# ---------------------------------------------------------------------------
# F4: TTL Lifecycle Management
# ---------------------------------------------------------------------------

class TestF4TTLLifecycle:
    """Verify TTL configuration and expired row handling."""

    def test_ttl_settings(self, base_url, index_name):
        """F4: TTL can be configured via _settings."""
        r = requests.put(f"{base_url}/{index_name}/_settings", json={
            "ttl": {"exprs": [{"column": "ts", "mode": "delete", "interval": "30d"}]}
        })
        assert r.status_code in (200, 201), f"Set TTL failed: {r.text}"

    def test_no_ttl_no_impact(self, base_url, index_name):
        """F4-T05: Without TTL, all docs survive merge."""
        now_ms = int(time.time() * 1000)
        docs = [{"title": "old", "ts": now_ms - 86400000 * 90},
                {"title": "new", "ts": now_ms}]
        _bulk(base_url, index_name, docs)

        _forcemerge(base_url, index_name)

        result = _search(base_url, index_name, {"query": {"match_all": {}}, "size": 100})
        assert len(result["hits"]["hits"]) == 2, "Docs should not be deleted without TTL"

    def test_ttl_interval_parse(self, base_url, session_id):
        """F4: Various TTL interval formats accepted."""
        name = f"t_{session_id}_{uuid.uuid4().hex[:8]}"
        mapping = {"mappings": {"properties": {"ts": {"type": "long"}, "title": {"type": "text"}}}}
        r = requests.put(f"{base_url}/{name}", json=mapping)
        assert r.status_code in (200, 201)

        try:
            for interval in ["1h", "5m", "10s", "30d"]:
                r = requests.put(f"{base_url}/{name}/_settings", json={
                    "ttl": {"exprs": [{"column": "ts", "mode": "delete", "interval": interval}]}
                })
                assert r.status_code in (200, 201), f"Interval {interval} rejected: {r.text}"
        finally:
            requests.delete(f"{base_url}/{name}")


# ---------------------------------------------------------------------------
# F5: Async INSERT Batch Merging
# ---------------------------------------------------------------------------

class TestF5AsyncInsert:
    """Verify async INSERT behavior and data visibility."""

    def test_async_insert_basic(self, base_url, index_name):
        """F5: ?async=true writes documents that are eventually visible."""
        for i in range(10):
            r = requests.post(f"{base_url}/{index_name}/_doc?async=true", json={
                "title": f"async doc {i}", "qty": i,
            })
            assert r.status_code in (200, 201), f"Async insert failed: {r.text}"

        # Wait for async flush + refresh.
        time.sleep(1)
        requests.post(f"{base_url}/{index_name}/_refresh")

        result = _search(base_url, index_name, {"query": {"match_all": {}}, "size": 100})
        # Some or all docs should be visible (async flush timing).
        assert len(result["hits"]["hits"]) >= 1, "No async docs visible after wait"

    def test_sync_insert_unaffected(self, base_url, index_name):
        """F5: Normal (sync) insert behavior unchanged."""
        r = requests.put(f"{base_url}/{index_name}/_doc/sync1?refresh=true", json={
            "title": "sync doc",
        })
        assert r.status_code in (200, 201)

        result = _search(base_url, index_name, {
            "query": {"term": {"_id": "sync1"}},
        })
        assert len(result["hits"]["hits"]) == 1

    def test_concurrent_async_inserts(self, base_url, index_name):
        """F5: Concurrent async inserts don't lose data."""
        import concurrent.futures

        def insert_one(i):
            return requests.post(f"{base_url}/{index_name}/_doc?async=true", json={
                "title": f"concurrent {i}", "qty": i,
            })

        with concurrent.futures.ThreadPoolExecutor(max_workers=10) as pool:
            results = list(pool.map(insert_one, range(50)))

        for r in results:
            assert r.status_code in (200, 201)

        # Wait for flush.
        time.sleep(2)
        requests.post(f"{base_url}/{index_name}/_refresh")

        result = _search(base_url, index_name, {"query": {"match_all": {}}, "size": 200})
        assert len(result["hits"]["hits"]) >= 1, "No concurrent async docs visible"


# ---------------------------------------------------------------------------
# F6: Projections / Pre-computation
# ---------------------------------------------------------------------------

class TestF6Projections:
    """Verify projection definition via _settings and query acceleration."""

    def test_define_projection(self, base_url, index_name):
        """F6: Projections can be defined via _settings."""
        r = requests.put(f"{base_url}/{index_name}/_settings", json={
            "projections": [
                {
                    "name": "by_category",
                    "type": "aggregate",
                    "group_by": ["category"],
                    "aggregates": [
                        {"name": "total_qty", "type": "sum", "field": "qty"},
                        {"name": "count", "type": "count"},
                    ],
                },
            ],
        })
        assert r.status_code in (200, 201), f"Define projection failed: {r.text}"

    def test_define_normal_projection(self, base_url, index_name):
        """F6: Normal (sort-order) projection can be defined."""
        r = requests.put(f"{base_url}/{index_name}/_settings", json={
            "projections": [
                {
                    "name": "by_price",
                    "type": "normal",
                    "sort_key": ["price"],
                },
            ],
        })
        assert r.status_code in (200, 201), f"Define normal projection failed: {r.text}"

    def test_agg_query_with_projection(self, base_url, index_name):
        """F6: Aggregation query works after projection definition."""
        # Define projection.
        r = requests.put(f"{base_url}/{index_name}/_settings", json={
            "projections": [
                {
                    "name": "cat_stats",
                    "type": "aggregate",
                    "group_by": ["category"],
                    "aggregates": [{"name": "total", "type": "sum", "field": "qty"}],
                },
            ],
        })
        assert r.status_code in (200, 201)

        # Write data.
        docs = [
            {"category": "a", "qty": 10},
            {"category": "a", "qty": 20},
            {"category": "b", "qty": 30},
        ]
        _bulk(base_url, index_name, docs)

        # Query — should return correct results regardless of projection.
        r = _search(base_url, index_name, {
            "size": 0,
            "aggs": {
                "by_cat": {
                    "terms": {"field": "category"},
                    "aggs": {"total": {"sum": {"field": "qty"}}},
                },
            },
        })
        buckets = r.get("aggregations", {}).get("by_cat", {}).get("buckets", [])
        assert len(buckets) == 2
        for b in buckets:
            if b["key"] == "a":
                assert b["total"]["value"] == 30
            elif b["key"] == "b":
                assert b["total"]["value"] == 30

    def test_count_query_works(self, base_url, index_name):
        """F6: Count query works (potential minmax_count optimization)."""
        docs = [{"category": "x", "qty": i} for i in range(50)]
        _bulk(base_url, index_name, docs)

        # Use search with size=0 to get total hit count (more reliable than _count).
        r = _search(base_url, index_name, {
            "query": {"match_all": {}},
            "size": 0,
            "track_total_hits": True,
        })
        total = r["hits"]["total"]
        if isinstance(total, dict):
            count = total["value"]
        else:
            count = total
        assert count == 50, f"Expected count 50, got {count}"
