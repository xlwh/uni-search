# AntSpec Extension Hooks - spec-new

本目录用于存放 spec-new 技能的扩展点文件。

## 可用扩展点

| 扩展点 | 触发时机 | 典型场景 |
|--------|----------|----------|
| `on_discovery` | 上下文影响分析完成后，需求访谈开始前 | 注入额外分析维度 |
| `on_interview_complete` | 需求访谈结束后，起草 Proposal 前 | 强化访谈策略 |

## 使用方法

1. 将本文件复制并重命名为扩展点名称（如 `on_interview_complete.md`）
2. 修改 YAML Frontmatter 中的字段
3. 在「扩展指令」章节中编写你的自定义逻辑

## 文件格式

```yaml
---
name: on_interview_complete    # 扩展点名称
description: 你的扩展用途说明    # 用途简述
enabled: true                   # 启用开关，设为 false 可临时禁用
---
```

正文使用 Markdown 格式编写扩展指令，AI 在执行到对应扩展点时会读取并执行。

## 示例：强化安全需求访谈

以下是一个完整的扩展文件示例。将此内容保存为 `on_interview_complete.md` 即可生效：

```markdown
---
name: on_interview_complete
description: 强化安全需求访谈
enabled: true
---

## 扩展指令

在需求访谈结束后，额外执行以下动作：

1. 检查本次需求是否涉及用户数据处理、权限变更或外部系统交互
2. 如涉及上述任一场景，追加一轮安全性专项访谈，覆盖：
   - 数据加密策略
   - 权限控制方案
   - 审计日志需求
3. 将安全性结论写入 Proposal 的「风险评估」章节
```
