# 开发入门

## 环境要求

| 要求 | 版本 | 说明 |
|------|------|------|
| Go | 1.26.0+ | 项目使用 Go 1.26.0 开发，建议使用最新稳定版 |
| Git | 2.20+ | 版本控制 |
| Make | 任意版本 | 构建入口 |
| 操作系统 | Linux / macOS / WSL2 | Windows 需通过 WSL2 |

UniSearch 全局禁用 CGO（`CGO_ENABLED=0`），不依赖任何 C 库，编译产出纯静态二进制。无需安装额外的系统依赖。

## Go 工具链安装

### 安装 Go

```bash
# macOS (Homebrew)
brew install go

# Linux (官方二进制)
wget https://go.dev/dl/go1.26.0.linux-amd64.tar.gz
sudo tar -C /usr/local -xzf go1.26.0.linux-amd64.tar.gz
export PATH=$PATH:/usr/local/go/bin

# 验证
go version
# 预期: go version go1.26.0 ...
```

### Go 环境配置

```bash
# 设置 GOPATH（如使用默认值可跳过）
export GOPATH=$HOME/go
export PATH=$PATH:$GOPATH/bin

# 配置 Go 模块代理（国内网络推荐）
go env -w GOPROXY=https://goproxy.cn,direct
go env -w GONOSUMDB=*
```

## 获取源码

```bash
git clone <repository-url> uni-search
cd uni-search
go mod download
```

## 首次构建

```bash
# 编译全部包
make build

# 或手动构建主服务二进制
CGO_ENABLED=0 go build -o unisearch ./cmd/unisearch

# 验证二进制
./unisearch --help
```

`make build` 等价于 `CGO_ENABLED=0 go build ./...`，编译所有包但不产出二进制文件。构建主服务二进制需指定 `./cmd/unisearch`。

## 首次启动

```bash
# 嵌入式单节点模式启动
./unisearch --embedded --data-dir ./data --listen :9200

# 预期输出（JSON 日志）:
# {"ts":"...","level":"INFO","msg":"starting unisearch","mode":"embedded","listen":":9200","data_dir":"./data"}
# {"ts":"...","level":"INFO","msg":"http listening","addr":":9200"}
# {"ts":"...","level":"INFO","msg":"grpc listening","addr":":9300"}
```

## 首次 API 验证

```bash
# 版本探测
curl http://localhost:9200/

# 集群健康
curl http://localhost:9200/_cluster/health

# 创建索引
curl -X PUT http://localhost:9200/test -H 'Content-Type: application/json' -d '{
  "mappings": { "properties": { "title": { "type": "text" } } }
}'

# 写入文档
curl -X POST http://localhost:9200/test/_doc -H 'Content-Type: application/json' -d '{
  "title": "Hello"
}'

# 搜索
curl -X POST http://localhost:9200/test/_search -H 'Content-Type: application/json' -d '{
  "query": { "match": { "title": "Hello" } }
}'

# 清理
curl -X DELETE http://localhost:9200/test
```

## 首次测试

```bash
# 运行全部测试
make test

# 或带 race 检测
CGO_ENABLED=0 go test -race ./...

# 运行指定包测试
CGO_ENABLED=0 go test ./internal/storage/...
```

## 验证配置

```bash
# 查看合并后的有效配置
./unisearch --embedded --dump-config

# 预期输出（YAML 格式）:
# mode: embedded
# listen: :9200
# data:
#   dir: ./data
#   memtable:
#     flush_size_mb: 512
#     flush_interval: 30s
# storage:
#   segment_block_rows: 8192
# index:
#   hnsw:
#     m: 16
#     ef_construction: 200
#     quantization: sq8
# tenant:
#   default_mode: shared-shard
```

## IDE 配置

### VS Code

1. 安装 Go 扩展（`golang.go`）
2. 配置 `settings.json`：

```json
{
  "go.buildFlags": ["-tags=none"],
  "go.testEnvVars": {
    "CGO_ENABLED": "0"
  },
  "go.lintTool": "golangci-lint",
  "go.lintOnSave": "workspace"
}
```

3. 调试配置（`.vscode/launch.json`）：

```json
{
  "version": "0.2.0",
  "configurations": [
    {
      "name": "Launch unisearch",
      "type": "go",
      "request": "launch",
      "mode": "exec",
      "program": "${workspaceFolder}/unisearch",
      "args": ["--embedded", "--data-dir", "./data", "--listen", ":9200"]
    }
  ]
}
```

### GoLand

1. 打开 Settings > Go > Build Tags
2. 设置 Environment: `CGO_ENABLED=0`
3. Run Configuration: Program arguments 设为 `--embedded --data-dir ./data --listen :9200`

## 常见问题

### Go 版本不匹配

```
go: go.mod requires go >= 1.26.0
```

解决方案：升级 Go 到 1.26.0 或更高版本。

### 模块下载失败

```
go: module download failed: Get "https://proxy.golang.org/...": dial tcp: connection refused
```

解决方案：配置 Go 模块代理：

```bash
go env -w GOPROXY=https://goproxy.cn,direct
```

### 端口被占用

```
{"level":"ERROR","msg":"http listening","err":"listen tcp :9200: bind: address already in use"}
```

解决方案：使用其他端口启动：`./unisearch --embedded --listen :9201`

### gRPC 监听失败

```
{"level":"WARN","msg":"grpc listener bind failed, skipping gRPC","addr":":9300","err":"listen tcp :9300: bind: address already in use"}
```

gRPC 监听失败不会阻塞服务启动，HTTP 服务正常可用。如需 gRPC，设置环境变量 `UNISEARCH_GRPC_ADDR=:9301`。

## 下一步

- [构建与测试](build-and-test.md) — 了解完整的构建系统和测试体系
- [项目结构](project-structure.md) — 了解代码组织方式
- [配置参考](../user-guide/configuration-reference.md) — 了解全部配置项
- [快速开始](../user-guide/quickstart.md) — 创建索引和搜索文档
