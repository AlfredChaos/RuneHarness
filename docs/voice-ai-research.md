# ALF-118 全双工语音 AI 调研 — Jarvis 式个人调度入口
> 日期 2026-10-12；底座 RuneHarness + fleet 调度 + mmx(MiniMax) 语音；调研非实现。

## TL;DR
- 推荐路线一句话：P0 用现有 mmx + runebot /v1/stream 做"按说→转写→执行→回话"半双工闭环（1–2 天可验证）；P1 产品化语音指令集与打断；P2 全双工需换实时 ASR（mmx CLI 不支持流式转写），骨架用 LiveKit Agents 或 Pipecat。
- mmx 实测结论：TTS 支持真流式输出（首 PCM ~0.5s）；ASR 只有整文件转写、无部分结果、无 VAD（4.7s 音频 1.1s）。故 mmx 现态只够半双工。
- runebot 已具备打断链路：POST /v1/chats/{id}/actions cancel 或 WS cancel 帧 → turn_end(interrupted)。

## 1. 业界全景
> 检索日期 2026-10-12。GitHub star/license 数据来自 `gh api` 实时查询，延迟与价格数据来自所引 URL 当日抓取；无法核实处标注 `[UNVERIFIED]`/`[估计]`。全量见原始文件 industry-research.md。

### 1.1 开源框架对比表（gh api，2026-10-12）

| 框架 | 主语言 | 架构模型 | License | Stars | 最近发布 / 最后提交 | 社区活跃度（近 30 天） | 原生全双工 + barge-in | 传输（telephony/WebRTC） |
|---|---|---|---|---|---|---|---|---|
| **LiveKit Agents** (`livekit/agents`) | Python（另有 Node.js/TS SDK） | **AgentSession 状态机 + 插件模型**：STT/LLM/TTS/VAD/turn-detector 均为可替换插件；同时支持级联 pipeline 与 realtime（S2S）模型客户端 | Apache-2.0 | **14,709**（3,916 forks） | `livekit-agents@1.8.6`（2026-10-09） | 100+ commits；699 open issues — **非常活跃** | **是**。VAD 驱动 interruption 管道 + Adaptive Interruption（声学区分真打断与 backchannel，LiveKit Cloud 可用）；preemptive generation 支持 | WebRTC 原生（LiveKit SFU）；电话经 LiveKit SIP 接入 |
| **Pipecat** (`pipecat-ai/pipecat`，Daily 出品) | Python | **FrameProcessor 管道（plugin 模型）**：帧（Frame）驱动的处理器链，user turn strategies 可插拔；不绑定任何云 | BSD-2-Clause | **16,392**（2,859 forks） | `v1.12.0`（2026-09-26） | 100+ commits；408 open issues — **非常活跃** | **是**。`InterruptionFrame`（SystemFrame）默认开启，取消全链路在途工作；pipecat-ai/smart-turn 开源 turn 检测模型 | WebRTC（Daily / SmallWebRTC / LiveKit transport）、WebSocket；电话经 Twilio/Plivo 等集成 |
| **Vocode** (`vocodedev/vocode-core`；注：`vocodedev/vocode` 已 404) | Python | **三接口抽象**（Transcriber / Agent / Synthesizer）+ StreamingConversation 编排 | MIT | **3,802**（656 forks） | 最后 push **2024-11-15**，无新 release | 0 commits；**已停滞/事实弃维护** | 代码层面支持 barge-in，但无人维护 | 电话（Twilio）+ WebSocket；React SDK 已归档 |
| **TEN Framework / TEN Agent** (`TEN-framework/ten-framework`，Agora 主导) | 多语言：Python / C / C++ / TypeScript / Rust / Go | **graph 运行时 + extension（组件）+ 包机制**：以图编排多模态扩展；TEN Agent 是其对话 AI 示例应用 | Apache-2.0（GitHub SPDX 显示 NOASSERTION，含附加条款） | **11,156**（1,362 forks） | `0.11.73`（2026-09-22） | 17 commits；247 open issues — **活跃** | **是**。全双工设计，生态自带 **TEN VAD** 与 **TEN Turn Detection** 专用模型 | Agora SD-RTN（RTC）原生；可自托管运行时 |
| **FastRTC** (`gradio-app/fastrtc`，原 `freddyaboulton/fastrtc`) | Python | **Gradio handler 模型**：任意 Python 函数 + `ReplyOnPause` 变成音视频流；非完整 agent 框架，定位快速原型 | MIT | **4,633**（436 forks） | `0.0.34`（2025-11-24）；最后 push 2026-01-12 | 近 30 天 0 commits — **明显减速** | 内置 VAD pause detection + turn taking（`ReplyOnPause`） | WebRTC / WebSocket 原生；`fastphone()` 提供免费临时电话号码 |

### 1.2 闭源 SaaS（一行一个）
- **Vapi**：开发者型 voice-agent 编排平台，**$0.05/min 平台费** + STT/LLM/TTS/电话按成本 pass-through（BYO 模型可省钱）；10 路并发含在内，加并发 $10/line/月。（vapi.ai/pricing，2026-10-12）
- **Retell AI**：一体化 per-minute 计价（含转写、声音、LLM），**$0.07–$0.31/min 依模型/音色**，电话 $0.015/min，无平台费。（retellai.com，数据标注截至 2026-05，检索 2026-10-12）
- **Bland AI**：电话外呼为主，**$0.14/min 全包**（无 token 计费、无 pass-through），免费层含 2 credits + 免费 inbound 号码。（bland.com/pricing，2026-10-12）

选型要点：**活跃且可自托管的首选是 LiveKit Agents 与 Pipecat**（月度以上发布节奏、百级月提交）；Vocode 已死；TEN 适合 graph 编排 + Agora RTC 生态；FastRTC 只适合一周内出 demo。LiveKit 差异化在**传输层**（自建 SFU、WebRTC 原生、SIP 一体化）与服务端推理配套（TurnDetector、Adaptive Interruption 部分 only-cloud）；Pipecat 差异化在**完全云中立**的管道抽象与最细粒度的 interruption 语义控制。个人项目常见组合：Pipecat（无云绑定）+ LiveKit（仅做 SFU/传输）。

### 1.3 关键技术组件结论

**VAD / endpointing（决定 barge-in 与响应节奏上限）**：事实标准是 **silero-vad**（10,412★，MIT，LiveKit/Pipecat 默认集成）。**LiveKit TurnDetector** 是音频 EOU 模型（直接编码用户音频，语义+韵律判断"说完没有"，不依赖 transcript），`v1` 云端 / `v1-mini` 本地 CPU 开放权重，支持 14 语言**含中文**，endpointing 默认 min_delay 0.5s→0.3s、max_delay 3.0s→2.5s。**Pipecat Smart Turn**（`pipecat-ai/smart-turn`，1,609★，BSD-2）开源同类，v3.2 宣称短语句准确率 +40%。**Barge-in 质量取决于三件事**：①VAD 起点灵敏度（太灵敏会把咳嗽/背景声当打断）；②EOU 误判——VAD-only 会在"let me think…"停顿处提前抢话（LiveKit 文档示例：文本模型 2.76s 抢话、音频模型等到 6.41s 说完）；③backchannel 过滤——"uh-huh/好的/对"不应让位，方案是 LiveKit Adaptive Interruption（声学模型，Cloud 专属）或 Pipecat `MinWordsUserTurnStartStrategy` + Krisp VIVA。

**AEC（回声消除）——什么时候必须有**：**扬声器外放 + 常开麦克风**（正是 Jarvis 形态）**必须 AEC**——agent 自己的 TTS 回声进麦克风会让 VAD 误判"用户在说话"→ agent 打断自己 + ASR 幻听污染上下文。戴耳机或 push-to-talk/wake-word 门控采集可免。Linux 标准解是 **webrtc-audio-processing**（PulseAudio/PipeWire `module-echo-cancel` 后端）；speexdsp 更轻但质量低于 AEC3；浏览器/移动端走 WebRTC 时 AEC3 内置自动生效。落地含义：**采集链路必须先过 AEC 再进 VAD/ASR**，否则全双工体验不成立。

**Barge-in 机制对照**：Pipecat 用单一 `InterruptionFrame`（SystemFrame）一次穿透全部阶段——双向广播、各 processor 取消当前任务、LLM mid-stream 取消、TTS 停止合成清空缓冲、output transport drain 未播队列（bot "goes silent within roughly one audio write"）、**上下文只保留真实播出过的文本**（被截断回复以"已说出部分"入库）。LiveKit 走 VAD 触发对当前 SpeechHandle 打断，门控可配 `interruption.mode: "vad"|"adaptive"`（adaptive 需 Cloud）；与 realtime 模型协作时关掉其 server-side turn detection 由客户端接管；另有 `preemptive_generation` 不等 turn 提交就开跑。

**流水线解剖（自建全双工语音 agent 的标准形态）**：

```
 mic ─► capture ─► [AEC/NS] ─► VAD ──────────┐（说话中随时触发 barge-in：广播打断）
                                             ▼
              streaming ASR（partial transcripts 持续更新）
                                             ▼
              EOU / endpointing（turn detector：VAD 静音 + 语义/音频"说完没"判定）
                                             ▼
              LLM stream（TTFT 首 token；preemptive generation 可提前开跑）
                                             ▼
              按句/标点切块 ─► TTS stream（first byte 出声） ─► playback
```

- **每一级都有打断点**：ASR partial 可丢弃；LLM 流取消；TTS 文本缓冲清空；播放队列 drain。
- **延迟的关键在重叠**：ASR 在 endpointing 静音窗口内持续出 partial；LLM 首句一旦成型即送 TTS；TTS 按句流式出音频。串行化任何一步都会把总延迟推高数百毫秒。

### 1.4 延迟基准

**人的对话节奏 = UX 标尺**：自然对话话轮间平均间隙约 **200ms**（Levinson & Torreira, Frontiers in Psychology 2015；Stivers et al., PNAS 2009 十语言交叉验证）。行业直接当产品标尺（AssemblyAI STT 延迟指南）。工程含义：**200–500ms 端到端是"像人"；≤1s 是"可用"；>2s 是"坏了"**（后者为行业共识 `[UNVERIFIED]`）。

**各阶段公开基准**（全部可溯源，检索 2026-10-12）：

| 阶段 | 实测数字 | 来源 |
|---|---|---|
| ASR 首个 partial（TTFS） | **Deepgram Nova-3：84ms**（Coval 榜 #5/28） | benchmarks.coval.ai/providers/deepgram（测量 2026-10-06） |
| ASR 首个 partial | **Gladia：<103ms**；其工程博客强调看 P99："150ms 均值但 P99 冲 1.2s 的供应商会毁掉体验" | gladia.io/product/real-time；gladia.io/blog/latency-benchmarks-for-streaming-speech-to-text-ttlb-and-p99（2026-08-28 发文） |
| ASR 首 token | **NVIDIA Nemotron 3 ASR：80ms 最小 chunk，首 token ≈80–160ms** | perspectives.nvidia.com/nemotron-speech |
| ASR（厂商口径） | Deepgram 宣称转写 "under 300ms" | deepgram.com/product/speech-to-text |
| LLM TTFT（Llama 3.3 70B 三供应商实测，3 次平均） | **Cerebras ≈100ms；Groq ≈180ms；Fireworks ≈250ms**；吞吐 1,200/400/90 tok/s | github.com/ashstep2/inference-benchmark |
| LLM TTFT（另一口径） | Groq Llama 3.3 70B **中位 TTFT 0.95s、329 tok/s**——差异来自是否含网络 RTT/预热/中位 vs 均值 | markaicode.com/benchmarks/groq-llama-31-m4-max-latency-benchmark |
| LLM 吞吐（gpt-oss-120b high） | Cerebras 1,761.5 tok/s ＞ SambaNova 708.8 ＞ Groq 471.3 | artificialanalysis.ai/models/gpt-oss-120b/providers |
| TTS 首字节 | **Deepgram Flux：TTFS 209ms**（Coval TTS 榜 #11/32） | benchmarks.coval.ai/providers/deepgram（测量 2026-10-06） |
| TTS 首音频（感知中位） | **Gradium：214ms 中位 TTFA**（480 样本/模型） | gradium.ai/content/tts-latency-benchmark-2026（2026-09-08） |
| **级联端到端（STT→LLM→TTS 全流式 + function calling）** | **TTFA 755ms（最佳 729ms；HTML 摘要 P50 947ms）**——Deepgram STT + vLLM + ElevenLabs，Stanford/Salesforce 团队 | arXiv:2603.05413（2026-03-05） |
| S2S 端到端参照 | **Qwen3-Omni DashScope Realtime ≈702ms 音频到音频**（不可自托管；论文论证级联管道仍是可自托管的最实用架构） | 同上 |
| 本地全自托管参照 | M 系列 Mac 本地跑 Pipecat（本地 ASR/LLM/TTS）可 **voice-to-voice <800ms** | github.com/kwindla/macos-local-voice-agents |
| Turn/endpointing 预算 | VAD-only 默认 0.5–3.0s；挂音频 turn detector 后 **0.3–2.5s** | docs.livekit.io/agents/build/turns/turn-detector |

**击穿 2 秒感知线的预算表**（目标：说完最后字 → 听到回复首音 ≤2s，良好 ≤800ms）：

| 阶段 | 预算 | 依据 |
|---|---|---|
| 采集 + VAD 起振 | 50–100ms | `[估计]` 常规 WebRTC/环形缓冲开销 |
| Endpointing（EOU） | **250–300ms** | LiveKit turn detector min_delay=0.3s；ASR partial 在此窗口**并行**到达 |
| ASR 首个 partial → turn 提交 | 84–300ms | Coval Nova-3 84ms；Deepgram <300ms；Gladia <103ms |
| LLM TTFT | 100–300ms | Cerebras ≈100ms / Groq ≈180ms 实测；普通云 LLM 300–500ms+ |
| TTS 首字节（按句切块后） | 200–250ms | Flux 209ms / Gradium 214ms |
| 网络 + 播放缓冲 | 50–100ms | `[估计]` |
| **合计** | **≈750–1,250ms** | 与 arXiv:2603.05413 实测 755ms（P50 947ms）吻合；留 ~750ms 余量给 2s 红线 |

800ms 预算拆分 = endpointing 300 + LLM 200 + TTS 200 + 链路 100（ASR 与 endpointing 重叠不单独计）。跌入 ">2s 笨" 区间的典型配方：VAD-only 保守 endpointing + 大模型慢 TTFT + 非流式 TTS + 每步串行网络 RTT。压延迟四杠杆：①语义/音频 turn detector；②fast-inference LLM 或 preemptive generation；③句读切块流式 TTS；④TTFS <150ms 的流式 ASR 全程不落盘。

### 1.5 全双工 S2S API 平台对比与"结构性错配"

| 平台 | 模型 | 协议 | 能力要点 | 定制边界 | 价格形态（官方页，2026-10-12） |
|---|---|---|---|---|---|
| **OpenAI Realtime API** | `gpt-realtime-2.1` / `2` / `2.1-mini`；另有 **gpt-live-1**（2026-09-10 上线全双工 voice 层，WebRTC 优先，来源 rime.ai，2026-09-28） | **WebRTC 或 WebSocket**；Realtime session 上限 60 分钟 | `semantic_vad` turn detection、barge-in、function calling、图像输入、服务端存储 prompt；gpt-live-1 按秒计费 | instructions、voice、tools、prompt 模板；**ASR/LLM/TTS 不可拆换**，记忆与会话状态托管在 OpenAI | Realtime：audio 入 $32/1M、出 $64/1M，text $4/$24；**gpt-live-1 $0.05/min 按秒计费**（developers.openai.com） |
| **Gemini Live API** | `gemini-3.8-live`（含 Extended Thinking）、`gemini-3.1-flash-live-preview`；另有 Live Translate / Transcribe Live | **有状态 WebSocket（WSS）**（WebRTC 经 LiveKit/Pipecat/agora 伙伴集成） | barge-in、proactive audio、affective dialog、70 语言、原生多模态（16k 入/24k 出）、function calling + Search grounding、双语转写 | 模型只能在 Live 家族内选；**turn-taking 由服务端主导**——LiveKit 文档明确其 "keeps turn-taking on their own signals"，与客户端打断门控不兼容 | audio 入 $3.00/1M（≈**$0.005/min**）、出 $12.00/1M（≈**$0.018/min**）；text $0.75/$4.50；有免费层（ai.google.dev，2026-10-06 更新） |
| **xAI Grok Voice（S2S API）** | `grok-voice-latest` = `grok-voice-think-fast-2.0` | **WebSocket**（`wss://api.x.ai/v1/realtime`），事件协议与 OpenAI Realtime 兼容；官方 cookbook 有 WebRTC + ephemeral token 与 Twilio 示例 | server_vad 或手动 commit、`reasoning.effort` 开关、内置 web_search/x_search/code_execution/MCP、**自定义克隆音色**、`idle_timeout_ms` 主动回访、session resumption、发音替换表 | instructions、voice（含克隆）、工具；自定义工具可经 MCP 挂自有服务端（三者中边界最宽） | **S2S $0.08/min（$4.80/hr）** + $0.004/文本输入；STT $0.10–0.20/hr；TTS $15/1M 字符（docs.x.ai） |

**为什么"自托管个人调度器"是它们结构性填不了的空位**——四点错配：
1. **成本结构错配**：S2S 按音频时长计费，个人入口是 24/7 常开麦克风。持续聆听下仅 Gemini audio 输入 ≈$216/月，gpt-live-1 ≈$2,160/月；自托管 VAD+wake-word 边际成本为零，只有"唤醒后"几十秒才值得送模型。这是计费函数形状问题，不是折扣能解决的。
2. **定制边界太浅**：三家定制面都是 instructions+voice+tools；无法把自己的记忆系统、日历/邮件/家居状态、子代理编排放进推理回路，只能 tool 回调外挂（每次往返 = 额外网络跳 + 音频→文本→工具→音频折返）。级联管道里 LLM 是自己 agent 图的一个节点：turn 提交前后可直接触发本地任务取消、多代理协商、私有 RAG。Gemini 更是不允许客户端接管 turn-taking。
3. **数据主权与会话模型**：个人调度器处理日历/邮件/健康/家庭数据；S2S 是无状态短会话（OpenAI 硬上限 60 分钟），记忆必须外置——既然外置不可避免，"大脑+记忆+音频前端"留本地反而更简单，还消除云端 RTT 并带来断网可用。
4. **对标差距已在误差范围内**：Qwen3-Omni 云端 S2S ≈702ms vs 自建级联 755ms（arXiv:2603.05413），本地 Mac 级联也能 <800ms。为最后几十毫秒放弃自持架构，对个人项目不成立。

**结论**：S2S API 适合"电话客服/语音 widget"类按通话计费、无状态场景；个人 Jarvis 的正确形态是**自托管级联管道（Pipecat/LiveKit Agents 骨架）+ 自有记忆与子代理层**，把 S2S（或 STT/LLM/TTS 单项）当可热插拔供应商选项，而不是地基。

## 2. mmx 能力实测（2026-10-12 真机实测）
> 全记录见原始文件 mmx-test.md。Binary `/Users/alfredchaos/.local/bin/mmx`，版本 mmx 1.0.26（node script）。

**后端身份**：mmx **不是本地模型**，是薄云 API 客户端。help 文本引用 `platform.minimaxi.com/docs/api-reference/...` → 后端为 **MiniMax**（TTS speech-2.8-hd，ASR asr-1.0）。需网络 + API key（已配置，配额有余）。

**synthesize 主要选项**：`--model`（默认 speech-2.8-hd）、`--text/--text-file`（≤10k chars，stdin 用 `-`）、`--voice`（60+ 音色，默认 English_expressive_narrator，中文如 `female-shaonv`）、`--speed/--volume/--pitch`、`--emotion`（happy/sad/angry/fearful/disgusted/surprised/calm/fluent/whisper）、`--text-normalization`、`--latex-read`、`--format`（mp3/pcm/flac/wav/pcmu_raw/pcmu_wav/opus，默认 mp3）、`--sample-rate`（默认 32000）、`--channels`、`--language`、`--subtitles`、`--pronunciation`、`--out`、**`--stream`（原始音频流到 stdout）**。`generate` = `synthesize` 别名（已验证）。

**transcribe 主要选项**：`--file`（mp3/wav/m4a/flac/aac/opus/ogg/aiff；**不支持 `-`，字面路径**）、`--model`（默认 asr-1.0）、`--response-format`（json/verbose_json/srt/vtt）、`--language`（BCP-47，省略自动检测）、`--timestamp-level`（sentence/word）、**`--stream`（名义上流式增量文本）**、`--out`。`recognize` = `transcribe` 别名（已验证）。

**实测延迟数字表**：

| 测试 | 音频时长 | 延迟 | 结果 |
|---|---|---|---|
| synthesize 文件模式 | 4.7s（wav, 32kHz Int16, 303,582B） | **1,595ms** | 正常；RTF ≈0.34× |
| synthesize 文件模式 | 19.0s | **2,858ms** | 正常 |
| synthesize `--stream` | 4.8s（PCM 24kHz） | **总 761ms，首 PCM ~530ms**，chunk 滴灌 ~230ms | **真流式**，可直管道播放 |
| transcribe | 4.7s | **1,093ms** | 中文往返逐字精确 |
| transcribe | 19.0s | **1,982ms** | 正常；延迟随文件长度增长（整文件上传处理） |
| transcribe `--file -` | — | — | **失败**：`-` 被当字面路径；改 `--file /dev/stdin` 可管道但仍为缓冲整文件上传 |

**Streaming 探针结论（关键）**：
- `transcribe --stream`：逐行时间戳测量显示**所有行同一毫秒到达**（19s 音频总耗时 1,988ms）——尽管帮助文档写 "Stream incremental text"，**实际无增量/部分结果，只输出最终 JSON 一次**。
- `synthesize --stream`：**真流式**。首 PCM 字节 ~0.5s，4.8s 音频 0.76s 内出完，可 `| mpv -` 直接播。
- `synthesize --text-file -` 管道输入文本：可用，但文本须在调用前完整（文件/stdin 读入，非 token 流）。
- **无 VAD/endpointing/interrupt 选项**：`grep -iE "vad|endpoint|silence|chunk|realtime|partial|mic|record"` 在 speech 与顶层 help 均无命中。ASR 走 HTTP 上传，CLI 未暴露 WebSocket 实时端点。
- **无录音命令**：资源列表无 record/capture；麦克风采集须外部（`sox rec` / `ffmpeg -f avfoundation`）。
- 错误/挂起：无挂起（全部 <3s），仅 `--file -` 字面路径失败一例。

**VERDICT（原样保留）**：
- **transcribe**: input = whole audio file only (pipe only via `--file /dev/stdin` trick); **no streaming input, no partial results** (`--stream` flag exists but delivers one final JSON). Latency ≈ **1.1 s for ~4 s audio**, ≈2.0 s @ 19 s.
- **synthesize**: **true incremental audio streaming** via `--stream` (first PCM bytes ~0.5 s; 4.8 s audio in ~0.76 s); file mode ~1.6 s @ 4.7 s audio. 60+ voices (default English; zh voices like `female-shaonv`), speed/pitch/emotion control, wav/mp3/pcm/opus/flac.
- **双工能力**：**只够半双工** (press-talk → 完整录音 → transcribe → 生成回复 → 流式播放)。不能支撑全双工：ASR 无实时流式/无部分结果、无 VAD/endpointing/barge-in（麦克风采集也得外部实现）。若要全双工需绕过此 CLI，直接用 MiniMax 的 WebSocket 实时 ASR/TTS API；用此 CLI 只能做"录完再转"的轮次式语音助手，回复端可用 TTS streaming 把感知延迟压到 ~0.5–1 s。

## 3. 与现有架构的接法
> 仓库快照 /Users/alfredchaos/home/code/RuneHarness（go.mod 模块 `runeharness`），结论全部来自实际读取代码，file:line 索引；全量见 integration-notes.md。

### 3.1 现状接缝图

```
┌────┐   ┌─────────┐   ┌────────────────────────── RuneHarness (runebot 进程) ──────────────────────────┐   ┌─────────┐
│mic │──▶│mmx STT  │──▶│ POST /v1/chats/{chat_id}/stream   body {"content": "<转写文本>"}              │   │mmx TTS  │
└────┘   └─────────┘   │   → gateway.handleStream          internal/gateway/handlers.go:378           │◀──│(消费事件)│
                        │     路由注册: internal/gateway/gateway.go:147                                 │   └────┬────┘
                        │     Bearer 认证: claims() gateway.go:168-174 / StaticAuth gateway.go:79-90    │        │
                        │        │ Manager.Submit  internal/runner/slot.go:281                          │        │
                        │        ▼                                                                      │        │
                        │   slot.runTurn  slot.go:115                                                   │        │
                        │     turnCtx = WithCancel(WithTurnID(slotCtx))  slot.go:120                    │        │
                        │        │ TurnRunner 闭包  cmd/runebot/main.go:277-297                         │        │
                        │        │   ├ sys = prompt.Build(env) 常驻 system  main.go:280                  │        │
                        │        │   └ TriggerUserPromptSubmit（memory InjectOnce 挂点）main.go:287-294  │        │
                        │        ▼                                                                      │        │
                        │   agent.Run（LLM→工具→回填 循环）  internal/agent/agent.go:198 起              │        │
                        │     llm.Chat(..., a.OnPartial)  agent.go:390（流式累积快照）                    │        │
                        │        │ OnPartial → emit EvDelta  internal/runner/harness.go:33-35           │        │
                        │        ▼                                                                      │        │
                        │   Ledger.Append（每 chat 单调 seq，ring buffer 2000）runner.go:92-101          │──NDJSON─┘
                        │        │                                                                      │
                        │   handleStream 转发帧至 HTTP 响应流  handlers.go:437-470（turn_end 即闭环）      │
                        └──────────────────────────────────────────────────────────────────────────────┘
   打断（barge-in）: POST /v1/chats/{id}/actions {"type":"cancel"}  handlers.go:174,190-195
                     → Manager.Interrupt  slot.go:311-328 → cancel turnCtx → EvInterrupt 事件
   备选上行轨（同一套事件账本）: WS /ws 帧 {"type":"message","chat_id":..,"content":..}  ws.go:91-109
   子任务派发（进程外）: fleet CLI  /Users/alfredchaos/home/agent-default/fleet/bin/fleet（纯 bash）
```

- 两条上行轨（HTTP/SSE+NDJSON 与 WS）共用同一 Manager/Ledger：`internal/gateway/gateway.go:1-3` 包注释明确"客户端任选其一接入"。
- TUI 路径对照：`Model.sendUserText` internal/tui/model.go:511、`TriggerUserPromptSubmit` model.go:525、`agent.Run` model.go:549；headless 轨刻意对齐同一 hook 序（main.go:283-294 注释"对齐 TUI model.go:524"）。

### 3.2 runebot /v1/chats/{id}/stream 契约要点

**服务装配**（cmd/runebot/main.go）：headless 网关无 TUI；默认监听 `127.0.0.1:8765`（main.go:52，`RUNE_LISTEN` 可覆盖）；认证必须配 `RUNE_TOKENS`（`"tk1=tenant:subject,..."`，main.go:58-62；空表直接报错 gateway.go:72-76）。

**请求**（handlers.go:378-409）：`POST /v1/chats/{id}/stream`，Header `Authorization: Bearer <token>`（缺失 → 401，handlers.go:380-384）。Body JSON `{"content": string(必填非空), "display"?: string, "attachments"?: [...]}`（content 空 → 400；附件非法 → WriteHeader(200) 之前 400）。`chat_id` 经 `chat_bindings` 持久映射到 session（Store 接口 gateway.go:103-108；无活跃槽位时自动建槽 slot.go:454-455）。

**响应**（handlers.go:405-470）：
- `200` + `Content-Type: application/x-ndjson` + `X-Accel-Buffering: no`（handlers.go:406-412）。
- 先 `Mgr.Subscribe(chat, since=0)` 再 `Submit`，保证事件不落订阅窗外（handlers.go:415-425）。
- 忙时**不回 409**，而是 200 + 内联 `{"event":"error","data":{"message":"chat is busy"}}` 帧，带在跑轮次的 turn 归属与 seq 上界（BusyError，handlers.go:427-434；定义 runner.go:79-88）。客户端需自行串行。
- 心跳每 15s 一帧 `{"event":"ping"}`，不入账本（handlers.go:443-449）。
- 客户端断开只退出读循环并留痕日志 `"stream: aborted mid-turn"`（handlers.go:451-465）——**轮次仍在服务端继续跑**。
- 收到 `turn_end` 后服务端主动结束响应（handlers.go:466-468），请求-响应单次闭环。
- 配套端点：`GET /v1/chats`（gateway.go:140）、`GET /v1/chats/{id}/history?before=&limit=`（gateway.go:144）、`GET /v1/chats/{id}/events`（SSE，Last-Event-ID 续传，gateway.go:146）、`GET /v1/health`（gateway.go:139）、`GET /v1/schedules`（gateway.go:151）、`GET /v1/notifications` + `POST /v1/notifications/ack`（gateway.go:152-153）。

**事件帧**：`runner.Event{seq, event, chat_id, turn_id, turn_seq, at, data}`（runner.go:38-47）。事件名与 data 契约（runner.go:50-64）：

| event | data | 语义 |
|---|---|---|
| `delta` | `{thinking?, content?}` | **累积快照，全量替换，不是增量 diff**（runner.go:52；`agent.Partial` agent/message.go:101-106） |
| `reasoning_delta` | | 推理流 |
| `tool_call` | `{id, name, arguments}` | 工具分发前（含将被 hook 阻止的调用，agent.go:44-47） |
| `tool_result` | `{id, output, is_error, blocked, duration_ms}` | 工具结果 |
| `inject` | `{content}` | hook 注入（todo nag、压缩提醒） |
| `ask` | `{ask_id, call, reason, seq, total}` | 权限确认挂起（slot 的 ask 通道路由到客户端，runner.go:255-263） |
| `interrupt` | `{reason}` | 本轮被打断（slot.go:322-325） |
| `turn_start` | `{turn_seq, queued_from?}` | 轮次开跑 |
| `turn_end` | `{status, turn_seq, usage?}` | 终局：`completed`/`interrupted`/`error`（slot.go:155-162） |
| `reset` | `{reason}` | 会话边界后移（压缩），since 前序作废 |
| `error` | `{message}` | 轮次内非致命异常 |
| `notification` | `{kind: bg\|cron\|missed, task_id?, prompt?}` | 旁路输入已注入 |

**WS 轨等价形态**（internal/gateway/ws.go）：上行帧 `wsIn{type, chat_id, content, attachments, since, ask_id, approved, req_id, before, limit}`（ws.go:27-39）；帧类型 `attach/detach/message/cancel/answer/history/ping`（ws.go:82-142）。`message` 忙时回 `{"type":"error","code":409}`（ws.go:100-103）；`cancel` 直接调 `Mgr.Interrupt`（ws.go:110-114）。对语音客户端，WS 单长连可同时承载"说话→事件流→打断"三件事，比 HTTP 流更适合 barge-in。

### 3.3 语音指令集建议（语音 UI 可安全暴露的操作 → 现有机制）

| 语音意图 | 落点 | 机制 |
|---|---|---|
| 发消息/提问 | `POST /v1/chats/{id}/stream` | STT 文本直接作 content；消费 delta→TTS |
| 打断/闭嘴 | `POST /v1/chats/{id}/actions {"type":"cancel"}` 或 WS `cancel` 帧 | Mgr.Interrupt（slot.go:311） |
| 回答权限确认 | `actions {"type":"answer","ask_id":..,"approved":bool}`（handlers.go:196-205）或 WS `answer` 帧 | Mgr.Answer（slot.go:330-347）；ask 事件带 ask_id |
| 查会话/历史 | `GET /v1/chats`、`GET /v1/chats/{id}/history` | 只读，安全 |
| 查定时任务 | `GET /v1/schedules`（gateway.go:151） | 按租户过滤 cron 清单 |
| 查/清通知 | `GET /v1/notifications`、`POST /v1/notifications/ack`（gateway.go:152-153） | cron/bg 通知 outbox |
| 派子任务（"派个任务"） | **进程外** `fleet dispatch <slug> [--project p] [--class c] [--brief "..."]` | fleet bash CLI |
| 查子任务/追问/打断子任务 | `fleet status` / `fleet peek <id>` / `fleet send <id> "..."` / `fleet steer <id> "..."` / `fleet kill <id>` | fleet 脚本 fleet:4-16 |

**fleet/vibe 层**：`bin/fleet` 是 captain 侧纯 bash CLI：dispatch 创建 tasks/<id>/（brief.md/meta.json/status.json/pending/）并 spawn tmux 会话 `fm-<id>` 跑交互式 omp，首条消息为 @brief.md；默认 `--vibe` 随后 `tmux send-keys -t "fm-$id" '/vibe' Enter`（fleet:118-120）。`vibe` 非独立二进制；`fm-report` 在 fleet/bin/fm-report 且已在 worker shell PATH。**语音派任务的程序化入口就是这条 bash 命令**：voice shell 直接 `exec fleet dispatch ...`，无需经 RuneHarness。worker 回报按 brief 协议写 `fm-report working/need-captain/done|failed` + report.md；captain 侧 `fleet drain/watch` 收事件。RuneHarness 进程内另有 `task` 工具（internal/subagent/subagent.go:22-23 ToolName="task"，Spec subagent.go:120-147：`{description, subagent_type: general_purpose|normal}`，同步 spawn、nest 可一层）：说"派个任务"给主 agent，模型可自主选择 task 工具或 fleet CLI。

### 3.4 Barge-in 可行性

**可行（已具备）**：
1. 轮次运行在 `turnCtx = WithCancel(WithTurnID(slotCtx))`（slot.go:120），生命周期归 slot，与 HTTP 请求 ctx 解耦——客户端断流不会误杀轮次（handlers.go:451-456）。
2. 显式打断：`Manager.Interrupt`（slot.go:311-328）发 `interrupt` 事件（reason=client_cancel）后 `cancel()`；`agent.Run` 捕获为 `*InterruptedError`（agent.go:175-181），已执行工具结果照常落历史、未执行 tool_calls 补占位、transcript 可续跑；turn_end `status=interrupted`（slot.go:155-158）。入口：HTTP actions/cancel（handlers.go:190-195）、WS cancel 帧（ws.go:110-114）。
3. 语音客户端 barge-in 流程：本地 VAD 检出用户开口 → 停 TTS 播放 → 发 cancel → 等 `interrupt` + `turn_end(interrupted)` 事件 → 进入下一轮。WS 单连接上这就是同一 socket 里的三帧。
4. 断线恢复：事件账本 seq 单调 + `Subscribe(since)` 追补（slot.go:352-358），落后出窗（ring 2000，runner.go:92）返回 ErrLag 触发 reset（runner.go:74-76）；HTTP 侧另有 SSE Last-Event-ID 续传轨。

**今日不可行 / 需外置**：
- **仓库内零音频代码**（全仓 grep 仅假阳性命中）。STT/TTS 必须完全由外部 mmx CLI 承担；RuneHarness 只见文本。
- `delta` 是**累积快照非增量**（runner.go:52）——TTS 侧需自己做文本 diff 取新增片段，或改为只在 `turn_end` 后整体朗读（无流式感）。
- 无"暂停/继续"语义，只有取消后重发；无服务端音频端点可挂；权限 `ask` 挂起期间语音侧需 UX 决策（朗读问题让用户口头 yes/no → answer 帧）。

### 3.5 个性化（SOUL / memory 注入路径）

- **SOUL**：`prompt.LoadSoul(path)` 启动读一次、进程内冻结（internal/prompt/soul.go:8-29）；路径来自 `RUNE_SOUL`（cmd/runebot/main.go:105；k3s 部署固定 `/data/SOUL.md`，deploy/k3s/runebot.yaml:53-56）。拼进 system：`prompt.Build` 在 Identity 之后插 `## 人设` 段（internal/prompt/prompt.go:85-88，段序 identity→人设→工具→记忆→技能→工作区）。runebot 侧每轮 prepend 且不落库（main.go:277-297）。**改人设 = 改 SOUL.md 文件 + 重启进程**；本机当前未发现 SOUL.md 实体文件——部署侧待创建。
- **记忆注入**：`Memory.InjectOnce` 是 UserPromptSubmit hook（internal/memory/memory.go:73-98）：会话首条 user 消息（或记忆变脏后首条）在 prompt 尾部追加 `<memory-index>` 索引 + AlwaysInject 全文（internal/memory/inject.go:15-73，单条 4KB 封顶），落在 Fold 首部永不压缩。注册点 runebot main.go:260；触发点 main.go:287-294 与 TUI model.go:525。
- **Jarvis 化路径**：语音语气/称呼/简洁度写进 SOUL.md（人设段）；用户画像、偏好等长期事实走 memory（profile 由 `RUNE_MEMORY_PROFILE` 选，main.go:53-57、memory.Lookup）。hook 链支持 UserPromptSubmit 的 `replace`（hooks.go:14-17）——语音侧可借此做"口语→指令"规整而不必改 agent 代码。

## 4. 产品形态三案对比

| 案 | 形态 | 工程量 | 依赖 | 体验上限 | 结论 |
| A 飞书语音消息壳 | 复用飞书语音收发+卡片 | 小 | 绑死飞书生态 | 语音留言级，非对话 | 排除：违反"不依赖飞书生态做语音收发"红线，且异步留言不构成 Jarvis 入口 |
| B 本地 mmx 半双工 | 按键说话→mmx transcribe→runebot stream→mmx synthesize --stream 播放 | 1–2 天出 P0 | 全部现有：mmx+runebot+afplay/sox | 按轮对话，感知延迟 ≈1.6s+LLM | **推荐起步**：唯一纯现有零件可达的路径 |
| C 全双工实时对话 | 常开麦+AEC+流式ASR+语义 turn detection+barge-in | 周级起 | mmx CLI 不支持流式 ASR，需 MiniMax WebSocket API 或 Deepgram/whisper.cpp；骨架 LiveKit Agents/Pipecat | 800ms 级自然对话 | 终态，但不应绕过 B 直接上 |

## 5. 推荐路线与分期

- **P0 半双工实验（1–2 天）**：外部录音(sox/ffmpeg avfoundation)→mmx transcribe→POST /v1/chats/{id}/stream→消费 NDJSON delta(累积快照做 diff)→mmx synthesize --stream 管道播放。验收：说一句中文→回话端到端 ≤5s；中文转写准确；"派个任务"经 fleet dispatch 或 task 工具走通。
- **P1 半双工产品化**：固定语音指令集（发消息/取消/answer/查任务）；按钮或快捷键发 cancel 打断；SOUL.md 落地 Jarvis 人设；memory 注入口语偏好。验收：打断闭环可用（播放中 cancel→停播→新轮次）；ask 权限确认可口头 yes/no。
- **P2 真双工**：语音侧换 LiveKit Agents 或 Pipecat 骨架（Python sidecar），VAD=silero，turn detection=LiveKit TurnDetector/smart-turn，ASR=MiniMax WS 或 Deepgram，外放必须 webrtc-audio-processing AEC；runebot 不变只作 LLM 节点。验收：常开麦自由插话；端到端 voice-to-voice ≤800ms 常态、<2s 上限。
- **风险**：mmx 全系走 MiniMax 云（非本地），离线/数据主权敏感场景 P2 需本地 ASR/TTS 替代（whisper.cpp 等）；依赖外部网络。

## 附：原始调研文件
任务目录 /Users/alfredchaos/home/agent-default/fleet/tasks/261012-215847-voice-ai-research/ 下 industry-research.md（业界全景全量）、mmx-test.md（实测全记录）、integration-notes.md（接缝 file:line 全索引）。
