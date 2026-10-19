# runebot 说话风格与纠正闭环：nanobot 借鉴与实现计划

- 文档日期：2026-10-19。
- 触发问题：用户反馈"大大熊"说话风格变差，纠正后不能持续生效。
- 证据来源：
  - 曼谷 runebot 只读排查（`sessions.db` / `memory.db` / `kubectl`，2026-10-19）。
  - nanobot 源码 `~/apps/nanobot`，HEAD `d6d52631`，比线上镜像 `7cede64f` 只多 1 个提交。
  - nanobot 线上数据 `~/nanobot-data/wq`。
- 范围约定：
  - 本计划第 1–4 阶段只改本地代码。
  - 配置、数据修复、部署放在第 5 阶段。其中"切回 M3"不依赖迁移，宜尽早执行；其余项等迁移到国内环境后执行，迁移后直连端点。

---

## 0. 结论先行

1. nanobot 能纠正，原因不是它的主代理有写记忆权限。nanobot 的主代理同样不能写记忆：模板规定只有 Dream 能改画像与长期记忆（`nanobot/templates/agent/identity.md:18`）。线上 6 个月里主代理只调用过 2 次 `edit_file`。
2. nanobot 靠三件事守住纠正：
   - 每轮上下文小（用户轮 14K–23K token）。
   - 全部记忆每轮重建进 system prompt。
   - 整理链路"纠正优先、只做小改动"。
3. runebot 在这三件事上都反着：
   - 每轮 225K–233K token。
   - 记忆只在进程内首条消息注入一次。
   - dream 整条覆盖，已经丢掉了风格细则和童话规则。
4. 计划分 5 个阶段：
   - 第 0 阶段：回放评测（量尺）。
   - 第 1 阶段：上下文治理。
   - 第 2 阶段：记忆常驻与运行时上下文。
   - 第 3 阶段：纠正闭环与 dream 防覆盖。
   - 第 4 阶段：可观测性与日志。
   - 第 5 阶段：运维与迁移（切 M3 先行，其余等迁移）。
5. 模型先切回 M3 观察（2026-10-19 用户决策），不依赖迁移即可执行。上下文压缩改为兜底策略：压缩线定为模型窗口的 50%（1M 窗口下约 500K），保留长上下文能力；如果 M3 下违规仍明显，再收紧窗口。

---

## 1. nanobot 机制拆解（源码 + 线上证据）

### 1.1 每轮重建 system prompt，记忆全量常驻

- `ContextBuilder.build_system_prompt` 每轮依次拼接以下各段（`nanobot/agent/context.py:101-152`），记忆总在最新状态：
  1. identity；
  2. `AGENTS.md`、`SOUL.md`、`USER.md`；
  3. `MEMORY.md` 全文；
  4. 技能；
  5. `[Archived Context Summary]`。
- 线上 `MEMORY.md` 是 41KB，每轮都发送。用户轮平均输入 14K–23K token（`llm_usage.sqlite3`，09-12 到 09-30）。
- 场景：dream 在 23:10 改了 `reply-style`。nanobot 在下一轮就读到新规则。runebot 只在"进程首条消息或 dirty 后首条"注入（`internal/memory/memory.go:76-96`）。注入块还跟着那条用户消息留在历史中段，离当前轮越来越远。

### 1.2 空闲压缩：会话结束即归档成"带标签的事实"

- 默认空闲 15 分钟后，Consolidator 把整段会话归档（`nanobot/config/schema.py:147-152`，`nanobot/agent/memory.py:1161-1236`）。
- 归档提示词 `nanobot/templates/agent/consolidator_archive.md` 的要求：
  - 每行一条事实，带标签 `[correction]` / `[permanent]` / `[durable]` / `[ephemeral]`；
  - "空间不够时优先保留用户的纠正与偏好"；
  - "以最新纠正为当前版本"。
- 归档结果有两个去向：
  - 回到下一轮 system prompt 的 `[Archived Context Summary]`；
  - 追加进 `memory/history.jsonl`，作为 Dream 的输入。
- 线上实例：`history.jsonl` 2026-09-25 01:39 的条目含 `[correction] 故事创作：①不要直接抄旧档里以前讲过的故事 ②讲完新故事才能继续对话`。

### 1.3 Dream：读压缩事实、用工具做小改动、git 留痕

- Dream 的输入是 `history.jsonl` 里未消化的条目，每批 20 条，每条截断到 1000 字（`nanobot/agent/memory.py:541-561`）。输入不是原始聊天行。
- 写入方式：Dream 是一个带受限文件工具的代理（`read_file` / `edit_file` / `apply_patch`，`nanobot/agent/memory.py:573-614`）。它可写的文件只有 `SOUL.md`、`USER.md`、`MEMORY.md`。
- 路由：行为规则写 `SOUL.md`；沟通风格写 `USER.md`；事实写 `MEMORY.md`（`nanobot/templates/agent/dream.md:3-12`）。`[correction]` 的处理是"原地替换旧的冲突事实"。
- 留痕：每次运行后按真实文件差异 git 提交（`nanobot/cli/gateway_runtime.py:572-618`）。线上有 34 次 dream 提交。
- 模型：`dream-deep` 预设，即 `MiniMax-M3` + `reasoningEffort: max`。
- 线上实例：提交 `0971ef1` 的改动是 `+1 -1`。它把第 16 条规则改写成"2026-09-30……校准·再次降档"，原因、日期与新口径都写在同一条里，其他规则不动。

### 1.4 效果证据

- 2026-09-25 03:00 她纠正"不要单发在呢"。之后 nanobot 的回复里再也没有出现过"在呢"（迁移历史全量检索，0 条命中）。
- 09-25 到 09-30 nanobot 用的是 `deepseek-v4.1-flash`，上下文约 21K，规则照样守住。这说明 Flash 级模型在"小上下文 + 规则常驻"下也能守规则。所以"模型是唯一原因"不成立。

---

## 2. runebot 现状差距

| 维度 | nanobot | runebot 现状 | 证据 |
|---|---|---|---|
| 每轮上下文 | 14K–23K | 225K–233K | `messages.usage_json`；`internal/config/config.go:75` 把 `minimax-` 定为 1M 窗口，压缩线 W−33K 永不触发 |
| 记忆位置 | system，每轮重建 | 进程首条 user 消息，一次性 | `internal/memory/memory.go:76-96` |
| 会话首部 | 无固定首部 | 前 3 条真实用户消息永久钉住，即 2026-03-22 的旧消息 | `internal/compact/segment.go:7` |
| 摘要口径 | 纠正优先、带标签 | 编码代理口径（文件、报错、待办） | `internal/compact/prompt.go:8-32` |
| 摘要超窗 | 分块归档 | 从头部砍掉一半事件，最多两次，被砍事件静默丢失 | `internal/compact/summary.go:108-133` |
| 整理输入 | 压缩事实 + 时间戳 | 原始行、无时间戳 | `internal/memory/render.go:20-47` |
| 整理写法 | 小改动，git 留痕 | `upsert` 整条替换 | `internal/memory/sqlite.go:269-283`；`memory_log` id 33/41/43 |
| 类型辞典 | 规则与事实分文件 | `bond` 强制六段事件结构，规则被写成事件 | `internal/memory/profile.go:105-111` |
| 时间感知 | 历史条目带时间 | 不给模型发消息时间 | `internal/llm` 无时间字段 |

---

## 3. 实现计划

所有阶段的统一约束：

- 先写失败测试，再实现。
- 每个阶段可以独立合入、独立回滚。
- 每个阶段结束运行 `go build . ./internal/... ./cmd/...` 和 `go test . ./internal/...`。

### 第 0 阶段：回放评测（先建量尺）

目标：把"风格差是模型还是上下文造成的"变成可复现的数字。

1. 新建 `cmd/replay`，结构参照 `cmd/e2e`。它是真端点驱动，不进 `go test`。功能：
   - 读一个本地 `sessions.db` 副本；
   - 给定会话 id 与一组"纠正点"行 id；
   - 按指定 `model` × `window` 重建发送视图，生成该点之后的 N 轮回复。
2. 新建 `internal/eval/style`（纯函数，进 `go test`），提供四个检查器，每个都配表驱动单测：
   - `EndsWithQuestion`：以问句结尾的回复占比。
   - `LatinMix`：中文回复里夹英文单词。白名单：`Alfred`、`bigbigbear`、`switch` 等专名。
   - `RepeatPhrase`：与前 3 条 assistant 回复共享长度 ≥ 12 字的片段。
   - `RoleKept`：用户指定的角色名（如"小小狗"）在故事回复中出现，且未被替换。
3. 纠正点清单（取自 10-15 / 10-18 实际对话）：
   - 23:24 "不要一直质问"；
   - 23:32 "不要夹杂英文"；
   - 23:34 "不要总重复下次她开口"；
   - 12:55 "为什么总是在问问题"；
   - 12:57 / 13:01 角色替换。
4. 验收：输出 2×2 表格，即 (M3 / Flash) × (原始 230K 视图 / 压缩后约 70K 的视图)，给出每格的四项违规率。用途是两个：上线前预判 M3 的收益；如果 M3 不达标，量化压缩视图的收益。

警告：回放数据是用户的真实私人对话。数据库副本与回放输出应放在 `~/.rune/replay/`，不应进入仓库。

### 第 1 阶段：上下文治理（预期影响最大）

1. **压缩线定为窗口的 50%（bot 专用，用户决策 2026-10-19）。** `compact.Config` 新增 `CompactFrac float64`：
   - 默认 0（关闭），行为与现行一致；bot 传 `0.5`。
   - 生效时 `CompactLine = min(W−33K, ⌊W×CompactFrac⌋)`；提醒线与阻断线不变。
   - bot 不声明小窗口，直接用模型真实窗口。M3 / Flash 均按 1M 解析，压缩线落在约 500K。
   - 场景：模型窗口不是瓶颈，占用率才是。经验口径是占用 <50% 时指令遵循可靠；50% 线把峰值占用压进安全区，同时保留长上下文。对现状无副作用：当前会话 230K 低于压缩线，行为完全不变。
   - 副作用说明：frac 生效时压缩线（500K）先于提醒线（944K）触发，提醒不再出现，属预期。
   - 测试：`compactor` 表驱动——frac=0 走旧线；W=1M frac=0.5 → 压缩线 500K；W=80K frac=0.5 → 压缩线 40K，且 提醒(24K) < 压缩(40K) < 阻断(60K) 次序成立。
2. **首部钉住可配置。** `compact.Config` 新增 `HeadMessages int`；零值保持现行 3 条，bot 传 `-1` 表示不钉。
   - 场景：bot 会话的前 3 条消息是半年前的寒暄，钉住它们没有价值。记忆改由第 2 阶段放进 system，不再依赖首部常驻。
   - 测试：`segment_test.go` 新增"无首部"用例，断言中段从第一条非 system 消息开始。
3. **陪伴版摘要提示词。** `compact.Config` 新增 `Prompt string`；空值用现行编码口径；bot 传陪伴版。陪伴版借鉴 `consolidator_archive.md`：
   - 输出 `- [correction|permanent|ephemeral] 事实` 逐行格式；
   - 本会话里用户的纠正逐条保留原话与日期，放在最前；
   - 同一主题以最新纠正为准；
   - 保留"正在聊的话题、未完的事、她当前情绪"作为 `[ephemeral]`。
   - `parseSummary` 仍取 `SUMMARY:` 段，契约不变。
   - 测试：断言 bot 配置下 `systemPrompt` 含纠正优先规则；编码配置下输出与现行一致。
4. **分块滚动摘要，替代砍半。** `renderInput` 超限时不再丢弃头部事件，改为按时间顺序切成块，逐块滚动更新摘要（上一块的输出作为下一块的 EXISTING SUMMARY）。
   - 场景：迁移会话的中段约 220K token，按现行逻辑会静默丢掉最早的 3/4。
   - 测试：用 fake LLM 构造超窗中段，断言 LLM 调用次数 = 块数、每个事件恰好进入一个块、最终摘要来自最后一块。
   - 风险：首次压缩耗时随块数线性增长，所以宜配合第 5 条在空闲时执行。
5. **空闲压缩（P1，借鉴 nanobot idle 15 min）。** runner `Manager` 新增定时检查：如果槽位空闲 ≥ 15 分钟、无进行中轮次、估算用量 ≥ 提醒线，就后台执行 `Compact(reason="idle")`。执行要求：
   - 持槽位锁；
   - 用户新消息到达时取消，与现有 `Interrupt` 同路径；
   - 失败计入现有熔断。
   - 测试：`runner_test.go` 用 fake 时钟断言触发条件，以及"新消息到达即取消"。

### 第 2 阶段：记忆常驻与运行时上下文

1. **bot 记忆进 system，每轮按版本重建。**
   - `memory.Memory` 新增 `Render(ctx) (text string, version int64)`，`version` 取本空间 `memory_log` 的最大 id。
   - `cmd/runebot` 的 TurnRunner 每轮比较 version，变了就用 `prompt.BuildBot(env, memText)` 重建 system；bot 不再挂 `InjectOnce`。TUI 保持现状。
   - 场景：dream 改完规则，下一轮就生效，而且位置固定在最前，不随历史漂移。
   - 前缀缓存：system 只在 dream 写入后变化，一天数次，可以接受。
   - 测试：
     - `prompt_test.go` 断言 BuildBot 含记忆段；
     - 构造 fake memory，断言 version 变化后下一轮 system 含新正文、不变时 system 字节相同。
2. **请求级临时上下文（不落库、不进历史）。** 在 `agent.Agent` 新增 `Decorate func(ctx, []Message) []Message`：
   - 只作用于发给 LLM 的请求副本，`Run` 返回的 history 不含它。
   - 这一层与 nanobot 的 `RuntimeContextBlock` 对齐（`nanobot/runtime_context.py:120-145`）。
   - runebot 用它在当前用户消息末尾追加 `<runtime-context>` 块，内容为：
     - 当前时间（`Asia/Shanghai`）；
     - 距上一条用户消息的间隔，例如"距上一条消息 3 天"。
   - 场景：10-18 隔了 3 天它还问"洗完了吗？"。
   - 测试：
     - agent 单测断言请求副本含块，返回的 history 与落库行都不含；
     - 间隔格式化函数用表驱动测试。
3. **修正 SystemGuide 的承诺口径。**
   - companion 分支增加一句："被纠正时本次对话立即照改；长期记忆由后台整理，不说'我已经记下了'这类没有发生的写入。"
   - 测试：断言 companion 的 `SystemGuide` 含该句。

### 第 3 阶段：纠正闭环与 dream 防覆盖

1. **收缩守卫（ADD-only 方向，对齐 ADR-2026-001 的"失效不删"）。**
   - 规则：`validateOps` 对已存在的 AlwaysInject 条目做 `upsert` 检查。如果新正文字节数 < 旧正文的 80%，就拒收，并打 `slog.Warn`。
   - 例外：确实要删规则时，应走 `supersede` 并写 `reason`。
   - 测试：用 `memory_log` id 33 与 id 43 的真实新旧正文做用例，断言两者都被拒；正常补充（变长）被接受。
2. **规则增量写法。**
   - 修改 companion 的 expression/boundary `Body` 口径：规则清单，每条带日期；新纠正追加为"YYYY-MM-DD 校准：……"；与旧条冲突时只改写该条，并保留原日期与改写原因。
   - `dreamSystem` 增加两句：
     - "upsert 已有条目时，正文应保留旧正文中仍有效的全部规则"；
     - "用户对说话方式的纠正，一律写入 expression 或 boundary"。
   - 测试：断言 companion 的 dreamSystem 含这两句。
3. **bond 只放事件。** bond 的 `Save` 增加："规则、偏好、讲故事的要求不进 bond，归 expression/boundary"。
   - 迁移遗留的 `bond-stories` 本质是规则集，由第 5 阶段的数据修复拆分。
4. **dream 输入带时间戳。** `renderRows` 每行前缀 `[YYYY-MM-DD HH:MM]`，取 `Message.CreatedAt`。
   - 场景：提取器需要判断"哪次纠正更新"，并写出正确的绝对日期。
   - 测试：断言渲染结果含时间前缀；`CreatedAt == 0` 时不输出前缀。
5. **纠正优先于预判门。** 预判门（`preflight`）的提示加一句："用户对助手说话方式的纠正一律值得沉淀"。
   - 测试：断言提示含该句。
6. **空闲触发 dream（P1）。** 新增自动门规则：如果最近活跃会话已空闲 ≥ 15 分钟且有欠账，冷却从 120 分钟降到 30 分钟；`MinRows` 对此路径不生效。
   - 场景：她晚上纠正完去洗澡，15–30 分钟后规则就进了 system，而不是等到 2 小时以后。
   - 测试：`dream_gate_test.go` 增加"空闲 + 冷却内"放行、"活跃 + 冷却内"拦截两例。

### 第 4 阶段：可观测性与日志

现状：本次排查几乎全部依赖事后翻 `sessions.db`。runebot 服务端日志覆盖很不全——`internal/llm`、`internal/agent`、`internal/runner`、`internal/memory` 的读路径（`memory.go`、`inject.go`）一条日志都没有；启动时只有一条 `runebot listening`（`cmd/runebot/main.go:340`），只带 addr 和 model。

1. 关键路径日志补齐。规范：

   - 沿用现有风格：`slog` + `"组件: 事件"` 消息 + key=value 字段（如 `memory: ticker dream done`）。
   - 只打 id、尺寸、计数、耗时、开关状态；**不打**消息正文、记忆正文、工具参数、凭据相关字段（与写入侧 `LooksLikeSecret` 闸同一隐私口径）。
   - 新增 `RUNE_LOG_LEVEL`（默认 info，支持 debug）；k3s 部署走 stdout 采集。
   - 每处打点配单测的，断言字段存在与语义即可，不断言字符串格式。

   打点清单：

| 位置 | 事件 | 字段 | 解决的排查盲区 |
|---|---|---|---|
| `cmd/runebot/main.go` 启动 | `runebot config` | model、reasoning_effort、max_completion_tokens、context_window 及其来源（env/模型家族/回落/端点钳制）、memory_profile、memory_db、soul 路径+字节数+mtime、已注册工具名列表 | 本次只能靠 `kubectl get` + 数据库反推生效配置 |
| `internal/llm` 每次请求完成 | `llm: response` | model、prompt_tokens、completion_tokens、cache_hit、thinking_len、finish_reason、duration_ms、attempts | Flash 61/61 条无推理、230K 输入，全部是翻库才发现 |
| `internal/llm` 出错/重试 | `llm: retry` / `llm: error` | status、attempt、backoff_ms | 端点抖动与重试行为完全不可见 |
| `internal/memory/inject.go` | `memory: injected` | session、space、index_bytes、body_bytes、version、原因（首次/dirty） | 无法确认某一轮的上下文里是否真带了纠正规则 |
| `internal/memory/dream.go` | 每条 op 应用成功 | op kind、name、space、old_bytes→new_bytes | id 33/41/43 整条替换丢规则，只能事后翻 `memory_log` |
| `internal/memory/dream.go` | `dream: preflight` | verdict、reason、sessions、rows | 预判门拒了哪些会话不可见 |
| `internal/compact/maintain.go` | 扩展现有 `context compacted` | 追加 head/mid/tail 各自 token、摘要块数、duration_ms | 已有触发日志，缺分段构成与分块耗时 |
| 第 3 阶段收缩守卫 | `memory: upsert rejected` | name、space、old_bytes、new_bytes、ratio | 新功能自带，防覆盖规则是否误拦要有据可查 |
| `internal/runner` 空闲压缩/空闲 dream | `runner: idle compact` / `memory: idle dream` | session、idle_minutes、用量估算 | 第 1.5 / 3.6 条新定时器的触发要有迹可查 |

已有（保持不动）：`memory: ticker/exit dream skipped|done` 的门控原因日志、`cron:` 系列、`context offloaded` / `context compacted` / `context window clamped`。

2. 推理缺失告警：设置了 `ReasoningEffort`，但连续 20 条回复 `Thinking` 为空时，打一次 `slog.Warn`，带上模型名。
   - 场景：Flash 61/61 条无推理，这次是人工翻库才发现。
   - 测试：计数器单测。
3. 每轮 `turn_end` 控制行追加 `prompt_tokens` 与 `system_bytes`（字段向后兼容，读侧忽略未知字段）。下次排查不必再从 `usage_json` 反推。
   - 测试：slot 单测断言字段存在。

### 第 5 阶段：运维与迁移

1. **先切回 M3（不依赖迁移，宜尽早执行）。** 删掉 deployment 里的 `OPENAI_MODEL` 覆盖，回落到 secret 的 `MiniMax-M3`；dream 跟随主模型一并切回。另设 `RUNE_MEMORY_MODEL=MiniMax-M3` 显式固定 dream 模型。
   - 观察期后如果违规仍明显，再收紧压缩策略（如固定窗口 128K，压缩线 95K）。
   - 注意：切换后核对一次生效配置——历史 nanobot M3 配置是 `reasoningEffort=medium`、`maxTokens=131072`，当前 runebot env 是 `RUNE_REASONING_EFFORT=high`、`RUNE_MAX_COMPLETION_TOKENS=8192`，宜用 nanobot 口径。
2. 迁移数据前，先在旧实例停写，并对 `sessions.db` 与 `memory.db` 执行 `PRAGMA wal_checkpoint(TRUNCATE)`，再拷贝。

   警告：当前 `sessions.db-wal` 有 21MB 未合并（2026-10-19），只拷 `.db` 会丢掉 10-15 以后的全部对话与记忆变更。
3. 修复记忆：
   - 用 `Revert` 把 `bond-stories` 回到 log id 30 的版本，再把规则部分拆进 boundary（`story-rules`），把 10-18 叫停事件单独存成 bond；
   - 把 id 33 / id 41 的 `old_body` 里丢失的细则合并回 `reply-style` 与 `empathy-style`。
4. 压缩线 50% 下，现有 230K 会话低于 500K 阈值，不会触发压缩；等会话增长到约 500K 时第 1 阶段第 4 条的分块摘要首次运行，首次耗时未经实测，宜在空闲时段观察。
5. 直连后，`OPENAI_BASE_URL` 不变（`api.minimaxi.com`）。重新测一次首 token 延迟与 `turn_end.duration_ms`，作为迁移前后对照。

---

## 4. 交付顺序、风险与回滚

| 顺序 | 内容 | 独立验收 | 回滚 |
|---|---|---|---|
| 1 | 第 0 阶段 | 2×2 违规率表 | 删除 `cmd/replay`，不影响线上 |
| 2 | 第 1 阶段第 1–4 条 | 单测：压缩线 = W×50%；回放：压缩后视图 ≈70K 违规率 | bot 去掉 `CompactFrac` 即恢复；提示词和首部是配置项，零值等于旧行为 |
| 3 | 第 2 阶段 | 单测 + 回放：违规率对比第 1 阶段 | bot 恢复挂 `InjectOnce` |
| 4 | 第 3 阶段第 1–5 条 | 单测（含真实 id 33/43 用例） | 守卫是纯校验，删除即恢复 |
| 5 | 第 1 阶段第 5 条、第 3 阶段第 6 条、第 4 阶段 | 单测 | 定时器开关 |
| 6 | 第 5 阶段 | 迁移后线上抽查 + 回放 | 保留旧实例与 checkpoint 后的数据副本 |

风险：

- 记忆进 system，修改了 `internal/prompt/prompt.go:1-4` 的约定"可变内容不进 system"，只对 bot 生效。宜在 `AGENTS.md` 记录这个例外与理由。
- 收缩守卫的 80% 阈值是经验值。如果正常精简也被拒，dream 会停在旧正文上。日志可以观测；必要时改成"行级包含检查"。
- 分块摘要的首次耗时与质量未经实测，应先用 `cmd/replay` 对迁移会话离线跑一次。

[待补：15 分钟空闲阈值的最终取值，以观察结果为准。压缩策略已定：压缩线 = 模型窗口 50%（M3/Flash 1M 窗口下约 500K）。如果切回 M3 后违规率仍高，下一档是固定窗口 128K（压缩线 95K），届时用第 0 阶段回放对比定档。]
