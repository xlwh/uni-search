# 构建与测试

## 构建系统概览

UniSearch 使用 Makefile 作为构建入口，所有目标强制 `CGO_ENABLED=0` 以产出纯静态二进制。

### Makefile 目标总表

| 命令 | 说明 |
|------|------|
| `make build` | 编译全部包 |
| `make test` | 运行全部测试 |
| `make lint` | 代码静态检查（go vet） |
| `make ci` | CI 全流程（lint + test + build） |
| `make coverage` | 生成覆盖率报告 |
| `make cross-build` | 交叉编译三平台二进制 |
| `make fuzz` | 模糊测试（4 个 target 各 60s） |
| `make proto` | 生成 Protobuf 代码 |
| `make clean` | 清理构建产物 |

## 编译构建

### 标准构建

```bash
# 编译全部包（不产出二进制）
make build

# 等价于
CGO_ENABLED=0 go build ./...
```

### 构建主服务二进制

```bash
# 构建 macOS arm64 二进制
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o unisearch ./cmd/unisearch

# 构建 Linux amd64 二进制
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o unisearch-linux ./cmd/unisearch
```

### 交叉编译

```bash
# 三平台静态二进制
make cross-build

# 产物位于 dist/ 目录:
# dist/unisearch-linux-amd64
# dist/unisearch-linux-arm64
# dist/unisearch-darwin-arm64
```

交叉编译脚本 `scripts/build.sh` 使用 `-ldflags "-X main.version=${VERSION}"` 注入版本信息。通过 `VERSION` 环境变量指定版本号：

```bash
VERSION=v0.1.0 make cross-build
```

详见 [交叉编译文档](../deployment/cross-build.md)。

## 单元测试

### 运行全量测试

```bash
make test
# 等价于 CGO_ENABLED=0 go test ./...
```

### 运行指定包

```bash
CGO_ENABLED=0 go test ./internal/storage/...
CGO_ENABLED=0 go test ./internal/engine/dsl/...
CGO_ENABLED=0 go test ./internal/index/hnsw/...
```

### Race 检测

```bash
# CI 必须零 race 报告
CGO_ENABLED=0 go test -race ./...

# 指定包 race 检测
CGO_ENABLED=0 go test -race ./internal/storage/...
```

### 覆盖率

```bash
# 生成覆盖率报告
make coverage

# 产出:
# coverage.out  — 原始覆盖率数据
# coverage.html — HTML 可视化报告
```

覆盖率报告按包聚合，`go tool cover -html=coverage.out` 在浏览器中查看。

## 模糊测试

UniSearch 维护 4 个常驻 fuzz target：

| Fuzz Target | 包路径 | 测试内容 |
|-------------|--------|----------|
| `FuzzParseQuery` | `internal/engine/dsl/` | DSL 查询解析器 |
| `FuzzWALReplay` | `internal/storage/wal/` | WAL 回放 |
| `FuzzSegmentFormat` | `internal/storage/segment/` | 段格式编解码 |
| `FuzzQueryString` | `internal/engine/dsl/` | query_string 语法 |

### 运行模糊测试

```bash
# 全部 fuzz target，每个 60 秒
make fuzz

# 单独运行，指定时长
CGO_ENABLED=0 go test -fuzz=FuzzParseQuery -fuzztime=300s ./internal/engine/dsl/
CGO_ENABLED=0 go test -fuzz=FuzzWALReplay -fuzztime=300s ./internal/storage/wal/
```

## 金标准测试

金标准测试翻译自 elasticsearch-py 核心 API 测试用例，验证 ES 兼容行为。

```bash
# 运行金标准测试
CGO_ENABLED=0 go test ./test/golden/...

# 带超时
CGO_ENABLED=0 go test -timeout 60s ./test/golden/...
```

金标准测试包含 27 个测试函数，覆盖：RootAndHealth、DocCRUD、Bulk、SearchMatch、SearchMatchAll、Mapping、CatIndices、Analyze、MGet、IfSeqNoConflict、MSearch、CountWithQuery、Aggregations、Highlight、PIT、FieldCaps、Explain、DeleteByQuery、Reindex、IndexTemplate、ILM、AliasCreate、Settings、CompatMatrix 等。

ES rest-api-spec YAML 加载器（`test/golden/es_spec/loader.go`）可解析 ES 官方测试规范并转换为可执行用例。

## 端到端测试

### Go E2E 测试

```bash
CGO_ENABLED=0 go test ./test/e2e/...
```

E2E 测试使用 `httptest.NewServer` 启动嵌入式引擎，覆盖 18 个场景：RAG 混合检索、重启零丢失、MemTable 即时搜索、聚合、KNN 向量搜索、PIT、别名、Cat 端点、track_total_hits、terminate_after、高亮、混合搜索、索引模板等。

### Docker E2E 集成测试

```bash
# 先构建 Docker 镜像
docker build -t unisearch:latest .

# 运行 E2E 集成测试
bash scripts/e2e-docker-test.sh
```

该脚本对运行中的 Docker 容器执行全量 HTTP API 测试，覆盖所有设计文档中的核心功能。

### 回归测试

```bash
bash scripts/regression-test.sh
```

回归测试使用 `docker restart` 进行重启零丢失验证，覆盖所有核心 API 和查询类型。

## Python 测试

Python 测试套件位于 `test/python/`，使用 pytest 框架，通过 Docker 容器进行 HTTP API 级别测试。

### 环境准备

```bash
cd test/python
pip install -r requirements.txt
# 依赖: requests>=2.28.0, pytest>=7.0.0
```

### 运行测试

```bash
# 构建并启动 Docker 容器，运行全部测试
cd test/python
pytest

# 按标记运行
pytest -m smoke       # 冒烟测试
pytest -m search      # 搜索功能
pytest -m agg         # 聚合功能
pytest -m vector      # 向量搜索
pytest -m es_compat   # ES 兼容性
pytest -m slow        # 慢测试
```

### 测试覆盖

Python 测试包含 34 个测试文件，覆盖：健康检查、索引 CRUD、文档 CRUD、bulk、搜索、聚合、向量搜索、混合搜索、cat 端点、别名、PIT/scroll、高亮、索引模板、msearch/mget、analyze、field_caps、terms_enum、delete_by_query、update_by_query、flush/forcemerge、track_total_hits、terminate_after、count、explain、reindex、错误处理、重启恢复、多索引、扩展聚合、scroll/PIT、索引设置、数据类型、边界情况、模板应用。

## 基准测试

### uni-bench 基准套件

```bash
# 运行全部基准
CGO_ENABLED=0 go test -bench=. ./test/bench/...

# 指定运行时长
CGO_ENABLED=0 go test -bench=. -benchtime=10x ./test/bench/...
```

基准套件 B1-B10：

| 基准 | 说明 | 目标 |
|------|------|------|
| B1 | 写入吞吐 | >= 20k docs/s |
| B2 | 向量 P99 | < 50ms |
| B3 | 过滤向量 P99 | < 80ms |
| B4 | BM25 P99 | < 80ms |
| B5 | 混合三路 P99 | < 120ms |
| B6 | 可见性 P99 | < 200ms |
| B8 | 聚合吞吐 | >= ClickHouse 同硬件 50% |
| B9 | 内存效率 | <= 3.5GB / 千万 768d |

### 性能回归测试

```bash
CGO_ENABLED=0 go test -run=TestPerformanceRegression ./test/bench/...
```

CI 中以微缩版基准（`-benchtime=10x`）运行，允许失败（非阻塞）。

## 混沌测试

```bash
CGO_ENABLED=0 go test ./test/chaos/...
```

混沌测试框架定义 C1-C6 场景：

| 场景 | 说明 |
|------|------|
| C1 | kill-leader |
| C2 | network-partition |
| C3 | slow-io |
| C4 | split-dual-kill |
| C5 | retry |
| C6 | split-brain-recovery |

测试框架生成 Docker Compose + Toxiproxy 配置，执行故障注入并验证恢复。

## 集成测试基础设施

`test/integration/testserver.go` 提供测试基础设施：

- `TestServer`：启动嵌入式引擎 + `httptest.Server`，用于测试
- `TestClient`：HTTP 请求封装（Put/Post/Get/Delete/PostNDJSON + 便捷方法）

其他测试包通过引用 `test/integration` 获取一致的测试服务器和客户端。

## 测试最佳实践

### 表格驱动测试

```go
func TestExample(t *testing.T) {
    tests := []struct {
        name     string
        input    string
        expected string
    }{
        {"basic", "hello", "hello"},
        {"empty", "", ""},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            got := process(tt.input)
            assert.Equal(t, tt.expected, got)
        })
    }
}
```

### testify 断言

项目使用 `github.com/stretchr/testify` 作为测试断言库：

```go
import (
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

// assert 失败继续执行
assert.Equal(t, expected, actual)

// require 失败立即停止
require.NoError(t, err)
```

### 测试命名约定

| 前缀 | 说明 | 示例 |
|------|------|------|
| `Test` | 标准单元测试 | `TestFlushPipeline` |
| `TestGolden_` | 金标准测试 | `TestGolden_DocCRUD` |
| `TestE2E_` | 端到端测试 | `TestE2E_HybridSearch` |
| `Benchmark` | 基准测试 | `BenchmarkWrite` |
| `Fuzz` | 模糊测试 | `FuzzParseQuery` |
