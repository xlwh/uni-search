# AntSpec Extension Hooks - spec-plan

本目录用于存放 spec-plan 技能的扩展点文件。

## 可用扩展点

| 扩展点 | 触发时机 | 典型场景 |
|--------|----------|----------|
| `on_design_review` | Spec 初稿生成后，授权检查前 | 注入额外的设计审查维度 |

## 使用方法

1. 将本文件复制并重命名为 `on_design_review.md`
2. 修改 YAML Frontmatter 中的字段
3. 在「扩展指令」章节中编写你的自定义逻辑

## 文件格式

```yaml
---
name: on_design_review         # 扩展点名称
description: 你的扩展用途说明    # 用途简述
enabled: true                   # 启用开关，设为 false 可临时禁用
---
```

正文使用 Markdown 格式编写扩展指令，AI 在执行到对应扩展点时会读取并执行。

## 示例：性能评估审查

以下是一个完整的扩展文件示例。将此内容保存为 `on_design_review.md` 即可生效：

```markdown
---
name: on_design_review
description: 在 Spec 审查阶段增加性能评估
enabled: true
---

## 扩展指令

在 Spec 生成后，额外执行以下审查动作：

1. 检查 Spec 中每个新增 API 的预期 QPS 和响应时间
2. 评估数据库查询是否需要新增索引
3. 如涉及列表类 API，确认是否有分页设计
4. 将性能评估结论追加到 Spec 的「非功能需求」章节
```
