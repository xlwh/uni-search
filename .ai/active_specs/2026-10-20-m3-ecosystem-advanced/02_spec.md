# Spec: M3 生态与进阶 W7.2-W7.8（W7.1 GPU 暂缓）

## 1. 概述 (Overview)

本 Spec 将 M3 Proposal 转化为可执行的工程细节。M3 是 UniSearch 路线图的最后一个里程碑，目标是让系统从"可用"升级为"可迁移、可扩展、可运营"。

设计评审共 2 轮，5 个技术决策点已确认。

### 1.1 设计评审决策汇总

| # | 决策点 | 选择 |
|---|--------|------|
| D1 | W7.2 ONNX 模型 | MiniLM Cross-Encoder（22MB，延迟 <50ms） |
| D2 | W7.5 插件架构 | 进程内注册 + gRPC sidecar 双模式 |
| D3 | W7.7 K8s Operator | controller-runtime 框架 |
| D4 | W7.6 uniadm 工作模式 | 静态分析模式（无集群连接） |
| D5 | W7.8 Go SDK 通信 | HTTP REST（兼容 ES 客户端接口） |

### 1.2 变更清单

| 类型 | 文件路径 | 说明 |
|------|----------|------|
| 新增 | `internal/engine/rerank/` | ONNX 本地推理 + MiniLM 模型加载 + 打分 |
| 新增 | `internal/engine/percolate/` | 查询即文档存储与匹配引擎 |
| 新增 | `internal/plugin/` | 插件 SDK（进程内注册 + gRPC sidecar 协议） |
| 新增 | `cmd/uniadm/escompat/` | ES 存量扫描评估工具（静态分析） |
| 新增 | `deploy/k8s/operator/` | K8s Operator CRD + controller-runtime |
| 新增 | `pkg/unisdk/` | Go SDK（HTTP REST 客户端） |
| 修改 | `internal/engine/fusion/fusion.go` | rerank 分支扩展 |
| 修改 | `internal/index/scalar/` | BKD 面/线编码（geo_shape） |
| 修改 | `internal/engine/dsl/parser.go` | 解除 percolate/geo_shape unsupported 限制 |
| 修改 | `cmd/uniadm/main.go` | es-compat 子命令骨架 |
| 修改 | `internal/index/registry.go` | 插件 RemoteIndex 接口 |

---

## 2. 详细设计 (Detailed Design)

### 2.1 W7.2 内置 rerank（ONNX 本地推理）

**设计引用**: `docs/go-design/03-query-engine.md §5`

**ONNX 模型**（D1: MiniLM Cross-Encoder）:
```go
// internal/engine/rerank/reranker.go
type Reranker interface {
    Rerank(query string, docs []RerankDoc) ([]RerankResult, error)
    Close() error
}

type RerankDoc struct {
    ID      string
    Title   string
    Content string
}

type RerankResult struct {
    ID    string
    Score float64
}

// ONNXReranker implements Reranker using ONNX Runtime.
type ONNXReranker struct {
    session  *onnxruntime.AdvancedSession
    tokenizer Tokenizer
}
```

**关键接口**:
```go
func NewONNXReranker(modelPath string) (*ONNXReranker, error)
func (r *ONNXReranker) Rerank(query string, docs []RerankDoc) ([]RerankResult, error)
```

**模型**: MiniLM-L6-H384 Cross-Encoder（22MB），输入 `[query, doc]` 对，输出 relevance score。
**延迟目标**: <50ms（单次 rerank 10 文档）。
**依赖**: `github.com/yalue/onnxruntime_go`

### 2.2 W7.3 geo_shape

**设计引用**: `docs/go-design/02-index-engine.md §6`, 概要设计 §8.3

```go
// internal/index/scalar/geo_shape.go
type GeoShape struct {
    Type        string  // "polygon" | "linestring" | "multipolygon"
    Coordinates [][][]float64
}

// BKD 扩展：从点（1D/2D）扩展到面/线编码
func EncodePolygon(polygon GeoShape) []byte
func EncodeLineString(line GeoShape) []byte
func QueryIntersects(shape GeoShape, bkd BKDIndex) []uint32
func QueryWithin(shape GeoShape, bkd BKDIndex) []uint32
```

**修改**: `internal/engine/dsl/parser.go` 解除 `geo_shape` unsupported 限制（当前标记为 v1.3 M3）。

### 2.3 W7.4 percolate 完整化

**设计引用**: `docs/go-design/04-es-compat-layer.md §3.3`

```go
// internal/engine/percolate/percolator.go
type Percolator struct {
    queries map[string]*StoredQuery  // query ID → stored DSL
}

type StoredQuery struct {
    ID    string
    Query []byte  // raw DSL JSON
}

func NewPercolator() *Percolator
func (p *Percolator) Register(queryID string, query []byte) error
func (p *Percolator) Match(doc []byte) ([]string, error)  // returns matching query IDs
func (p *Percolator) Unregister(queryID string) error
```

### 2.4 W7.5 插件 SDK

**设计引用**: `docs/go-design/02-index-engine.md §1.2`

**双模式**（D2: 进程内 + gRPC sidecar）:
```go
// internal/plugin/registry.go
type PluginRegistry struct {
    inProcess map[string]index.IndexFactory
    sidecars  map[string]*SidecarClient
}

// 进程内插件：Go interface 注册
func RegisterInProcess(name string, factory index.IndexFactory)

// sidecar 插件：gRPC 通信
type SidecarClient struct {
    conn *grpc.ClientConn
}
func RegisterSidecar(name, addr string) error

// RemoteIndex 接口（sidecar 插件实现）
type RemoteIndex interface {
    Build(ctx, provider, output) error
    Load(input) (index.LoadedIndex, error)
    Search(ctx, query, budget) (*index.SearchResult, error)
}
```

### 2.5 W7.6 uniadm es-compat

**设计引用**: `docs/go-design/04-es-compat-layer.md §9`

**静态分析模式**（D4）:
```go
// cmd/uniadm/escompat/escompat.go
func RunScan(mappingFile string, queryLogFile string) (*CompatReport, error)

type CompatReport struct {
    IndexName    string
    MappingScore float64  // 兼容性评分 0-1
    Issues       []CompatIssue
    QueryCoverage float64
}

type CompatIssue struct {
    Field     string
    Type      string  // "unsupported_type" | "unsupported_query" | "mapping_conflict"
    Detail    string
    Severity  string  // "error" | "warning" | "info"
}
```

**工作流程**: 读取 ES mapping JSON → 用 M1 兼容矩阵规则评估 → 读取查询日志 → 评估查询兼容性 → 输出报告。

### 2.6 W7.7 K8s Operator

**设计引用**: `docs/go-design/05-distributed.md §8`

**controller-runtime**（D3）:
```go
// deploy/k8s/operator/api/v1alpha1/unisearchcluster_types.go
type UniSearchCluster struct {
    Spec   UniSearchClusterSpec
    Status UniSearchClusterStatus
}

type UniSearchClusterSpec struct {
    MetaReplicas    int32
    CoordinatorReplicas int32
    ShardReplicas   int32
    Storage         StorageSpec
}

// deploy/k8s/operator/controllers/unisearchcluster_controller.go
type UniSearchClusterReconciler struct {
    client.Client
    Scheme *runtime.Scheme
}
func (r *UniSearchClusterReconciler) Reconcile(ctx, req) (ctrl.Result, error)
```

### 2.7 W7.8 Go SDK

**设计引用**: `docs/go-design/00-overview.md §4`, 概要设计 §10`

**HTTP REST**（D5）:
```go
// pkg/unisdk/client.go
type Client struct {
    baseURL string
    http    *http.Client
}

func NewClient(addr string) *Client
func (c *Client) Index(ctx, index, id string, doc any) (*IndexResult, error)
func (c *Client) Search(ctx, index string, body []byte) (*SearchResult, error)
func (c *Client) Bulk(ctx, body []byte) (*BulkResult, error)
func (c *Client) Get(ctx, index, id string) (*GetResult, error)
func (c *Client) Delete(ctx, index, id string) error
func (c *Client) CreateIndex(ctx, index string, body []byte) error
func (c *Client) Refresh(ctx, index string) error
```

---

## 3. 接口设计 (Interface Design)

### 3.1 Rerank 接口
```go
type Reranker interface {
    Rerank(query string, docs []RerankDoc) ([]RerankResult, error)
    Close() error
}
```

### 3.2 插件 SDK 接口
```go
type RemoteIndex interface {
    Build(ctx context.Context, provider index.ColumnProvider, output index.IndexOutput) error
    Load(input index.IndexInput) (index.LoadedIndex, error)
    Search(ctx context.Context, q index.Query, budget index.SearchBudget) (*index.SearchResult, error)
}
```

### 3.3 Go SDK 接口
```go
type Client interface {
    Index(ctx context.Context, index, id string, doc any) (*IndexResult, error)
    Search(ctx context.Context, index string, body []byte) (*SearchResult, error)
    Bulk(ctx context.Context, body []byte) (*BulkResult, error)
}
```

---

## 4. 测试规划 (Test Plan)

### 4.1 测试策略表

| 测试类型 | 覆盖范围 | 目标覆盖率 |
|----------|---------|-----------|
| 单元测试 | rerank/percolate/plugin/geo_shape/SDK | ≥ 90% |
| 集成测试 | rerank 端到端/percolate 匹配/SDK API | ≥ 80% |
| E2E 测试 | uniadm 扫描/K8s Operator 部署 | 关键路径 |

### 4.2 关键测试场景

| 场景 | 输入 | 期望 | 优先级 |
|------|------|------|--------|
| rerank 10 文档 | query + 10 docs | 延迟 <50ms，评分正确 | P0 |
| geo_shape polygon 查询 | polygon + intersects | 正确返回相交文档 | P0 |
| percolate 注册+匹配 | 注册查询 → 匹配文档 | 返回匹配 query IDs | P0 |
| 插件进程内注册 | Go plugin 注册 | 索引可用 | P1 |
| 插件 sidecar 连接 | gRPC sidecar | 索引可用 | P1 |
| uniadm 静态扫描 | ES mapping JSON | 兼容性报告 | P0 |
| Go SDK Index+Search | SDK 客户端 | 正确索引和搜索 | P0 |
| K8s Operator 部署 | CRD YAML | 集群启动 | P1 |

### 4.3 Mock 清单

| 依赖 | Mock 方式 | 场景 |
|------|----------|------|
| ONNX Runtime | mock Reranker 接口 | rerank 单元测试 |
| K8s API | envtest（controller-runtime 测试框架） | Operator 测试 |
| HTTP Server | httptest.NewServer | Go SDK 测试 |
| ES 集群 | 本地 JSON 文件 | uniadm 静态分析测试 |

---

## 5. 验收标准 (Acceptance Criteria)

### M3 出口评审

- [ ] **首个 ES 存量用户整体迁移案例**（uniadm 报告 → 迁移 → 金标准回归通过）
- [ ] `go build ./...` / `go test ./...` / `go vet ./...` 全绿
- [ ] 嵌入式/云原生模式不受影响

### 各任务 DoD

- [ ] W7.2: rerank 延迟 <50ms（小模型）
- [ ] W7.3: geo_shape 查询族过金标准
- [ ] W7.4: percolate 金标准
- [ ] W7.5: 第三方示例插件
- [ ] W7.6: 对 3 个真实 ES 集群输出报告
- [ ] W7.7: e2e 自动化用例
- [ ] W7.8: SDK 文档 + 版本化承诺

---

## 6. 实现清单 (Implementation Checklist)

### W7.2 内置 rerank
- [ ] 引入 onnxruntime_go 依赖
- [ ] 实现 ONNXReranker（模型加载 + tokenizer + 打分）
- [ ] 集成到 fusion.go（rerank 分支）
- [ ] 延迟 <50ms 验证测试

### W7.3 geo_shape
- [ ] 扩展 BKD 面/线编码（geo_shape.go）
- [ ] 修改 parser.go 解除 geo_shape unsupported
- [ ] 实现 geo_shape 查询族（intersects/within/disjoint）
- [ ] 金标准测试

### W7.4 percolate 完整化
- [ ] 实现 Percolator（注册 + 匹配 + 注销）
- [ ] 修改 parser.go 解除 percolate unsupported
- [ ] 集成到搜索 API
- [ ] 金标准测试

### W7.5 插件 SDK
- [ ] 实现 PluginRegistry（进程内 + sidecar）
- [ ] 定义 RemoteIndex 接口
- [ ] 实现 SidecarClient（gRPC）
- [ ] 第三方示例插件

### W7.6 uniadm es-compat
- [ ] 实现 escompat 静态扫描
- [ ] 复用 M1 兼容矩阵规则
- [ ] 输出兼容性报告
- [ ] 对 3 个真实 ES 集群输出报告

### W7.7 K8s Operator
- [ ] 引入 controller-runtime 依赖
- [ ] 定义 UniSearchCluster CRD
- [ ] 实现 Reconciler
- [ ] e2e 自动化用例

### W7.8 Go SDK
- [ ] 实现 pkg/unisdk/ HTTP REST 客户端
- [ ] 实现 Index/Search/Bulk/Get/Delete API
- [ ] SDK 文档 + 版本化
