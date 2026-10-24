"""
Test 16: Field capabilities.
"""
import json
import requests

class TestFieldCaps:
    def test_field_caps(self, base_url, index_name):
        """GET /_field_caps returns field capabilities."""
        # Index docs with various field types
        lines = []
        for i in range(3):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"fc{i}"}}))
            lines.append(json.dumps({
                "text_field": f"text {i}",
                "keyword_field": f"keyword_{i}",
                "int_field": i,
                "float_field": float(i) * 1.5,
            }))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        r = requests.get(f"{base_url}/_field_caps?fields=*")
        assert r.status_code == 200
        data = r.json()
        assert "fields" in data or "indices" in data or len(data) > 0

    def test_field_caps_specific(self, base_url, index_name):
        """GET /{index}/_field_caps for specific index."""
        lines = []
        for i in range(2):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"fc2{i}"}}))
            lines.append(json.dumps({"title": f"cap {i}", "price": i * 10}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        r = requests.get(f"{base_url}/{index_name}/_field_caps?fields=title,price")
        assert r.status_code == 200
        data = r.json()
        assert "fields" in data or len(data) > 0
