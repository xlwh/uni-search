# Proposal: UniSearch Web 管控页面

## 1. 背景与动机 (Background & Motivation)

### 1.1 当前问题

UniSearch 当前是纯 API 服务器，提供 100+ ES 兼容 REST 端点，但没有任何前端/静态文件处理机制。运维和测试人员只能通过 curl 命令或第三方工具（如 Postman、Kibana）操作服务，使用门槛高、操作效率低。

具体痛点：
- 无可视化界面，无法直观查看集群健康状态、索引列表、文档内容
- 搜索测试需手动拼接 JSON DSL，无交互式编辑器
- 多租户配额管理无 API 端点暴露，无法通过 REST 操作
- 监控指标仅以 Prometheus 文本格式暴露，需额外搭建 Grafana 才能可视化
- 快照、同义词、Ingest Pipeline 等管理操作无图形化引导

### 1.2 用户期望

提供一个开箱即用的 Web 管控页面，内嵌在 UniSearch 二进制中，无需额外部署前端服务，打开浏览器即可进行服务管控。覆盖 8 个功能模块：集群总览、索引管理、搜索控制台、文档管理、多租户管理、快照与备份、监控指标、同义词与管道。

## 2. 目标 (Objective)

1. **单一二进制部署**：通过 `go:embed` 将 Vue 3 前端构建产物嵌入 Go 二进制，`make build` 一键构建全栈，零额外依赖
2. **独立端口管控**：管控页面使用独立端口 :9201，与 ES API :9200 分离，互不干扰
3. **8 个管控模块全覆盖**：集群总览 Dashboard、索引管理、搜索控制台、文档管理、多租户管理、快照与备份、监控指标、同义词与管道

## 3. 方案设计 (Approach)

### 3.1 设计原则

- **零侵入**：不修改现有 ES 兼容 API 路由和处理器，通过反向代理复用所有现有端点
- **编译安全**：提交占位 `dist/index.html` 确保 `go build ./...` 始终能编译，不依赖 Node.js 环境
- **单一二进制**：前端构建产物通过 `go:embed` 嵌入，部署只需一个二进制文件
- **端口隔离**：管控页面独立端口 :9201，ES API 保持 :9200 不变

### 3.2 核心方案

独立端口 + 反向代理架构：

```
浏览器 → :9201 AdminServer
              ├── /api/proxy/*    → httputil.ReverseProxy → :9200 ES API
              ├── /api/tenants    → 直接操作 tenant.Manager（新增端点）
              ├── /api/dashboard  → 聚合端点（新增）
              ├── /api/metrics    → JSON 指标端点（新增）
              └── /*              → go:embed 静态文件（SPA 回退）
```

AdminServer 与 esapi.Server 共享同一个 `embedded.Engine` 实例。ES API 调用通过反向代理转发到 :9200，避免重构 esapi.Server 内部 store。多租户 CRUD 和 Dashboard 聚合是新增端点，直接操作 engine 和 tenant.Manager。

### 3.3 核心流程

服务启动流程（`runEmbedded` 修改）：

```
main → root.go → runEmbedded()
  ├── embedded.New(cfg) → engine
  ├── esapi.NewServer(engine) → http :9200
  ├── tenant.NewManager() → tenantMgr
  ├── admin.NewAdminServer(engine, tenantMgr, cfg.Listen) → http :9201
  └── 优雅关闭: admin.Shutdown → esapi.Shutdown → tenantMgr.Close → engine.Close
```

### 3.4 技术选型

| 维度 | 选型 | 理由 |
|------|------|------|
| 前端框架 | Vue 3 + TypeScript | 轻量，Element Plus 组件库成熟，适合管控页面 |
| UI 组件库 | Element Plus | Table/Form/Dialog/Card 等组件齐全 |
| 构建工具 | Vite 5.x | 快速 HMR，Vue 3 原生支持 |
| 状态管理 | Pinia | Vue 3 官方推荐 |
| HTTP 客户端 | Axios | 拦截器、错误处理 |
| 代码编辑器 | CodeMirror 6 | DSL 编辑器、JSON 编辑器，比 Monaco 轻量 |
| 图表 | ECharts 5 | Prometheus 指标可视化 |
| 嵌入方式 | go:embed | 单一二进制部署 |
| API 复用 | httputil.ReverseProxy | 零侵入复用现有 ES API |

## 4. 变更范围 (Impact)

### 4.1 新增文件

| 文件路径 | 说明 |
|----------|------|
| `internal/server/admin/server.go` | AdminServer 结构体、NewAdminServer、ServeHTTP 分发 |
| `internal/server/admin/proxy.go` | 反向代理到 ES API :9200 |
| `internal/server/admin/embed.go` | go:embed 指令 + 静态文件服务（SPA 回退） |
| `internal/server/admin/tenant_handlers.go` | 租户 CRUD 端点（GET/POST/PUT/DELETE /api/tenants） |
| `internal/server/admin/dashboard_handlers.go` | Dashboard 聚合端点（GET /api/dashboard） |
| `internal/server/admin/metrics_handlers.go` | JSON 指标端点（GET /api/metrics） |
| `internal/server/admin/dist/index.html` | 占位 HTML（提交 Git，确保 go:embed 编译通过） |
| `web/admin/package.json` | 前端项目依赖定义 |
| `web/admin/vite.config.ts` | Vite 构建配置，outDir 指向 Go dist |
| `web/admin/tsconfig.json` | TypeScript 配置 |
| `web/admin/index.html` | Vite 入口 HTML |
| `web/admin/src/main.ts` | 应用入口 |
| `web/admin/src/App.vue` | 根组件 |
| `web/admin/src/router/index.ts` | Vue Router（8 个路由） |
| `web/admin/src/api/client.ts` | Axios 实例 + 拦截器 |
| `web/admin/src/api/proxy.ts` | ES API 代理封装 |
| `web/admin/src/api/*.ts` | 各功能模块 API 封装（cluster/indices/search/documents/tenants/snapshots/metrics/synonyms/pipelines/scripts） |
| `web/admin/src/views/*.vue` | 8 个页面视图组件 |
| `web/admin/src/components/*.vue` | 复用组件（AppLayout/JsonEditor/MetricCard/DataTable/ConfirmDialog/StatusBadge/Pagination） |
| `web/admin/src/composables/*.ts` | 组合式函数（usePolling/usePagination） |
| `web/admin/src/stores/*.ts` | Pinia 状态管理 |

### 4.2 修改文件

| 文件路径 | 修改内容 |
|----------|----------|
| `cmd/unisearch/server.go` | `runEmbedded()` 中新增 tenant.Manager 创建、AdminServer 启动和优雅关闭 |
| `cmd/unisearch/root.go` | 新增 `--admin` 和 `--admin-listen` CLI flag |
| `internal/config/config.go` | 新增 `AdminConfig` 结构体，`Config` 添加 `Admin` 字段，`Default()` 和 `Validate()` 扩展 |
| `internal/config/load.go` | 新增 `UNISEARCH_ADMIN_ENABLED/LISTEN` 环境变量和 flag 映射 |
| `internal/tenant/tenant.go` | 新增 `Manager.List()`、`Manager.Delete()`、`ResourceGroup.Usage()`、`ResourceGroup.Quota()` 方法 |
| `Makefile` | 新增 `build-frontend` 目标，修改 `build` 依赖链 |
| `.github/workflows/ci.yml` | 新增 Node.js setup 步骤，build 改用 `make build` |
| `.gitignore` | 新增 `web/admin/node_modules/`、`internal/server/admin/dist/assets/` |

### 4.3 不涉及

- 现有 ES 兼容 REST API 路由和处理器不修改（通过反向代理复用）
- 认证/鉴权机制不实现（当前 API 本身无认证，管控页面也不加）
- gRPC 服务不修改
- 存储引擎、索引引擎、查询引擎不修改
- Cloud 分布式模式不涉及（仅 embedded 模式）

## 5. 接口定义 (Interface Definition)

### 5.1 Admin REST API 端点

**ES API 代理**：
- `ANY /api/proxy/{path...}` — 反向代理到 :9200，路径剥离 `/api/proxy/` 前缀

**租户管理**（新增，直接操作 tenant.Manager）：
```
GET    /api/tenants          — 列出所有租户（含配额和使用量）
POST   /api/tenants          — 注册新租户（body: {tenant_id, quota})
GET    /api/tenants/{id}     — 获取单个租户详情
PUT    /api/tenants/{id}     — 更新租户配额
DELETE /api/tenants/{id}     — 删除租户
```

**Dashboard 聚合**（新增）：
```
GET    /api/dashboard        — 返回集群健康+节点信息+索引数+文档数等聚合数据
```

**JSON 指标**（新增）：
```
GET    /api/metrics          — 返回结构化指标 JSON（指标名、类型、当前值、标签）
```

### 5.2 CLI 参数

```
--admin              启用/禁用 Admin 管控页面（默认 true）
--admin-listen ADDR  Admin 监听地址（默认 :9201）
```

### 5.3 配置项

```yaml
admin:
  enabled: true
  listen: ":9201"
```

环境变量：`UNISEARCH_ADMIN_ENABLED`、`UNISEARCH_ADMIN_LISTEN`

## 6. 稳定性架构 (Stability Architecture)

### 6.1 限流与熔断

管控页面为低频人工操作场景，不单独实现限流。ES API 代理请求复用后端 :9200 已有的 tenant 配额管控和背压机制。

### 6.2 资源隔离与限制

- Admin 服务器使用独立端口和独立 HTTP Server 实例，与 ES API 服务隔离
- 管控页面的资源消耗极低（静态文件服务 + 少量 API 代理），不设置额外资源限制
- tenant.Manager 后台 goroutine 的令牌补充间隔不受影响

### 6.3 灾难恢复

- Admin 服务器是无状态的（除 tenant.Manager 外），重启后自动恢复
- tenant.Manager 的配额状态为内存态，重启后重置为默认配置
- 前端构建产物嵌入二进制，不受文件系统损坏影响

## 7. 可测性设计 (Testability Design)

### 7.1 本地手动验证

- Step 1: `make build` 构建包含前端的完整二进制
- Step 2: `./unisearch --embedded --data-dir ./data --listen :9200` 启动服务
- Step 3: 浏览器打开 `http://localhost:9201` 验证管控页面加载
- Step 4: 在 Dashboard 页面验证集群健康状态显示
- Step 5: 在索引管理页面创建/删除索引，验证操作生效
- Step 6: 在搜索控制台执行查询，验证结果展示
- Step 7: 在多租户管理页面创建租户、设置配额，验证数据持久
- Step 8: 在监控页面查看指标图表渲染

### 7.2 自动化测试策略

- Unit Tests: AdminServer 路由分发逻辑、租户 CRUD handler、metrics handler
- Integration Tests: 启动 admin 服务器 + ES API，验证代理转发、聚合端点、租户端点
- E2E Tests: 前端构建产物加载、SPA 路由回退

## 8. 风险与缓解

| 风险 | 缓解措施 |
|------|----------|
| go:embed 编译失败（dist 目录为空） | 提交占位 `dist/index.html` 到 Git，确保始终可编译 |
| 端口冲突（:9201 被占用） | `Validate()` 中校验 `Admin.Listen` 与 `Listen` 不同；用户可通过 `--admin-listen` 自定义 |
| tenant.Manager 后台 goroutine 泄漏 | `runEmbedded` defer 链中调用 `manager.Close()` 停止 goroutine |
| 前端构建环境缺失导致 `make build` 失败 | `build-frontend` 目标检测 npm 是否可用，不可用时跳过并保留占位文件 |
| 反向代理路径处理错误（双重斜杠） | 剥离 `/api/proxy/` 前缀时确保 `strings.TrimPrefix` 正确处理 |
| CI 直接 `go build ./...` 未构建前端 | 占位 `dist/index.html` 确保 CI 编译通过；CI workflow 同时新增 `make build` 步骤 |

## 9. 验收标准

- [ ] **业务**: `make build` 一键构建包含前端的二进制，`go build ./...` 始终能编译通过
- [ ] **业务**: 浏览器访问 `http://localhost:9201` 能正常加载管控页面
- [ ] **业务**: 8 个功能模块全部可用（集群总览/索引管理/搜索控制台/文档管理/多租户管理/快照与备份/监控指标/同义词与管道）
- [ ] **业务**: `--admin false` 能禁用管控页面，`--admin-listen :9301` 能自定义端口
- [ ] **稳定性**: Admin 服务器独立端口隔离，不影响 ES API :9200 正常服务
- [ ] **稳定性**: 优雅关闭顺序正确（admin → esapi → tenantMgr → engine）
- [ ] **可测性**: 本地手动验证 8 步全部通过
- [ ] **可测性**: `make build && make test` 全绿
