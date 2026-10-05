通过 Golang 写 AI Agent

接入 OpenAI 兼容 API

## 构建与测试

`personal/` 是手工练习目录，本身不保证可编译。构建/测试请限定范围：

```
go build . ./internal/...
go test . ./internal/...
```

## 架构约定

- 权限、nag 等横切逻辑目前以 hook（`internal/agent/hooks.go`）实现——
  这是当前惯例而非强制约束；agent loop 形态未定，新逻辑放循环体还是
  挂 hook 按简单直观原则取舍。
- 权限判定实现为 `internal/permission` 的 PreToolUse hook；todo nag
  实现为 `internal/todo` 的 PreChat hook，均由 main.go 装配。
- "/" 斜杠命令与建议下拉在 `internal/tui/suggest.go`：系统命令注册表
  （slashCommands）+ 技能（启动扫描的 `[]skill.Meta`，由 main.go 传入）
  + `/resume` 的会话建议；TUI 内 `/resume` 换绑 scope 续写同一会话，
    与 `--resume` 启动参数语义一致。
- "@" 文件提及在 `internal/tui/files.go`：光标处 @token 实时补全
  workspace 文件（清单走 `git ls-files -co --exclude-standard`，非仓库
  回退 WalkDir，TTL 缓存）；选中只补全不提交，目录项补 "@dir/" 下钻，
  含空格路径用 @"..." 引号形态。提交时 `expandMentions` 把全部 @path
  展开为 `<attachments><file>` 内容块附在消息后（路径可越 workspace，
  单文件 32KB / 总量 128KB 上限）；回放时 splitAttachments 剥离内容块。
- 文件访问边界：读工具（read_file/list_dir）不限 workspace，模型可自主
  读任意路径；写只能经 run_command，由 permission 三道闸把关（Gate1
  ForbiddenPaths 敏感文件硬拒，Gate3 未白名单命令弹确认）。

### 输出约束

在解释、步骤、文档、报告时，应用 ASD-STE100的约束语法来规范输出，至少达到ASD-STE100 80% 的程度。
