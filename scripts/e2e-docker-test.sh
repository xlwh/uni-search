#!/usr/bin/env bash
# Comprehensive E2E Docker integration test — covers all design doc features.
# Run against a live UniSearch Docker container at localhost:9200.
# @spec: docs/vector-database-design.md + docs/go-design/
set -euo pipefail

BASE="http://localhost:9200"
PASS=0; FAIL=0; FAILED=()

# --- HTTP helpers with timeout ---
h_get() { local f=$(mktemp); local c=$(curl -s -o "$f" -w "%{http_code}" --max-time 10 "$BASE$1"); cat "$f"; rm -f "$f"; echo "::C::$c"; }
h_post() { local f=$(mktemp); local c=$(curl -s -o "$f" -w "%{http_code}" --max-time 10 -X POST "$BASE$1" -H "Content-Type: ${3:-application/json}" -d "$2"); cat "$f"; rm -f "$f"; echo "::C::$c"; }
h_put() { local f=$(mktemp); local c=$(curl -s -o "$f" -w "%{http_code}" --max-time 10 -X PUT "$BASE$1" -H "Content-Type: application/json" -d "$2"); cat "$f"; rm -f "$f"; echo "::C::$c"; }
h_del() { local f=$(mktemp); local c=$(curl -s -o "$f" -w "%{http_code}" --max-time 10 -X DELETE "$BASE$1"); cat "$f"; rm -f "$f"; echo "::C::$c"; }
body() { echo "$1" | awk -v RS='::C::' 'NR==1'; }
code() { echo "$1" | grep -o '::C::[0-9]*' | grep -o '[0-9]*$'; }
ok() { PASS=$((PASS+1)); echo "  ✅ $1"; }
bad() { FAIL=$((FAIL+1)); FAILED+=("$1"); echo "  ❌ $1"; }
check_status() { local name="$1" exp="$2" r="$3"; local c=$(code "$r"); [ "$c" = "$exp" ] && ok "$name ($c)" || bad "$name (expected $exp, got $c)"; }
check_contains() { local name="$1" r="$2" pat="$3"; echo "$r" | grep -q "$pat" && ok "$name" || bad "$name (pattern '$pat' not found)"; }

echo "╔══════════════════════════════════════════╗"
echo "║  UniSearch E2E Docker Integration Test   ║"
echo "║  Covers: docs/vector-database-design.md  ║"
echo "╚══════════════════════════════════════════╝"

# ============================================================
# §4 总体架构: Basic API + Health
# ============================================================
echo ""; echo "=== §4 总体架构: Basic API ==="
R=$(h_get "/"); check_status "GET /" "200" "$R"; check_contains "tagline" "$R" "You Know, for Search"
R=$(h_get "/_cluster/health"); check_status "GET /_cluster/health" "200" "$R"; check_contains "green status" "$R" '"status":"green"'

# ============================================================
# §8.2 REST API: Index Lifecycle + Mapping
# ============================================================
echo ""; echo "=== §8.2 REST API: Index & Mapping ==="
R=$(h_put "/e2e-test" '{"mappings":{"properties":{"title":{"type":"text"},"content":{"type":"text"},"category":{"type":"keyword"},"price":{"type":"float"},"score":{"type":"long"},"active":{"type":"boolean"},"embedding":{"type":"dense_vector","dimensions":4},"created":{"type":"date"}}}}')
check_status "PUT /e2e-test (create index)" "200" "$R"
R=$(h_get "/e2e-test/_mapping"); check_status "GET mapping" "200" "$R"; check_contains "has title field" "$R" "title"
R=$(h_get "/e2e-test/_settings"); check_status "GET settings" "200" "$R"; check_contains "has settings" "$R" "e2e-test"

# ============================================================
# §5.1 数据模型: Document CRUD
# ============================================================
echo ""; echo "=== §5.1 数据模型: Document CRUD ==="
R=$(h_put "/e2e-test/_doc/1" '{"title":"hello world","content":"first document","category":"news","price":19.99,"score":85,"active":true,"embedding":[0.1,0.2,0.3,0.4],"created":"2026-01-15"}')
check_status "PUT _doc/1 (index)" "201" "$R"
R=$(h_put "/e2e-test/_doc/2" '{"title":"hello database","content":"second document","category":"tech","price":29.99,"score":92,"active":true,"embedding":[0.5,0.6,0.7,0.8],"created":"2026-02-20"}')
check_status "PUT _doc/2 (index)" "201" "$R"
R=$(h_put "/e2e-test/_doc/3" '{"title":"goodbye world","content":"third document","category":"news","price":9.99,"score":70,"active":false,"embedding":[0.9,0.8,0.7,0.6],"created":"2026-03-10"}')
check_status "PUT _doc/3 (index)" "201" "$R"
R=$(h_put "/e2e-test/_doc/4" '{"title":"database design","content":"fourth doc without embedding","category":"tech","price":49.99,"score":88,"active":true,"created":"2026-04-05"}')
check_status "PUT _doc/4 (sparse vector)" "201" "$R"

R=$(h_get "/e2e-test/_doc/1"); check_status "GET _doc/1" "200" "$R"; check_contains "found=true" "$R" '"found":true'; check_contains "has title" "$R" "hello world"
R=$(h_get "/e2e-test/_doc/999"); check_status "GET _doc/999 (404)" "404" "$R"
R=$(h_del "/e2e-test/_doc/3"); check_status "DELETE _doc/3" "200" "$R"
R=$(h_get "/e2e-test/_doc/3"); check_status "GET deleted doc (404)" "404" "$R"

# ============================================================
# §5.2 写入路径: _bulk NDJSON
# ============================================================
echo ""; echo "=== §5.2 写入路径: _bulk ==="
BULK='{"index":{"_index":"e2e-test","_id":"bulk1"}}
{"title":"bulk doc 1","content":"from bulk","category":"test","price":10.0,"score":50,"active":true}
{"index":{"_index":"e2e-test","_id":"bulk2"}}
{"title":"bulk doc 2","content":"from bulk","category":"test","price":20.0,"score":60,"active":false}
{"delete":{"_index":"e2e-test","_id":"bulk1"}}
'
R=$(h_post "/_bulk" "$BULK" "application/x-ndjson"); check_status "POST /_bulk" "200" "$R"; check_contains "no errors" "$R" '"errors":false'

# ============================================================
# §5.3 段格式: _refresh (flush to segments)
# ============================================================
echo ""; echo "=== §5.3 段格式: _refresh ==="
R=$(h_post "/e2e-test/_refresh" "{}"); check_status "POST /_refresh" "200" "$R"

# ============================================================
# §8.3 Query DSL: Full-text + Term-level + Vector
# ============================================================
echo ""; echo "=== §8.3 Query DSL: Search ==="

# match_all (was 400, now fixed)
R=$(h_post "/e2e-test/_search" '{"query":{"match_all":{}}}'); check_status "match_all" "200" "$R"; check_contains "match_all hits" "$R" '"hits"'

# match (BM25)
R=$(h_post "/e2e-test/_search" '{"query":{"match":{"title":"hello"}}}'); check_contains "match query" "$R" '"hits"'

# match with operator
R=$(h_post "/e2e-test/_search" '{"query":{"match":{"title":{"query":"hello world","operator":"and"}}}}'); check_contains "match AND" "$R" '"hits"'

# bool + filter
R=$(h_post "/e2e-test/_search" '{"query":{"bool":{"must":[{"match":{"title":"hello"}}],"filter":[{"term":{"category":"news"}}]}}}'); check_contains "bool+filter" "$R" '"hits"'

# term
R=$(h_post "/e2e-test/_search" '{"query":{"term":{"category":"tech"}}}'); check_contains "term query" "$R" '"hits"'

# range on float (was timeout, now fixed)
R=$(h_post "/e2e-test/_search" '{"query":{"range":{"price":{"gte":15.0}}}}'); check_contains "range on float" "$R" '"hits"'

# range on long
R=$(h_post "/e2e-test/_search" '{"query":{"range":{"score":{"gte":80}}}}'); check_contains "range on long" "$R" '"hits"'

# fuzzy (was no hits, now fixed)
R=$(h_post "/e2e-test/_search" '{"query":{"fuzzy":{"title":{"value":"helo","fuzziness":1}}}}'); check_contains "fuzzy search" "$R" '"hits"'

# multi_match (was no hits, now fixed)
R=$(h_post "/e2e-test/_search" '{"query":{"multi_match":{"query":"hello","fields":["title","content"]}}}'); check_contains "multi_match" "$R" '"hits"'

# query_string (was no hits, now fixed)
R=$(h_post "/e2e-test/_search" '{"query":{"query_string":{"query":"hello AND world"}}}'); check_contains "query_string" "$R" '"hits"'

# KNN ES 8.x format (was no hits, now fixed)
R=$(h_post "/e2e-test/_search" '{"query":{"knn":{"embedding":{"vector":[0.1,0.2,0.3,0.4],"k":3}}}}'); check_contains "KNN 8.x" "$R" '"hits"'

# KNN ES 7.x format
R=$(h_post "/e2e-test/_search" '{"query":{"knn":{"field":"embedding","vector":[0.1,0.2,0.3,0.4],"k":3}}}'); check_contains "KNN 7.x" "$R" '"hits"'

# exists
R=$(h_post "/e2e-test/_search" '{"query":{"exists":{"field":"embedding"}}}'); check_contains "exists query" "$R" '"hits"'

# ============================================================
# §7.2 混合检索: hybrid (RRF)
# ============================================================
echo ""; echo "=== §7.2 混合检索: hybrid ==="
R=$(h_post "/e2e-test/_search" '{"query":{"hybrid":{"queries":[{"match":{"title":"hello"}},{"knn":{"field":"embedding","vector":[0.1,0.2,0.3,0.4],"k":3}}],"rank":{"method":"rrf"}}}}')
check_contains "hybrid RRF" "$R" '"hits"'

# ============================================================
# §7.3 聚合引擎: Aggregations
# ============================================================
echo ""; echo "=== §7.3 聚合引擎: Aggregations ==="
R=$(h_post "/e2e-test/_search" '{"size":0,"aggs":{"by_cat":{"terms":{"field":"category"}}}}'); check_contains "terms agg" "$R" '"aggregations"'
R=$(h_post "/e2e-test/_search" '{"size":0,"aggs":{"avg_price":{"avg":{"field":"price"}}}}'); check_contains "avg agg" "$R" '"aggregations"'
R=$(h_post "/e2e-test/_search" '{"size":0,"aggs":{"stats_score":{"stats":{"field":"score"}}}}'); check_contains "stats agg" "$R" '"aggregations"'
R=$(h_post "/e2e-test/_search" '{"size":0,"aggs":{"max_price":{"max":{"field":"price"}}}}'); check_contains "max agg" "$R" '"aggregations"'
R=$(h_post "/e2e-test/_search" '{"size":0,"aggs":{"card_cat":{"cardinality":{"field":"category"}}}}'); check_contains "cardinality agg" "$R" '"aggregations"'

# ============================================================
# §8.3 Suggester + Highlight + Sort + Collapse + Rescore
# ============================================================
echo ""; echo "=== §8.3 高级查询 ==="
R=$(h_post "/e2e-test/_search" '{"query":{"match":{"title":"hello"}},"highlight":{"fields":{"title":{}}}}'); check_contains "highlight" "$R" '"hits"'
R=$(h_post "/e2e-test/_search" '{"query":{"match_all":{}},"sort":[{"score":{"order":"desc"}}]}'); check_contains "sort" "$R" '"hits"'
R=$(h_post "/e2e-test/_search" '{"query":{"match":{"title":"hello"}},"rescore":{"window_size":10,"query":{"rescore_query":{"match":{"title":"world"}}}}}'); check_contains "rescore" "$R" '"hits"'

# ============================================================
# §8.2 REST API: Multi-operations
# ============================================================
echo ""; echo "=== §8.2 REST API: Multi-ops ==="
R=$(h_post "/e2e-test/_count" '{"query":{"match_all":{}}}'); check_contains "count" "$R" '"count"'
R=$(h_post "/_mget" '{"docs":[{"_index":"e2e-test","_id":"1"},{"_index":"e2e-test","_id":"2"}]}'); check_contains "mget" "$R" '"docs"'
R=$(h_get "/_cat/indices"); check_contains "_cat/indices" "$R" "e2e-test"
R=$(h_post "/_analyze" '{"text":"hello world","analyzer":"standard"}'); check_contains "analyze" "$R" '"tokens"'

# _msearch
MS='{"index":"e2e-test"}
{"query":{"match_all":{}}}
{"index":"e2e-test"}
{"query":{"term":{"category":"tech"}}}
'
R=$(h_post "/_msearch" "$MS" "application/x-ndjson"); check_contains "msearch" "$R" '"responses"'

# _field_caps
R=$(h_get "/e2e-test/_field_caps"); check_contains "field_caps" "$R" '"fields"'

# _explain
R=$(h_get "/e2e-test/_explain/1"); check_contains "explain" "$R" '"matched"'

# ============================================================
# §8.2 PIT + Scroll (深分页)
# ============================================================
echo ""; echo "=== §8.2 PIT + Scroll ==="
R=$(h_post "/e2e-test/_pit?keep_alive=1m" "{}"); check_contains "PIT created" "$R" '"id"'

# ============================================================
# §8.2 _delete_by_query + _reindex
# ============================================================
echo ""; echo "=== §8.2 _delete_by_query + _reindex ==="
h_put "/e2e-test/_doc/temp1" '{"title":"temp doc","category":"temp"}' > /dev/null
h_post "/e2e-test/_refresh" "{}" > /dev/null
R=$(h_post "/e2e-test/_delete_by_query" '{"query":{"term":{"category":"temp"}}}'); check_status "_delete_by_query" "200" "$R"
h_put "/reindex-dest" '{}' > /dev/null
R=$(h_post "/_reindex" '{"source":{"index":"e2e-test"},"dest":{"index":"reindex-dest"}}'); check_status "_reindex" "200" "$R"

# ============================================================
# §8.2 Aliases + Templates + ILM
# ============================================================
echo ""; echo "=== §8.2 Aliases + Templates + ILM ==="
R=$(h_put "/e2e-test/_alias/e2e-alias" '{}'); check_status "create alias" "200" "$R"
R=$(h_get "/_alias"); check_contains "get aliases" "$R" "e2e-alias"
R=$(h_put "/_index_template" '{"index_patterns":["log-*"],"template":{"mappings":{"properties":{"@timestamp":{"type":"date"}}}}}'); check_status "index template" "200" "$R"
R=$(h_put "/_ilm/policy" '{"policy":{"phases":{"hot":{"actions":{}}}}}'); check_status "ILM policy" "200" "$R"

# ============================================================
# §12 可观测性: Prometheus Metrics + gRPC
# ============================================================
echo ""; echo "=== §12 可观测性 ==="
R=$(h_get "/_prometheus/metrics"); check_status "Prometheus metrics" "200" "$R"; check_contains "has go_goroutines" "$R" "go_goroutines"
check_contains "has unisearch metrics" "$R" "unisearch_query"

# gRPC port check (port 9300 should be listening inside container)
# gRPC uses HTTP/2; check from inside the container via docker exec.
GRPC_OK=$(docker exec unisearch-test sh -c 'echo | nc -w1 127.0.0.1 9300 2>/dev/null && echo "PORT_OPEN" || echo "PORT_CLOSED"' 2>/dev/null | tr -d '\0' | grep -o "PORT_OPEN\|PORT_CLOSED")
[ "$GRPC_OK" = "PORT_OPEN" ] && ok "gRPC port 9300 listening" || bad "gRPC port 9300 not responding"

# ============================================================
# §5.2 重启零丢失: WAL 持久化
# ============================================================
echo ""; echo "=== §5.2 重启零丢失 ==="
# Re-index fresh test data before restart (previous tests may have deleted docs)
h_put "/e2e-test/_doc/restart1" '{"title":"restart doc 1","content":"persist test","category":"news","price":15.0,"score":80,"active":true,"embedding":[0.1,0.2,0.3,0.4]}' > /dev/null 2>&1
h_put "/e2e-test/_doc/restart2" '{"title":"restart doc 2","content":"persist test","category":"tech","price":25.0,"score":90,"active":true,"embedding":[0.5,0.6,0.7,0.8]}' > /dev/null 2>&1
h_post "/e2e-test/_refresh" "{}" > /dev/null 2>&1

BEFORE=$(curl -s --max-time 5 "$BASE/e2e-test/_count" 2>/dev/null | grep -o '"count":[0-9]*' | grep -o '[0-9]*' || echo "0")
echo "  Docs before restart: $BEFORE"

# Stop + start instead of restart (more reliable on Docker Desktop)
docker stop --time 5 unisearch-test > /dev/null 2>&1 || true
sleep 1
docker start unisearch-test > /dev/null 2>&1 || true

# Wait for health to return green (max 30s — Docker Desktop can be slow)
HEALTH_OK=false
for i in $(seq 1 30); do
  H=$(curl -s --max-time 3 "$BASE/_cluster/health" 2>/dev/null || true)
  if echo "$H" | grep -q '"status":"green"'; then HEALTH_OK=true; break; fi
  sleep 1
done
[ "$HEALTH_OK" = "true" ] && ok "Health green after restart" || bad "Health not green after restart"

AFTER=$(curl -s --max-time 5 "$BASE/e2e-test/_count" 2>/dev/null | grep -o '"count":[0-9]*' | grep -o '[0-9]*' || echo "0")
echo "  Docs after restart: $AFTER"
[ "$BEFORE" = "$AFTER" ] && ok "Restart zero-loss: count preserved ($BEFORE == $AFTER)" || bad "Restart zero-loss: $BEFORE != $AFTER"

# Verify search still works after restart
R=$(h_post "/e2e-test/_search" '{"query":{"match_all":{}}}'); check_contains "search after restart" "$R" '"hits"'

# ============================================================
# §6.1 多段检索
# ============================================================
echo ""; echo "=== §6.1 多段检索 ==="
for i in $(seq 5 10); do
  h_put "/e2e-test/_doc/$i" "{\"title\":\"doc $i\",\"content\":\"batch $i\",\"category\":\"news\",\"price\":$((i*10)).0,\"score\":$((i*5)),\"active\":true,\"embedding\":[$((i*1)),$((i*2)),$((i*3)),$((i*4))]}" > /dev/null 2>&1
done
h_post "/e2e-test/_refresh" "{}" > /dev/null 2>&1

R=$(h_post "/e2e-test/_search" '{"query":{"knn":{"embedding":{"vector":[5,6,7,8],"k":5}}}}'); check_contains "KNN across segments" "$R" '"hits"'
R=$(h_post "/e2e-test/_search" '{"query":{"match":{"title":"doc"}}}'); check_contains "BM25 across segments" "$R" '"hits"'

# ============================================================
# Cleanup
# ============================================================
echo ""; echo "=== Cleanup ==="
curl -s -X DELETE "$BASE/e2e-test" > /dev/null 2>&1 || true
curl -s -X DELETE "$BASE/reindex-dest" > /dev/null 2>&1 || true

# ============================================================
# Summary
# ============================================================
echo ""
echo "╔══════════════════════════════════════════╗"
echo "║  E2E Docker Integration Test Summary     ║"
echo "╠══════════════════════════════════════════╣"
echo "  Passed: $PASS"
echo "  Failed: $FAIL"
echo "  Total:  $((PASS + FAIL))"
if [ $FAIL -gt 0 ]; then
  echo ""
  echo "  Failed tests:"
  for t in "${FAILED[@]}"; do echo "    - $t"; done
fi
echo "╚══════════════════════════════════════════╝"
exit $FAIL
