"""
Test 41: Ingest Pipeline APIs (es-feature-gap-fill).
Covers: pipeline CRUD, _simulate, ?pipeline= write integration.
"""
import pytest
import requests
import json

class TestIngestPipeline:
    def test_put_pipeline(self, base_url, index_name):
        r = requests.put(f"{base_url}/_ingest/pipeline/test-pipeline", json={
            "description": "test pipeline",
            "processors": [
                {"set": {"field": "processed", "value": True}},
                {"convert": {"field": "price", "type": "float"}}
            ]
        })
        assert r.status_code == 200
        assert r.json()["acknowledged"] is True

    def test_get_pipeline(self, base_url, index_name):
        requests.put(f"{base_url}/_ingest/pipeline/get-pipe", json={
            "description": "get test",
            "processors": [{"set": {"field": "x", "value": 1}}]
        })
        r = requests.get(f"{base_url}/_ingest/pipeline/get-pipe")
        assert r.status_code == 200
        data = r.json()
        assert data["id"] == "get-pipe"

    def test_list_pipelines(self, base_url, index_name):
        requests.put(f"{base_url}/_ingest/pipeline/list-pipe", json={
            "processors": [{"set": {"field": "y", "value": 2}}]
        })
        r = requests.get(f"{base_url}/_ingest/pipeline")
        assert r.status_code == 200
        assert "count" in r.json()

    def test_delete_pipeline(self, base_url, index_name):
        requests.put(f"{base_url}/_ingest/pipeline/del-pipe", json={
            "processors": [{"set": {"field": "z", "value": 3}}]
        })
        r = requests.delete(f"{base_url}/_ingest/pipeline/del-pipe")
        assert r.status_code == 200
        assert r.json()["acknowledged"] is True

    def test_simulate_pipeline(self, base_url, index_name):
        requests.put(f"{base_url}/_ingest/pipeline/sim-pipe", json={
            "processors": [{"set": {"field": "added", "value": "yes"}}]
        })
        r = requests.post(f"{base_url}/_ingest/pipeline/sim-pipe/_simulate", json={
            "doc": {"message": "hello"}
        })
        assert r.status_code == 200
        doc = r.json()["doc"]
        assert doc.get("added") == "yes"

    def test_simulate_inline_pipeline(self, base_url, index_name):
        r = requests.post(f"{base_url}/_ingest/pipeline/_simulate", json={
            "pipeline": {"processors": [{"set": {"field": "x", "value": 42}}]},
            "doc": {"msg": "test"}
        })
        assert r.status_code == 200

    def test_pipeline_write_integration(self, base_url, index_name):
        requests.put(f"{base_url}/_ingest/pipeline/write-pipe", json={
            "processors": [{"set": {"field": "enriched", "value": True}}]
        })
        r = requests.post(f"{base_url}/{index_name}/_doc/p1?pipeline=write-pipe&refresh=true",
                          json={"title": "pipeline test"})
        assert r.status_code in [200, 201]
        # Verify the enriched field was added
        r2 = requests.get(f"{base_url}/{index_name}/_doc/p1")
        assert r2.status_code == 200
        src = r2.json()["_source"]
        assert src.get("enriched") is True

    def test_pipeline_remove_processor(self, base_url, index_name):
        requests.put(f"{base_url}/_ingest/pipeline/rm-pipe", json={
            "processors": [
                {"set": {"field": "keep", "value": 1}},
                {"set": {"field": "remove_me", "value": "x"}},
                {"remove": {"field": "remove_me"}}
            ]
        })
        r = requests.post(f"{base_url}/_ingest/pipeline/rm-pipe/_simulate", json={
            "doc": {"keep": 0}
        })
        assert r.status_code == 200
        doc = r.json()["doc"]
        assert "remove_me" not in doc
        assert doc.get("keep") == 1

    def test_pipeline_rename_processor(self, base_url, index_name):
        requests.put(f"{base_url}/_ingest/pipeline/rn-pipe", json={
            "processors": [{"rename": {"field": "old_name", "target_field": "new_name"}}]
        })
        r = requests.post(f"{base_url}/_ingest/pipeline/rn-pipe/_simulate", json={
            "doc": {"old_name": "value"}
        })
        assert r.status_code == 200
        doc = r.json()["doc"]
        assert "new_name" in doc
        assert "old_name" not in doc
