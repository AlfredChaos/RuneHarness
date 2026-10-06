通过 Golang 写 AI Agent

接入 OpenAI 兼容 API

## 构建与测试

`personal/` 是手工练习目录，本身不保证可编译。构建/测试请限定范围：

```
go build . ./internal/... ./cmd/...
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

### 上下文压缩（internal/compact，设计见 docs/runeharness-compaction-plan.html）

- 存储是 append-only 的消息行（含 kind 控制行），发送形态由
  `session.Fold` 组装：system + 首部（前 3 条真实用户消息）+ 最新摘要 +
  尾部（最近 40 个用户轮、≤40K token）。压缩只动中段。
- 三条线（token 估算，128K 窗口为例）：提醒线 W−56K 注入"可择机压缩"
  提醒（每周期一次）；强制压缩线 W−33K 触发卸载→摘要；阻断线 W−20K 拒发。
- 三个入口共用 `Compactor.Compact`：模型 `compact` 工具（loop 按名特判，
  不执行 Run）、TUI `/compact`（manual，熔断时仍可试探）、reactive
  （端点报 ErrContextLength 后压缩一次重试一次）。
- 卸载是写侧行为：工具结果回填时 >50K 落 blob 换 `blob://<ref>` 索引；
  压缩时中段结果全部落 blob（过期结果与大参数先行）。`read_file` 用
  `blob://` 读回，`blob://` 只能读当前会话及其祖先的 blob。
- 摘要为滚动增量（旧摘要 + 新中段事件），失败三类（调用错/缺 SUMMARY 段/
  压缩无效）计入 `session_state` 的熔断计数，连续 3 次熔断；日志在
  `~/.rune/rune.log`（TUI 占终端，不写 stderr）。
- 子代理共享同一压缩器：Subagent 模式把任务消息钉入首部、提醒换成
  "收尾"文案、normal 型 spawn 前对继承快照先卸载；子会话以 `kind='fork'`
  行记父会话水印，读侧回放 = 父视图 + 子行。
- `Recorder.Append` 返回行 id，`Message.ID/Kind/Usage` 与 requests 表
  （水印+视图哈希+失败 payload）支撑发送形态重放。
- 上下文窗口解析：显式 `RUNE_CONTEXT_TOKENS` > 模型家族表（`internal/config`
  最长前缀匹配）> 回落 200K；端点报文里的窗口数字会被动钳小并持久化。
- `cmd/e2e` 是压缩管线的端到端驱动（真端点，需 `.env`）：
  `go run ./cmd/e2e -w 80000 -keep`——小窗口下几轮即可触发提醒/卸载/摘要，
  结束打印控制行、blob、requests 重放校验。不进 `go test`，仅手工运行。

### 输出约束

在解释、步骤、文档、报告时，应用 ASD-STE100的约束语法来规范输出，至少达到ASD-STE100 80% 的程度。
