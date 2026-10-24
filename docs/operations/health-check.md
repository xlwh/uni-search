# 健康检查

## 概述

UniSearch 提供 Elasticsearch 兼容的健康检查 API，用于集群状态监控、容器编排探针和负载均衡器健康检测。

## 健康状态定义

| 状态 | 说明 |
|------|------|
| `green` | 所有分片副本组 quorum 健康 |
| `yellow` | 副本缺失但主副本可用 |
| `red` | 任一分片无可用副本组 |

嵌入式单节点模式下，集群状态始终为 `green`。

## 健康检查 API

### GET /_cluster/health

```bash
curl http://localhost:9200/_cluster/health
```

响应示例：

```json
{
  "status": "green",
  "cluster_name": "unisearch",
  "number_of_nodes": 1,
  "active_primary_shards": 0,
  "active_shards": 0
}
```

### 查询参数

| 参数 | 说明 |
|------|------|
| `wait_for_status` | 阻塞等待指定状态（green/yellow/red） |
| `timeout` | 等待超时时间（如 `30s`） |

示例：

```bash
# 等待集群变为 green，最多等 30 秒
curl "http://localhost:9200/_cluster/health?wait_for_status=green&timeout=30s"
```

## _cat/health 端点

### 文本格式

```bash
curl http://localhost:9200/_cat/health
```

### 带列名

```bash
curl http://localhost:9200/_cat/health?v
```

### JSON 格式

```bash
curl http://localhost:9200/_cat/health?format=json
```

## 版本探测

### GET /

```bash
curl http://localhost:9200/
```

响应包含集群名称、版本号和分发标识，可用于验证服务是否正常运行：

```json
{
  "name": "node-1",
  "cluster_name": "unisearch",
  "version": {
    "number": "8.12.0",
    "distribution": "unisearch"
  },
  "tagline": "You Know, for Search"
}
```

## Docker HEALTHCHECK

```bash
docker run -d \
  --name unisearch \
  -p 9200:9200 \
  --health-cmd="curl -f http://localhost:9200/_cluster/health || exit 1" \
  --health-interval=10s \
  --health-timeout=5s \
  --health-retries=3 \
  --restart unless-stopped \
  unisearch:latest
```

查看健康状态：

```bash
docker inspect --format='{{.State.Health.Status}}' unisearch
# 预期: healthy
```

## K8s 探针

### Liveness Probe

```yaml
livenessProbe:
  httpGet:
    path: /_cluster/health
    port: 9200
  initialDelaySeconds: 10
  periodSeconds: 10
  failureThreshold: 3
```

### Readiness Probe

```yaml
readinessProbe:
  httpGet:
    path: /_cluster/health
    port: 9200
  initialDelaySeconds: 5
  periodSeconds: 5
  failureThreshold: 3
```

### Startup Probe

```yaml
startupProbe:
  httpGet:
    path: /_cluster/health
    port: 9200
  initialDelaySeconds: 0
  periodSeconds: 10
  failureThreshold: 30
```

## 负载均衡器健康检查

### Nginx

```nginx
upstream unisearch {
  server host1:9200;
  server host2:9200;
  server host3:9200;

  health_check interval=10s fails=3 passes=2;
}
```

### HAProxy

```
backend unisearch
  option httpchk GET /_cluster/health
  server node1 host1:9200 check
  server node2 host2:9200 check
  server node3 host3:9200 check
```

## 监控告警

### Prometheus 告警规则

```yaml
- alert: UniSearchUnhealthy
  expr: unisearch_query_errors_total rate > 0.1
  for: 2m
  labels:
    severity: critical
  annotations:
    summary: "UniSearch cluster experiencing errors"
```

### 主动健康检查脚本

```bash
#!/bin/bash
STATUS=$(curl -s http://localhost:9200/_cluster/health | jq -r '.status')
if [ "$STATUS" != "green" ]; then
  echo "ALERT: UniSearch status is $STATUS"
  exit 1
fi
echo "OK: UniSearch is green"
```

## 模式差异

### 嵌入式模式

- 单节点状态始终为 `green`
- `number_of_nodes` = 1
- 无副本概念

### 云原生模式

- 集群健康 = 所有分片健康聚合
- 分片故障影响集群状态（变为 `yellow` 或 `red`）
- Meta 节点 Raft 仲裁失败导致 `red`
- Shard 节点离线导致 `yellow`（副本不足）

## 健康检查最佳实践

| 场景 | 建议间隔 | 超时 |
|------|----------|------|
| Docker HEALTHCHECK | 10s | 5s |
| K8s liveness | 10s | 5s |
| K8s readiness | 5s | 3s |
| 负载均衡器 | 10s | 5s |
| 监控告警 | 15s | 10s |
