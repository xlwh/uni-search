"""
Test 40: Snapshot APIs (es-feature-gap-fill).
Covers: repo management, snapshot create/restore/delete/verify.
"""
import pytest
import requests
import json
import os
import tempfile

def _bulk_docs(base_url, index, docs):
    lines = []
    for i, d in enumerate(docs):
        lines.append('{"index": {"_index": "%s", "_id": "p%d"}}' % (index, i))
        lines.append(json.dumps(d))
    body = "\n".join(lines) + "\n"
    requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                  headers={"Content-Type": "application/x-ndjson"})


class TestSnapshot:
    def test_put_fs_repo(self, base_url, index_name):
        r = requests.put(f"{base_url}/_snapshot/test-repo",
                         json={"type": "fs", "settings": {"location": "/tmp/snap-test-repo"}})
        assert r.status_code == 200
        assert r.json()["acknowledged"] is True

    def test_list_repos(self, base_url, index_name):
        requests.put(f"{base_url}/_snapshot/test-repo2",
                      json={"type": "fs", "settings": {"location": "/tmp/snap-test-repo2"}})
        r = requests.get(f"{base_url}/_snapshot")
        assert r.status_code == 200
        assert "count" in r.json()

    def test_create_snapshot(self, base_url, index_name):
        _bulk_docs(base_url, index_name, [{"title": "snap-doc-1"}])
        requests.put(f"{base_url}/_snapshot/snap-repo",
                     json={"type": "fs", "settings": {"location": "/tmp/snap-create-test"}})
        r = requests.put(f"{base_url}/_snapshot/snap-repo/snap-001")
        assert r.status_code == 200
        assert "snapshot" in r.json()

    def test_list_snapshots(self, base_url, index_name):
        requests.put(f"{base_url}/_snapshot/list-repo",
                     json={"type": "fs", "settings": {"location": "/tmp/snap-list-test"}})
        _bulk_docs(base_url, index_name, [{"title": "x"}])
        requests.put(f"{base_url}/_snapshot/list-repo/snap-list-001")
        r = requests.get(f"{base_url}/_snapshot/list-repo")
        assert r.status_code == 200
        assert "snapshots" in r.json()

    def test_delete_snapshot(self, base_url, index_name):
        requests.put(f"{base_url}/_snapshot/del-repo",
                     json={"type": "fs", "settings": {"location": "/tmp/snap-del-test"}})
        _bulk_docs(base_url, index_name, [{"title": "x"}])
        requests.put(f"{base_url}/_snapshot/del-repo/snap-del-001")
        r = requests.delete(f"{base_url}/_snapshot/del-repo/snap-del-001")
        assert r.status_code == 200
        assert r.json()["acknowledged"] is True

    def test_verify_repo(self, base_url, index_name):
        requests.put(f"{base_url}/_snapshot/verify-repo",
                     json={"type": "fs", "settings": {"location": "/tmp/snap-verify-test"}})
        r = requests.post(f"{base_url}/_snapshot/verify-repo/_verify")
        assert r.status_code == 200
        assert r.json()["acknowledged"] is True

    def test_delete_repo(self, base_url, index_name):
        requests.put(f"{base_url}/_snapshot/delrepo",
                     json={"type": "fs", "settings": {"location": "/tmp/snap-delrepo-test"}})
        r = requests.delete(f"{base_url}/_snapshot/delrepo")
        assert r.status_code == 200
        assert r.json()["acknowledged"] is True

    def test_restore_snapshot(self, base_url, index_name):
        requests.put(f"{base_url}/_snapshot/restore-repo",
                     json={"type": "fs", "settings": {"location": "/tmp/snap-restore-test"}})
        _bulk_docs(base_url, index_name, [{"title": "restore-me"}])
        requests.put(f"{base_url}/_snapshot/restore-repo/snap-restore-001")
        r = requests.post(f"{base_url}/_snapshot/restore-repo/snap-restore-001/_restore")
        assert r.status_code == 200
        assert r.json()["acknowledged"] is True
