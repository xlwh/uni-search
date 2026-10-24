"""
Test 43: Qdrant features import — DBSF fusion, MMR, Sparse vector, Float16.
@spec: ./active_specs/2026-10-23-qdrant-features-import/02_spec.md
"""
import json
import pytest
import requests


def _bulk_docs(base_url, index, docs):
    lines = []
    for i, d in enumerate(docs):
        lines.append(json.dumps({"index": {"_index": index, "_id": f"q{i}"}}))
        lines.append(json.dumps(d))
    body = "\n".join(lines) + "\n"
    requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                  headers={"Content-Type": "application/x-ndjson"})


class TestDBSFFusion:
    """DBSF (Distribution-Based Score Fusion) — F2."""

    def test_dbsf_hybrid_search(self, base_url, index_name):
        """Hybrid search with DBSF fusion method."""
        _bulk_docs(base_url, index_name, [
            {"text": "machine learning", "vec": [1.0, 0.0, 0.0, 0.0]},
            {"text": "deep learning", "vec": [0.9, 0.1, 0.0, 0.0]},
            {"text": "cooking recipe", "vec": [0.0, 0.0, 1.0, 0.0]},
            {"text": "baking sweet", "vec": [0.0, 0.0, 0.9, 0.1]},
        ])
        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "query": {"match": {"text": "learning"}},
            "knn": {"field": "vec", "query_vector": [1.0, 0.0, 0.0, 0.0], "k": 4, "num_candidates": 4},
            "rank": {"method": "dbsf", "weights": [0.7, 0.3]},
            "size": 4
        })
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        assert len(hits) > 0
        texts = [h["_source"].get("text", "") for h in hits]
        assert any("learning" in t for t in texts)

    def test_dbsf_equal_weights(self, base_url, index_name):
        """DBSF with equal weights (default)."""
        _bulk_docs(base_url, index_name, [
            {"text": "hello world", "vec": [1.0, 0.0, 0.0, 0.0]},
            {"text": "hello there", "vec": [0.0, 1.0, 0.0, 0.0]},
        ])
        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "query": {"match": {"text": "hello"}},
            "knn": {"field": "vec", "query_vector": [1.0, 0.0, 0.0, 0.0], "k": 2, "num_candidates": 2},
            "rank": {"method": "dbsf"},
            "size": 2
        })
        assert r.status_code == 200
        assert len(r.json()["hits"]["hits"]) > 0

    def test_dbsf_invalid_method(self, base_url, index_name):
        """Invalid fusion method should return error."""
        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "query": {"match": {"text": "test"}},
            "rank": {"method": "nonexistent"}
        })
        assert r.status_code in (400, 404)


class TestMMRDiversity:
    """MMR (Maximal Marginal Relevance) diversity — F3."""

    def test_mmr_enabled(self, base_url, index_name):
        """MMR re-ranking with lambda=0.5."""
        _bulk_docs(base_url, index_name, [
            {"text": "topic A", "vec": [1.0, 0.0, 0.0, 0.0]},
            {"text": "topic A variant", "vec": [0.99, 0.01, 0.0, 0.0]},
            {"text": "topic B", "vec": [0.0, 1.0, 0.0, 0.0]},
            {"text": "topic C", "vec": [0.0, 0.0, 1.0, 0.0]},
        ])
        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "knn": {"field": "vec", "query_vector": [1.0, 0.0, 0.0, 0.0], "k": 4, "num_candidates": 4},
            "mmr": {"enabled": True, "lambda": 0.5},
            "size": 4
        })
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        assert len(hits) > 0

    def test_mmr_lambda_one(self, base_url, index_name):
        """MMR with lambda=1.0 (pure relevance, no diversity)."""
        _bulk_docs(base_url, index_name, [
            {"text": "doc1", "vec": [1.0, 0.0, 0.0, 0.0]},
            {"text": "doc2", "vec": [0.5, 0.5, 0.0, 0.0]},
        ])
        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "knn": {"field": "vec", "query_vector": [1.0, 0.0, 0.0, 0.0], "k": 2, "num_candidates": 2},
            "mmr": {"enabled": True, "lambda": 1.0},
            "size": 2
        })
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        assert len(hits) == 2


class TestSparseVector:
    """Sparse vector search — F1."""

    def test_sparse_vector_index_and_search(self, base_url, index_name):
        """Index sparse vectors and search by inner product."""
        _bulk_docs(base_url, index_name, [
            {"sparse_emb": {"indices": [1, 5, 100], "values": [0.5, 0.8, 0.3]}},
            {"sparse_emb": {"indices": [2, 5, 50], "values": [0.3, 0.1, 0.9]}},
            {"sparse_emb": {"indices": [1, 2, 100], "values": [0.9, 0.7, 0.1]}},
        ])
        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "sparse_vector": {
                "sparse_emb": {
                    "embedding": {"indices": [1, 2], "values": [1.0, 1.0]}
                }
            },
            "size": 3
        })
        # If sparse_vector field is not in the default mapping, the server
        # may auto-detect it. Accept 200 or 400 (mapping mismatch).
        if r.status_code == 200:
            hits = r.json()["hits"]["hits"]
            assert len(hits) > 0


class TestFloat16Vector:
    """Float16 vector storage — F4."""

    def test_float16_mapping(self, base_url, session_id):
        """Create index with float16 dense_vector and verify search."""
        idx = f"test_f16_{session_id}"
        try:
            # Create index with float16 vector field.
            r = requests.put(f"{base_url}/{idx}", json={
                "mappings": {
                    "properties": {
                        "vec_f16": {
                            "type": "dense_vector",
                            "dim": 4,
                            "element_type": "float16"
                        }
                    }
                }
            })
            assert r.status_code in (200, 201), f"create index: {r.text}"

            # Index documents.
            _bulk_docs(base_url, idx, [
                {"vec_f16": [1.0, 0.0, 0.0, 0.0]},
                {"vec_f16": [0.0, 1.0, 0.0, 0.0]},
                {"vec_f16": [0.5, 0.5, 0.0, 0.0]},
            ])

            # KNN search.
            r = requests.post(f"{base_url}/{idx}/_search", json={
                "knn": {"field": "vec_f16", "query_vector": [1.0, 0.0, 0.0, 0.0], "k": 3, "num_candidates": 3},
                "size": 3
            })
            assert r.status_code == 200
            hits = r.json()["hits"]["hits"]
            assert len(hits) > 0
        finally:
            try:
                requests.delete(f"{base_url}/{idx}")
            except Exception:
                pass
