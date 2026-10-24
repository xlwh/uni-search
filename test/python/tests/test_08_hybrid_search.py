"""
Test 08: Hybrid search (BM25 + KNN with fusion).
"""
import json
import pytest
import requests

def _bulk_hybrid_docs(base_url, index):
    docs = [
        {"text": "machine learning model", "vec": [1.0, 0.0]},
        {"text": "deep learning neural", "vec": [0.95, 0.05]},
        {"text": "cooking recipe food", "vec": [0.0, 1.0]},
        {"text": "baking dessert sweet", "vec": [0.05, 0.95]},
    ]
    lines = []
    for i, d in enumerate(docs):
        lines.append(json.dumps({"index": {"_index": index, "_id": f"h{i}"}}))
        lines.append(json.dumps(d))
    body = "\n".join(lines) + "\n"
    requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                  headers={"Content-Type": "application/x-ndjson"})


class TestHybridSearch:
    def test_hybrid_search(self, base_url, index_name):
        """Hybrid search combines BM25 + KNN with RRF fusion."""
        _bulk_hybrid_docs(base_url, index_name)
        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "query": {"match": {"text": "learning"}},
            "knn": {
                "field": "vec",
                "query_vector": [1.0, 0.0],
                "k": 4,
                "num_candidates": 4
            },
            "size": 4
        })
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        assert len(hits) > 0
        # Should find learning-related docs
        texts = [h["_source"].get("text", "") for h in hits]
        assert any("learning" in t for t in texts)

    def test_hybrid_rrf(self, base_url, index_name):
        """Hybrid search with explicit RRF fusion."""
        _bulk_hybrid_docs(base_url, index_name)
        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "query": {"match": {"text": "learning"}},
            "knn": {
                "field": "vec",
                "query_vector": [1.0, 0.0],
                "k": 4,
                "num_candidates": 4
            },
            "rank": {"rrf": {"window_size": 10, "rank_constant": 60}},
            "size": 4
        })
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        assert len(hits) > 0
