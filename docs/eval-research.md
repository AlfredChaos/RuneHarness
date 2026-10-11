# RuneHarness 评测研究（ALF-111）

> 事实基准日 **2026-10-11**：所有外部基准/指南页面均于当日访问核对；代码事实来自当日工作区，
> `file:line` 锚点为当日 read/grep 所得，后续改动会漂移。
> 时效声明：SWE-bench（论文 2023-10）、WebArena（论文 2023-07，数据集 v0.2.0 后冻结）、OSWorld（论文 2024-04）
> 的论文表述已部分过期，本文一律以 2026 年现行官网/仓库状态为准并逐项标注。
> 关联文档：`docs/flagship-gap-analysis.html`（产品差距 C1–C7 结论以彼处为准，本文只引用不重复）。
> 文中所有人日/适配成本均为**估算**，仅供相对排序。

## 0. 结论先行

1. **公开基准的计分判分器以 outcome 终态为准**：SWE-bench 看补丁过测试、Terminal-Bench 看独立验证容器对产物的校验、
   OSWorld 看虚拟机终态、WebArena 看最终 URL/答案/页面内容。唯一例外形态是 Terminal-Bench 对**成功轨迹**的
   LLM 判官审计——但那是反作弊（reward hacking 一票作废），不构成对记忆/压缩/审批的能力计分。
   **因此没有任何公开基准直接给 RuneHarness 的差异化能力打分**，要评这些只能自建 eval（§4）。
2. RuneHarness 的**账本已具备"代码/数据回放"能力**：append-only 消息行 + 控制行 + `LoadView(upto)` 确定性重放 +
   `ViewHash` 核对（§3.1）。注意其精确含义：**断言的是"历史某次请求的发送形态"，不重跑模型、不重放工具副作用**（§3.2）。
3. **"精确重放模型交互"目前不可行**：持久层缺原始 wire/流式分片、每请求响应信封（finish_reason）、工具 spec 快照、
   每请求模型/参数/种子与逐 LLM 调用时延（§3.2）。但**补丁式外部判分（任务套件/SWE-bench）不依赖录制层**——
   真模型跑真任务、按终态判分即可；录制仅作为 CI 确定性的可选组件（P1-4c）。
4. Anthropic 官方 agent-eval 指南（2026-01-09，§2.1）与本仓库现状高度互补：先 20–50 个来自真实失败的任务、
   确定性 outcome 判分（单元测试）为主、transcript 判分为辅、多 trial 统计——这是本文 P0/P1 的设计依据。
5. **推荐路径**：P0 账本/压缩/权限/记忆 eval（估算 **8–12 人日**）→ P1 自建编码任务套件（首选）或
   SWE-bench Lite 适配器（估算 **5–10 人日**）→ P2 在线评测：canary/shadow + 配对回归检测 + 隐私/同意门槛
   （估算 **6–9 人日**）。Terminal-Bench/OSWorld/WebArena 为**暂缓可选项**，不是 P2 的替代品：判终态、
   环境成本最高、对差异化能力覆盖最弱（§5 末尾含分阶段理由汇总）。

## 1. 行业基准：事实表（分数口径与适配成本分列）

| 基准 | 规模 | 任务形态 | 判分/统计 | 现行版本（2026-10-11） | 适配成本（估算） | 一手来源（当日访问） |
|---|---|---|---|---|---|---|
| SWE-bench Full | 2,294 实例，12 个 Python 仓库 | repo+base_commit+issue → diff 补丁 | Docker 容器内应用补丁跑测试集，FAIL_TO_PASS/PASS_TO_PASS 全过才 resolved；榜上 % Resolved | 论文 v1 2023-10-10（ICLR'24 oral，表述已旧）；repo 推送 2026-09-18 | **高（跑量）**：x86_64 + ~120GB Docker 农场或云跑，300–2294 例/轮 | [arXiv:2310.06770](https://arxiv.org/abs/2310.06770) · [官网](https://www.swebench.com/) · [仓库](https://github.com/SWE-bench/SWE-bench) |
| SWE-bench Verified | 500 实例，人工过滤（OpenAI，2024-08-13） | 同上 | 同上；官网当前默认视图为 Bash-only 统一 mini-SWE-agent 环境 | 同上 | **中**：同 Lite，判分只看补丁 | [OpenAI 公告](https://openai.com/index/introducing-swe-bench-verified/) · [官网](https://www.swebench.com/) |
| SWE-bench Lite | 300 实例（2024-03） | 同上，低成本子集 | 同上 | 同上 | **中**：补丁 JSONL + 现成 Docker 判分链（本地或 sb-cli/Modal）；与 RuneHarness 的 patch-only 交互天然契合 | [官网](https://www.swebench.com/) · [评测指南](https://github.com/SWE-bench/SWE-bench/blob/main/docs/guides/evaluation.md) |
| Terminal-Bench | 任务数官方页未列 | Harbor task format：instruction.md + task.toml + environment/ Dockerfile + solution/ oracle + tests/；开放联网 | agent 结束后**独立验证容器**只读 artifacts/镜像内文件/sidecar 跑 tests；榜上 accuracy ± 95% CI，≥5 trials/任务全覆盖，出错 trial 计 reward 0；另有维护者 LLM 判官审计成功轨迹的反作弊（见 §0.1） | **4.0，2026-08-28**（谱系 1.0 2025-05-19 → 4.0；4.0 平顶 8h 超时；社区提交暂关闭） | **高**：Harbor/容器编排 + 8h 超时预算 + ≥5 trials 重复跑 + 统计口径自建 | [基准谱系](https://www.tbench.ai/benchmarks) · [4.0 公告](https://www.tbench.ai/news/terminal-bench-4-0) · [仓库](https://github.com/harbor-framework/terminal-bench) |
| OSWorld | 369 任务（官方允许剔 8 个 GDrive 任务 → 361） | 真实 Ubuntu VM；任务 = 初始 setup 配置 + 执行式 eval 脚本 | 结束后 getters（取 VM 文件/命令输出/应用状态）+ metrics（比对规则）判成功；全站 134 个执行式 eval 函数；榜为维护者代跑 | v1 论文 2024-04-11（NeurIPS'24，表述已旧）；OSWorld-Verified 2025-07-28；[**OSWorld 2.0**](https://osworld-v2.xlang.ai/) 2026-06-26 上线（**2.0 判分细节未核实，本文不作断言**） | **极高**：VM/UI 全栈（VMware/Docker/KVM、云沙箱）+ 网络依赖（GDrive 任务） | [arXiv:2404.07972](https://arxiv.org/abs/2404.07972) · [v1 站点](https://os-world.github.io/) · [v2 站点](https://osworld-v2.xlang.ai/) · [仓库](https://github.com/xlang-ai/OSWorld) |
| WebArena | 812 任务 | 自托管 4 站点（电商/CMS/Reddit/GitLab）+ 地图/维基/主页 | 每任务 JSON `eval.eval_types` 路由判分器组合，得分连乘 ∈ {0,1}：`string_match`（答案 exact/must_include/LLM fuzzy）、`url_match`（终态 URL+query）、`program_html`（目标页 JS 取元素比对） | 论文 v1 2023-07-25（ICLR'24）；数据集 v0.2.0 自 2023-10-24 冻结；repo 推送 2025-11-26，官方推荐迁移 AgentLab/BrowserGym | **高**：多站点自托管 + 每轮环境重置 + 判分配置迁移 | [arXiv:2307.13854](https://arxiv.org/abs/2307.13854) · [站点](https://webarena.dev/) · [判分源码](https://github.com/web-arena-x/webarena/blob/main/evaluation_harness/evaluators.py) |

### 1.1 效度/时效注意（只列有出处的批评，来源均 2026-10-11 访问）

- 测试集取自原始 PR 合并态、可能被 golden patch 形态"背答案"——该批评出自第三方立场论文
  [arXiv:2606.17799](https://arxiv.org/html/2606.17799)（§4.2 转述），非 SWE-bench 官方结论；引用时保持归因。
- 官网把 Verified 默认视图改为统一 mini-SWE-agent 环境（[swebench.com](https://www.swebench.com/)），
  说明**脚手架差异足以扰动分数**：RuneHarness 跑出的数字不能与官方榜直接比。
- 检索到的其他"污染/数据质量"数字（含第三方 ICSE 论文的量化断言）未能在本次一手核对中确认原文，
  本文不引用具体数值、不下未经核实的结论。

### 1.2 Terminal-Bench 4.0 对自建 eval 的可借鉴点

- 语义化版本：agent-environment 变更 → 重跑（x）；verifier 变更 → 重判（y）；元数据 → 复用（z）
  （[4.0 公告](https://www.tbench.ai/news/terminal-bench-4-0)）。
- 统计口径：每任务 ≥5 trials、报 95% CI、错误 trial 不剔除而计 0 分；反作弊：LLM 判官审计成功轨迹，
  harness cheating 一票否决、reward hacking 逐条作废（[SUBMIT.md](https://github.com/harbor-framework/terminal-bench/blob/main/leaderboard/SUBMIT.md)）。

### 1.3 补充基准：Aider Polyglot 与 SWE-rebench（2026-10-11 访问）

**Aider Polyglot**：225 道 Exercism 练习、6 语言（C++/Go/Java/JS/Python/Rust），从 697 题中取"7 个当时模型
≤3 个解出"的最难档；官方 harness（[aider 仓库 benchmark/](https://github.com/Aider-AI/aider/blob/main/benchmark/README.md)，
题库 [Aider-AI/polyglot-benchmark](https://github.com/Aider-AI/polyglot-benchmark)）在 Docker 内逐题跑单元测试，
一题通过当且仅当全部测试通过；指标为 pass_rate_1 / pass_rate_2（第 1/2 次尝试全过占比，pass@1 即前者）与
percent_cases_well_formed（编辑格式可解析率）；选题材见[官方博客](https://aider.chat/2024/12/21/polyglot.html)。
局限：题集自 2024-12 公开静态托管（存在污染窗口）；分数耦合 aider 的 edit format，测的是"编辑可解析 + 单测全过"
的端到端，不是开放式 agent loop。对 RuneHarness 的适配成本**估算为低-中**（补丁/文件级判分、单容器），但需注意
其判分预设 edit-format 输出形态。

**SWE-rebench**：抗污染、持续更新的 SWE-bench 对照口径。自动管线持续采 GitHub"issue + 已合并 PR"（须含测试
改动），LLM 生成安装 recipe 并容器内验证；数据集 [nebius/SWE-rebench](https://huggingface.co/datasets/nebius/SWE-rebench)
在 [arXiv:2505.20411](https://arxiv.org/abs/2505.20411)（v2，2025-11-04）报告 21,336 题/3,468 仓库；**线上榜无固定
题数**——按 issue 创建时间窗滚动选题（[swe-rebench.com](https://swe-rebench.com) 当日默认窗显示 111 题/65 仓库，
评测数据按月更新），凡评测窗含"模型发布前 issue"即在榜上显式标 Potential contamination。判分：维护者统一代跑，
官方 SWE-bench harness 的 [fork](https://github.com/SWE-rebench/SWE-bench-fork) + 预构建镜像，resolved 判据同
SWE-bench 口径（fail-to-pass + pass-to-pass）；统计为固定 scaffold、统一 128K 上下文、每模型 5 次报 Resolved±SEM
与 pass@5。注意：论文 v2 数字系 2025-11 快照勿引作现值；语言覆盖以 Python 为主。适配成本**估算与 SWE-bench Lite
相当或略低**（现成镜像免去环境装配），换来的好处是滚动去污染与 5 次重复的统计口径可直接借鉴到 P1/P2。

## 2. 方法论与框架来源

### 2.1 Anthropic：Demystifying evals for AI agents（2026-01-09 发布，页面标注更新 2026-03-18）

来源：[anthropic.com/engineering/demystifying-evals-for-ai-agents](https://www.anthropic.com/engineering/demystifying-evals-for-ai-agents)。
当日核对到的要点（全部为原文所述）：

- **概念区分**：task（单条测试：输入+成功标准）/ trial（一次尝试，模型输出有随机性所以要多 trial）/
  grader（打分逻辑，可多条 assertion）/ transcript（=trace/trajectory，一次 trial 的完整记录）/ outcome
  （**环境终态**，不是模型嘴上说的"已完成"）/ eval harness / agent harness（脚手架与模型合起来被评）。
- **判分三层**：code-based（字符串/单元测试 fail-to-pass、pass-to-pass/静态分析/终态校验）、model-based（rubric、成对、
  多判官）、human（校准用）。**先保证 outcome 的确定性判分（编码 agent 首选单元测试），再补 transcript 判分**
  ——且警告"按产出评，不按路径评"，路径判分过于脆。
- **多 trial 指标**：pass@k（k 次内至少一次成功）与 pass^k（k 次全成功，一致性）；按产品语义选。
- **从零到一**：起步 **20–50 个来自真实失败的任务**即可；任务要无歧义、带参考解；正反例平衡；
  每 trial 独立干净环境（并举例：曾有 agent 从上一 trial 的 git 历史作弊）；capability 与 regression eval 分开，
  高通过率的 capability 任务"毕业"为回归套件。
- 对本计划的意义：P0 的账本断言属于 **transcript 层确定性**；P1 任务套件补 **outcome 层**（单元测试判分）；
  P2 在线评测对应其 regression/production monitoring 位置。

### 2.2 OpenAI Evals

[github.com/openai/evals](https://github.com/openai/evals)（2026-10-11 访问，推送 2026-04-14）。主指南
（[README](https://github.com/openai/evals/blob/main/README.md)、[build-eval.md](https://github.com/openai/evals/blob/main/docs/build-eval.md)、
[eval-templates.md](https://github.com/openai/evals/blob/main/docs/eval-templates.md)）核对到的机制：
数据是 **JSONL**（每样本一个 JSON 对象，`input` + 模板要求的键）；评测经 **registry YAML** 注册
（`evals/registry/evals/<name>.yaml`，elsuite 格式）；用 **`oaieval` CLI** 运行。基础模板为确定性字符串逻辑
（Match/Includes/FuzzyMatch/JsonMatch）；开放式输出走 ModelBasedClassify——补全包进评估提示、解析
choice_strings 映射 choice_scores（fact/closedqa/battle YAML）。官方明确建议：模型判分 eval 应配 **meta-eval**——
人工给样本标注"判官本应选的 choice"，跑 meta-eval 得 metascore（应接近 1.0）来校准判官质量，这是本文对
LLM 判官的采用前提（替代无出处的"循环性/解析脆弱"式断言）。另有：repo 目前不接受自定义代码 eval；
评测也可在 [Dashboard](https://platform.openai.com/docs/guides/evals) 配置。

### 2.3 LangSmith

[评估概念文档](https://docs.langchain.com/langsmith/evaluation-concepts)（2026-10-11 访问，页面无发布日期）：
不是固定基准而是平台——offline 跑 datasets/examples 产出 experiments，online 跑生产 runs/threads；
evaluator 返回 feedback `{key, score|value, comment}`；路线：人工标注队列、代码判分（确定性）、
LLM-as-judge（自带"需校准"警告）、成对比较。对 RuneHarness：P2 在线评测的**现成看板/上报面**（可选，不绑定）。

### 2.4 轨迹/会话回放通则

回放"存下来的轨迹数据" ≠ 回放"与模型的精确交互"。前者只需账本 + 确定性组装函数；后者需逐请求录制
请求/响应/参数/种子。RuneHarness 现状属于前者（§3.2），评测命名与文档必须区分两者。Anthropic 的 transcript
定义（§2.1）同样指"记录"而非"可复现"。

## 3. RuneHarness 现状审计（file:line）

### 3.1 会话账本：存储与可回放性（持久层）

- **存储**：单文件 SQLite，默认 `~/.rune/sessions.db`（`internal/config/config.go:142-150`；
  记忆库分文件 `~/.rune/memory.db`，同文件 `:152-158`）。WAL + 单写者连接，schema 版本走 `PRAGMA user_version`。
- **Schema**（`internal/session/sqlite/sqlite.go:79-145`）：v1 建 `sessions`（含**会话级 `model` 标签**）/
  `messages`（`:92-101`：role/content/**thinking**/**tool_calls/tool_call_id**/is_error/created_at）；v2 加 `kind`/
  `usage_json` 列与 `blobs`/`session_state`/`requests` 三表（`:117-145`）。
- **持久的是"规整后的消息"**：Append 把 assistant 响应的 `Content`/`ToolCalls`（含内部 `Thinking`）整行写入
  （`internal/session/sqlite/sqlite.go:257-304`）；tool 结果以 role=tool 行的 content 持久；tool_calls 参数原文随行持久。
  **例外**：被卸载/超限截断的工具输出不留在消息行，改 blob 指针，原文在 `blobs` 表（`LoadBlob` 沿 fork 链找祖先，
  `internal/session/sqlite/sqlite.go:455-486`）。
- **控制行**：`Kind` 区分普通消息与 4 种控制行 compact_boundary/view_clear/fork/turn_end，
  控制行不进发送形态（`internal/agent/message.go:28-43`）。
- **Fold**：按 id 升序把原始行组装成发送形态（`internal/session/view.go:43-47`）。
- **LoadView(upto)**：按水印重放历史任一请求的发送形态，子会话沿 fork 链继承
  （接口 `internal/session/session.go:57-61`；SQLite 实现 `internal/session/sqlite/sqlite.go:386-392`）。
- **ViewHash**：发送形态 JSON 的 sha256（`internal/agent/view.go:26-37`）。
- **requests 留痕表**：`session_id, upto_msg_id, upto_blob_id, view_hash, payload_json, error, created_at`
  （`internal/session/sqlite/sqlite.go:135-145`）；LogRequest 落库时取双 MAX(id) 作水印，**必须连控制行一起覆盖**
  （`:517-531`）。
- **用户轮分段（持久消息账本口径）**：压缩分段只认真实用户消息——排除 hook 注入与摘要
  （`internal/compact/segment.go:24-25`）；首部 3 条/8K、尾部 40 个用户轮/40K、摘要段 8K（`:7-14`）；
  tool 消息之前永不切（`:29-31`）。注意这与 §3.3 的 runner 事件环是**两套 substrate**：前者持久、后者易失。
- 已有单测雏形：`internal/session/view_test.go:9-61`、`internal/session/sqlite/compact_store_test.go:98-123,155-157`。

### 3.2 关键区分：回放代码/数据 ≠ 回放模型交互（持久层有/无清单）

- **本地回放模型的精确定义**：`LoadView(upto)` + `ViewHash` 只做一件事——对任意历史请求 i，断言"账本前缀经 Fold
  组装出的发送形态"与当时留痕一致。纯函数级确定性断言：不调用模型、不重放工具副作用、不还原当时响应内容。
- **持久层有**：规整后消息（assistant Content/ToolCalls/Thinking、tool 结果、user 输入）、控制行（含 **turn_end
  终态行**，见 §3.3）、会话级 `model` 标签、每请求发送形态水印 + ViewHash、错误请求的整包发送形态 JSON
  （`internal/agent/agent.go:100-110`；写入点 `:393-403`）、token usage（`usage_json`）。
- **持久层无**：原始 wire/流式分片、**每请求响应信封（如 finish_reason）**、工具 spec 快照、
  **每请求模型/推理配置/温度/种子**（仅有会话级 `model` 标签，非逐请求快照）、**逐 LLM 调用时延**。
  部分工具/轮次时延只在易失事件环里（§3.3），不构成逐请求持久记录。
- 推论：能确定性断言的是"账本 → 发送形态"（代码/数据回放，P0）；要断言"重放得到同样模型输出"必须新增录制层
  （P1 可选组件），且录制数据与账本分离、按同意/脱敏策略管理（§5 P2）。

### 3.3 轮次事件底座：易失事件环 + 持久终态行（两套 substrate）

- **runner.Event**：字段 `Seq/ChatID/Event/TurnID/TurnSeq/At/Data`（`internal/runner/runner.go:38-47`）；
  事件名含 delta/tool_call/tool_result/inject/ask/interrupt/turn_start/turn_end/reset/error/notification
  （`:50-64`）。**Ledger 是每 chat 一个的内存 ring buffer，默认 2,000 条，不落盘**（`:87-99`；断线续传靠
  LoadHistory + since 追增量）。
- **但轮次终态有持久记录**：`finishTurn` 把 `TurnOutcome{Status,TurnSeq,Duration,Usage,Error}` 作为
  `KindTurnEnd` 控制行 Append 进 session SQLite（`internal/runner/slot.go:166-184`），同时才广播 EvTurnEnd 事件。
  即：**事件帧易失，终态结论持久**。
- **对 eval 的含义**：事件流可作为**进程内采集源**（hook `emit` 即可旁路采样/导出），但**不经显式导出就不能做
  持久回放**——这是 P2 在线评测需要新增落库的直接原因。轮次/事件分段断言须区分：消息账本（持久，`realUser`
  + 控制行，§3.1）与事件环（易失，§3.3）。

### 3.4 既有 e2e、测试替身与五维度盘点

- **e2e**：`cmd/e2e` 真实 LLM + sqlite + compact + subagent 按脚本灌水观察压缩全路径（`cmd/e2e/main.go:1-6`）——
  依赖真网，不可重复、进不了 CI。
- **测试替身**：fakeLLM（`internal/compact/helpers_test.go:52-56`）、内存账本 fakeStore（`internal/runner/runner_test.go:15-19`、
  `internal/gateway/gateway_test.go:19-23`、`internal/subagent/subagent_test.go:271-275`，`LogRequest` no-op `:308-309`）、
  记忆域 fakeLLM/fakeSess（`internal/memory/memory_test.go:18-57`）、fakeSpawner（`internal/tools/tool_test.go:91-94`）。
- **streaming**：`internal/llm/openai.go:40-53` 流式 Chat + `IncludeUsage`；finish_reason 归一化 `:92-94`。
  **think 标签跨 chunk 切分已有确定性测试**（`internal/llm/openai_test.go:8-62`：跨 chunk 标签拆开/未闭合/无标签四例）——
  适合直接扩为 P0 表驱动断言。
- **tools/loop 缝**：脚本化确定性检查的现成缝是 loop 的工具执行段（`internal/agent/agent.go:294-355`：批量收齐、
  中断补占位、PreToolUse 阻止即回填、PostToolUse 观察）与 hook 回调类型（`internal/agent/hooks.go:20-41`：
  UserPromptSubmit/PreToolUse/PostToolUse/Stop/PreChat）。工具接口 `internal/tools/tool.go:10-22`；
  runebot 装配 `cmd/runebot/main.go:138-176`，权限 hook 挂载 `:212-216`。→ **脚本化确定性检查适合 P0，
  真任务 outcome 判分归 P1**（对齐 §2.1 的 outcome/transcript 分层）。
- **permissions**：三道闸（`internal/permission/permission.go:7-11`）、纯函数 `Desk.Check`（`:66-91`）、
  元字符保守升 Ask（`:97-113`）、注释自列五条黑名单失效场景（`:30-38`）。持久化/模式缺口归 C4。
- **memory**：`InjectOnce` 首条/dirty 重注 user 消息尾注记忆块（`internal/memory/memory.go:73-96`）；
  工具面 save/forget/list/read/about/history 且 **save 过 LooksLikeSecret 硬拒**（`internal/memory/tool.go:59-112`）；
  dream 生命周期 = 锁 + 熔断/冷却/物料门 + arrears 消化 + tidy 合并（`internal/memory/dream.go:33-85`）；
  持久表 `memories/memory_log/memory_meta`（`internal/memory/sqlite.go:61-95`）。
- **compaction**：提醒线 `W−56K`（`internal/compact/compactor.go:42-45`）；Maintain/`ErrContextFull`
  （`internal/compact/maintain.go:30-34`）；Prime 只卸载（`:67-72`）；ReminderHook 同轮去重（`internal/compact/reminder.go:29-35`）；
  **过期结果卸载判定在 offload**（`internal/compact/offload.go:28-31`：同一键被较新调用取代即无论大小卸载并加过期标记）；
  backfill 只管单条 >50K/整批 200K 的超限 spill（`internal/compact/backfill.go:17-21`）；reactive 压缩重试一次
  （`internal/agent/agent.go:199-203`）；失败恢复 TODO 归 C5（`:223-246`、`:273-277`）。

## 4. 公开基准的能力覆盖局限（本报告的关键限制）

- SWE-bench 判分完全独立于 agent loop——补丁是唯一接口，脚手架的记忆/压缩/审批行为对判分器**不可见**。
- Terminal-Bench 计分在独立验证容器看产物终态；对轨迹的唯一介入是反作弊审计（§0.1），不构成对 compaction
  质量/记忆持久性的正向计分。
- OSWorld/WebArena 看环境终态；OpenAI Evals 看单次补全；LangSmith 技术上能评多轮轨迹，但判分逻辑全部
  由使用者自定义，不构成基准权威。
- 判分器自身的非确定性：WebArena `string_match` 内 `fuzzy_match` 走 LLM 判官
  （[evaluators.py](https://github.com/web-arena-x/webarena/blob/main/evaluation_harness/evaluators.py)），
  同一轨迹复跑可能翻分；自建 eval 应优先确定性判分，LLM 判官只用于审计位并配 meta-eval 校准（§2.2）。
- 结论：**RuneHarness 特有的 memory 跨会话持久、Fold/压缩决策质量、审批链路（Ask 路由/确认疲劳），
  公开基准一个都测不到**——必须自建 eval（P0/P1/P2），公开基准只能作为终态稳定性的补充回归（暂缓可选项）。

## 5. 分阶段计划（人日均为估算；理由含 done/deferred 取舍）

### P0 — 账本与决策 eval，零新依赖（估算合计 **8–12 人日**）

| # | 项 | 入口锚点 | 场景 | 估算 | 为什么现在做 |
|---|---|---|---|---|---|
| P0-1 | Fold/ViewHash 回放回归集：单测雏形升级为表驱动 fixture（含 fork 链、嵌套 boundary、坏数据） | `internal/session/view.go:43-47`、`internal/session/sqlite/sqlite.go:517-531` | append→boundary→view_clear→fork 后 `LoadView(upto)` 逐请求重放且 ViewHash 全对；损坏行注入 Fold 不崩 | 2–3 人日 | 账本是一切 eval 的地基；纯单机 SQLite；transcript 层确定性（§2.1） |
| P0-2 | 压缩行为基准：Estimate/segment/Maintain/Prime/offload/Backfill 快照断言 | `internal/compact/segment.go:7-31`、`internal/compact/maintain.go:30-34`、**`internal/compact/offload.go:28-31`（过期结果卸载）**、`internal/compact/backfill.go:17-21`（仅 >50K/200K spill 阈值） | 切点合法性（tool 前不切）、压后低于提醒线、**同键被取代的过期结果无论大小被卸载**、ReminderHook 同轮不重触发 | 3–4 人日 | 压缩是长板（gap-analysis C7），公开基准测不到，回归风险最高；fakeLLM 现成 |
| P0-3 | 权限判定矩阵 + loop 脚本化缝：Desk.Check 三态 × 名单 × 元字符；工具执行段（中断占位/阻止回填）与 hook 回调 | `internal/permission/permission.go:66-113`、`internal/agent/agent.go:294-355`、`internal/agent/hooks.go:20-41` | Deny/Ask/Allow 全组合；`ls && rm` 必升 Ask；hook 阻止后 tool result 回填正确；流式 think-tag 切分回归（`internal/llm/openai_test.go:8-62` 并入） | 2–3 人日 | 纯函数/脚本化确定性检查，零环境依赖；outcome 级检查留待 P1 |
| P0-4 | **记忆行为 fixture**：inject-once、dirty 重注、save/read/forget、**secret 拒写**；dream 门控（锁/熔断/物料） | `internal/memory/memory.go:73-96`、`internal/memory/tool.go:59-112`、`internal/memory/dream.go:33-85`；替身 `internal/memory/memory_test.go:18-57` | 首条 user 注入/记忆变更后重注/未变更不注；save 凭据形态被硬拒；dream 被门拦下时 Skipped 不算失败 | 1–2 人日 | 记忆是差异化能力（§4），fakeLLM/fakeSess 替身已在，边际成本低 |

### P1 — 自建编码任务套件（首选）或 SWE-bench Lite / SWE-rebench 适配器（估算合计 **5–10 人日**）

| # | 项 | 现有接入点 | 场景 | 估算 | 为什么做/怎么做 |
|---|---|---|---|---|---|
| P1-4a | **自建编码任务套件**（20–50 个任务起步，§2.1）：每任务 = instruction + 沙箱工作目录 + 验证脚本 + 参考解；驱动复用 cmd/e2e 装配方式换真任务 | 装配范例 `cmd/e2e/main.go:1-6`；agent 入口 `internal/agent/agent.go:177`；工具注册 `internal/tools/tool.go:19-22` + `cmd/runebot/main.go:154-157` | 修带失败单测的 bug（outcome=测试转绿）、按 issue 写工具过验收脚本、重构后测试保持通过；outcome 判分为主，transcript（轮数/token/工具序列）记录为辅 | 5–7 人日 | **首选**：自主可控、可测差异化行为、无外部环境依赖；任务格式对齐 Harbor task format 便于将来迁移 |
| P1-4b | **SWE-bench Lite / SWE-rebench 适配器**（可与 4a 二选一或并行）：跑 issue → 工作目录 `git diff` 导出为 model_patch → predictions JSONL → 官方 Docker 判分；SWE-rebench 侧用其预构建镜像与 5 次/SEM 口径 | loop 出口 `internal/agent/agent.go:177` 后取 diff（**补丁导出为新增胶水，非现有代码**）；有利面：headless 缝已解耦（`internal/runner/harness.go:20-28` 仅依赖 emit/ask 回调）、shell 工具即宿主 `sh -c`（`internal/tools/shell.go:33-34`，置于任务容器即工作面）；格式见 [评测指南](https://github.com/SWE-bench/SWE-bench/blob/main/docs/guides/evaluation.md) | 300 例（Lite）或滚动窗（rebench）；判分完全外部化，**不需要录制层** | 8–10 人日 | patch-only 交互与 RuneHarness 天然契合；缺口 = headless 单发入口 + 补丁导出 + 容器内装配 + ask 自动批准路径；自跑分数不能与官方榜直接比（脚手架差异） |
| P1-4c | **（可选前置组件）LLM 录制/回放 cassette**：实现 `agent.LLM` 接口的包装器，录制请求形态+响应+finish_reason+usage | 接缝唯一：`internal/agent/agent.go:26-29` + `:390`；现实现 `internal/llm/openai.go:42` | 仅把 cmd/e2e 式真网路径变为 CI 可重放；**不是 4a/4b 的前置** | 3–4 人日 | 可选：外部判分只看终态；录制数据属敏感轨迹，遵循 P2 的同意/脱敏约束 |

### P2 — 在线评测：canary/shadow + 配对回归检测（必做；估算合计 **6–9 人日**）

| # | 项 | 现有挂点（file:line） | 设计与阈值（阈值为提案估算，未见足够数据前不作为发布闸门） | 估算 |
|---|---|---|---|---|
| P2-6 | **采样与影子运行（canary/shadow）**：新版本对固定 canary 任务集 + **脱敏且 opt-in 的真实轨迹样本**做影子对比；工作区用**隔离克隆或只读影子**；**绝不在线重放会变更状态的工具调用** | 事件底座 `internal/runner/runner.go:38-47,50-64`（Event 字段/事件名）、`:87-99`（内存环，**不落盘**）；终态持久行 `internal/runner/slot.go:166-184`；LLM 缝 `internal/agent/agent.go:390`；留痕 `:393-403`；Recorder `:75-80`；TUI 面 `main.go:352-355`；运维日志 `internal/compact/maintain.go:45,128-136`、`internal/memory/dream.go:444-445` | 对比维度：outcome 通过率、压缩触发点漂移、审批 Ask 频率漂移、轮次时延分布（终态行 Duration，`slot.go:166-184`）。**在线记录现状不持久**（事件环内存only），影子结果需新增落库 + 同意开关 | 3–4 人日 |
| P2-7 | **配对回归检测与阈值**：baseline/candidate **同任务+同模型+同配置成对 trial**（≥5 对起步），比较**分数差的 95% CI** 而非两次独立跑的 CI 是否重叠；capability→regression 毕业制（§2.1） | 复用 P0 fixture + P1 套件作回归库 | "跌幅 >5pp"仅是**提案估算**：在最小对数下 CI 宽、噪声大，需先积累数据校准，达标前只作 warn 不作阻断 | 1–2 人日 |
| P2-8 | **结果/轨迹落库、同意与隐私门槛** | 现状缺口：事件不持久（§3.3）、无结构化指标、无 outcome 标注、无采样/同意开关 | 新增 eval 落库（复用 SQLite 模式）；**`LooksLikeSecret` 只是"凭据形态文本"的启发式扫描**（`internal/memory/secrets.go:5-27`），**不是完整脱敏**——不承诺去除全部 PII/秘密；prompt 与工具结果（Append 全文持久 `internal/session/sqlite/sqlite.go:257-304`；错误请求还存整包发送形态 `internal/agent/agent.go:103`）属敏感数据：默认不采，**opt-in 同意 + 租户级开关 + 保留期 + 必要时人工脱敏** | 2–3 人日 |

**暂缓可选项（deferred，非 P2 替代品）**：Terminal-Bench 4.0（Harbor/容器 + ≥5 trials × 8h 预算，社区提交已关闭需自跑自报）、
OSWorld（VM/UI 全栈 + 维护者代跑；2.0 判分细节未核实）、WebArena（多站点自托管 + 环境重置）。理由：§1 适配成本列 +
§4 覆盖为零；待 P1/P2 落地且需要外部终态背书时再评估——SWE-bench Lite/SWE-rebench（P1-4b）已提供同类补丁式外部判分。

**分阶段理由汇总**：P0 全是纯函数/单机 SQLite 可测项，护住账本与差异化决策，边际成本最低（8–12 人日估算）；
P1 补 outcome 层并打通外部判分口径，任务套件优先、公开子集并行（5–10 人日估算）；P2 把评测搬进真实运行面，
安全边界（不重放可变动作、默认不采、配对统计）先于看板（6–9 人日估算）；公开重型基准全部 deferred，
因其判终态、环境成本最高、对 §4 差异化能力覆盖为零。

## 6. 来源清单（全部 2026-10-11 访问；均为一手）

- SWE-bench：[论文 arXiv:2310.06770](https://arxiv.org/abs/2310.06770) · [官网](https://www.swebench.com/) ·
  [Verified 公告](https://openai.com/index/introducing-swe-bench-verified/) · [仓库](https://github.com/SWE-bench/SWE-bench) ·
  [评测指南](https://github.com/SWE-bench/SWE-bench/blob/main/docs/guides/evaluation.md)；效度批评（第三方）：
  [立场论文 arXiv:2606.17799](https://arxiv.org/html/2606.17799)
- Terminal-Bench：[基准谱系](https://www.tbench.ai/benchmarks) · [4.0 公告](https://www.tbench.ai/news/terminal-bench-4-0) ·
  [仓库](https://github.com/harbor-framework/terminal-bench)（[CONTRIBUTING.md](https://github.com/harbor-framework/terminal-bench/blob/main/CONTRIBUTING.md)、
  [SUBMIT.md](https://github.com/harbor-framework/terminal-bench/blob/main/leaderboard/SUBMIT.md)；推送 2026-10-09）
- OSWorld：[论文 arXiv:2404.07972](https://arxiv.org/abs/2404.07972) · [v1 站点](https://os-world.github.io/) ·
  [v2 站点](https://osworld-v2.xlang.ai/)（2.0 判分细节未核实） · [仓库](https://github.com/xlang-ai/OSWorld)
- WebArena：[论文 arXiv:2307.13854](https://arxiv.org/abs/2307.13854) · [站点](https://webarena.dev/) ·
  [判分源码](https://github.com/web-arena-x/webarena/blob/main/evaluation_harness/evaluators.py)
- Aider Polyglot：[官方博客](https://aider.chat/2024/12/21/polyglot.html) ·
  [benchmark README](https://github.com/Aider-AI/aider/blob/main/benchmark/README.md) ·
  [题库仓库](https://github.com/Aider-AI/polyglot-benchmark)
- SWE-rebench：[论文 arXiv:2505.20411](https://arxiv.org/abs/2505.20411)（v2 2025-11-04） ·
  [线上榜](https://swe-rebench.com) · [数据集](https://huggingface.co/datasets/nebius/SWE-rebench) ·
  [判分 fork](https://github.com/SWE-rebench/SWE-bench-fork)
- OpenAI Evals：[仓库](https://github.com/openai/evals) · [build-eval 指南](https://github.com/openai/evals/blob/main/docs/build-eval.md) ·
  [模板文档](https://github.com/openai/evals/blob/main/docs/eval-templates.md) · [Dashboard 指南](https://platform.openai.com/docs/guides/evals)
- LangSmith：[评估概念](https://docs.langchain.com/langsmith/evaluation-concepts)
- Anthropic agent-eval 指南：[Demystifying evals for AI agents](https://www.anthropic.com/engineering/demystifying-evals-for-ai-agents)（2026-01-09 发布）
- 代码事实：本文 §3 全部 `file:line` 锚点（当日 read/grep）；gap 分析结论见 `docs/flagship-gap-analysis.html`（C1–C7）。
