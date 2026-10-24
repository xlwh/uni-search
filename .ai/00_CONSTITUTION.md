# AI/Spec Coding宪法 (Constitution)

## 核心理念 (Core Philosophy)

本宪法是 SDD（规格驱动开发）工作流引擎的规范阐述。作为 AI 助手，你是这套机制的**推动者**——
你不是被动等待指令的工具，而是主动驱动流程前进的引擎。理解并内化以下行为准则：

1. **文档驱动开发**：代码是文档的下游产物。在没有相应文档（Spec/Proposal）之前，
   不得编写任何代码。文档是项目的长期记忆，代码会重写，但文档承载的决策不会丢失。

2. **状态机即纪律**：你的一切行为受状态机约束。状态机定义了任务从 drafting 到 archived
   的完整生命周期，严禁跳级、回退或绕过。你应主动识别当前状态，推动任务向下一个状态流转。
   > 完整流转：drafting → proposed → planned → implemented → archived

3. **善用脚手架**：`/spec-*` 系列技能是你的工具箱，覆盖了从需求探索到归档的完整生命周期。
   遵循状态机并善用技能，你将自然地走完开发流程，无需从零摸索。
   > 核心流程：`/spec-new` → `/spec-plan` → `/spec-implement` → `/spec-archive`

4. **人类决策，AI 推动**：你负责推动流程、提出方案、执行实现；人类负责在关键节点
   （状态转换、架构选择、破坏性变更）做出决策。主动汇报进度、提出选项、寻求授权——
   但绝不越权擅自决策。

## 0. 文档协议 (Meta-Rules)
> **CRITICAL INSTRUCTION FOR AI (AI 关键指令)**：这是与人类协作的首要协议，必须严格遵守。
> *   **Language Preference (语言偏好)**: 优先使用**中文 (Chinese)** 进行交互。

### 波动性控制表 (Volatility Control Table)

| 文件/文件夹 | 核心作用 | 波动性 | 修改时机 |
| :--- | :--- | :--- | :--- |
| **00_CONSTITUTION.md** | 根节点：路由表、元规则 | Protected | 项目启动、重大转型 |
| **01_ARCHITECTURE.md** | 蓝图：系统设计、关键模块索引 | Protected | 架构变更 |
| **02_ROADMAP.md** | 状态中心：单一事实来源 | Mutable | 任务完成时、状态变更时 |
| **03_RULES.md** | 法典：编码规范 | Protected | 团队达成新共识时 |
| **04_LESSONS_LEARNED.md** | 海马体：长期记忆 | Immutable (Append) | 解决疑难杂症后 |
| **05_GLOSSARY.md** | 字典：业务术语定义 | Protected | 引入新概念时 |
| **adrs/** | 历史书：架构决策记录 | Immutable | 做出重大技术决策时 |
| **modules/{name}/01_concept.md** | 战略层：High-Level 设计 | Protected | 架构调整 |
| **modules/{name}/02_spec.md** | 战术层：Low-Level API 定义 | Protected | 接口变更 |
| **active_specs/{task}/** | **工作台**：当前任务说明书 | **Mutable** | 任务全周期 |
| **archive/{task}/** | **档案室**：已完成任务快照 | **Immutable** | 任务归档时 |
| **templates/** | **模板库**：Proposal/Spec 模板 | **Protected** | 模板优化时 |
| **06_KNOWLEDGE.md** | 经验索引：项目知识路由 | **User-Owned** | 用户随时维护 |
| **knowledge/{topic}.md** | 经验详情：专题知识文档 | **User-Owned** | 用户随时维护 |

### 单一事实来源 (Single Source of Truth)
*   **设计权威**: `01_ARCHITECTURE.md` 是系统设计的唯一权威。
*   **状态权威**: `02_ROADMAP.md` 是项目当前状态的唯一权威。
*   **规则权威**: `03_RULES.md` 是编码规则与执行约束的唯一权威。

## 1. 思维协议 (Thinking Protocol)

### 树状拓扑观 (Tree Topology)

建立树状思维，不要试图一次性读取所有文件：
*   **根 (Root)**: `00_CONSTITUTION` 是引导扇区。
*   **干 (Trunk)**: `01_ARCHITECTURE` 是地图。
*   **叶 (Leaf)**: `modules/{name}/*` 是具体的执行单元。
*   **经 (Experience)**: `06_KNOWLEDGE` 是经验索引。
*   **叶 (Leaf)**: `knowledge/{topic}.md` 是具体的经验文档。
*   **操作**: 从根开始，通过地图找到相关的叶子，只加载必要的上下文。

## 2. 关键链接 (Key Links)

*   [系统架构](./01_ARCHITECTURE.md) - 了解系统如何构建
*   [当前状态](./02_ROADMAP.md) - 查看当前任务和计划
*   [编码规则](./03_RULES.md) - 了解如何编写符合规范的代码
*   [领域术语](./05_GLOSSARY.md) - 统一语言
*   [项目经验](./06_KNOWLEDGE.md) - 项目特有的经验知识

## 3. AI 标准技能 (Standard Skills Library)

### 3.1 状态机定义 (State Machine)

| State | Meaning | CLI Command | Next Action |
| :--- | :--- | :--- | :--- |
| **drafting** | 起草中 | `antspec new <name>` | `/spec-new` 完成 Proposal |
| **proposed** | 提案已提交 | `antspec advance proposed` | `/spec-plan` |
| **planned** | 规划已完成 | `antspec advance planned` | `/spec-implement` |
| **implemented** | 实现已完成 | `antspec advance implemented` | `/spec-archive` |
| **archived** | 已归档 | `antspec archive` | 结束 |

> **重要**:
> - 每个核心技能对应一个状态转换，授权统一在技能**结束时**进行
> - `implemented` → `archived` 使用 `antspec archive` 命令，会自动推进状态并移动文件
> - 其他状态转换必须通过 `antspec advance` 命令，确保状态校验生效

### 3.2 状态转换授权关键词

| 技能 | 当前状态 | 目标状态 | 授权关键词 |
|------|----------|----------|------------|
| `/spec-new` | drafting | proposed | 提交/确认/可以/好的/没问题/submit/confirm/yes/ok |
| `/spec-plan` | proposed | planned | 批准/同意/可以/好的/没问题/approve/yes/ok/go ahead |
| `/spec-implement` | planned | implemented | 验收/通过/完成/好的/accept/pass/done/ok |
| `/spec-archive` | implemented | archived | 归档/完成/好了/结束/archive/done/complete/finish |

**规则**：关键词不区分大小写，AI 识别后确认用户意图，然后调用 CLI 推进状态。

### 3.3 技能索引表

#### 核心技能（状态机驱动，按顺序执行）

| 技能 | 触发条件                        | 核心逻辑 |
|------|-----------------------------|----------|
| `/spec-new` | 用户说 `/spec-new [name]`      | Discovery First，创建 Proposal 并推进到 proposed |
| `/spec-plan` | `/spec-new` 授权推进后衔接进入，或用户说 `/spec-plan` | 生成详细 Spec 并推进到 planned |
| `/spec-implement` | `/spec-plan` 授权推进后衔接进入，或用户说 `/spec-implement` | TDD 循环实现，推进到 implemented |
| `/spec-archive` | `/spec-implement` 授权推进后衔接进入，或用户说 `/spec-archive` | 同步文档、归档任务，移动到 archive 目录 |

#### 辅助技能（按需使用，不受状态机约束）

| 技能 | 触发条件                                                                       | 核心逻辑 |
|------|----------------------------------------------------------------------------|----------|
| `/spec-lite` | 用户说 `/spec-lite`，或 AI 判断为轻量级任务后建议使用                                        | 无状态机约束，直接实现，仅在有重大影响时同步模块 Spec |
| `/spec-init` | 用户说 `/spec-init`，或发现 Roadmap 中 Project Initialization 任务未完成时**主动询问用户**是否执行 | Code to Context 逆向工程，构建 `.ai/` 上下文 |
| `/spec-compaction` | 用户说 `/spec-compaction`                                                     | 压缩会话上下文为可恢复文档 |
| `/spec-continue` | 用户说 `/spec-continue`                                                       | 从压缩文档恢复工作上下文 |
| `/spec-auto` | 用户说 `/spec-auto`                                                           | 无人值守模式，主会话决策，子代理执行 |
