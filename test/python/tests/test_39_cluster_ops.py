"""
Test 39: Cluster ops APIs (es-feature-gap-fill).
Covers: _cluster/state, _cluster/stats, _nodes/stats, _nodes/info,
_cluster/settings, _cat/aliases, _cat/segments, _cat/templates,
_cat/recovery, _cat/tasks, _cat/thread_pool,
_nodes/usage, _nodes/capabilities, _remote/info.
"""
import pytest
import requests
import json

def _bulk_docs(base_url, index, docs):
    lines = []
    for i, d in enumerate(docs):
        lines.append('{"index": {"_index": "%s", "_id": "c%d"}}' % (index, i))
        lines.append(json.dumps(d))
    body = "\n".join(lines) + "\n"
    requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                  headers={"Content-Type": "application/x-ndjson"})


class TestClusterOps:
    def test_cluster_state(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"title": "a"}])
        r = requests.get(f"{base_url}/_cluster/state")
        assert r.status_code == 200
        data = r.json()
        assert "cluster_name" in data
        assert "nodes" in data
        assert "routing_table" in data

    def test_cluster_state_with_metric(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"title": "a"}])
        r = requests.get(f"{base_url}/_cluster/state/metadata")
        assert r.status_code == 200
        assert "metadata" in r.json()

    def test_cluster_stats(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"title": "a"}, {"title": "b"}])
        r = requests.get(f"{base_url}/_cluster/stats")
        assert r.status_code == 200
        data = r.json()
        assert "indices" in data
        assert "nodes" in data

    def test_nodes_stats(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"title": "a"}])
        r = requests.get(f"{base_url}/_nodes/stats")
        assert r.status_code == 200
        data = r.json()
        assert "nodes" in data
        assert "node-1" in data["nodes"]

    def test_nodes_info(self, base_url, index_name):
        r = requests.get(f"{base_url}/_nodes/info")
        assert r.status_code == 200
        data = r.json()
        assert "nodes" in data
        assert "node-1" in data["nodes"]

    def test_nodes_short(self, base_url, index_name):
        r = requests.get(f"{base_url}/_nodes")
        assert r.status_code == 200
        assert "nodes" in r.json()

    def test_get_cluster_settings(self, base_url, index_name):
        r = requests.get(f"{base_url}/_cluster/settings")
        assert r.status_code == 200
        data = r.json()
        assert "persistent" in data
        assert "transient" in data

    def test_put_cluster_settings(self, base_url, index_name):
        r = requests.put(f"{base_url}/_cluster/settings",
                         json={"persistent": {"cluster.max_shards_per_node": "1000"}})
        assert r.status_code == 200
        assert r.json()["acknowledged"] is True

    def test_cat_aliases(self, base_url, index_name):
        requests.put(f"{base_url}/{index_name}/_alias/test-alias")
        r = requests.get(f"{base_url}/_cat/aliases")
        assert r.status_code == 200

    def test_cat_segments(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"title": "a"}])
        r = requests.get(f"{base_url}/_cat/segments")
        assert r.status_code == 200

    def test_cat_templates(self, base_url, index_name):
        r = requests.get(f"{base_url}/_cat/templates")
        assert r.status_code == 200

    def test_cat_recovery(self, base_url, index_name):
        r = requests.get(f"{base_url}/_cat/recovery")
        assert r.status_code == 200

    def test_cat_tasks(self, base_url, index_name):
        r = requests.get(f"{base_url}/_cat/tasks")
        assert r.status_code == 200

    def test_cat_thread_pool(self, base_url, index_name):
        r = requests.get(f"{base_url}/_cat/thread_pool")
        assert r.status_code == 200

    def test_nodes_usage(self, base_url, index_name):
        r = requests.get(f"{base_url}/_nodes/usage")
        assert r.status_code == 200
        assert "nodes" in r.json()

    def test_nodes_capabilities(self, base_url, index_name):
        r = requests.get(f"{base_url}/_nodes/capabilities")
        assert r.status_code == 200
        assert "capabilities" in r.json()

    def test_remote_info(self, base_url, index_name):
        r = requests.get(f"{base_url}/_remote/info")
        assert r.status_code == 200
