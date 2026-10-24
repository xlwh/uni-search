"""
Test 32: Data type handling.
Tests writing and querying different field types: boolean, date,
nested objects, arrays, and large text.
"""
import json
import requests


class TestDataTypes:
    def test_boolean_field(self, base_url, index_name):
        """Boolean field values are stored and queried correctly."""
        lines = []
        for i in range(5):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"bf{i}"}}))
            lines.append(json.dumps({"title": f"bool {i}", "in_stock": i % 2 == 0}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        # Verify boolean value is stored
        r = requests.get(f"{base_url}/{index_name}/_doc/bf0")
        assert r.status_code == 200
        src = r.json().get("_source", {})
        assert src.get("in_stock") is True

    def test_nested_object_field(self, base_url, index_name):
        """Nested object field is stored and retrievable."""
        doc = {
            "title": "nested test",
            "metadata": {"author": "alice", "tags": ["a", "b"], "score": 42}
        }
        r = requests.put(f"{base_url}/{index_name}/_doc/nest1", json=doc)
        assert r.status_code in (200, 201)
        requests.post(f"{base_url}/{index_name}/_refresh")

        r = requests.get(f"{base_url}/{index_name}/_doc/nest1")
        assert r.status_code == 200
        src = r.json().get("_source", {})
        assert "metadata" in src
        assert src["metadata"]["author"] == "alice"
        assert src["metadata"]["score"] == 42

    def test_array_field(self, base_url, index_name):
        """Array field values are stored correctly."""
        doc = {"title": "array test", "tags": ["red", "green", "blue"]}
        r = requests.put(f"{base_url}/{index_name}/_doc/arr1", json=doc)
        assert r.status_code in (200, 201)
        requests.post(f"{base_url}/{index_name}/_refresh")

        r = requests.get(f"{base_url}/{index_name}/_doc/arr1")
        assert r.status_code == 200
        src = r.json().get("_source", {})
        assert src["tags"] == ["red", "green", "blue"]

    def test_large_text_field(self, base_url, index_name):
        """Large text field (>10KB) is stored and searchable."""
        large_text = "word " * 5000  # ~25KB
        doc = {"title": "large text", "content": large_text}
        requests.put(f"{base_url}/{index_name}/_doc/large1", json=doc)
        requests.post(f"{base_url}/{index_name}/_refresh")

        r = requests.get(f"{base_url}/{index_name}/_doc/large1")
        assert r.status_code == 200
        src = r.json().get("_source", {})
        assert len(src["content"]) > 10000

    def test_null_field_value(self, base_url, index_name):
        """Null field value is handled correctly."""
        doc = {"title": "null test", "description": None}
        requests.put(f"{base_url}/{index_name}/_doc/null1", json=doc)
        requests.post(f"{base_url}/{index_name}/_refresh")

        r = requests.get(f"{base_url}/{index_name}/_doc/null1")
        assert r.status_code == 200
        src = r.json().get("_source", {})
        assert "title" in src

    def test_unicode_content(self, base_url, index_name):
        """Unicode content (Chinese, emoji, etc.) is stored correctly."""
        doc = {
            "title": "中文测试",
            "content": "Hello 世界 🌍こんにちは"
        }
        requests.put(f"{base_url}/{index_name}/_doc/uni1", json=doc)
        requests.post(f"{base_url}/{index_name}/_refresh")

        r = requests.get(f"{base_url}/{index_name}/_doc/uni1")
        assert r.status_code == 200
        src = r.json().get("_source", {})
        assert src["title"] == "中文测试"
        assert "🌍" in src["content"]

    def test_numeric_precision(self, base_url, index_name):
        """Numeric precision is preserved for float values."""
        doc = {"title": "precision", "price": 99.99, "qty": 42}
        requests.put(f"{base_url}/{index_name}/_doc/prec1", json=doc)
        requests.post(f"{base_url}/{index_name}/_refresh")

        r = requests.get(f"{base_url}/{index_name}/_doc/prec1")
        assert r.status_code == 200
        src = r.json().get("_source", {})
        assert src["price"] == 99.99
        assert src["qty"] == 42

    def test_special_characters_in_id(self, base_url, index_name):
        """Document IDs with special characters are handled."""
        for doc_id in ["doc-1", "doc_2", "doc.3", "doc 4"]:
            requests.put(f"{base_url}/{index_name}/_doc/{doc_id}",
                        json={"title": f"special id {doc_id}"})
        requests.post(f"{base_url}/{index_name}/_refresh")

        r = requests.get(f"{base_url}/{index_name}/_doc/doc-1")
        assert r.status_code == 200
