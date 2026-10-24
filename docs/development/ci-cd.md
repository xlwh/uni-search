# CI/CD

## 概述

UniSearch 使用 GitHub Actions 作为 CI/CD 平台，配置文件位于 `.github/workflows/ci.yml`。每次 push 或 PR 到 `main`/`master` 分支触发三个并行 Job。

## CI 流程

### 触发条件

```yaml
on:
  push:
    branches: [main, master]
  pull_request:
    branches: [main, master]
```

### Job 1: lint-test-build

主 CI 流程，按顺序执行：

| 步骤 | 命令 | 说明 |
|------|------|------|
| 1. golangci-lint | `golangci-lint run --timeout=5m` | 代码静态检查（v1.60） |
| 2. test | `CGO_ENABLED=0 go test -race -timeout 120s ./...` | 全量测试 + race 检测 |
| 3. build | `CGO_ENABLED=0 go build ./...` | 编译全部包 |
| 4. vet | `go vet ./...` | Go 官方 vet |
| 5. golden tests | `CGO_ENABLED=0 go test -timeout 60s ./test/golden/...` | ES 兼容金标准测试 |

Go 版本：1.23（CI 环境）。

### Job 2: cross-build

三平台交叉编译：

| 步骤 | 命令 | 产物 |
|------|------|------|
| linux/amd64 | `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o dist/unisearch-linux-amd64 ./cmd/unisearch/` | `dist/unisearch-linux-amd64` |
| linux/arm64 | `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o dist/unisearch-linux-arm64 ./cmd/unisearch/` | `dist/unisearch-linux-arm64` |
| darwin/arm64 | `CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o dist/unisearch-darwin-arm64 ./cmd/unisearch/` | `dist/unisearch-darwin-arm64` |

产物通过 `upload-artifact` 上传，可在 PR 页面下载。

### Job 3: bench-regression

性能回归微基准：

```bash
CGO_ENABLED=0 go test -bench=. -benchtime=10x -timeout=600s ./test/bench/... || true
```

- 使用 `|| true` 确保非阻塞（允许失败）
- `-benchtime=10x` 运行微缩版基准（非完整跑）
- 结果供 PR 评审参考

## 本地 CI 模拟

### make ci

```bash
make ci
# 等价于: lint + test + build
# 1. go vet ./...
# 2. CGO_ENABLED=0 go test ./...
# 3. CGO_ENABLED=0 go build ./...
```

### scripts/ci.sh

平台无关的 CI 入口脚本：

```bash
bash scripts/ci.sh
```

执行流程：

1. `go vet ./...`
2. `golangci-lint run ./...`（若已安装）
3. `CGO_ENABLED=0 go test -race ./...`
4. `CGO_ENABLED=0 go build ./...`

### 单独运行各阶段

```bash
# 仅 lint
make lint

# 仅 test
make test

# 仅 build
make build

# 带覆盖率
make coverage

# 金标准测试
CGO_ENABLED=0 go test ./test/golden/...
```

## CI 门禁规则

| 检查项 | 门禁级别 | 说明 |
|--------|----------|------|
| golangci-lint | 阻塞 | 所有 linter 零报告 |
| race 检测 | 阻塞 | `go test -race` 必须零报告 |
| 编译 | 阻塞 | `go build ./...` 必须成功 |
| go vet | 阻塞 | 必须零报告 |
| 金标准测试 | 阻塞 | ES 兼容测试必须通过 |
| 性能回归 | 非阻塞 | 允许失败，结果供参考 |

## 发布流程

### 版本号

使用语义化版本 `v<major>.<minor>.<patch>`，如 `v0.1.0`。

### 交叉编译发布

```bash
# 指定版本号交叉编译
VERSION=v0.1.0 make cross-build

# 产物
ls dist/
# unisearch-linux-amd64
# unisearch-linux-arm64
# unisearch-darwin-arm64
```

### Docker 镜像发布

```bash
# 构建 Linux 二进制
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o unisearch-linux ./cmd/unisearch

# 构建镜像
docker build -t unisearch:v0.1.0 .

# 推送镜像
docker push unisearch:v0.1.0
```

### 兼容矩阵报告

金标准测试中的 `TestCompatMatrix` 生成 ES 兼容性矩阵报告，随版本发布。

## 性能回归看护

| 基准 | CI 运行 | 阈值 |
|------|---------|------|
| B1 写入吞吐 | 微缩版 10x | +-10% 告警 |
| B2 向量 P99 | 微缩版 10x | +-10% 告警 |
| B6 可见性 P99 | 微缩版 10x | +-10% 告警 |

性能预算超支时 48 小时内响应。详见 [性能调优](../operations/performance-tuning.md) 和 [质量保障设计](../go-design/06-quality-and-observability.md)。

## Lint 配置

`.golangci.yml` 启用 5 个 linter：

| Linter | 配置 |
|--------|------|
| errcheck | `check-type-assertions: true` |
| govet | 默认 |
| staticcheck | 默认 |
| revive | 默认 |
| gocritic | `enabled-tags: [diagnostic, performance]` |

排除规则：`_test.go` 文件豁免 `errcheck` 和 `gocritic`；`internal/raft/` 豁免 `gocritic`。

详见 [代码规范](code-style.md)。
