# 代码规范

## 概述

UniSearch 遵循 Go 官方代码规范，并在此基础上增加项目特定约束。所有规范通过 golangci-lint 自动化执行。

## Lint 规则

### 启用的 Linter

`.golangci.yml` 配置文件启用 5 个 linter：

| Linter | 说明 |
|--------|------|
| `errcheck` | 检查未处理的错误返回值 |
| `govet` | Go 官方 vet 工具 |
| `staticcheck` | 静态分析（unused code、简化建议等） |
| `revive` | Go 代码风格检查（替代 golint） |
| `gocritic` | 代码诊断与性能建议 |

### gocritic 配置

```yaml
gocritic:
  enabled-tags:
    - diagnostic
    - performance
```

启用 `diagnostic`（诊断）和 `performance`（性能）两个标签集。

### 豁免规则

```yaml
exclude-rules:
  - path: _test\.go
    linters: [errcheck, gocritic]
  - path: internal/raft/
    linters: [gocritic]
```

- 测试文件（`*_test.go`）豁免 `errcheck` 和 `gocritic`
- `internal/raft/` 包豁免 `gocritic`（Raft 实现复杂度高）

### 零上限配置

```yaml
max-issues-per-linter: 0
max-same-issues: 0
```

不限制每个 linter 和每种问题的最大报告数量，所有问题都需修复。

## CGO 约束

项目全局禁用 CGO：

```makefile
# Makefile 中所有目标
CGO_ENABLED=0 go build ./...
CGO_ENABLED=0 go test ./...
```

**禁止引入任何 C 依赖库**。所有功能必须使用纯 Go 实现。设计原因：

1. 单一静态二进制是核心卖点（设计约束 G3）
2. 交叉编译矩阵免折腾（无需目标平台 C 工具链）
3. 容器镜像极小（Alpine + 静态二进制约 60MB）

## 错误处理规范

### 错误码体系

`internal/errors/` 定义 ES 对齐的错误码体系：

- 错误响应使用 ES 兼容的 `APIError` 结构
- 错误类型常量对齐 ES 8.x 错误类型名
- 所有 HTTP 错误通过 `writeError()` 统一输出

### 错误处理原则

```go
// 好的做法：包装错误并添加上下文
if err := engine.Flush(); err != nil {
    return fmt.Errorf("flush shard %d: %w", shardID, err)
}

// 好的做法：检查错误
result, err := engine.Search(ctx, indices, body)
if err != nil {
    return nil, err
}

// 禁止：忽略错误
result, _ := engine.Search(ctx, indices, body) // errcheck 会报错
```

### 错误日志

错误日志必须携带 `trace_id`（由 `traceHandler` 自动注入）：

```go
slog.ErrorContext(ctx, "flush failed", "err", err, "shard", shardID)
// 日志输出自动包含 trace_id 和 span_id
```

## 日志规范

### slog JSON 格式

使用标准库 `log/slog`，JSON handler 输出到 stdout：

```go
slog.Info("starting unisearch", "mode", cfg.Mode, "listen", cfg.Listen)
// 输出: {"ts":"...","level":"INFO","msg":"starting unisearch","mode":"embedded","listen":":9200"}
```

### 字段命名

| 字段 | 说明 |
|------|------|
| `ts` | 时间戳（替代默认的 `time`，通过 `ReplaceAttr` 处理） |
| `level` | 日志级别 |
| `msg` | 消息内容 |
| `trace_id` | OTel trace ID（自动注入） |
| `span_id` | OTel span ID（自动注入） |

业务字段使用 snake_case 命名。

### 日志级别约定

| 级别 | 使用场景 |
|------|----------|
| `Info` | 请求日志（method/path/duration/status）、服务启动/关闭 |
| `Warn` | 慢查询告警（>1s）、追踪初始化失败、gRPC 监听失败 |
| `Error` | HTTP/gRPC 服务错误、关闭异常、flush 失败 |

## 代码注释规范

### 包注释

每个包应在 `doc.go` 或首文件中包含包注释，标注设计文档引用：

```go
// Package esapi implements the Elasticsearch-compatible REST API
// surface for M0 (design 04-es-compat-layer §2.2/§6).
package esapi
```

### @spec 标注

代码中通过 `@spec:` 注释标记指向设计文档或归档 spec：

```go
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#4.1
```

### 设计决策注释

重要的设计决策应在代码注释中说明原因：

```go
// CGO is prohibited project-wide (design 00-overview §2.1: single
// static binary, G3).
```

## 测试代码规范

### 表格驱动模式

```go
func TestFlushPipeline(t *testing.T) {
    tests := []struct {
        name     string
        docs     []IndexDoc
        expected int
    }{
        {"single doc", []IndexDoc{{...}}, 1},
        {"empty batch", []IndexDoc{}, 0},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            got := processBatch(tt.docs)
            assert.Equal(t, tt.expected, got)
        })
    }
}
```

### testify 使用

```go
import (
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)
```

- `assert` — 失败继续执行，收集所有断言结果
- `require` — 失败立即停止当前测试

### 测试命名

| 前缀 | 场景 |
|------|------|
| `Test` | 标准单元测试 |
| `TestGolden_` | 金标准兼容测试 |
| `TestE2E_` | 端到端测试 |
| `Benchmark` | 基准测试 |
| `Fuzz` | 模糊测试 |

## 依赖管理

### 直接依赖

项目 `go.mod` 中约 20 个直接依赖，核心依赖包括：

| 依赖 | 用途 |
|------|------|
| `hashicorp/raft` | Raft 共识 |
| `minio/minio-go/v7` | S3 对象存储 |
| `prometheus/client_golang` | Prometheus 指标 |
| `opentelemetry.io/otel` | 分布式追踪 |
| `google.golang.org/grpc` | gRPC 框架 |
| `spf13/cobra` | CLI 命令 |
| `klauspost/compress` | zstd 压缩 |
| `go-ego/gse` | 中文分词 |
| `goccy/go-json` | 高性能 JSON |
| `stretchr/testify` | 测试断言 |

### 禁止引入清单

以下库因设计约束禁止引入（详见 `go-design/00-overview.md §2.3`）：

| 库 | 禁止原因 |
|------|----------|
| bleve / bluge | 倒排索引自研，不内嵌 Lucene 系 |
| gojieba | 中文分词使用 gse |
| etcd | 元数据 Raft 内置，不引入外部 etcd |
| Kafka | WAL 自研，不引入消息队列 |
| Go plugin | 禁用 CGO，plugin 依赖 CGO |

## Git 提交规范

### Commit Message 格式

```
<type>: <subject>

<body>
```

Type 取值：`feat`（新功能）、`fix`（修复）、`refactor`（重构）、`test`（测试）、`docs`（文档）、`chore`（杂项）。

### PR 流程

1. 创建分支：`feat/<name>` 或 `fix/<name>`
2. 确保 `make ci` 通过
3. 提交 PR，关联 spec 文档（如有）
4. CI 检查：lint + test + build + golden tests
