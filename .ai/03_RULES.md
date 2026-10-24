# 编码规则与标准 (Coding Rules & Standards)

> **核心定位**: 本文件不是通用的编码规范，而是 Spec 范式的**执行约束**。这些规则强制 AI 遵循文档驱动的工作流，确保开发过程的可追溯性和状态一致性。

## 1. 文档驱动强制约束 (Documentation-Driven Enforcement)

### 1.1 Spec 优先原则 (Spec-First Principle)
*   **强制流程**:
    1. 任何功能变更必须先在 `active_specs/{task}/02_spec.md` 中定义
    2. Spec 应包含足够的实现细节（如涉及代码变更，应包含 API 签名、数据结构、错误处理逻辑等）
    3. 编码时原则上应按照 Spec 实施；如需偏离，依然遵循先更新Spec，再编码的原则
*   **可追溯性**: 所有代码变更必须能够追溯到具体的 Spec 条目。建议使用注释标记，指向**归档后的文档路径**：`# @spec: ./archive/{task}/02_spec.md#section`

## 2. CLI 调用强制约束 (CLI Invocation Enforcement)

> 所有状态变更操作必须通过 CLI 执行，确保状态校验生效。

### 2.1 强制 CLI 调用场景 (Mandatory CLI Invocation)

以下操作**必须**通过 CLI 命令执行，禁止手动修改文件：

| 操作 | CLI 命令 | 禁止行为 |
|------|----------|----------|
| 创建任务 | `antspec new <name>` | 手动创建目录和 meta.json |
| 查询状态 | `antspec status [--json]` | 直接读取 meta.json |
| 状态推进 | `antspec advance <state>` | 手动修改 meta.json 中的 status 字段 |
| 列出任务 | `antspec list` | 手动遍历 active_specs 目录 |

### 2.2 状态查询与错误处理 (Status Query & Error Handling)

AI 可以通过 CLI 了解当前spec任务状态和活跃任务列表：

```bash
antspec status --json    # 当前任务状态
antspec list --json      # 活跃任务列表
```

*   CLI 失败时，读取错误信息并按建议修正
*   `antspec advance <state>`的`--force` option，仅在用户进行授权后使用

## 3. 上下文管理规则 (Context Management Rules)

### 3.1 时态区分强制 (Temporal Distinction Enforcement)
*   **Present Tense (真理)**: `modules/` 下的文档描述代码的**当前状态**。编码时可以引用，但不能直接修改（须通过 `/spec-archive` 归档时同步更新）。
*   **Future Tense (草稿)**: `active_specs/` 下的文档描述**目标状态**。这是编码的依据。
*   **禁止混淆**: 编码时必须基于 `active_specs/` 中的 Spec，而不是 `modules/` 中的现有文档。

### 3.2 读写分离 (Read/Write Separation)
*   **设计阶段**: 只读 `01_concept.md`，关注战略决策和 Why
*   **编码阶段**: 只读 `02_spec.md`，关注 API 和 How
*   **禁止干扰**: 编码时不要被 `01_concept.md` 中的哲学讨论干扰

## 4. 图表规范 (Diagram Rules)

### 4.1 工具与类型
*   **工具**: 必须使用 **Mermaid**
*   **类型**:
    *   架构图: `graph TD` / `C4Context`
    *   时序图: `sequenceDiagram` (用于复杂交互)
    *   类图: `classDiagram` (用于详细设计)

### 4.2 语法安全 (Syntax Safety)
*   **禁止嵌套 Break**: 在 `sequenceDiagram` 中，`break` 是顶级控制块，**严禁**将其嵌套在 `alt` 或 `opt` 块内部（会导致渲染错误）
*   **正确写法**:
    ```mermaid
    alt Condition
        ...
    end
    break Exit Condition
        ...
    end
    ```
*   **特殊字符**: 节点文本中包含 `()`, `[]`, `{}` 等特殊字符时，必须使用双引号包裹，例如 `id["Label (Content)"]`

## 5. 代码风格 (Style)

### 5.1 语言风格指南 (Language Style Guides)
*   **C++**: 遵循 Google C++ Style Guide
*   **Python**: 遵循 Google Python Style Guide
*   **Java**: 遵循 Google Java Style Guide
*   **Go**: 遵循 Effective Go 和 Go Code Review Comments
*   **TypeScript/JavaScript**: 遵循 Google TypeScript Style Guide

### 5.2 卫语句 (Guard Clauses)
*   优先使用 Early Return 处理错误或非法参数
*   避免深层缩进（`if-else` 嵌套），确保正常逻辑位于最外层

## 6. 日志规范 (Logging)

### 6.1 打点原则
*   关键业务函数需在 Entry/Exit/Error 处打点
*   日志内容应包含：Input Data Size, Return Code, Error Message

### 6.2 性能约束
*   高频循环内部禁止打日志，避免性能损耗

### 6.3 输出策略
*   **开发环境**: Console + Log File
*   **生产环境**: Log File only

## 7. 扩展点执行协议 (Extension Point Protocol)

> 核心技能（spec-new、spec-plan、spec-implement、spec-archive）在执行步骤中包含 `Extension Point` 标记，用于支持用户自定义扩展。本章节定义统一的检查和执行流程。

### 7.1 扩展点检查流程
当技能执行步骤中遇到 `Extension Point` 标记时，按以下流程处理：
1. 检查标记中指定的 Hook 文件路径（如 `.ai/hooks/spec-new/on_discovery.md`）是否存在
2. 若文件存在，读取 YAML Frontmatter 中的 `enabled` 字段
3. 若 `enabled: true`（或未设置 enabled 字段），读取 Markdown 正文中的扩展指令并执行
4. 若 `enabled: false` 或文件不存在，跳过此扩展点，继续执行下一步

### 7.2 容错原则
*   扩展点失败**不应阻断**核心流程
*   Hook 文件 YAML Frontmatter 格式错误时，跳过并输出警告提示用户检查
*   扩展指令执行异常时，报告异常后继续执行核心步骤


## 8. 禁止行为清单 (Prohibited Behaviors)

以下行为**严格禁止**，违反将导致架构一致性破坏：

- ❌ 在没有 Spec 的情况下编写代码
- ❌ 跳过状态机的任何状态
- ❌ 在用户未确认的情况下进行状态转换
- ❌ **手动修改 `00_meta.json` 中的状态字段**
- ❌ **绕过 CLI 直接创建任务目录**
- ❌ **忽略 CLI 返回的错误信息**
- ❌ 假设存在未记录的外部知识或依赖
- ❌ 一次性递归读取所有 `.ai/` 文件
- ❌ 编码时基于 `modules/` 文档而非 `active_specs/` 文档
- ❌ 同步未通过测试的代码
- ❌ 将计算机通用词汇添加到 Glossary
- ❌ 在未征求用户同意的情况下进行破坏性变更
- ❌ 需求变更时直接修改代码而不先更新文档
- ❌ 归档时未更新 `02_ROADMAP.md`

