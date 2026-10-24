# 领域词汇表 (Domain Glossary)

## 核心术语 (Core Terms)

### 存储引擎 (Storage Engine)
*   **DedupWindow**: 幂等写入去重窗口，通过 client_id+sequence LRU 淪淘汰机制防止重复写入
*   **MiniInv**: MemTable 内的迷你倒排索引，使未刷新数据立即可被 match 查询搜索到
*   **PartialState**: 跨段聚合中间态，每段产生 PartialState 后由协调器合并为最终结果

### 索引引擎 (Index Engine)
*   **RaBitQ**: 1-bit 随机投影量化，将向量内存降至 1/32，配合精排回表维持 ≥95% 召回率
*   **Cross-Link**: 段间 HNSW 顶层邻接，使查询可跨段跳转而无需遍历所有段图
*   **MinHash**: 参数化哈希函数族的最小值集合签名，用于近似 Jaccard 相似度计算和文档去重
*   **GranularZone**: 段块内细粒度桶统计（min/max/rowCount），范围查询时桶级剪枝跳过不匹配行
*   **FilterCache**: 缓存 filter DSL→roaring.Bitmap 的查询结果，按 AST 规范化 hash 做 key，段变更时失效

### 分布式 (Distributed)
*   **RaftLogStore**: 基于 NVMe 文件实现的 hashicorp/raft LogStore + StableStore 接口
