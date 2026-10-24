# 系统架构 (System Architecture)

## 1. 使命宣言 (Mission Statement)
> **[Project Name]** 致力于 [项目核心使命]。
> [在此处描述项目的愿景、核心价值和目标。]

## 2. 概述 (Overview)
[在此处提供系统架构的高层概述。描述核心设计原则和该系统解决的问题。]

### 2.1 技术栈 (Tech Stack)
- **编程语言**: [语言及版本]
- **构建工具**: [构建工具]
- **主要框架**: [框架列表]
- **测试框架**: [测试框架]
- **包管理**: [包管理工具]

### 2.2 整体架构 (Architecture Pattern)
[描述采用的架构模式，如分层架构、微服务、单体、领域驱动设计等]

### 2.3 [层级/组件 1]
> **位置**: `src/...`

[描述该层级的职责及其在系统中的作用。]

*   **职责**:
    *   [职责 1]
    *   [职责 2]
*   **核心组件**:
    *   `[组件名称]`

### 2.4 [层级/组件 2]
> **位置**: `src/...`

[描述]

## 3. 项目结构 (Project Structure)

### 3.1 目录结构 (Directory Structure)
```
[project_name]/
├── [dir1]/          # [说明]
├── [dir2]/          # [说明]
└── [dir3]/          # [说明]
```

### 3.2 关键文件 (Key Files)
| 文件 | 用途 | 重要性 |
|------|------|--------|
| [file] | [purpose] | [高/中/低] |

## 4. 数据流 (Data Flow)
[描述数据如何在系统中流动。可以使用 Mermaid 图表。]

1.  **[步骤 1]**: ...
2.  **[步骤 2]**: ...

## 5. 关键模块索引 (Key Module Index)
> **注意**: 此列表仅用于导航**核心**架构组件。它是手动维护的。

### 核心模块 (Core Modules)
*   **[模块名称]**:
    *   **Source**: `src/...`
    *   **Context**: [Concept](modules/example/01_concept.md) | [Spec](modules/example/02_spec.md)
    *   **Desc**: [简述]

### 业务模块 (Business Modules)
*(维护协议: 新增核心模块时，请在 `03_RULES.md` 指导下更新此索引)*

*   **[功能 A]**:
    *   **Source**: `src/...`
    *   **Context**: [Concept](modules/feature_a/01_concept.md) | [Spec](modules/feature_a/02_spec.md)

## 6. 关键概念 (Key Concepts)

### 6.1 领域术语 (Domain Terms)
| 术语 | 定义 |
|------|------|
| [term] | [definition] |

### 6.2 核心抽象 (Core Abstractions)
[描述系统中的核心抽象概念，如状态、实体、值对象等]

### 6.3 设计模式 (Design Patterns)
[列出项目中使用的主要设计模式及其应用场景]

## 7. 开发工作流 (Development Workflow)

### 7.1 编码标准 (Coding Standards)
[描述代码风格、命名规范等]

### 7.2 贡献指南 (Contributing Guide)
[描述开发环境搭建、提交流程等]

## 8. 常见任务 (Common Tasks)

### 8.1 [任务类型 1]
**涉及文件**: [files]
**步骤**: [steps]

### 8.2 [任务类型 2]
[同上格式]

## 9. 参考资料 (References)
[参考的项目内文档、外部文档链接等]

### 9.1 内部文档 (Internal Docs)
- [文档名](./path/to/doc.md) - 说明

### 9.2 外部链接 (External Links)
- [资源名](https://example.com) - 说明
