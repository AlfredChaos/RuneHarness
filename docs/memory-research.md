# RuneHarness 记忆研究：业界盘点、现状审计与下一代方案

- 文档日期：2026-10-11。
- 证据分两类：RuneHarness 论断标 `文件:行号`（来自源码逐行审计），业界论断标 `[n]`（来源见第 4 章）。
- 调研产物：`industry-frameworks.md`（6 框架矩阵）、`industry-patterns.md`(模式分类学与评估)、`source-audit.md`（源码审计全文）。

---

## 0. 结论先行

- 推荐：双层记忆（情景 episode 层 + 语义 bitemporal 层），配显著性打分召回与 ADD-only 冲突标记，两阶段落地，P1 不建图（详见 ADR-2026-001）。
- 理由一：时间性建模已是 2025–2026 记忆系统的分水岭，Zep 与 Mem0 都收敛到"失效不删 + 读取重排"[4][13][14]。
- 理由二：RuneHarness 的 dream 写路径已是离线消化形态，离线预计算的收益（约 5 倍测试时算力节省）已被论文证实[10]，补时间性与打分即可兑现。
- 理由三：记忆行数百级的规模假设（docs/runeharness-memory-plan.html:774）撑不起图存储，`SQLite FTS5` + 实体匹配的检索已够用，图谱是 P2 的可选项而非 P1 的必需品。

---

## 1. 业界盘点

### 1.1 压缩对比表（6 框架 × 5 维度）

阅读口径：各家 benchmark 数字不可横向直比，引用时带出处与口径（见 1.2 第 5 条）；`[n]` 对应第 4 章参考来源；RuneHarness 侧代码引用统一在 §2 与 §3 给出。

| 框架 | 时间性建模 | 冲突处理 | 写入策略 | 读取召回 | 代表启示 |
|---|---|---|---|---|---|
| Mem0 | 2026 Temporal Reasoning：写入打时间签名 + `state key`，旧状态自动 `event_end`，零删除；Memory Decay 按最近访问 0.3×–1.5× 重排 [4][5] | 从 LLM-resolve（ADD/UPDATE/DELETE）反转为 ADD-only：新旧并存，靠时间重排保当前态；knowledge-update 仍是最难类（LongMemEval 93.6）[1][3][4][7] | 2026 单遍 ADD-only 提取 + 哈希去重 + 实体链接，异步写入，提取延迟约减半 [3][6] | semantic + keyword + entity + temporal 四路融合打分，top-K 注入，约 7k tokens/查询 [3][6][7] | 消解可以推迟到读取端；调和式改写被官方判定为"破坏上下文"[3] |
| Letta / MemGPT | 无事实有效期；MemFS 每次编辑一次 git commit，历史即"过去"[12] | 无自动机制：agent 工具改写 + git 版本化兜底 [9][12] | 显式 `/remember` + agent 工具编辑 + dreaming 睡眠代理离线整理（anytime 更新）[10][11] | 根记忆文件每轮注入 system prompt，其余按文件树按需读 [11][12] | 常驻块 + 按需外存的二级结构；离线整理不阻塞主代理 [10][11] |
| Zep / Graphiti | bi-temporal 四时间戳：`t_valid`/`t_invalid` + `t'_created`/`t'_expired`；LLM 时间抽取可修正 [13][15][16] | 边失效：LLM 判定矛盾 → 旧边置 `t_invalid`（不删除）→ 事实文本改写为过去叙事 [13][14][16] | episode 增量入图：实体抽取 → 消歧 → 事实抽取去重 → 时间抽取 → 边失效，多 LLM 异步流水线 [13][14] | cosine + BM25 + BFS 图遍历混合检索，多档 reranker，top 边 + 实体摘要注入 [13][14] | episode/语义双层 + 事实回溯到 episode；LongMemEval +18.5%、延迟 -90% [13] |
| LangMem | 无时间机制，仅"强度 = 最近/频繁使用"的设计取向 [18] | LLM 判定 delete/invalidate/update/consolidate；profile 形态天然覆盖 [18] | hot path（即时抽取）与 background（会话后反思）双模式，LLM enrichment 平衡新建/合并 [18][19] | namespace 多级分域；profile 直查注入 / collection 语义 top-k [18] | 显式 semantic/episodic/procedural 三分法；hot/background 分离是显式 API 设计 [18][19] |
| Memobase | 事件时间线 + `time_range_in_days` 时间窗过滤；profile 槽位仅当前值，无有效期 [22][23][24] | 槽位 latest-wins 覆盖，历史留事件线，无仲裁 [20][22] | 用户级 buffer 批量提炼（固定 3 次 LLM 调用，token 成本降 40–50%），写入前 schema 校验 [20][22] | `context()` 把画像 + 相关事件渲染成 prompt 片段，支持 `max_tokens` 限长与配比调节 [24] | 画像 + 事件线的双物模型；在线读取只需几次 SQL [20] |
| MemoryOS | 无事实有效期；Heat 公式含访问时间衰减 `exp(-Δt/μ)` [25] | 未描述；KB/traits 靠 FIFO 容量淘汰 [25] | STM 满页迁 MTM → 相似度分段聚合 → Heat 淘汰 → 高热段 LLM 抽取 90 维 traits/KB [25] | STM 全量 + MTM 两段式 top-m/top-k + LPM top-10 合并单次 prompt，检索后回写热度 [25] | 访问热度驱动的分层淘汰；LoCoMo F1 +49.11%（自报口径）[25][26] |

### 1.2 跨框架结论

1. **invalidate-not-delete 已成主流**。Zep 边失效"old facts are invalidated — not deleted"[14][16]；Mem0 时间签名"零删除"、旧状态写 `event_end` 而非抹除[4]；XTDB 的 bitemporal 文档历史是同一模式的数据库原生形态[32]。物理删除只留给显式用户指令。
2. **Mem0 从 LLM-resolve 反转为 ADD-only + 时间重排**。2025 论文版让 LLM 对新旧事实做 ADD/UPDATE/DELETE 调和[1]；2026 版整体移除该步骤，官方理由是调和"destroyed context"（覆盖抹掉关键信息、删除误删未来有用的信息）[3]。消解责任转移到读取端的时间重排[4]。写入端只做加法正在成为新的默认[40]。
3. **bitemporal 是时间性事实的标准形态**。valid-time（现实世界何时为真）与 transaction-time（系统何时记录）正交[32]；Zep 每条边存四个时间戳并支持"现在真/过去任一时点真"两类查询[13][15][16]。落地最小集是 `valid_from / valid_to / recorded_at` 三元组[32][14]。
4. **写入全面离线化**。Letta dreaming[11]、Memobase buffer（固定 3 次 LLM 调用）[20]、Mem0 异步提取[3]、MemoryOS FIFO 级联[25]、ChatGPT dreaming 后台进程[35]，全部把 LLM 密集整理挪出在线路径（2026 综述已把记忆形式化为"写—管—读"闭环，管理整体后移[38]）。sleep-time compute 论文证明离线预计算可把同等准确率的测试时算力降约 5 倍[10]。
5. **基准数字不可横向直比**。Mem0 官方自认 LoCoMo/LongMemEval 协议不统一、可被激进检索策略刷高[2][7][27]；各家"LoCoMo SOTA"分别用了不同 judge、不同检索深度与不同复跑口径（Mem0 66.88→92.5、Memobase 75.78、MemoryOS F1 +49%）[1][21][25]。
6. **情景与语义双层是结构性共识**。类型学框架出自 CoALA 的 working/episodic/semantic/procedural 四分法[28]；Zep 的 episode 子图自比 episodic 记忆、事实必须回溯到 episode[13][14]；LongMemEval 实测"压缩成单条事实会损害整体性能（多跳除外），round 才是最优存取粒度"[30][31]；MIRIX 把 Episodic 做成独立存储组件[39]；ChatGPT 的 saved memories 与 reference chat history 也是并行双来源[35]。
7. **认知三分法多是叙事，落地者少**。Mem0 的 semantic/episodic 枚举未接线、只有 `procedural_memory` 真正实现[8]；Zep 显式承认无 procedural 对应物[13]；Letta 按"上下文位置"分层而非认知类型[9][11]。类型学应服务于注入与检索策略，不应直接映射成表结构。
8. **评估应从第一天内建**。LongMemEval 的五能力（抽取/多跳/时间/更新/弃答）是现成的维度框架[30]；Mem0 官方评估页公开协议并自曝局限[2][7]；把五能力子集与 token/延迟预算嵌进 CI 防回归，是所有框架的共同缺口与机会[27][37]。

### 1.3 模式谱系对照（调研分类学 × RuneHarness 现状 × ADR 取舍）

下表把 `industry-patterns.md` 的写入/读取/冲突三谱系映射到代表框架、RuneHarness 现状与 ADR-2026-001 的取舍，作为第 2、3 章的索引。

| 谱系 | 模式（成本从低到高） | 代表实现 | RuneHarness 现状 | ADR-2026-001 取舍 |
|---|---|---|---|---|
| 写入 | store-all 全存 | MemGPT archival [9]；Claude memory tool [44] | 无：dream 只存提炼条目（`dream.go:214-217`） | 不采纳 |
| 写入 | salience 过滤（写时打分） | Generative Agents importance [29] | 无 | P1 采纳：`importance 1-10` |
| 写入 | LLM distill 内联蒸馏 + 写时裁决 | Mem0 论文版 ADD/UPDATE/DELETE [1]；LangMem hot path [18] | 近似：dream 提取，矛盾判断在提示层（`dream.go:236-238`） | 改 ADD-only，裁决移到读取端 |
| 写入 | 异步/离线消化 | Letta dreaming [11]；Memobase buffer [20]；ChatGPT dreaming [35] | 已具备：dream 三触发器（`dream.go:36-85`） | 保持 |
| 读取 | 全量注入 | full-context 基线（26k tokens、p95 17s）[1] | 无 | 不采纳 |
| 读取 | 常驻块 + 按需外存 | MemGPT/Letta blocks [9][11] | 近似：`InjectOnce` + `memory read`（`memory.go:76-96`、`tool.go:134-140`） | 保持；预算改 token |
| 读取 | RAG top-k 检索注入 | Mem0 多信号融合 [1][6] | 无：按 `updated_at` 序注入（`inject.go:27-45`） | P1 采纳：FTS5 + 实体打分 top-k |
| 读取 | 图遍历多跳 | Graphiti BFS [14]；HippoRAG [43] | 无 | P2 备选 |
| 读取 | 分层摘要（reflection） | Generative Agents [29]；MemoryBank 日摘要 [33] | 无：dream 只出条目，无摘要层 | P1 采纳：episode 摘要行 |
| 冲突 | LWW 同键覆盖 | LangGraph Store put [41]；Memobase profile [22] | 是：`memory save` 同名覆盖（`tool.go:93-111`） | 收敛为"新增 + 标记旧条目失效" |
| 冲突 | LLM-resolve 写时裁决 | Mem0 论文版 [1]；LangMem invalidate/update [18] | 部分：dream supersede 提示 + 校验（`dream.go:236-238,332-340`） | 放弃写时裁决，改 ADD-only |
| 冲突 | supersede 审计链（失效不删） | Graphiti 边失效 [14][16]；XTDB [32] | 雏形：`memory_log` 有流水、缺结构化链（`sqlite.go:268-301`） | P1 采纳：`valid_to + superseded_by` |
| 冲突 | 置信度/来源加权 | 无主流开源实现 [未核实：两份调研均未见到落地案例] | 无 | 不做 |

---

## 2. RuneHarness 现状与缺口

### 2.1 现状

记忆层是单文件 SQLite 的 flat 模型。`memories` 表以 `(tenant_id, space, name)` 为主键存当前信念（`internal/memory/sqlite.go:62-77`）；`memory_log` 记 op 级审计流水（`internal/memory/sqlite.go:78-91`）。写路径双轨：`memory` 工具直写与 dream 离线消化（手动 / 12 分钟 ticker / 退出收尾，`internal/memory/dream.go:26,36-85,429-466`）。读路径 `InjectOnce` 把索引加常驻正文注入首条 user 消息（`internal/memory/inject.go:20-81`），排序按画像分组加 `updated_at DESC`（`internal/memory/inject.go:27-45`、`internal/memory/sqlite.go:126-128`）。

### 2.2 缺口表（源码审计 14 条缺口合并为 10 行）

合并规则：审计原文 14 条按成因归类（时间性 2 条、类型分层 1 条、打分 2 条、冲突 2 条、去重 1 条、召回 2 条、隔离 1 条、健壮性 3 条），完整证据链见 `source-audit.md` 第 6 章。

| # | 缺口（审计证据） | 业界对照 |
|---|---|---|
| 1 | 无时间性字段：`Row` 仅 created_at/updated_at，无 valid_from/valid_to/superseded_by/last_confirmed（`store.go:26-36`、`sqlite.go:62-73`） | Zep/Graphiti bi-temporal 四时间戳 [13][15]；XTDB [32] |
| 2 | supersede≡upsert、merge-into≡delete，存储层无专属语义；旧信念仅存 `memory_log.old_body`；merge-into 的"合并进目标"靠模型自觉（`sqlite.go:268-301`、`dream.go:386-389`） | Graphiti 边失效 + 过去叙事改写 [14][16]；Mem0 旧状态 `event_end` 闭合 [4] |
| 3 | 无内容级去重：无 embedding/FTS/哈希查重（`internal/` 全目录 grep 零命中）；同名主键覆盖是唯一硬机制（`sqlite.go:74,278`） | Mem0 提取后哈希去重 + 实体链接 [3]；Graphiti 边去重限同实体对防误合 [13] |
| 4 | 无显著性/重要性打分：注入排序仅画像分组 + `updated_at DESC`（`inject.go:27-45`、`sqlite.go:126-128`） | Generative Agents `recency+importance+relevance` 三分量打分 [29]；Mem0 Decay 访问衰减 [5]；MemoryBank Ebbinghaus 保留度 [33] |
| 5 | 注入配额是字节不是 token：8192/16384/4096B 三级（`profile.go:81`、`inject.go:12,41-44,74-76`） | Mem0 以 memory tokens 披露注入预算 [1]；Letta 常驻块设 token 上限 [9] |
| 6 | 工具直写无冲突处理：`memory save` 同名直接覆盖、reason 非必填；冲突消解只在 dream 提示层（`tool.go:42,84-111`、`dream.go:236-238`） | LangMem 显式 invalidate/update 决策 [18]；Claude memory tool 把改写权交给 agent [44]；冲突消解有专门能力基准 [37] |
| 7 | 无情景层：原始会话行被 dream 消费后记忆侧无留存（`dream.go:116-130`），`memory_log` 仅 op 审计（`store.go:47-55`） | Zep episode 层事实可回溯 [14]；MIRIX 独立 Episodic 组件 [39]；LongMemEval round 粒度最优 [30][31]；Mem0 的 semantic/episodic 枚举同样未接线 [8]；episodic 缺失论 [42] |
| 8 | 无相关性召回：检索 API 缺失，`GET /svc/search` 501（`gateway/handlers.go:370-371`）；非常驻正文靠模型主动 read（`memory.go:117`） | Mem0 四路融合 top-K [3][6]；Graphiti cosine+BM25+BFS [13][14] |
| 9 | subject/tenant 空间会话归属未实现：`inSpace` 恒 true，本地 SubjectID 恒空退化为单用户空间（`dream.go:141-152`、`store.go:112-116`、`main.go:106-107`） | 按 `user_id` 分域是标配 [6]；LangGraph namespace 多级分域 [18][41] |
| 10 | 健壮性缺口：熔断无半开恢复、四处 SetMeta 吞错、存储层 `ApplyOps` 不扫凭据（`dream.go:89-93,185-186,417-425`、`sqlite.go:236-304` vs `tool.go:103-105`） | 业界框架未公开对应实现细节 [未核实]；静默吞错与防线位置属内部工程问题 |

### 2.3 现状保留项（已对齐业界模式的实现，换代时应保持）

1. **写入离线化**：dream 的手动 / ticker / 退出收尾三触发器与业界 sleep-time/dreaming 同型[10][11][35]，全套门控（锁、熔断、冷却、物料）与逐会话水位已可用（`dream.go:36-100,429-466`）。
2. **append-only 审计**：`memory_log` 已具备 supersede 审计链的原料，只是缺结构化 `superseded_by`（`sqlite.go:78-91,236-304`）[14]。
3. **失效不删的雏形**：delete/merge-into 走流水 + tombstone 防复活（`sqlite.go:289-295,335-361`、`dream.go:261-266`），方向与 invalidate-not-delete 一致[14][32]。
4. **常驻 + 按需外存**：注入骑 Fold 首部（`memory.go:74-96`、`internal/compact/segment.go:7-8`）近似 Letta core blocks 模式[9][11]；`memory read/about/history` 提供按需外存通道。
5. **画像驱动的类型辞典与空间三档**：`Profile` 决定类型/空间/写工具/节奏（`profile.go:29-45`、`store.go:16-23`），换场景不改机制[19]。

---

## 3. ADR-2026-001 时间性记忆层

- 状态：提案
- 日期：2026-10-11
- 关联：ALF-111 eval、`docs/runeharness-memory-plan.html`、`docs/runeharness-memory-tradeoffs.html`

### 3.1 背景与约束

1. 单用户本地 SQLite：`TenantID=local`、`SubjectID` 恒空，subject 空间退化为 `subject/local-user` 单空间（`main.go:106-107`、`store.go:112-116`）；单写者串行化（`sqlite.go:43`）。
2. 无 embedding/FTS/向量依赖：`internal/` 全目录 grep `embedding|fts|vector|cosine` 零命中；检索 API 只有按名取与实体 LIKE（`sqlite.go:162-172,215-220`），`GET /svc/search` 直接 501（`gateway/handlers.go:370-371`）。
3. 规模假设是单空间行数百级（docs/runeharness-memory-plan.html:774），v1 明确放弃 embedding 检索。
4. 已有 `memory_log` append-only 审计流水与 tombstone 防复活机制（`sqlite.go:78-91,335-361`），可重放是既定能力。
5. dream 写路径已离线化：手动 + 12 分钟 ticker + 退出收尾（`dream.go:26,36-85,429-466`），与业界 sleep-time 方向一致[10][11][35]。
6. 读路径骑 Fold 首部常驻（`memory.go:74-96`、`internal/compact/segment.go:7-8`），配额为字节且三级截断（`inject.go:12,41-44,74-76`）。
7. 云端 gateway 已在 scope 携带 `TenantID+SubjectID`（`gateway/handlers.go:247-250`），但 subject 的会话归属未实现，`inSpace` 对 subject/tenant 空间恒 true（`dream.go:141-152`）。
8. 仓库维护约定：删除优于兼容，不留兼容层（本次任务给定；`AGENTS.md:62-76` 记忆层条目未设兼容层条款）。

### 3.2 备选方案

#### 方案 A：最小演进（flat + 有效期 + LWW）

- 做法：`memories` 加 `valid_from/valid_to/last_confirmed` 三列。同名覆盖或 supersede 时旧条目置 `valid_to=now`。查询默认过滤 `valid_to IS NULL OR valid_to > now`。
- 优点：改动最小，一个 migrate 分支加一处查询条件；保留审计流水；解决"改口后旧事实仍被注入"。
- 缺点：检索弱与语义重复原样保留；LWW 不区分语义矛盾与补充更新；无打分召回。
- 成本：约 1–2 人日；无新依赖。
- 业界先例与适用前提：Memobase profile 槽位 + LangGraph Store put 同属此档[22][41]；适用于"记忆只服务注入、查询永远按名/按实体"的现状冻结假设。一旦引入检索，LWW 的信息损失立即显形。
- 验证钩子：只需 3.5 表第 2/3 行（矛盾率、陈旧率）即可判定，成本最低。

#### 方案 B：全量时序知识图谱（Graphiti 式）

- 做法：episode 节点 + 实体节点 + 事实边四时间戳 + 边失效 + 图遍历召回，镜像 Zep 架构[13][14][15]。
- 优点：天花板最高；多跳与时序演化推理有实证收益（Zep LongMemEval +18.5%）[13]；episode 溯源完整。
- 缺点：引入图存储依赖（Neo4j/FalkorDB 等）[14]；写入是多 LLM 异步流水线，图构建有小时级后台消化延迟[1][13]；与单文件 SQLite 部署和行数百级规模不匹配；边失效存在全图误杀的已知缺陷[17]。
- 成本：数周起，另加图库运维面。
- 业界先例与适用前提：适用于百万级事实、多实体多跳查询密布的场景[15][17]；行数百级时其检索收益与 FTS5 差距未被任何公开基准证实[2][27]。
- 验证钩子：需 3.5 表第 1/8/9 行全量（recall、多跳、弃答），并单独计量图构建延迟——这是 Mem0 论文对 Zep 的核心批评点[1]。

#### 方案 C：双层记忆（episode 层 + bitemporal 语义层）

- 做法：
  1. 情景事件层：episode 行 = 会话行区间引用（`[session_id, from_id, to_id]`）+ LLM 摘要。原始行留会话库不复制（`session/sqlite/sqlite.go:92-95` append-only）；dream 游标已按行 id 记区间（`dream.go:122-130`），引用天然可得。
  2. 语义层升级 bitemporal：`valid_from/valid_to/recorded_at/superseded_by`，失效不删[14][32]。
  3. 显著性：写入时 LLM 打 `importance 1-10`[29]；读取回写 `access_count/last_accessed`，排序含衰减项[5][29][33]。
  4. 读侧打分召回 top-k：relevance 用 SQLite FTS5 或实体匹配，无向量依赖；注入按分数截断。
  5. 写侧 ADD-only 冲突标记：LLM 判矛盾 → 旧条目置 `valid_to + superseded_by`，不删不改写[3][4][14]。
  6. 注入预算字节改 token：复用 `compact.TextTokens`（`internal/compact/estimate.go:44-56`）。
- 优点：对齐业界全部主流模式（双层[14][39]、失效不删[4][14]、打分召回[29]、离线消化已有）；全程 SQLite 可实现（FTS5 内建）；episode 溯源让蒸馏错误可回查[42]。
- 缺点：改动面最大（schema、注入、dream、工具、gateway 全动）；FTS5 中文分词弱；importance 评分依赖 LLM 输出稳定性。
- 成本：P1 约 1–2 人周；P2 另计。
- 业界先例与适用前提：episode/语义双层取自 Zep[13][14] 与 MIRIX[39]；"失效不删 + 读取重排"取自 Mem0 2026 转向[3][4]；打分召回取自 Generative Agents 三分量[29]与 Memory Decay[5]。适用前提：单空间行数百级、查询以"当前态事实"为主，多跳需求未证实。
- 验证钩子：9 项指标全量（3.5 表），其中第 1/2/3/5/6 行是 P1 验收硬项。

### 3.3 决策

推荐方案 C，分两阶段：

- **P1**：flat 表 bitemporal 化 + 打分召回 + ADD-only 冲突标记 + episode 区间引用层。不建图，不引向量。
- **P2**：仅当召回指标证伪"多跳需求存在"时，才升级实体图。

阶段验收条件：

- P1 验收：`schemaVersion=2` 迁移在旧库副本上通过；矛盾率与陈旧率相对 P0 基线下降且多跳子集不回退（指标见 3.5）；CI 门（fake 回放）全绿。
- P2 触发条件：P1 上线一个迭代周期后，recall@5 仍低于金集基线的 80%，且失败案例中多跳类占比过半——此时才评估实体图[43]；否则图永远不进路线。

P1 实施顺序（依赖驱动）：

1. 迁移与回填（3.4）先行——后续全部工作依赖新列。
2. 写侧 ADD-only 标记（`dream.go` 契约 + `validateOps` + `tool.go`）第二——先保证新数据带时间性。
3. 读侧打分召回（FTS5 + `importance` + 注入 token 预算）第三——此时已有足够带时间性的数据可检索。
4. episode 层最后——它依赖前两步稳定，且受开放问题 6 约束。

理由（逐条落证据）：

1. 时间性是一等公民已成定局：Zep bi-temporal 与 Mem0 Temporal Reasoning 在 2025–2026 先后落地，且都收敛到"失效不删 + 读取重排"[4][13][14][15]。
2. ADD-only 修正了 LLM-resolve 的已证缺陷：Mem0 官方承认调和步骤破坏上下文[3]，knowledge-update 仍是其最难类别（LongMemEval 93.6，低于其他类）[7]。一次写入时的裁决不可信，标注失效比改写安全。
3. 离线消化收益已证实：sleep-time compute 论文给出约 5 倍测试时算力节省与多查询摊销 2.5 倍成本下降[10]；RuneHarness dream 已是该形态（`dream.go:36-85`），补时间性与打分即可吃到收益，ChatGPT dreaming 同路线[35]。
4. 粒度教训：LongMemEval Finding 1 证明 round 级历史优于压缩成单条事实（多跳除外）[30][31]，episode 区间引用层直接采纳该结论；纯事实蒸馏会丢事件结构[42]。
5. 图谱在当前规模是杀鸡用牛刀：单空间行数百级（docs/runeharness-memory-plan.html:774），FTS5 + 实体匹配够用；图遍历召回（HippoRAG 式）的价值要在海量语料下才显现[43]；小基准分数可刷、不可作为建图依据[2][27]。
6. 保留升级路径：P1 的 `superseded_by` 边与实体标签就是 P2 图谱的边表与节点表，升级是加索引不是换模型。

同时记录"不做的决定"与各自的证伪条件（Negative decisions）：

| 不做 | 理由 | 证伪条件（何时重新评估） |
|---|---|---|
| P1 不引 embedding 向量 | 行数百级（docs/runeharness-memory-plan.html:774），FTS5 + 实体匹配可达；向量库破坏单文件部署 | FTS5 召回 recall@5 连续低于判定线（3.5 表第 1 行），且失败案例为同义改写类 |
| 不做置信度加权合并 | 业界无成熟先例 [未核实]；`importance` 已覆盖排序需求 | MemoryAgentBench 类冲突消解子集[37]成为硬需求且打分无法区分 |
| 不做多屋空间（private/team） | 沿设计文档未决项（docs/runeharness-memory-plan.html 第 15 章）；subject 归属本身未实现（`dream.go:141-152`） | 云端多租户落地且出现真实共享需求 |
| 不改 `memory_log` 结构 | 流水已满足审计与回填需求（3.4）；变更会破坏既有 `History` 消费方（`tool.go:162-176`、gateway） | 回填性能实测不可接受（百万行级流水才可能出现） |

### 3.4 迁移路径（memory.db 现有数据）

1. `schemaVersion 1→2`：在 `migrate` 追加 `v<2` 分支（`sqlite.go:18,52-110` 现有模式）。DDL：

   ```sql
   ALTER TABLE memories ADD COLUMN valid_from     INTEGER NOT NULL DEFAULT 0;
   ALTER TABLE memories ADD COLUMN valid_to       INTEGER;
   ALTER TABLE memories ADD COLUMN superseded_by  TEXT NOT NULL DEFAULT '';
   ALTER TABLE memories ADD COLUMN last_confirmed INTEGER;
   ALTER TABLE memories ADD COLUMN importance     INTEGER NOT NULL DEFAULT 5;
   ALTER TABLE memories ADD COLUMN access_count   INTEGER NOT NULL DEFAULT 0;
   ALTER TABLE memories ADD COLUMN last_accessed  INTEGER;
   CREATE VIRTUAL TABLE memories_fts USING fts5(name, descr, body, content='');
   ```

   `memory_log` 结构不变。

2. 回填规则：`valid_from = created_at`（列已存在，`sqlite.go:72`）；supersede 链从 `memory_log` 折叠回填——按 `History` 的 `id ASC` 遍历每个 name，遇到 `op='supersede'` 流水时，把覆盖前状态置 `valid_to=该流水 created_at`、`superseded_by=新条目 name`（流水查询模式见 `sqlite.go:306-331`）；tombstone 从 `RecentDeletes` 的同款查询导入 `valid_to=delete 流水 created_at`（`sqlite.go:335-361`）。
3. 迁移策略：本地单用户文件库，采用"备份 + 就地迁移 + 删除旧语义"。`Open()` 在 migrate 前复制 `memory.db → memory.db.bak-schema2`，成功后不保留旧语义代码路径，不留兼容层（符合第 3.1 条第 8 款约定）。
4. 迁移校验与回滚：迁移事务内校验"行数一致 + 每行 `valid_from` 非空"，校验失败即回滚迁移并保留 `.bak`；运行期发现异常时，删除新库、还原 `.bak` 即回到 schema 1（旧二进制可读，`schemaVersion` 判定见 `sqlite.go:52-60`）。
5. 迁移后检查清单（一次性命令或 `go test` 子用例）：每行 `valid_to > valid_from` 或为 NULL；`superseded_by` 指向的 name 存在或为空；tombstone 名单与 `RecentDeletes` 输出一致；FTS 虚表行数与 `memories` 有效行一致。
6. 风险与信息损失：旧流水里 `merge-into` 的语义不可重建——"目标条目内容已被模型合并过"这件事没有结构化记录（`sqlite.go:289-295` 只删来源条目）。迁移接受该损失：来源条目置 `valid_to=merge 时刻`，目标条目 `superseded_by` 留空，链从此处断开。新写入走 ADD-only 标记后不再产生此损失。

### 3.5 验证指标（挂 ALF-111 eval）

可达性检查结论（写指标前核实）：

- token 估算**已存在**：`compact.TextTokens`（ASCII 按 4 字符 1 token、非 ASCII 按 0.7 token/字，`internal/compact/estimate.go:44-56`）；`Estimate` 支持 usage 锚点（`estimate.go:17-23`）。注入 token 成本指标可直接复用，无需新分词器。
- e2e harness 形态**已有两种**：`cmd/e2e` 是"真 LLM + sqlite + 脚本灌水"的单文件驱动（装配与事件打印见 `cmd/e2e/main.go:1-4,46-120`），面向压缩链路；`internal/memory/e2e_real_test.go` 已有真端点 dream 测试，`RUNE_E2E=1` 门控 + `logLLM` 原始输出留痕 + 矛盾 supersede 场景（`e2e_real_test.go:3-6,27-40,115-171`）。指标挂载沿用这两条形态。

| # | 指标 | 可操作度量法 | 判定线（P1 验收口径） | 挂载点 |
|---|---|---|---|---|
| 1 | recall@k | 自标金集："该问题应命中的记忆条目"；对打分召回的 top-k 统计命中率 | recall@5 ≥ 0.8 且不低于 P0 基线 | CI：fake-LLM 回放（`memory_test.go:44-57` 替身模式）；真端点：`cmd/e2e` 加记忆场景 |
| 2 | 矛盾率 | 统计"同时 `valid_to IS NULL` 且语义互斥"的条目对占有效条目比例；LLM 冲突检测器按冲突分类提示词[36] | 较 P0 基线下降，且 e2e 改口场景 0 残留 | 真端点：扩展 `e2e_real_test.go:151-171` 苹果改口场景为批量化 |
| 3 | 陈旧率 | 被召回条目中查询时刻 `valid_to < now` 的占比 | P1 目标 < 5%；代理指标 = LongMemEval knowledge-update 子集[30][31] | `cmd/e2e` 脚本化"改口轮 + 追问答题" |
| 4 | supersede 链完整性 | 抽样沿 `superseded_by` 走链，统计断链率与环率 | 新写入断链率 = 0；存量断链仅限 merge-into（3.4 第 5 条） | CI：纯 sqlite 单测（fake store，无需 LLM） |
| 5 | 注入 token 成本 | `renderInject` 输出过 `compact.TextTokens`（`estimate.go:44-56`）记均值/p95 | p95 ≤ 4k tokens（对照现 8192B 字节口径的实际值） | CI：fake 回放断言 token 预算上限 |
| 6 | dream 提取精度 | FactScore 风格原子事实 precision：蒸馏条目逐条对照原始区间行[34] | precision ≥ 0.85；多会话推理子集不回退 | 真端点：`RUNE_E2E=1` 场景 + episode 区间引用使对照可行 |
| 7 | 写入延迟 | `Dream()` 与 `ApplyOps` 计时 p50/p95（`Stats` 已有 Rows/OpsApplied，`dream.go:71-76`，可挂时长字段） | 退出收尾 p95 仍在 30s 预算内（`dream.go:27,453-466`） | `cmd/e2e` 打点输出 |
| 8 | 多跳不回退 | LongMemEval 多跳子集上线前后对比，防止 episode 层压缩伤多跳[30][31] | 相对 P0 基线不下降 | `cmd/e2e` 或独立 eval 脚本 |
| 9 | 弃答正确性 | LoCoMo adversarial（期望拒答）与 LongMemEval abstention 子集：该拒答时拒答=对，幻觉作答=错[30][34] | 不低于 P0 基线 | `cmd/e2e` 追加对抗问答轮 |

口径统一说明：

1. token 一律走 `compact.TextTokens`（`estimate.go:44-56`），全表一致，禁止与字节数混排。
2. 时间戳一律 unix 秒（与 `memories.created_at` 现口径一致，`sqlite.go:72`）。
3. LLM-as-a-Judge 固定同一模型与同一提示词模板，避免 judge 漂移；跨家数字仅作背景，不作验收（1.2 第 5 条[2][27]）。
4. 每次 eval 运行把 `Stats`（`dream.go:71-76`）与指标 JSON 一并落盘，保证可复盘。

CI 门用 fake-LLM 回放（替身已存在，`memory_test.go:44-57`）；真端点走 `cmd/e2e` 或 `RUNE_E2E=1` 手工运行（与现状约定一致，`e2e_real_test.go:3-6`）。

P0 基线采集（指标对比的分母）：

1. 在当前 main 分支跑同一套 e2e 脚本，采矛盾率、陈旧率、recall@5、注入 token 成本四项现值，存为基线 JSON。
2. 脚本素材：coding 画像下 5 组多会话对话（每组含一次刻意改口轮），沿用 `e2e_real_test.go:115-171` 的种数据 + 追问形态。
3. fake 回放门只做回归断言（判定线 3.5 表），不做绝对质量判断——绝对值一律以真端点为准。

### 3.6 P1 改动面清单（评审用，按文件）

| 文件 | 改动 | 涉及现状 |
|---|---|---|
| `internal/memory/sqlite.go` | migrate 加 `v<2` 分支；查询默认过滤 `valid_to`；`ApplyOps` 支持 invalidate 语义；新增 FTS5 虚表与打分查询 | 现有迁移框架 `sqlite.go:52-110` |
| `internal/memory/store.go` | `Row`/`Header` 加时间性与显著性字段；`Store` 接口加打分召回方法 | 接口现状 `store.go:82-105` |
| `internal/memory/dream.go` | `opsContract` 换 ADD-only + 矛盾标记语义；`validateOps` 校验新字段；episode 行写入 | 契约现状 `dream.go:214-217,306-375` |
| `internal/memory/inject.go` | 预算字节改 `compact.TextTokens`；注入列表改打分序 | 现状 `inject.go:20-81` |
| `internal/memory/tool.go` | `save` 传 importance；返回冲突标记结果 | 现状 `tool.go:82-113` |
| `internal/gateway/handlers.go` | `GET /svc/search` 从 501 改接打分召回 | 现状 `handlers.go:370-371` |
| `main.go` / `cmd/runebot/main.go` | 无装配结构变化；仅 `memory.Config` 传新开关 | 装配现状 `main.go:125-152` |
| `internal/tui/`、`internal/compact/`、`internal/session/` | 不改 | `compact` 只被复用 `TextTokens`（`estimate.go:44-56`）；session 只被 episode 引用（`session/sqlite/sqlite.go:92-95`） |

### 3.7 开放问题

1. subject 归属依赖 `session.Meta` 增加 `SubjectID` 列（`dream.go:142-146` 已留扩展点；docs/runeharness-db.html:172-174 已规划该列）。云端落地前，subject 空间的 dream 消化范围不可信。归属：session 存储侧独立任务，不阻塞 P1（coding/generic 画像走 workspace 空间不受影响）。
2. `BodyBudget=24576` 字节天花板（`profile.go:82,127,162`）：episode 摘要进 dream 输入后，预算口径应否同步改 token（同第 3.2-C-6 条）。归属：P1 实现内一并处理，无独立任务。
3. embedding 的引入时点与阈值误合并：Graphiti 存在失效检索全图误杀的 open bug[17]，P1 不引入向量即规避；P2 评估时需先回答阈值问题。归属：P2 触发条件满足后另立 ADR。
4. FTS5 中文分词局限：默认 tokenizer 不切中文词，实体匹配能否兜住中文召回需 P1 实测；不行则需评估可分词方案。归属：P1 第一个 spike，直接决定检索路线。
5. 熔断无半开恢复（`dream.go:89-93,417-425`）：时间性层写入更频繁后，一次坏模型输出是否会长期冻结自动消化，需定自动恢复策略。归属：独立小任务，宜与 P1 同期。
6. episode 区间引用的稳定性：会话压缩会把中段卸载为 `blob://` 指针（`internal/memory/render.go:19` 注释、`internal/compact/offload.go`），episode 行引用的原始行 id 区间在卸载后是否仍可解析 [未核实：blob 指针长期稳定性不在本次审计范围]。归属：P1 设计评审必答项；答"不可"则 episode 行退化为只存摘要 + 溯源靠 `source_sess`。

---

## 4. 参考资料

来源分级：论文（arXiv）> 官方文档 > 官方博客/仓库 > 二手资料；所有 URL 访问日期均为 2026-10-11。`[未核实]` 表示两份调研产物内未能找到可靠来源支撑，正文已就地标注。

| # | 来源 | URL |
|---|---|---|
| [1] | Mem0 论文（arXiv:2504.19413） | https://arxiv.org/abs/2504.19413 |
| [2] | Mem0 Research 页 | https://mem0.ai/research |
| [3] | Mem0 博客：Token-Efficient Memory Algorithm | https://mem0.ai/blog/mem0-the-token-efficient-memory-algorithm |
| [4] | Mem0 博客：Temporal Reasoning | https://mem0.ai/blog/introducing-temporal-reasoning-in-mem0 |
| [5] | Mem0 博客：Memory Decay | https://mem0.ai/blog/introducing-memory-decay-in-mem0 |
| [6] | Mem0 文档：How Mem0 Works | https://docs.mem0.ai/core-concepts/how-it-works |
| [7] | Mem0 文档：Memory Evaluation | https://docs.mem0.ai/core-concepts/memory-evaluation |
| [8] | Mem0 文档：Memory Types | https://docs.mem0.ai/core-concepts/memory-types |
| [9] | MemGPT 论文（arXiv:2310.08560） | https://arxiv.org/abs/2310.08560 |
| [10] | Sleep-time Compute 论文（arXiv:2504.13171） | https://arxiv.org/abs/2504.13171 |
| [11] | Letta 文档：Memory & dreaming | https://docs.letta.com/configuration/memory/ |
| [12] | Letta 文档：MemFS | https://docs.letta.com/concepts/memfs/ |
| [13] | Zep 论文（arXiv:2501.13956） | https://arxiv.org/abs/2501.13956 |
| [14] | Graphiti GitHub README | https://github.com/getzep/graphiti |
| [15] | Zep 文档：Graphiti Overview v3 | https://help.getzep.com/v3/graphiti/getting-started/overview |
| [16] | Zep 博客：Beyond Static Graphs | https://www.getzep.com/blog/beyond-static-knowledge-graphs/ |
| [17] | Graphiti Issue #1728（边失效全图误杀） | https://github.com/getzep/graphiti/issues/1728 |
| [18] | LangMem 概念指南 | https://github.com/langchain-ai/langmem/blob/main/docs/docs/concepts/conceptual_guide.md |
| [19] | LangChain 文档：Memory overview | https://docs.langchain.com/oss/python/concepts/memory |
| [20] | Memobase 仓库与 README | https://github.com/memodb-io/memobase |
| [21] | Memobase LoCoMo 复测 README | https://github.com/memodb-io/memobase/blob/main/docs/experiments/locomo-benchmark/README.md |
| [22] | Memobase 文档：Profile Fundamentals | https://docs.memobase.io/features/profile/profile |
| [23] | Memobase 文档：Event Fundamentals | https://docs.memobase.io/features/event/event |
| [24] | Memobase 文档：context API | https://docs.memobase.io/features/context |
| [25] | MemoryOS 论文（arXiv:2506.06326） | https://arxiv.org/abs/2506.06326 |
| [26] | MemoryOS 仓库 | https://github.com/BAI-LAB/MemoryOS |
| [27] | Mem0 博客：2026 记忆基准指南 | https://mem0.ai/blog/ai-memory-benchmarks-in-2026 |
| [28] | CoALA 论文（arXiv:2309.02427） | https://arxiv.org/abs/2309.02427 |
| [29] | Generative Agents 论文（arXiv:2304.03442） | https://arxiv.org/abs/2304.03442 |
| [30] | LongMemEval 论文（arXiv:2410.10813） | https://arxiv.org/abs/2410.10813 |
| [31] | LongMemEval 项目页 | https://xiaowu0162.github.io/long-mem-eval/ |
| [32] | XTDB 文档：Bitemporality | https://v1-docs.xtdb.com/concepts/bitemporality |
| [33] | MemoryBank 论文（arXiv:2305.10250） | https://arxiv.org/abs/2305.10250 |
| [34] | LoCoMo 论文（arXiv:2402.17753） | https://arxiv.org/abs/2402.17753 |
| [35] | OpenAI 博客：ChatGPT memory dreaming | https://openai.com/index/chatgpt-memory-dreaming/ |
| [36] | Knowledge Conflicts 综述（arXiv:2403.08319） | https://arxiv.org/abs/2403.08319 |
| [37] | MemoryAgentBench（arXiv:2507.05257） | https://arxiv.org/abs/2507.05257 |
| [38] | 文本记忆综述（arXiv:2404.13501） | https://arxiv.org/html/2404.13501 |
| [39] | MIRIX 论文（arXiv:2507.07957） | https://arxiv.org/abs/2507.07957 |
| [40] | Mem0 Issue #4956（append-only 图记忆讨论） | https://github.com/mem0ai/mem0/issues/4956 |
| [41] | LangGraph 文档：Memory（put 覆盖语义） | https://docs.langchain.com/oss/javascript/langgraph/memory |
| [42] | Episodic memory 是缺失拼图（arXiv:2502.06975） | https://arxiv.org/abs/2502.06975 |
| [43] | HippoRAG 论文（arXiv:2405.14831） | https://arxiv.org/abs/2405.14831 |
| [44] | Claude memory tool 文档 | https://platform.claude.com/docs/en/agents-and-tools/tool-use/memory-tool |

调研产物（本文件的引用来源，未重写其内容）：

- `/Users/alfredchaos/home/agent-default/fleet/tasks/261011-224645-memory-research/industry-frameworks.md`
- `/Users/alfredchaos/home/agent-default/fleet/tasks/261011-224645-memory-research/industry-patterns.md`
- `/Users/alfredchaos/home/agent-default/fleet/tasks/261011-224645-memory-research/source-audit.md`
