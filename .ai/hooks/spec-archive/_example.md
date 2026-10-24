# AntSpec Extension Hooks - spec-archive

本目录用于存放 spec-archive 技能的扩展点文件。

## 可用扩展点

| 扩展点 | 触发时机 | 典型场景 |
|--------|----------|----------|
| `on_pre_archive` | 文档同步完成后，执行归档前 | 生成变更日志、通知相关方 |

## 使用方法

1. 将本文件复制并重命名为 `on_pre_archive.md`
2. 修改 YAML Frontmatter 中的字段
3. 在「扩展指令」章节中编写你的自定义逻辑

## 文件格式

```yaml
---
name: on_pre_archive            # 扩展点名称
description: 你的扩展用途说明    # 用途简述
enabled: true                    # 启用开关，设为 false 可临时禁用
---
```

正文使用 Markdown 格式编写扩展指令，AI 在执行到对应扩展点时会读取并执行。

## 示例：生成变更日志

以下是一个完整的扩展文件示例。将此内容保存为 `on_pre_archive.md` 即可生效：

```markdown
---
name: on_pre_archive
description: 归档前自动生成变更日志
enabled: true
---

## 扩展指令

在文档同步完成后、归档前，执行以下动作：

1. 基于 Proposal 和 Spec 生成变更摘要
2. 将摘要追加到项目根目录的 `CHANGELOG.md` 文件
3. 格式遵循 Keep a Changelog 规范
4. 向用户展示生成的变更日志条目，确认后继续归档
```
