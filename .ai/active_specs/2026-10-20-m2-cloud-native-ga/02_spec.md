# Spec: M2 云原生 GA 全量 W6.1-W6.10

## 1. 概述 (Overview)

本 Spec 将 M2 Proposal 转化为可执行的工程细节。M2 将 UniSearch 从单机嵌入式升级为云原生分布式架构：Raft 元数据集群、对象存储 WAL、分片 Raft 组、Coordinator 协调层、在线分裂、存算分离、冷热分层、Cross-Link Merge 全量、RaBitQ 温层、混沌测试 + 10 亿基准。

设计评审共 2 轮，6 个技术决策点已确认。

### 1.1 设计评审决策汇总

| # | 决策点 | 选择 |
|---|--------|------|
| D1 | Raft FSM 编码 | Protobuf 命令 + JSON 快照 |
| D2 | WAL 双写策略 | NVMe 先行 + S3 异步追赶 |
| D3 | Coordinator 路由 | 直连 Shard（无中间层） |
| D4 | 在线分裂协议 | Gen 双版本 + 位图继承 |
| D5 | 冷热分层触发 | 基于段年龄自动迁移 |
| D6 | 混沌测试基础设施 | Docker Compose + toxiproxy |

### 1.2 变更清单

| 类型 | 文件路径 | 说明 |
|------|----------|------|
| 新增 | `internal/raft/` | hashicorp/raft 封装 + FSM + 快照 + Watch |
| 新增 | `internal/objstore/` | S3 客户端封装 + LogStore |
| 新增 | `internal/coordinator/` | Query/Ingest 协调器 |
| 新增 | `internal/storage/tiering/` | TierManager + 冷热分层 |
| 新增 | `api/proto/` | gRPC 协议定义 |
| 新增 | `test/chaos/` | 混沌编排 C1-C6 |
| 修改 | `internal/meta/` | 单机 → Raft FSM |
| 修改 | `internal/storage/` | WAL 支持对象存储 |
| 修改 | `cmd/unisearch/` | 新增 --cloud --role |
| 修改 | `internal/config/` | 云原生配置 |
| 修改 | `internal/tenant/` | dedicated/cold 模式 |

---

## 2. 详细设计 (Detailed Design)

### 2.1 W6.1 Meta Raft 集群

**设计引用**: `docs/go-design/05-distributed.md §1`

**MetaState FSM**（D1: Protobuf 命令 + JSON 快照）:
```go
// internal/raft/fsm.go
type MetaState struct {
    Version    uint64                       // 状态机版本
    Schemas    map[types.IndexID]*meta.IndexMeta
    Routes     map[types.ShardID]*RouteEntry
    Tenants    map[types.TenantID]*TenantMeta
    Watermarks map[types.ShardID]types.SeqNo
    Members    []NodeInfo
}

type RouteEntry struct {
    ShardID     types.ShardID
    IndexID     types.IndexID
    RaftGroup   []NodeID
    Generation  uint64  // 分裂/迁移代数（D4: Gen 双版本依赖）
}

// FSM 命令类型（Protobuf 编码）
type FSMCommand struct {
    Type    FSMCmdType
    Payload []byte // Protobuf 编码的具体命令
}
// CmdType: CreateIndex | DeleteIndex | UpdateMapping | AllocateShard |
//           SplitShard | MigrateShard | UpdateQuota | RegisterNode
```

**关键接口**:
```go
// internal/raft/cluster.go
type MetaCluster struct {
    raft      *raft.Raft
    fsm       *MetaFSM
    transport raft.Transport
}

func NewMetaCluster(cfg MetaClusterConfig) (*MetaCluster, error)
func (c *MetaCluster) Apply(cmd *FSMCommand) (any, error)  // Raft Apply
func (c *MetaCluster) Get(index string) (*meta.IndexMeta, error)  // ReadIndex 线性一致读
func (c *MetaCluster) Watch() <-chan MetaEvent  // 路由/水位推送
func (c *MetaCluster) Snapshot() ([]byte, error)  // JSON+gzip 快照
func (c *MetaCluster) Restore(data []byte) error
func (c *MetaCluster) Join(nodeID, addr string) error
func (c *MetaCluster) Leave(nodeID string) error
```

**快照格式**（D1）: `MetaState` 序列化为 JSON + gzip，存储到对象存储。恢复时拉快照 + 重放增量日志。

**依赖**: `github.com/hashicorp/raft`, `github.com/hashicorp/raft-boltdb`

### 2.2 W6.2 对象存储 WAL

**设计引用**: `docs/go-design/05-distributed.md §2`

**双写策略**（D2: NVMe 先行 + S3 异步追赶）:
```go
// internal/objstore/logstore.go
type S3LogStore struct {
    local     *wal.LocalWAL   // 本地 NVMe（同步 fsync，参与 Raft quorum）
    s3        S3Client        // S3 异步上传
    shardID   types.ShardID
    uploadCh  chan<- *LogSegment  // 异步上传通道
}

// AppendLogs 先写本地 NVMe（fsync），再异步上传 S3
func (s *S3LogStore) AppendLogs(logs []*raft.Log) error {
    // 1. 同步写入本地 NVMe WAL
    if err := s.local.Append(logs); err != nil { return err }
    // 2. 异步上传到 S3（非阻塞，追赶协议）
    go s.uploadSegment(logs)
    return nil
}
```

**S3 客户端接口**:
```go
// internal/objstore/s3.go
type S3Client interface {
    PutObject(ctx context.Context, key string, data []byte) error
    GetObject(ctx context.Context, key string) (io.ReadCloser, error)
    ListObjects(ctx context.Context, prefix string) ([]string, error)
    DeleteObject(ctx context.Context, key string) error
}
```

**日志段格式**: 复用 M0 WAL 记录格式（`entry_type/payload` 不变），Raft 只是复制通道。路径: `wal/{shard_id}/{gen}/{seq}.log`。

**追赶协议**: follower 从对象存储并行拉段，限速令牌桶（避免打爆出口带宽）。

### 2.3 W6.3 Shard Raft 组

**设计引用**: `docs/go-design/05-distributed.md §4`

```go
// internal/storage/shard_raft.go
type ShardRaftGroup struct {
    raft      *raft.Raft
    shardID   types.ShardID
    fsm       *ShardFSM  // MemTable + SegmentSet + DeleteTracker
    logStore  *S3LogStore
}

func (g *ShardRaftGroup) Write(ctx context.Context, ops []storage.WriteOp) (types.SeqNo, error)
func (g *ShardRaftGroup) Snapshot() (types.Snapshot, *storage.SegmentSet)
func (g *ShardRaftGroup) Close() error
```

**故障接管**: leader 失联 → Raft 选举超时（1.5s）→ 新 leader 从本地 NVMe + 对象存储补齐 → 回放 FSM。DoD: kill -9 RTO <30s。

### 2.4 W6.4 Coordinator 层

**设计引用**: `docs/go-design/05-distributed.md §6`

**路由方式**（D3: 直连 Shard）:
```go
// internal/coordinator/query.go
type QueryCoordinator struct {
    metaClient  MetaClient    // 从 meta 获取路由表
    shardConns  map[NodeID]*grpc.ClientConn  // 直连 shard 节点
}

func (c *QueryCoordinator) Search(ctx context.Context, indices []string, body []byte) (*esapi.SearchResult, error) {
    // 1. 从 meta 获取 shard 路由表
    routes := c.metaClient.GetRoutes(indices)
    // 2. 并行 gRPC 调用各 shard 节点
    results := c.fanOutSearch(ctx, routes, body)
    // 3. 归并排序 + 聚合合并
    return c.mergeResults(results)
}
```

**Ingest Coordinator**: `_bulk` 按路由键分片内攒批（4MB/2ms），背压 per-shard 队列水位（上限 64MB）。

### 2.5 W6.5 存算分离 + 冷热分层

**设计引用**: `docs/go-design/01-storage-engine.md §9`, `02-index-engine.md §2.5`

**分层策略**（D5: 基于段年龄自动迁移）:
```go
// internal/storage/tiering/tier_manager.go
type TierManager struct {
    hotAge   time.Duration  // 段年龄 < hotAge → 热层（NVMe + HNSW）
    warmAge  time.Duration  // hotAge ≤ age < warmAge → 温层（RaBitQ）
    // age ≥ warmAge → 冷层（DiskANN + 对象存储）
}

func (tm *TierManager) MaybeMigrate(seg *SegmentEntry, age time.Duration) MigrationAction
```

### 2.6 W6.6 在线分裂

**设计引用**: `docs/go-design/05-distributed.md §3.2`

**分裂协议**（D4: Gen 双版本 + 位图继承）:
```go
// internal/raft/split.go
type SplitShardCmd struct {
    ParentShardID types.ShardID
    ChildShardIDs []types.ShardID  // 2 个子分片
    NewGeneration uint64
    SplitKey      uint32  // hash 取模切分点
}

// 分裂流程:
// 1. Meta Raft 发起 SplitShard 命令（路由表双版本 gen=N+1）
// 2. 父分片进入 dual-read 冻结代数（段文件不重写，只读共享）
// 3. 创建两个子分片 Raft 组（继承父段引用 + DeleteBitmap）
// 4. 原子切换 gen N+1（新写进子分片）
// 5. 子分片拉增量 WAL 追平
// 6. 父分片下线（段引用计数转移）
```

### 2.7 W6.7 Cross-Link Merge 全量

**设计引用**: `docs/go-design/02-index-engine.md §2.4.3`

增量合并替换全量重建。合并时 crosslinks 重建，新段 SegmentID 不变（共享段引用不变）。

### 2.8 W6.8 RaBitQ 温层

**设计引用**: `docs/go-design/02-index-engine.md §2.5`

1-bit 量化 + 精排回表。内存 1/32，召回 ≥95%。

### 2.9 W6.9 再均衡 + dedicated/cold 租户

**设计引用**: `docs/go-design/05-distributed.md §3.3, §5`

三阶段迁移：目标节点拉快照 → 增量日志同步 → 路由表原子切换（Gen+1）。dedicated 独占 Shard Node，cold 暂停段加载。

### 2.10 W6.10 混沌 + 10 亿基准

**设计引用**: `docs/go-design/05-distributed.md §9`

**混沌基础设施**（D6: Docker Compose + toxiproxy）:

| 场景 | 描述 | 验证 |
|------|------|------|
| C1 | kill -9 shard leader | RTO <30s，查询自动切副本 |
| C2 | 网络分区（meta 少数派） | quorum 维持，少数派只读降级 |
| C3 | 慢 IO 注入（对象存储延迟 5s） | P99 退化 <2×（NVMe 热路径） |
| C4 | 分裂期双杀（两个子分片 leader 同时宕机） | 查询零感知（gen 回退） |
| C5 | 分裂重试（子分片组选举失败重试） | 分裂最终成功 |
| C6 | 脑裂恢复（网络恢复后 quorum 重建） | 数据一致性校验通过 |

---

## 3. 接口设计 (Interface Design)

### 3.1 云原生进程入口

```go
// cmd/unisearch/cloud.go
func runCloud(cfg *config.Config) error {
    switch cfg.Cloud.Role {
    case "meta":       return raft.RunMetaNode(cfg)
    case "coordinator": return coordinator.RunCoordinator(cfg)
    case "shard":      return shard.RunShardNode(cfg)
    }
}
```

### 3.2 gRPC 协议（设计 05 §7）

```protobuf
// api/proto/shard.proto
service ShardService {
  rpc Write(WriteBatch) returns (WriteAck);
  rpc Search(SubPlan) returns (stream LocalResult);
  rpc Get(GetRequest) returns (GetResponse);
  rpc PullWAL(WALCursor) returns (stream LogChunk);
  rpc Snapshot(SnapshotRequest) returns (SnapshotMeta);
}

// api/proto/meta.proto
service MetaService {
  rpc Apply(MetaCommand) returns (ApplyResult);
  rpc Watch(WatchRequest) returns (stream MetaEvent);
  rpc Heartbeat(NodeInfo) returns (HeartbeatAck);
}
```

### 3.3 S3 客户端接口

```go
type S3Client interface {
    PutObject(ctx context.Context, key string, data []byte) error
    GetObject(ctx context.Context, key string) (io.ReadCloser, error)
    ListObjects(ctx context.Context, prefix string) ([]string, error)
    DeleteObject(ctx context.Context, key string) error
}
```

---

## 4. 内部设计 (Internal Design)

### 4.1 internal/raft/ 包

| 文件 | 职责 |
|------|------|
| `cluster.go` | MetaCluster 封装 hashicorp/raft |
| `fsm.go` | MetaFSM 实现（Apply/Snapshot/Restore） |
| `transport.go` | gRPC Transport 实现 |
| `snapshot.go` | JSON+gzip 快照存储到对象存储 |
| `split.go` | 在线分裂协议 |
| `watch.go` | 路由/水位事件推送 |

### 4.2 internal/objstore/ 包

| 文件 | 职责 |
|------|------|
| `s3.go` | S3 兼容客户端（MinIO 测试） |
| `logstore.go` | S3LogStore（NVMe 先行 + S3 异步追赶） |
| `config.go` | S3 配置（endpoint/bucket/credentials） |

### 4.3 internal/coordinator/ 包

| 文件 | 职责 |
|------|------|
| `query.go` | QueryCoordinator（直连 shard，归并） |
| `ingest.go` | IngestCoordinator（攒批/背压） |
| `routing.go` | 路由表缓存 + meta watch |
| `merge.go` | 结果归并（SortMerge/FinalAgg） |

---

## 5. 数据模型 (Data Model)

### 5.1 MetaState

```go
type MetaState struct {
    Version    uint64
    Schemas    map[types.IndexID]*meta.IndexMeta
    Routes     map[types.ShardID]*RouteEntry
    Tenants    map[types.TenantID]*TenantMeta
    Watermarks map[types.ShardID]types.SeqNo
    Members    []NodeInfo
}
```

### 5.2 NodeInfo

```go
type NodeInfo struct {
    ID        NodeID
    Addr      string
    Role      string  // meta | coordinator | shard
    LastSeen  time.Time
    Resources ResourceReport  // CPU/Mem/Disk/IO 水位
}
```

---

## 6. 错误处理 (Error Handling)

| 错误类型 | 触发条件 | 处理方式 |
|---------|---------|---------|
| `ErrNotLeader` | 写入到非 leader 节点 | 返回 leader 地址，客户端重试 |
| `ErrQuorumLost` | Raft 丢失 quorum | 只读降级，返回 503 |
| `ErrS3Unavailable` | 对象存储不可达 | NVMe 热路径继续服务，S3 追赶延后 |
| `ErrSplitInProgress` | 分裂进行中 | 新写入路由到子分片，旧查询 dual-read |
| `ErrShardMigrating` | 再均衡中 | 旧副本继续服务，迁移完成后切换 |

---

## 7. 测试规划 (Test Plan)

### 7.1 测试策略表

| 测试类型 | 覆盖范围 | 目标覆盖率 |
|----------|---------|-----------|
| 单元测试 | FSM/LogStore/Coordinator/TierManager | ≥ 90% |
| 集成测试 | 3 节点 meta 集群/shard 故障接管 | ≥ 80% |
| 混沌测试 | C1-C6 全量（Docker Compose + toxiproxy） | 全部通过 |
| 基准测试 | 10 亿向量 B2/B5/B9 规模版 | 归档报告 |

### 7.2 关键测试场景

| 场景 | 输入 | 期望 | 优先级 |
|------|------|------|--------|
| 3 节点选举 | 启动 3 节点 meta 集群 | 选举出 leader，路由表一致 | P0 |
| kill -9 恢复 | 杀死 shard leader | RTO <30s，查询自动切副本 | P0 |
| 网络分区 | toxiproxy 分区 meta 少数派 | quorum 维持，少数派只读 | P0 |
| 慢 IO 注入 | toxiproxy 延迟 S3 5s | P99 退化 <2× | P0 |
| 在线分裂 | 对运行中的分片执行分裂 | 查询零感知，dual-read 正确 | P0 |
| 分裂双杀 | 分裂时杀死两个子分片 leader | gen 回退，查询继续 | P1 |
| 段自动迁移 | flush 后 N 天的段 | 热层→温层→冷层自动迁移 | P1 |
| 10 亿基准 | 10 亿 768d 向量 | B2/B5/B9 达标 | P1 |

### 7.3 Mock 清单

| 依赖 | Mock 方式 | 场景 |
|------|----------|------|
| S3 对象存储 | MinIO 本地容器 | 全部测试 |
| 网络延迟 | toxiproxy 代理 | C2/C3 混沌 |
| 节点宕机 | Docker kill | C1/C4 混沌 |
| gRPC 通信 | httptest/gRPC mock | 单元测试 |

---

## 8. 验收标准 (Acceptance Criteria)

### M2 出口评审

- [ ] 混沌报告全绿（C1-C6 全部通过）
- [ ] 10 亿基准归档（B2/B5/B9 规模版）
- [ ] 存算分离成本报告
- [ ] `go build ./...` / `go test ./...` / `go vet ./...` 全绿
- [ ] 嵌入式模式 `--embedded` 不受影响

### 各任务 DoD

- [ ] W6.1: 3 节点选举/恢复注入测试通过
- [ ] W6.2: C3 慢 IO 注入 P99 退化 <2×
- [ ] W6.3: C1 kill -9 RTO <30s
- [ ] W6.4: 分片预聚合归并正确性对拍单机
- [ ] W6.5: 冷段查询 + 召回补偿验证
- [ ] W6.6: C5 分裂期双杀；查询零感知
- [ ] W6.7: B10 召回 ≥ 基线-2%
- [ ] W6.8: 召回 ≥95% + 内存 1/32
- [ ] W6.9: 迁移期间查询零错误
- [ ] W6.10: 演练报告 + 10 亿基准归档

---

## 9. 实现清单 (Implementation Checklist)

### W6.1 Meta Raft 集群
- [ ] 引入 hashicorp/raft + raft-boltdb 依赖
- [ ] 定义 MetaState + FSMCommand protobuf
- [ ] 实现 MetaFSM（Apply/Snapshot/Restore）
- [ ] 实现 MetaCluster（New/Join/Leave/Apply/Get/Watch）
- [ ] gRPC Transport 实现
- [ ] JSON+gzip 快照到对象存储
- [ ] 3 节点选举/恢复集成测试

### W6.2 对象存储 WAL
- [ ] 引入 minio-go/v7 依赖
- [ ] 实现 S3Client 接口
- [ ] 实现 S3LogStore（NVMe 先行 + S3 异步追赶）
- [ ] 实现追赶协议（follower 从 S3 拉段）
- [ ] C3 慢 IO 注入测试

### W6.3 Shard Raft 组
- [ ] 实现 ShardFSM（MemTable + SegmentSet）
- [ ] 实现 ShardRaftGroup（Write/Snapshot/Close）
- [ ] 故障接管（kill -9 RTO <30s）
- [ ] C1 kill -9 混沌测试

### W6.4 Coordinator
- [ ] 实现 QueryCoordinator（直连 shard，归并）
- [ ] 实现 IngestCoordinator（攒批/背压）
- [ ] 路由表缓存 + meta watch
- [ ] 分片预聚合归并正确性对拍

### W6.5 存算分离
- [ ] 实现 TierManager（基于段年龄自动迁移）
- [ ] 段文件对象存储加载/卸载
- [ ] DiskANN 冷层
- [ ] 冷段查询 + 召回补偿验证

### W6.6 在线分裂
- [ ] 实现 SplitShard 命令
- [ ] Gen 双版本 + 位图继承
- [ ] dual-read 冻结代数
- [ ] 子分片拉增量 WAL 追平
- [ ] C5 分裂双杀混沌测试

### W6.7 Cross-Link Merge 全量
- [ ] 增量合并替换全量重建
- [ ] crosslinks 重建
- [ ] B10 召回 ≥ 基线-2%

### W6.8 RaBitQ 温层
- [ ] 1-bit 量化 + 精排回表
- [ ] 召回 ≥95% + 内存 1/32 验证

### W6.9 再均衡 + 租户
- [ ] 三阶段迁移协议
- [ ] dedicated 独占 Shard Node
- [ ] cold 租户暂停/唤醒
- [ ] 迁移期间查询零错误

### W6.10 混沌 + 10 亿基准
- [ ] Docker Compose 编排脚本
- [ ] toxiproxy 网络故障注入
- [ ] C1-C6 全量混沌测试
- [ ] 10 亿向量基准（B2/B5/B9）
- [ ] 存算分离成本报告
