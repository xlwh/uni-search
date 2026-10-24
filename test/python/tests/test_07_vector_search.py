"""
Test 07: KNN vector search.
"""
import json
import pytest
import requests

def _bulk_vectors(base_url, index):
    docs = [
        {"text": "animal cat", "vec": [1.0, 0.0, 0.0, 0.0]},
        {"text": "animal dog", "vec": [0.9, 0.1, 0.0, 0.0]},
        {"text": "vehicle car", "vec": [0.0, 0.0, 1.0, 0.0]},
        {"text": "vehicle truck", "vec": [0.0, 0.0, 0.9, 0.1]},
        {"text": "food pizza", "vec": [0.5, 0.5, 0.5, 0.5]},
    ]
    lines = []
    for i, d in enumerate(docs):
        lines.append(json.dumps({"index": {"_index": index, "_id": f"v{i}"}}))
        lines.append(json.dumps(d))
    body = "\n".join(lines) + "\n"
    requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                  headers={"Content-Type": "application/x-ndjson"})


class TestVectorSearch:
    def test_knn_search(self, base_url, index_name):
        """KNN search returns nearest vectors."""
        _bulk_vectors(base_url, index_name)
        r = requests.post(f"{base_url}/{index_name}/_knn_search", json={
            "knn": {
                "field": "vec",
                "query_vector": [1.0, 0.0, 0.0, 0.0],
                "k": 3,
                "num_candidates": 5
            }
        })
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        assert len(hits) > 0
        # The first hit should be the closest to [1,0,0,0]
        assert hits[0]["_source"].get("text") in ("animal cat", "animal dog")

    def test_knn_with_size(self, base_url, index_name):
        """KNN search with size parameter."""
        _bulk_vectors(base_url, index_name)
        r = requests.post(f"{base_url}/{index_name}/_knn_search", json={
            "knn": {
                "field": "vec",
                "query_vector": [0.0, 0.0, 1.0, 0.0],
                "k": 5,
                "num_candidates": 5
            },
            "size": 2
        })
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        assert len(hits) <= 2

    def test_knn_in_search(self, base_url, index_name):
        """KNN inside _search query."""
        _bulk_vectors(base_url, index_name)
        r = requests.post(f"{base_url}/{index_name}/_search", json={
            "knn": {
                "field": "vec",
                "query_vector": [0.5, 0.5, 0.5, 0.5],
                "k": 3,
                "num_candidates": 5
            }
        })
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        assert len(hits) > 0

    def test_knn_scores(self, base_url, index_name):
        """KNN results have _score > 0."""
        _bulk_vectors(base_url, index_name)
        r = requests.post(f"{base_url}/{index_name}/_knn_search", json={
            "knn": {
                "field": "vec",
                "query_vector": [1.0, 0.0, 0.0, 0.0],
                "k": 3,
                "num_candidates": 5
            }
        })
        assert r.status_code == 200
        hits = r.json()["hits"]["hits"]
        for h in hits:
            assert h.get("_score", 0) > 0
