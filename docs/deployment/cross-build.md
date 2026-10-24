# 交叉编译与发布

## 概述

UniSearch 使用 `CGO_ENABLED=0` 纯静态编译，支持三平台交叉编译，无需目标平台 C 工具链。交叉编译通过 `scripts/build.sh` 脚本或 `make cross-build` 执行。

## 支持的目标平台

| 平台 | GOOS | GOARCH | 说明 |
|------|------|-------|------|
| Linux AMD64 | linux | amd64 | x86_64 服务器 |
| Linux ARM64 | linux | arm64 | ARM 服务器 / AWS Graviton |
| macOS ARM64 | darwin | arm64 | Apple Silicon |

> Windows 平台不在支持列表中。Windows 用户建议通过 WSL2 或 Docker 使用。

## 构建方式

### make cross-build

```bash
make cross-build
```

等价于调用 `scripts/build.sh`，产出三个二进制到 `dist/` 目录。

### 手动单平台构建

```bash
# Linux AMD64
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o dist/unisearch-linux-amd64 ./cmd/unisearch

# Linux ARM64
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o dist/unisearch-linux-arm64 ./cmd/unisearch

# macOS ARM64
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o dist/unisearch-darwin-arm64 ./cmd/unisearch
```

## build.sh 脚本详解

```bash
#!/usr/bin/env bash
set -euo pipefail

VERSION="${VERSION:-dev}"
DIST_DIR="${DIST_DIR:-dist}"
mkdir -p "$DIST_DIR"

TARGETS=(
  "linux/amd64"
  "linux/arm64"
  "darwin/arm64"
)

for target in "${TARGETS[@]}"; do
  GOOS="${target%/*}"
  GOARCH="${target#*/}"
  output="$DIST_DIR/unisearch-${GOOS}-${GOARCH}"
  CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" \
    go build -ldflags "-X main.version=${VERSION}" -o "$output" ./cmd/unisearch
done
```

### 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `VERSION` | `dev` | 版本号，通过 `-ldflags` 注入 `main.version` |
| `DIST_DIR` | `dist` | 输出目录 |

### 版本注入

`-ldflags "-X main.version=${VERSION}"` 将版本号注入二进制。运行时可通过版本信息确认构建版本。

## 产物说明

```bash
# 查看产物
ls -lh dist/
# unisearch-linux-amd64   ~60MB
# unisearch-linux-arm64   ~60MB
# unisearch-darwin-arm64  ~60MB

# 验证静态链接（Linux 产物）
file dist/unisearch-linux-amd64
# 预期: ELF 64-bit LSB executable, statically linked

# 验证无动态依赖
ldd dist/unisearch-linux-amd64
# 预期: not a dynamic executable
```

## CI 交叉编译

GitHub Actions `cross-build` Job 自动执行三平台交叉编译：

```yaml
cross-build:
  runs-on: ubuntu-latest
  steps:
    - uses: actions/checkout@v4
    - uses: actions/setup-go@v5
      with:
        go-version: '1.23'
    - name: cross-build linux/amd64
      run: CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o dist/unisearch-linux-amd64 ./cmd/unisearch/
    - name: cross-build linux/arm64
      run: CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o dist/unisearch-linux-arm64 ./cmd/unisearch/
    - name: cross-build darwin/arm64
      run: CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o dist/unisearch-darwin-arm64 ./cmd/unisearch/
    - uses: actions/upload-artifact@v4
      with:
        name: binaries
        path: dist/
```

产物上传为 GitHub Artifact，可在 PR 页面下载。

## 版本发布流程

```bash
# 1. 指定版本号交叉编译
VERSION=v0.1.0 make cross-build

# 2. 验证产物
ls -lh dist/
file dist/unisearch-linux-amd64
file dist/unisearch-linux-arm64
file dist/unisearch-darwin-arm64

# 3. 创建发布包
cd dist/
tar czf unisearch-v0.1.0-linux-amd64.tar.gz unisearch-linux-amd64
tar czf unisearch-v0.1.0-linux-arm64.tar.gz unisearch-linux-arm64
tar czf unisearch-v0.1.0-darwin-arm64.tar.gz unisearch-darwin-arm64

# 4. 计算校验和
shasum -a 256 *.tar.gz > checksums.txt
```

## Docker 镜像构建

Dockerfile 使用预编译的 Linux 二进制：

```bash
# 1. 编译 Linux ARM64 二进制
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o unisearch-linux ./cmd/unisearch

# 2. 构建镜像
docker build -t unisearch:latest .

# 3. 构建多架构镜像（使用 buildx）
docker buildx build --platform linux/amd64,linux/arm64 -t unisearch:latest .
```

详见 [Docker 单节点部署](docker-single-node.md)。

## 注意事项

### CGO 禁用

`CGO_ENABLED=0` 是项目全局约束（设计约束 G3）。所有依赖必须为纯 Go 实现，禁止引入任何 C 依赖库。

### 纯 Go 依赖验证

交叉编译前确认 `go.mod` 中的依赖均支持纯 Go：

```bash
# 检查是否有依赖需要 CGO
go list -deps ./cmd/unisearch | grep -i cgo
```

### ARM64 兼容性

项目使用 SIMD 加速向量距离计算，支持 AVX2+FMA（x86）和 NEON（ARM）双架构。ARM64 平台自动使用 NEON 路径，无需特殊配置。纯 Go 降级路径在 SIMD 不可用时自动启用。
