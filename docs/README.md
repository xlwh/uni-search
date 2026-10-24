# UniSearch 文档

## 文档体系总览

UniSearch 文档分为四大类：设计文档、研发文档、部署文档、运维文档和用户指南。设计文档描述系统架构与设计决策，研发文档面向开发者，部署和运维文档面向 DevOps/SRE，用户指南面向 API 使用者。

## 设计文档

| 文档 | 说明 |
|------|------|
| [概要设计](vector-database-design.md) | 项目背景、竞品调研、架构目标、演进路线图 |
| [开发规划](development-plan.md) | M0-M3 全量 WBS 任务分解、团队角色、风险登记 |
| [Go 详细设计总纲](go-design/00-overview.md) | 技术选型、模块划分、进程模型、公共规范 |
| [存储引擎设计](go-design/01-storage-engine.md) | MemTable/WAL/Segment 格式、Flush 流程、MVCC |
| [索引引擎设计](go-design/02-index-engine.md) | HNSW/倒排/标量索引、Cross-Link Merge、量化 |
| [查询引擎设计](go-design/03-query-engine.md) | DSL 解析、计划优化、Volcano 执行、融合排序 |
| [ES 兼容层设计](go-design/04-es-compat-layer.md) | REST 路由、DSL 兼容矩阵、mapping 语义映射 |
| [分布式架构设计](go-design/05-distributed.md) | Raft 元数据、对象存储 WAL、分片路由、多租户 |
| [质量保障与可观测性](go-design/06-quality-and-observability.md) | 指标体系、OTel 追踪、测试策略、CI/CD |

## 研发文档

| 文档 | 说明 |
|------|------|
| [开发入门](development/getting-started.md) | 环境搭建、首次构建、启动验证 |
| [构建与测试](development/build-and-test.md) | 编译、单元测试、模糊测试、金标准测试、E2E 测试 |
| [项目结构](development/project-structure.md) | 目录结构、模块职责、依赖关系 |
| [代码规范](development/code-style.md) | Lint 规则、CGO 约束、错误处理、日志规范 |
| [CI/CD](development/ci-cd.md) | GitHub Actions 流程、门禁规则、发布流程 |

## 部署文档

| 文档 | 说明 |
|------|------|
| [Docker 单节点](deployment/docker-single-node.md) | 容器化单节点部署，适合开发/测试 |
| [Docker Compose 集群](deployment/docker-compose-cluster.md) | 分布式集群部署，3 meta + 2 shard + MinIO + Jaeger |
| [K8s Operator](deployment/k8s-operator.md) | Kubernetes CRD 驱动的集群生命周期管理 |
| [交叉编译](deployment/cross-build.md) | 三平台静态二进制构建与发布 |

## 运维文档

| 文档 | 说明 |
|------|------|
| [Prometheus 监控](operations/monitoring.md) | 指标体系、Grafana 面板、告警规则 |
| [分布式追踪](operations/tracing.md) | OTel 四级 Span 树、Jaeger 配置、链路分析 |
| [日志](operations/logging.md) | slog JSON 格式、trace_id 关联、慢查询日志 |
| [健康检查](operations/health-check.md) | 集群健康 API、Docker/K8s 探针集成 |
| [故障排查](operations/troubleshooting.md) | 启动/写入/查询/分布式/内存问题诊断 |
| [备份恢复](operations/backup-restore.md) | 数据备份策略、WAL 回放、灾难恢复 |
| [性能调优](operations/performance-tuning.md) | 写入/查询/向量/段/内存多维调优 |

## 用户指南

| 文档 | 说明 |
|------|------|
| [快速开始](user-guide/quickstart.md) | 5 分钟上手：安装、创建索引、写入、搜索 |
| [配置参考](user-guide/configuration-reference.md) | 全量配置项、环境变量映射、YAML 模板 |
| [API 参考](user-guide/api-reference.md) | REST API 端点完整文档 |
| [查询 DSL](user-guide/query-dsl.md) | 全文/词项/组合/向量/混合查询语法 |
| [映射与设置](user-guide/mappings-and-settings.md) | 字段类型、mapping 管理、分析器、索引模板 |
| [向量搜索](user-guide/vector-search.md) | HNSW 索引、kNN 搜索、量化、过滤策略 |
| [混合检索](user-guide/hybrid-search.md) | hybrid 查询、RRF/Weighted/RelativeScore 融合 |
| [聚合查询](user-guide/aggregations.md) | 指标/桶/管道聚合、近似算法 |
| [ES 兼容性](user-guide/es-compatibility.md) | 兼容矩阵、行为差异、迁移指南 |
| [Go SDK](user-guide/sdk-guide.md) | Go 客户端安装与使用 |

## 里程碑路线图

| 里程碑 | 状态 | 说明 |
|--------|------|------|
| M0 | 已完成 | 核心引擎：存储/索引/查询/ES 兼容 REST API |
| M1 | 已完成 | ES 兼容 GA：扩展 API（_msearch/_explain/_reindex/模板/ILM/PIT/scroll） |
| M2 | 已完成 | 云原生 GA：Raft 元数据集群、分片节点、对象存储、K8s Operator |
| M3 | 进行中 | 生态与进阶：Go SDK、uniadm 工具、高级特性 |

详见 [开发规划](development-plan.md)。
