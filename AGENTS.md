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

### 记忆层（internal/memory，设计见 docs/runeharness-memory-plan.html 与
### docs/runeharness-memory-tradeoffs.html）

- 独立库 `~/.rune/memory.db`：`memories` 当前视图 + `memory_log` 变更
  流水（upsert/supersede/delete/merge-into 全留痕）+ `memory_meta`
  （游标/锁/熔断）。tenant+space 从 ctx scope 与画像推导，接口无租户参数。
- 画像（Profile）决定空间维度/类型辞典/写工具/dream 节奏：
  `RUNE_MEMORY_PROFILE=coding|companion|generic`（coding 冷却 60min、
  companion 120min，退出收尾与 12min 心跳兜底欠账）。
- 读路径：UserPromptSubmit hook（`InjectOnce`）把索引+常驻正文注入首条
  user 消息（Fold 首部常驻）；写路径：`memory` 工具（safe 名单内）
  + dream 消化（手动 /dream、定时、退出收尾三触发器）。
- 凭据两道闸：提示词 DoNot + `LooksLikeSecret` 代码层扫描，写入一律过。
- 相关 env：RUNE_MEMORY（总开关）、RUNE_MEMORY_DB、RUNE_MEMORY_PROFILE、
  RUNE_MEMORY_DREAM、RUNE_MEMORY_MODEL（dream 提取可换轻量模型）。

### 后台任务（internal/bgtask，对齐 learn.shareai.run s13 / CC LocalShellTask）

- `run_command` 的 `run_in_background=true` 走 `Manager.Spawn`：进程组独立
  （Setpgid，pgid==pid），stdout/stderr 合并写
  `~/.rune/tasks/<sessionID>/<bgN>.output`，工具结果立即返回任务 id 与
  输出路径——模型用 `read_file` 读输出（CC 已弃 TaskOutputTool 改此口径）。
- 任务生存期只受 timeout（默认/上限 30min）、`task_kill`、进程退出约束；
  调用方取消经 `context.WithoutCancel` 剥离，用户 Esc 杀不掉后台任务。
  `cmd.Cancel` 级联 `SIGKILL` 整个进程组，不留孤儿；退出前 `Shutdown` 清场。
- 完成/停滞事件进内存队列 `pending`：`Drain` 挂 PreChat hook 在运行中
  注入 `<task_notification>` user 消息（KindInject）；TUI 空闲时收
  `BgTaskMsg` 或正常收尾由 `maybeDrainBg` 注入并续跑一轮（中断/撞墙/
  调用失败等错误收尾不自动续跑，通知留队列等下一轮）——CC command-queue
  语义落到本项目的 hook + TUI 两通道。`Drain` 只在被消费时清空，通知必达；
  TUI 注入落库失败经 `Requeue` 放回队首。bg 注入轮不走 UserPromptSubmit
  （旁路信息不是新用户轮，不重置 todo 轮次、不触发记忆"首条"注入）。
- `task_kill` 置 `notified` 抑制完成通知（kill 结果已由工具结果告知）；
  停滞看门狗（5s 查输出文件、45s 不增长且尾行像交互提示）发无 `<status>`
  的一次性提醒。队列是进程全局：跨 /resume 会话切换后通知仍送达当前会话。
- `task_list`/`task_kill` 在 safe 名单（blast radius 限于本进程 spawn 的
  任务）；`run_in_background` 与前台命令走同一 permission 三道闸；
  TUI `/tasks` 列任务。session id 拼输出路径前过 [A-Za-z0-9_-] 白名单。
- `cmd/e2e` 未装配 bg：`run_in_background` 在那里明确报错，按设计。
- `permission.Config` 按值拷进 `Desk`：**全部 `SafeTools` append 必须发生在
  `permission.New` 之前**，之后追加的名字对 desk 不可见（切片扩容重分配），
  工具会被误判 Ask 每次弹确认。

### 定时调度（internal/cron，对齐 learn.shareai.run s14 / CC ScheduledTasks）

- 调度与执行解耦：`Scheduler` 只把到点任务进 pending 队列（1s tick，
  cron 粒度是分钟）；交付在 TUI 侧——`OnFire`→`CronMsg`→空闲时
  `drainQueued`→`maybeDrainCron` 注入 `<scheduled_task>` user 消息
  （KindInject）跑一轮；忙碌时积压等 `turnDone`。错误收尾不自动续跑，
  与 bg 通知同规则。cron 交付走 UserPromptSubmit 钩子链（新用户轮语义）；
  拦截或落库失败经 `Requeue` 归还事件，prompt 文本进块前转义防破块。
- 表达式是标准五段式 `M H DoM Mon DoW`（`*`、`*/N`、`N`、`N-M`、`N-M/S`、
  列表；dow 7=周日；DOM/DOW 双受限走 OR）。每条任务带 `tz`（IANA，空=
  本地），`Schedule.Next` 在任务时区内求值，搜索界 8 年（覆盖跨世纪
  8 年闰周期间隔）；DST 空档用 stepTime 守卫防归一回退死循环。
- 存储两档：`durable` 落 `~/.rune/cron_tasks.json`（按 tenant+workspace
  过滤，tmp+rename 原子写，`cron_tasks.lock` flock 串行化跨进程读写）；
  `session` 只在进程内存。任务上限 MaxJobs=50。`Store.List` 顺手做
  mtime 变更检查（非属主的 /cron、cron_list 不拿陈旧镜像）；文件被删
  时镜像清空，已删任务不继续点火。
- 多实例双火防护：每 workspace 一把调度属主锁 `cron_sched_<hash>.lock`
  （flock 非阻塞抢锁，持锁者才触发 durable 任务；进程死内核自动放锁，
  非属主每 15s 重试接管）。session 任务进程私有不受锁约束。抢锁成功
  先 ReloadIfChanged 刷新镜像再清扫——非属主期间的文件变更不漏判。
- 一次性任务触发即焚（焚毁写失败则冻结排程——已交付不重复点火，
  文件残留由下次接管清扫兜底）；周期任务 `LastFired` 落盘作锚，重启/
  停机后首次 check 就地追赶一次（不补跑积压）。停机期间错过的一次性
  任务在抢锁成功时汇总成一条 missed 通知（模型先问用户再执行），随即删除。
- 模型工具：`cron_create`/`cron_list`/`cron_delete`（safe 名单内）；
  TUI `/cron` 列任务。总开关 `RUNE_CRON`（默认开）。

### 输出约束

在解释、步骤、文档、报告时，应用 ASD-STE100的约束语法来规范输出，至少达到ASD-STE100 80% 的程度。
