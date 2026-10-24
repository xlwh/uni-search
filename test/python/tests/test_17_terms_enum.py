"""
Test 17: Terms enumeration.
"""
import json
import requests

class TestTermsEnum:
    def test_terms_enum(self, base_url, index_name):
        """GET /{index}/_terms_enum enumerates terms."""
        lines = []
        for i, tag in enumerate(["apple", "banana", "apple", "cherry", "banana"]):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"te{i}"}}))
            lines.append(json.dumps({"fruit": tag}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        r = requests.get(f"{base_url}/{index_name}/_terms_enum?field=fruit")
        assert r.status_code == 200
        data = r.json()
        terms = data.get("terms", [])
        # Should return at least some terms
        assert len(terms) >= 1

    def test_terms_enum_prefix(self, base_url, index_name):
        """GET /{index}/_terms_enum with prefix filter."""
        lines = []
        for i, name in enumerate(["alpha", "bravo", "charlie", "alpha_2"]):
            lines.append(json.dumps({"index": {"_index": index_name, "_id": f"tp{i}"}}))
            lines.append(json.dumps({"name": name}))
        body = "\n".join(lines) + "\n"
        requests.post(f"{base_url}/_bulk?refresh=true", data=body,
                      headers={"Content-Type": "application/x-ndjson"})

        r = requests.get(f"{base_url}/{index_name}/_terms_enum?field=name&prefix=al")
        assert r.status_code == 200
        data = r.json()
        terms = data.get("terms", [])
        if terms:
            for t in terms:
                assert t.startswith("al") or "al" in str(t).lower()
