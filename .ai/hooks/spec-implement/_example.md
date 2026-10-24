# AntSpec Extension Hooks - spec-implement

本目录用于存放 spec-implement 技能的扩展点文件。

## 可用扩展点

| 扩展点 | 触发时机 | 典型场景 |
|--------|----------|----------|
| `on_code_review` | 编码完成、测试通过后，授权检查前 | 推送分支、触发 CI/CD |

## 使用方法

1. 将本文件复制并重命名为 `on_code_review.md`
2. 修改 YAML Frontmatter 中的字段
3. 在「扩展指令」章节中编写你的自定义逻辑

## 文件格式

```yaml
---
name: on_code_review            # 扩展点名称
description: 你的扩展用途说明    # 用途简述
enabled: true                    # 启用开关，设为 false 可临时禁用
---
```

正文使用 Markdown 格式编写扩展指令，AI 在执行到对应扩展点时会读取并执行。

## 示例：代码推送与 CI 触发

以下是一个完整的扩展文件示例。将此内容保存为 `on_code_review.md` 即可生效：

```markdown
---
name: on_code_review
description: 测试通过后自动推送分支并触发 CI
enabled: true
---

## 扩展指令

在所有测试通过后，执行以下动作：

1. 基于当前任务名创建 Git 分支：`feature/{task_name}`
2. 提交所有变更，commit message 格式：`feat: {task_description}`
3. 推送分支到远端：`git push origin feature/{task_name}`
4. 向用户确认是否创建 Pull Request
```
