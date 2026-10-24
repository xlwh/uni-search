# K8s Operator 部署

## 概述

UniSearch 提供 K8s Operator（`cmd/operator`），通过 CRD `UniSearchCluster` 管理集群全生命周期。Operator 使用 controller-runtime 框架，自动创建和管理 Meta/Coordinator/Shard 角色的 StatefulSet、Headless Service 和 ConfigMap。

## 前置条件

- K8s 集群 1.25+
- kubectl 已配置
- Operator 二进制（`cmd/operator`）

## CRD 定义

### UniSearchCluster Spec

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `metaReplicas` | int32 | 3 | Meta 节点副本数 |
| `coordinatorReplicas` | int32 | 2 | Coordinator 副本数 |
| `shardReplicas` | int32 | 3 | Shard 副本数 |
| `image` | string | `unisearch:latest` | 容器镜像 |
| `storage` | StorageSpec | — | 对象存储配置 |

### StorageSpec

| 字段 | 说明 |
|------|------|
| `objectStoreEndpoint` | S3 端点地址 |
| `accessKey` | S3 AccessKey |
| `secretKey` | S3 SecretKey |
| `bucket` | S3 Bucket 名 |

### UniSearchCluster Status

| 字段 | 说明 |
|------|------|
| `phase` | `pending` / `running` / `failed` |
| `ready` | 是否就绪 |
| `leaderAddr` | Leader 节点地址 |

## 安装 Operator

### 1. 注册 CRD

CRD YAML 定义在 `deploy/k8s/operator/types.go` 的 `CRDYAML` 常量中。提取并应用：

```bash
# 应用 CRD
kubectl apply -f - <<'EOF'
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: unisearchclusters.unisearch.db
spec:
  group: unisearch.db
  names:
    kind: UniSearchCluster
    plural: unisearchclusters
    singular: unisearchcluster
  scope: Namespaced
  versions:
  - name: v1alpha1
    served: true
    storage: true
    subresources:
      status: {}
    schema:
      openAPIV3Schema:
        type: object
        properties:
          spec:
            type: object
            properties:
              metaReplicas:
                type: integer
                default: 3
              coordinatorReplicas:
                type: integer
                default: 2
              shardReplicas:
                type: integer
                default: 3
              image:
                type: string
                default: "unisearch:latest"
              storage:
                type: object
                properties:
                  objectStoreEndpoint:
                    type: string
                  accessKey:
                    type: string
                  secretKey:
                    type: string
                  bucket:
                    type: string
          status:
            type: object
            properties:
              phase:
                type: string
              ready:
                type: boolean
              leaderAddr:
                type: string
EOF
```

### 2. 部署 Operator

编译 Operator 二进制：

```bash
CGO_ENABLED=0 go build -o operator ./cmd/operator
```

以 Deployment 方式部署（需要 RBAC 权限管理 StatefulSet/Service/ConfigMap）。

Operator 启动参数：

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--metrics-bind-address` | `:8080` | Prometheus 指标端口 |
| `--health-probe-bind-address` | `:8081` | 健康探针端口 |
| `--leader-elect` | false | 是否启用 Leader 选举 |

## 创建 UniSearch 集群

```bash
kubectl apply -f - <<'EOF'
apiVersion: unisearch.db/v1alpha1
kind: UniSearchCluster
metadata:
  name: my-cluster
  namespace: default
spec:
  metaReplicas: 3
  coordinatorReplicas: 2
  shardReplicas: 3
  image: unisearch:latest
  storage:
    objectStoreEndpoint: http://minio:9000
    accessKey: minioadmin
    secretKey: minioadmin
    bucket: unisearch
EOF
```

## 查看集群状态

```bash
# 查看集群 CR
kubectl get unisearchcluster my-cluster

# 预期输出:
# NAME          PHASE      READY   LEADERADDR
# my-cluster    running    true   my-cluster-meta-0

# 查看详情
kubectl describe unisearchcluster my-cluster

# 查看创建的资源
kubectl get statefulsets
kubectl get services
kubectl get configmaps
```

## Reconciler 工作流程

Operator 的 `UniSearchClusterReconciler` 执行以下调谐循环：

1. **获取 CRD** — 读取 `UniSearchCluster` 资源
2. **确保 StatefulSet** — 为 meta/coordinator/shard 各角色创建 StatefulSet
3. **确保 Headless Service** — 创建无头服务用于 Raft 节点发现
4. **确保 ConfigMap** — 创建集群拓扑配置
5. **更新状态** — 更新 CRD Status（phase/ready/leaderAddr）

### StatefulSet 管理

每个角色（meta/coordinator/shard）创建独立的 StatefulSet：

- 镜像：从 `spec.image` 获取
- 副本数：从对应的 `*Replicas` 字段获取
- 持久化：通过 PVC 挂载数据卷
- 端口：9200（HTTP）和 9300（gRPC）
- 启动参数：根据角色生成对应的命令行参数

### Service 管理

- **Headless Service**：用于 Raft 节点间 DNS 解析和发现
- **Client Service**：可选，用于客户端负载均衡

### ConfigMap 管理

集群拓扑信息写入 ConfigMap，包含 Meta 节点地址列表、Raft 配置等。

## 集群访问

### 端口转发

```bash
# 转发 Meta 节点端口
kubectl port-forward svc/my-cluster-meta 9200:9200

# 访问
curl http://localhost:9200/_cluster/health
```

### Ingress 配置

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: unisearch-ingress
spec:
  rules:
  - host: unisearch.example.com
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: my-cluster-meta
            port:
              number: 9200
```

## 扩缩容

```bash
# 扩容 Shard 节点
kubectl patch unisearchcluster my-cluster --type=merge -p '{
  "spec": { "shardReplicas": 5 }
}'

# 缩容
kubectl patch unisearchcluster my-cluster --type=merge -p '{
  "spec": { "shardReplicas": 2 }
}'
```

Operator 检测到 Spec 变更后自动调整 StatefulSet 副本数。

## 升级

```bash
# 更新镜像版本
kubectl patch unisearchcluster my-cluster --type=merge -p '{
  "spec": { "image": "unisearch:v0.2.0" }
}'
```

Operator 触发 StatefulSet 滚动更新，逐个替换 Pod。

## 监控集成

### Prometheus 抓取

UniSearch 节点在 `/_prometheus/metrics` 暴露指标。配置 Prometheus ServiceMonitor：

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: unisearch-monitor
spec:
  selector:
    matchLabels:
      app: unisearch
  endpoints:
  - port: http
    path: /_prometheus/metrics
    interval: 15s
```

### Operator 自身指标

Operator 在 `:8080` 端口暴露 controller-runtime 指标。

### 健康探针

Operator 在 `:8081` 端口提供 `healthz` 和 `readyz` 端点，用于 K8s liveness/readiness probe。

## RBAC 权限

Operator 需要以下权限：

| 资源 | 权限 |
|------|------|
| `UniSearchCluster` | get/list/watch/update/patch |
| `StatefulSet` | get/list/watch/create/update/patch/delete |
| `Service` | get/list/watch/create/update/patch/delete |
| `ConfigMap` | get/list/watch/create/update/patch/delete |
| `Event` | create/patch |

## 故障排查

```bash
# 查看 Operator 日志
kubectl logs -l app=unisearch-operator -f

# 查看 Pod 状态
kubectl get pods -l app=unisearch

# 查看特定 Pod 日志
kubectl logs my-cluster-meta-0 -f

# 查看事件
kubectl get events --sort-by='.lastTimestamp'
```

详见 [故障排查](../operations/troubleshooting.md)。
