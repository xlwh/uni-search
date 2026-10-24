#!/usr/bin/env bash
# Comprehensive regression test against design docs (docs/go-design/)
# Run against a live UniSearch instance at localhost:9200
set -euo pipefail

BASE="http://localhost:9200"
PASS=0
FAIL=0
FAILED_TESTS=()

# Portable: get HTTP status code and body separately
http_get() {
    local url="$1"
    local tmpf=$(mktemp)
    local code=$(curl -s -o "$tmpf" -w "%{http_code}" "$url")
    cat "$tmpf"
    rm -f "$tmpf"
    echo "::CODE::$code"
}

http_post() {
    local url="$1"
    local data="$2"
    local ct="${3:-application/json}"
    local tmpf=$(mktemp)
    local code=$(curl -s -o "$tmpf" -w "%{http_code}" -X POST "$url" -H "Content-Type: $ct" -d "$data")
    cat "$tmpf"
    rm -f "$tmpf"
    echo "::CODE::$code"
}

http_put() {
    local url="$1"
    local data="$2"
    local tmpf=$(mktemp)
    local code=$(curl -s -o "$tmpf" -w "%{http_code}" -X PUT "$url" -H "Content-Type: application/json" -d "$data")
    cat "$tmpf"
    rm -f "$tmpf"
    echo "::CODE::$code"
}

http_delete() {
    local url="$1"
    local tmpf=$(mktemp)
    local code=$(curl -s -o "$tmpf" -w "%{http_code}" -X DELETE "$url")
    cat "$tmpf"
    rm -f "$tmpf"
    echo "::CODE::$code"
}

# Extract body and code from combined output
get_body() { echo "$1" | awk -v RS='::CODE::' 'NR==1'; }
get_code() { echo "$1" | grep -o '::CODE::[0-9]*' | grep -o '[0-9]*$'; }

assert_status() {
    local name="$1" expected="$2" actual="$3"
    if [ "$expected" = "$actual" ]; then
        PASS=$((PASS + 1))
        echo "  ✅ $name (status: $actual)"
    else
        FAIL=$((FAIL + 1))
        FAILED_TESTS+=("$name (expected: $expected, got: $actual)")
        echo "  ❌ $name (expected: $expected, got: $actual)"
    fi
}

assert_contains() {
    local name="$1" body="$2" pattern="$3"
    if echo "$body" | grep -q "$pattern"; then
        PASS=$((PASS + 1))
        echo "  ✅ $name"
    else
        FAIL=$((FAIL + 1))
        FAILED_TESTS+=("$name (pattern '$pattern' not found)")
        echo "  ❌ $name (pattern '$pattern' not found)"
    fi
}

echo "=========================================="
echo "UniSearch Design Doc Regression Tests"
echo "=========================================="

# ---- 00-overview.md: Basic API ----
echo ""
echo "=== 00-overview.md: Basic API ==="

RESP=$(http_get "$BASE/")
BODY=$(get_body "$RESP")
CODE=$(get_code "$RESP")
assert_status "GET / returns 200" "200" "$CODE"
assert_contains "GET / has tagline" "$BODY" "You Know, for Search"

RESP=$(http_get "$BASE/_cluster/health")
BODY=$(get_body "$RESP")
CODE=$(get_code "$RESP")
assert_status "GET /_cluster/health returns 200" "200" "$CODE"
assert_contains "Health status is green" "$BODY" '"status":"green"'

# ---- 04-es-compat-layer.md: Index & Mapping ----
echo ""
echo "=== 04-es-compat-layer.md: Index & Mapping ==="

RESP=$(http_put "$BASE/regression-test" '{"mappings":{"properties":{"title":{"type":"text"},"content":{"type":"text"},"category":{"type":"keyword"},"price":{"type":"float"},"embedding":{"type":"dense_vector","dimensions":4}}}}')
CODE=$(get_code "$RESP")
assert_status "PUT /regression-test creates index" "200" "$CODE"

RESP=$(http_get "$BASE/regression-test/_mapping")
BODY=$(get_body "$RESP")
CODE=$(get_code "$RESP")
assert_status "GET /regression-test/_mapping returns 200" "200" "$CODE"
assert_contains "Mapping has title field" "$BODY" "title"

# ---- 01-storage-engine.md: Document CRUD ----
echo ""
echo "=== 01-storage-engine.md: Document CRUD ==="

RESP=$(http_put "$BASE/regression-test/_doc/1" '{"title":"hello world","content":"first document","category":"news","price":19.99,"embedding":[0.1,0.2,0.3,0.4]}')
CODE=$(get_code "$RESP")
assert_status "PUT /regression-test/_doc/1 (index)" "201" "$CODE"

RESP=$(http_put "$BASE/regression-test/_doc/2" '{"title":"hello database","content":"second document","category":"tech","price":29.99,"embedding":[0.5,0.6,0.7,0.8]}')
CODE=$(get_code "$RESP")
assert_status "PUT /regression-test/_doc/2 (index)" "201" "$CODE"

RESP=$(http_post "$BASE/regression-test/_doc" '{"title":"goodbye world","content":"third document","category":"news","price":9.99}')
CODE=$(get_code "$RESP")
assert_status "POST /regression-test/_doc (auto ID)" "201" "$CODE"

RESP=$(http_get "$BASE/regression-test/_doc/1")
BODY=$(get_body "$RESP")
CODE=$(get_code "$RESP")
assert_status "GET /regression-test/_doc/1 returns 200" "200" "$CODE"
assert_contains "Doc 1 found" "$BODY" '"found":true'
assert_contains "Doc 1 has title" "$BODY" "hello world"

RESP=$(http_get "$BASE/regression-test/_doc/999")
CODE=$(get_code "$RESP")
assert_status "GET /regression-test/_doc/999 returns 404" "404" "$CODE"

RESP=$(http_delete "$BASE/regression-test/_doc/2")
CODE=$(get_code "$RESP")
assert_status "DELETE /regression-test/_doc/2" "200" "$CODE"

RESP=$(http_get "$BASE/regression-test/_doc/2")
CODE=$(get_code "$RESP")
assert_status "GET deleted doc returns 404" "404" "$CODE"

# ---- 01-storage-engine.md: _bulk NDJSON ----
echo ""
echo "=== 01-storage-engine.md: _bulk API ==="

BULK_BODY='{"index":{"_index":"regression-test","_id":"bulk1"}}
{"title":"bulk doc 1","content":"from bulk","category":"test","price":10.0}
{"index":{"_index":"regression-test","_id":"bulk2"}}
{"title":"bulk doc 2","content":"from bulk","category":"test","price":20.0}
{"delete":{"_index":"regression-test","_id":"bulk1"}}
'
RESP=$(http_post "$BASE/_bulk" "$BULK_BODY" "application/x-ndjson")
BODY=$(get_body "$RESP")
CODE=$(get_code "$RESP")
assert_status "POST /_bulk returns 200" "200" "$CODE"
assert_contains "Bulk has no errors" "$BODY" '"errors":false'

# ---- 03-query-engine.md: Search & DSL ----
echo ""
echo "=== 03-query-engine.md: Search & DSL ==="

curl -s -X POST "$BASE/regression-test/_refresh" > /dev/null

RESP=$(http_post "$BASE/regression-test/_search" '{"query":{"match_all":{}}}')
BODY=$(get_body "$RESP")
CODE=$(get_code "$RESP")
assert_status "match_all search returns 200" "200" "$CODE"
assert_contains "Search has hits" "$BODY" '"hits"'

RESP=$(http_post "$BASE/regression-test/_search" '{"query":{"match":{"title":"hello"}}}')
BODY=$(get_body "$RESP")
assert_contains "Match query returns results" "$BODY" '"hits"'

RESP=$(http_post "$BASE/regression-test/_search" '{"query":{"bool":{"must":[{"match":{"title":"hello"}}],"filter":[{"term":{"category":"news"}}]}}}')
BODY=$(get_body "$RESP")
assert_contains "Bool+filter query returns results" "$BODY" '"hits"'

RESP=$(http_post "$BASE/regression-test/_search" '{"query":{"term":{"category":"news"}}}')
BODY=$(get_body "$RESP")
assert_contains "Term query returns results" "$BODY" '"hits"'

RESP=$(http_post "$BASE/regression-test/_search" '{"query":{"range":{"price":{"gte":15.0}}}}')
BODY=$(get_body "$RESP")
assert_contains "Range query returns results" "$BODY" '"hits"'

# ---- 03-query-engine.md: Aggregation ----
echo ""
echo "=== 03-query-engine.md: Aggregation ==="

RESP=$(http_post "$BASE/regression-test/_search" '{"size":0,"aggs":{"by_category":{"terms":{"field":"category"},"avg_price":{"avg":{"field":"price"}}}}}')
BODY=$(get_body "$RESP")
assert_contains "Aggregation returns buckets" "$BODY" '"aggregations"'

# ---- 03-query-engine.md: Sort ----
echo ""
echo "=== 03-query-engine.md: Sort ==="

RESP=$(http_post "$BASE/regression-test/_search" '{"query":{"match_all":{}},"sort":[{"price":{"order":"desc"}}]}')
BODY=$(get_body "$RESP")
assert_contains "Sort query returns results" "$BODY" '"hits"'

# ---- 02-index-engine.md: Vector Search ----
echo ""
echo "=== 02-index-engine.md: Vector Search ==="

RESP=$(http_post "$BASE/regression-test/_search" '{"query":{"knn":{"embedding":{"vector":[0.1,0.2,0.3,0.4],"k":5}}}}')
BODY=$(get_body "$RESP")
assert_contains "KNN vector search returns hits" "$BODY" '"hits"'

# ---- 04-es-compat-layer.md: _count ----
echo ""
echo "=== 04-es-compat-layer.md: _count ==="

RESP=$(http_post "$BASE/regression-test/_count" '{"query":{"match_all":{}}}')
BODY=$(get_body "$RESP")
assert_contains "Count returns count" "$BODY" '"count"'

# ---- 04-es-compat-layer.md: _mget ----
echo ""
echo "=== 04-es-compat-layer.md: _mget ==="

RESP=$(http_post "$BASE/_mget" '{"docs":[{"_index":"regression-test","_id":"1"}]}')
BODY=$(get_body "$RESP")
assert_contains "MGet returns docs" "$BODY" '"docs"'

# ---- 04-es-compat-layer.md: _cat/indices ----
echo ""
echo "=== 04-es-compat-layer.md: _cat/indices ==="

RESP=$(http_get "$BASE/_cat/indices")
BODY=$(get_body "$RESP")
assert_contains "_cat/indices returns index" "$BODY" "regression-test"

# ---- 04-es-compat-layer.md: _analyze ----
echo ""
echo "=== 04-es-compat-layer.md: _analyze ==="

RESP=$(http_post "$BASE/_analyze" '{"text":"hello world","analyzer":"standard"}')
BODY=$(get_body "$RESP")
assert_contains "Analyze returns tokens" "$BODY" '"tokens"'

# ---- 04-es-compat-layer.md: _refresh ----
echo ""
echo "=== 04-es-compat-layer.md: _refresh ==="

RESP=$(http_post "$BASE/regression-test/_refresh" '')
CODE=$(get_code "$RESP")
assert_status "_refresh returns 200" "200" "$CODE"

# ---- 04-es-compat-layer.md: _aliases ----
echo ""
echo "=== 04-es-compat-layer.md: _aliases ==="

RESP=$(http_put "$BASE/regression-test/_alias/reg-alias" '{}')
CODE=$(get_code "$RESP")
assert_status "Create alias returns 200" "200" "$CODE"

RESP=$(http_get "$BASE/_alias")
BODY=$(get_body "$RESP")
assert_contains "Get aliases returns data" "$BODY" "reg-alias"

# ---- 04-es-compat-layer.md: _settings ----
echo ""
echo "=== 04-es-compat-layer.md: _settings ==="

RESP=$(http_get "$BASE/regression-test/_settings")
BODY=$(get_body "$RESP")
assert_contains "Get settings returns data" "$BODY" "regression-test"

# ---- 06-quality-and-observability.md: Prometheus Metrics ----
echo ""
echo "=== 06-quality-and-observability.md: Prometheus Metrics ==="

RESP=$(http_get "$BASE/_prometheus/metrics")
BODY=$(get_body "$RESP")
CODE=$(get_code "$RESP")
assert_status "GET /_prometheus/metrics returns 200" "200" "$CODE"
assert_contains "Metrics has go_goroutines" "$BODY" "go_goroutines"

# ---- 04-es-compat-layer.md: Highlight ----
echo ""
echo "=== 04-es-compat-layer.md: Highlight ==="

RESP=$(http_post "$BASE/regression-test/_search" '{"query":{"match":{"title":"hello"}},"highlight":{"fields":{"title":{}}}}')
BODY=$(get_body "$RESP")
assert_contains "Highlight returns results" "$BODY" '"hits"'

# ---- 04-es-compat-layer.md: _msearch ----
echo ""
echo "=== 04-es-compat-layer.md: _msearch ==="

MSBODY='{"index":"regression-test"}
{"query":{"match_all":{}}}
'
RESP=$(http_post "$BASE/_msearch" "$MSBODY" "application/x-ndjson")
BODY=$(get_body "$RESP")
assert_contains "MSearch returns responses" "$BODY" '"responses"'

# ---- 04-es-compat-layer.md: _field_caps ----
echo ""
echo "=== 04-es-compat-layer.md: _field_caps ==="

RESP=$(http_get "$BASE/regression-test/_field_caps")
BODY=$(get_body "$RESP")
assert_contains "Field caps returns fields" "$BODY" '"fields"'

# ---- 04-es-compat-layer.md: _explain ----
echo ""
echo "=== 04-es-compat-layer.md: _explain ==="

RESP=$(http_get "$BASE/regression-test/_explain/1")
BODY=$(get_body "$RESP")
assert_contains "Explain returns matched" "$BODY" '"matched"'

# ---- 04-es-compat-layer.md: PIT ----
echo ""
echo "=== 04-es-compat-layer.md: PIT ==="

RESP=$(http_post "$BASE/regression-test/_pit?keep_alive=1m" '')
BODY=$(get_body "$RESP")
assert_contains "PIT returns id" "$BODY" '"id"'

# ---- 04-es-compat-layer.md: _delete_by_query ----
echo ""
echo "=== 04-es-compat-layer.md: _delete_by_query ==="

curl -s -X PUT "$BASE/regression-test/_doc/del-me" -H "Content-Type: application/json" -d '{"title":"delete me","category":"temp"}' > /dev/null
curl -s -X POST "$BASE/regression-test/_refresh" > /dev/null

RESP=$(http_post "$BASE/regression-test/_delete_by_query" '{"query":{"term":{"category":"temp"}}}')
CODE=$(get_code "$RESP")
assert_status "_delete_by_query returns 200" "200" "$CODE"

# ---- 04-es-compat-layer.md: _reindex ----
echo ""
echo "=== 04-es-compat-layer.md: _reindex ==="

curl -s -X PUT "$BASE/reindex-dst" -H "Content-Type: application/json" -d '{}' > /dev/null
RESP=$(http_post "$BASE/_reindex" '{"source":{"index":"regression-test"},"dest":{"index":"reindex-dst"}}')
CODE=$(get_code "$RESP")
assert_status "_reindex returns 200" "200" "$CODE"

# ---- 04-es-compat-layer.md: Templates & ILM ----
echo ""
echo "=== 04-es-compat-layer.md: Templates & ILM ==="

RESP=$(http_put "$BASE/_index_template" '{"index_patterns":["log-*"],"template":{"mappings":{"properties":{"@timestamp":{"type":"date"}}}}}')
CODE=$(get_code "$RESP")
assert_status "PUT /_index_template returns 200" "200" "$CODE"

RESP=$(http_put "$BASE/_ilm/policy" '{"policy":{"phases":{"hot":{"actions":{}}}}}')
CODE=$(get_code "$RESP")
assert_status "PUT /_ilm/policy returns 200" "200" "$CODE"

# ---- 02-index-engine.md: Fuzzy Search ----
echo ""
echo "=== 02-index-engine.md: Fuzzy Search ==="

RESP=$(http_post "$BASE/regression-test/_search" '{"query":{"fuzzy":{"title":{"value":"helo","fuzziness":1}}}}')
BODY=$(get_body "$RESP")
assert_contains "Fuzzy search returns results" "$BODY" '"hits"'

# ---- 03-query-engine.md: Multi-match ----
echo ""
echo "=== 03-query-engine.md: Multi-match ==="

RESP=$(http_post "$BASE/regression-test/_search" '{"query":{"multi_match":{"query":"hello","fields":["title","content"]}}}')
BODY=$(get_body "$RESP")
assert_contains "Multi-match returns results" "$BODY" '"hits"'

# ---- 03-query-engine.md: Query String ----
echo ""
echo "=== 03-query-engine.md: Query String ==="

RESP=$(http_post "$BASE/regression-test/_search" '{"query":{"query_string":{"query":"hello AND world"}}}')
BODY=$(get_body "$RESP")
assert_contains "Query string returns results" "$BODY" '"hits"'

# ---- 01-storage-engine.md: Restart Zero-Loss ----
echo ""
echo "=== 01-storage-engine.md: Restart Zero-Loss ==="

BEFORE=$(curl -s "$BASE/regression-test/_count" | grep -o '"count":[0-9]*' | grep -o '[0-9]*')
echo "  Docs before restart: $BEFORE"

docker restart unisearch-test > /dev/null 2>&1
sleep 3

RESP=$(http_get "$BASE/_cluster/health")
assert_contains "Health green after restart" "$RESP" '"status":"green"'

AFTER=$(curl -s "$BASE/regression-test/_count" | grep -o '"count":[0-9]*' | grep -o '[0-9]*')
echo "  Docs after restart: $AFTER"

if [ "$BEFORE" = "$AFTER" ]; then
    PASS=$((PASS + 1))
    echo "  ✅ Restart zero-loss: doc count preserved ($BEFORE == $AFTER)"
else
    FAIL=$((FAIL + 1))
    FAILED_TESTS+=("Restart zero-loss: $BEFORE != $AFTER")
    echo "  ❌ Restart zero-loss: $BEFORE != $AFTER"
fi

# ---- Cleanup ----
echo ""
echo "=== Cleanup ==="
curl -s -X DELETE "$BASE/regression-test" > /dev/null
curl -s -X DELETE "$BASE/reindex-dst" > /dev/null

# ---- Summary ----
echo ""
echo "=========================================="
echo "Regression Test Summary"
echo "=========================================="
echo "  Passed: $PASS"
echo "  Failed: $FAIL"
echo "  Total:  $((PASS + FAIL))"
if [ $FAIL -gt 0 ]; then
    echo ""
    echo "Failed tests:"
    for t in "${FAILED_TESTS[@]}"; do
        echo "  - $t"
    done
fi
echo "=========================================="

exit $FAIL
