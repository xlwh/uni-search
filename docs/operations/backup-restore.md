# 备份与恢复

## 概述

UniSearch 的备份与恢复依赖存储引擎的 WAL（预写日志）机制和段文件的不可变特性。本文档覆盖单节点和分布式集群的备份恢复策略。

## 备份策略

### 嵌入式模式

嵌入式模式的数据存储在 `--data-dir` 指定的本地目录中。备份方式为文件级复制。

**数据目录结构**：

```
data/
├── meta/              # 索引元数据（Mapping/Settings/IndexMeta）
├── wal/               # WAL 日志文件
├── segments/          # 不可变段文件
└── checkpoint         # 检查点文件
```

### 云原生模式

云原生模式下，WAL 和段文件归档到 S3 对象存储。备份可通过 S3 快照或文件级复制实现。

## 备份方法

### 方法一：文件级备份（冷备）

**适用场景**：嵌入式模式，可接受短暂停机。

```bash
# 1. 停止服务（确保 WAL 完全刷盘）
./unisearch --embedded --data-dir ./data
# Ctrl+C 优雅关闭，等待 10s 超时

# 2. 备份整个数据目录
tar czf unisearch-backup-$(date +%Y%m%d).tar.gz data/

# 3. 重启服务
./unisearch --embedded --data-dir ./data &
```

### 方法二：在线备份（热备）

**适用场景**：不能停机的生产环境。

```bash
# 1. 触发 flush，将 MemTable 刷盘
curl -X POST http://localhost:9200/_flush

# 2. 创建数据目录快照（利用文件系统快照或 rsync）
rsync -a --delete ./data/ ./backup/

# 3. 验证备份完整性
ls -la ./backup/
# 应包含 meta/、wal/、segments/、checkpoint
```

> 注意：在线备份期间如果有写入操作，WAL 文件可能正在追加。rsync 的 `--delete` 选项确保增量同步。对于一致性要求高的场景，建议使用文件系统级快照（如 LVM、ZFS）。

### 方法三：Docker Volume 备份

```bash
# 1. flush 数据
docker exec unisearch curl -X POST http://localhost:9200/_flush

# 2. 备份 Volume
docker run --rm \
  -v unisearch-data:/data:ro \
  -v $(pwd):/backup \
  alpine tar czf /backup/unisearch-backup.tar.gz /data

# 3. 验证
ls -lh unisearch-backup.tar.gz
```

### 方法四：S3 快照（云原生模式）

云原生模式下，段文件已归档到 S3。可通过 S3 版本控制或 Bucket 复制实现备份：

```bash
# 启用 S3 Bucket 版本控制
aws s3api put-bucket-versioning \
  --bucket unisearch \
  --versioning-configuration Status=Enabled

# 或配置跨区域复制
aws s3api put-bucket-replication \
  --bucket unisearch \
  --replication-configuration file://replication.json
```

## 恢复方法

### 恢复文件级备份

```bash
# 1. 停止服务
# (停止正在运行的 unisearch 进程)

# 2. 清空或移走旧数据目录
mv data/ data.old/

# 3. 解压备份
tar xzf unisearch-backup-20260101.tar.gz

# 4. 启动服务
./unisearch --embedded --data-dir ./data

# 5. 验证
curl http://localhost:9200/_cluster/health
curl http://localhost:9200/_cat/indices?v
```

### 恢复 Docker Volume

```bash
# 1. 停止容器
docker stop unisearch

# 2. 恢复 Volume
docker run --rm \
  -v unisearch-data:/data \
  -v $(pwd):/backup \
  alpine sh -c "rm -rf /data/* && tar xzf /backup/unisearch-backup.tar.gz -C /"

# 3. 重启容器
docker start unisearch

# 4. 验证
docker exec unisearch curl http://localhost:9200/_cluster/health
```

### WAL 重放恢复

UniSearch 启动时自动执行 WAL 重放：

1. 读取 `checkpoint` 文件，获取最后一致的 WAL LSN
2. 从该 LSN 开始重放 WAL 日志
3. 重放完成后，MemTable 数据恢复到崩溃前状态
4. 进入正常服务

此过程自动完成，无需手动干预。如果 WAL 文件损坏，可删除 `wal/` 目录后启动（会丢失最后一次 flush 后的写入）。

### 段文件恢复

段文件是不可变的，恢复时直接加载即可。如果段文件损坏：

```bash
# 删除损坏的段文件（会丢失该段的数据）
rm data/segments/<corrupt_segment>

# 重启服务，系统会从 WAL 重放恢复丢失的数据
./unisearch --embedded --data-dir ./data
```

## 备份恢复验证

### 数据一致性检查

```bash
# 1. 检查索引数量
curl http://localhost:9200/_cat/indices?v

# 2. 检查文档数
curl http://localhost:9200/_cat/indices?v | awk '{print $3, $7}'

# 3. 对比备份数据量
# 备份前
curl -s http://localhost:9200/my-index/_count | jq '.count'
# 恢复后
curl -s http://localhost:9200/my-index/_count | jq '.count'

# 4. 抽样验证文档内容
curl http://localhost:9200/my-index/_doc/<doc_id>
```

### 搜索功能验证

```bash
# 全文搜索
curl -X POST http://localhost:9200/my-index/_search -H 'Content-Type: application/json' -d '{
  "query": { "match_all": {} }
}'

# 向量搜索
curl -X POST http://localhost:9200/my-index/_knn_search -H 'Content-Type: application/json' -d '{
  "knn": { "field": "embedding", "query_vector": [0.1, 0.2], "k": 10, "num_candidates": 100 }
}'

# 混合搜索
curl -X POST http://localhost:9200/my-index/_search -H 'Content-Type: application/json' -d '{
  "query": { "hybrid": { "queries": [ {"match": {"title": "test"}}, {"knn": {"field": "embedding", "query_vector": [0.1], "k": 10} } ] } }
}'
```

## 备份策略建议

| 环境 | 备份方式 | 频率 | 保留 |
|------|----------|------|------|
| 开发 | 文件级冷备 | 按需 | 1 份 |
| 测试 | 文件级冷备 | 每日 | 3 份 |
| 生产（单节点） | 在线 rsync + 文件系统快照 | 每日 | 7 份 |
| 生产（集群） | S3 版本控制 + 跨区域复制 | 实时 | 30 天 |

## 灾难恢复

### RPO 和 RTO

| 模式 | RPO（数据丢失容忍） | RTO（恢复时间目标） |
|------|---------------------|---------------------|
| 嵌入式 + 文件备份 | 最近一次备份后写入 | < 5 分钟 |
| 嵌入式 + WAL | 0（WAL 完整情况下） | < 1 分钟 |
| 云原生 + S3 | 0（S3 持久化） | < 10 分钟 |

### 集群故障恢复

云原生模式下，Meta 节点 Raft 日志和 Shard 节点 WAL 均持久化到 S3。恢复流程：

1. 启动新的 Meta 节点集群，从 S3 恢复 Raft 状态
2. 启动 Shard 节点，从 S3 恢复 WAL 和段文件
3. Shard 节点向 Meta 节点注册
4. 集群恢复服务

详见 [Docker Compose 集群部署](../deployment/docker-compose-cluster.md) 和 [K8s Operator 部署](../deployment/k8s-operator.md)。
