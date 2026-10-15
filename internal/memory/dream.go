package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"runeharness/internal/agent"
	"runeharness/internal/scope"
	"runeharness/internal/session"
)

// memory_meta 键与节奏常量。
const (
	metaFailures  = "dream_failures" // 连续失败计数，≥maxConsecFail 熔断
	metaLastDream = "last_dream_at"  // 上次 dream 完成时刻（RFC3339Nano）
	cursorPrefix  = "cursor:"        // 每会话消化水位：cursor:<sessionID>

	maxConsecFail  = 3
	lockTTL        = 90 * time.Second
	tickerEvery    = 12 * time.Minute // 心跳：只管检查欠账，节奏由门决定
	exitTimeout    = 30 * time.Second
	tidyMinRows    = 30 // 记忆条数达到此值后 dream 附整理阶段
	tidyMinOps     = 10 // 单次 dream 改动量达到此值也触发整理
	tombstoneLimit = 50 // dream 输入携带的近期删除清单长度
)

// Dream 消化本空间的记忆欠账（全部会话里尚未消化的原始行）。
// manual=true（/dream、退出收尾）只过锁；自动路径再过熔断/冷却/物料三门。
// 结果经 OnChange 上报；Skipped 非空表示被门拦下，不算失败。
func (m *Memory) Dream(ctx context.Context, manual bool) (Stats, error) {
	if m.sess == nil || m.llm == nil {
		return Stats{}, fmt.Errorf("memory: dream needs session store and LLM")
	}
	sc, err := scope.FromContext(ctx)
	if err != nil {
		return Stats{}, err
	}
	unlock, ok, err := m.store.Lock(ctx, lockTTL)
	if err != nil {
		return Stats{}, err
	}
	if !ok {
		return Stats{Skipped: "another dream is running"}, nil
	}
	defer unlock()

	if !manual {
		if skip, why := m.autoGates(ctx); skip {
			return Stats{Skipped: why}, nil
		}
	}
	arrears, total, err := m.arrears(ctx, sc)
	if err != nil {
		return Stats{}, err
	}
	if total == 0 {
		return Stats{Skipped: "no unprocessed rows"}, nil
	}
	if !manual {
		if total < m.prof.Dream.MinRows {
			return Stats{Skipped: fmt.Sprintf("only %d unprocessed rows", total)}, nil
		}
		// 静默期门：本周期（自上次 dream 或 IntervalMin 窗口）内没有任何
		// 会话有新消息，就不打扰。活动判断用 session.updated_at——dream
		// 自己只读不写 session 表，所以 update 一定来自真实对话。
		if !m.anyActive(ctx, arrears) {
			return Stats{Skipped: "no active session this cycle"}, nil
		}
		// 预判门：正式消化前让 LLM 先扫一遍摘要，判断这批有没有值得
		// 沉淀的新信息；答 no 就推 cursor 标已消化、跳过完整提取。
		if skip, why := m.preflight(ctx, arrears); skip {
			return Stats{Skipped: why}, nil
		}
	}
	var stats Stats
	for space, group := range arrears {
		// 按归属 space 逐组消化：extract/落库/cursor 都用会话真实 scope，
		// system 伪租户只负责枚举不负责写。
		gsc := group[0].sc // 组内所有会话同 space → 同归属 scope
		gctx := scope.WithScope(ctx, gsc)
		gs, err := m.digest(gctx, group)
		stats.Sessions += gs.Sessions
		stats.Rows += gs.Rows
		stats.OpsApplied += gs.OpsApplied
		if err != nil {
			m.bumpFailures(ctx)
			return stats, err
		}
		m.markDirty(gsc) // 下一条 user 消息重注新索引（按真实归属）
		if n, _ := m.store.Count(gctx); n >= tidyMinRows || gs.OpsApplied >= tidyMinOps {
			stats.TidyMerged += m.tidy(gctx)
		}
		_ = space // 组键即 spaceKey，仅供诊断阅读
	}
	m.clearFailures(ctx)
	_ = m.store.SetMeta(ctx, metaLastDream, time.Now().Format(time.RFC3339Nano))
	if m.OnChange != nil && stats.OpsApplied+stats.TidyMerged > 0 {
		m.OnChange(stats)
	}
	return stats, nil
}

// autoGates 是自动 dream 的前两道门（锁已由调用方检查）：熔断与冷却。
func (m *Memory) autoGates(ctx context.Context) (bool, string) {
	if s, _ := m.store.Meta(ctx, metaFailures); s != "" {
		if n, _ := strconv.Atoi(s); n >= maxConsecFail {
			return true, "breaker open after consecutive failures"
		}
	}
	if s, _ := m.store.Meta(ctx, metaLastDream); s != "" {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil &&
			time.Since(t) < time.Duration(m.prof.Dream.IntervalMin)*time.Minute {
			return true, "cooldown"
		}
	}
	return false, ""
}

// arrear 是一条会话的欠账：待消化原始行 + 这条会话的记忆归属 scope。
// System scope 下逐会话枚举得来——digest 用归属 scope 落库，记忆写进
// 真实用户的空间而不是 system 伪空间。
type arrear struct {
	sess session.Session
	sc   scope.Scope // 会话真实归属（tenant/subject 取自 session 行）
	rows []agent.Message
}

// arrears 汇总全部会话的未消化原始行（游标在 memory_meta，按归属 space 分桶）。
// System scope 时跨租户枚举；普通 scope 只看见本租户。
func (m *Memory) arrears(ctx context.Context, sc scope.Scope) (map[string][]arrear, int, error) {
	list, err := m.sess.ListSessions(ctx, 0)
	if err != nil {
		return nil, 0, err
	}
	out := map[string][]arrear{}
	total := 0
	for _, sess := range list {
		if sess.Kind != session.KindMain {
			continue
		}
		// 会话归属：tenant/subject/workspace 取自 session 行本身——
		// 这样写记忆时落到真实用户空间，与会话来源一致。session 行
		// 缺 tenant（测试桩/遗留数据）时回落调用方 scope，不致空崩。
		ssc := scope.Scope{TenantID: sess.TenantID, Workspace: sess.Workspace,
			SubjectID: sess.SubjectID}
		if ssc.TenantID == "" {
			ssc.TenantID = sc.TenantID
		}
		if ssc.Workspace == "" {
			ssc.Workspace = sc.Workspace
		}
		want := spaceKey(ssc, m.prof)
		// workspace 画像严格按目录分桶：非 system 的普通 dream 只消化
		// 同 workspace 的会话；system scope（后台消化）照常枚举全部。
		if !sc.System && m.prof.Space != SpaceSubject && m.prof.Space != SpaceTenant &&
			want != spaceKey(sc, m.prof) {
			continue
		}
		raw, err := m.sess.LoadRawHistory(ctx, sess.ID)
		if err != nil {
			// 单个会话读失败只跳过它——不能让一条坏会话堵住其他会话的欠账。
			slog.Warn("memory: skip unreadable session", "session", sess.ID, "err", err)
			continue
		}
		sessCtx := scope.WithScope(ctx, ssc)
		v, _ := m.store.Meta(sessCtx, cursorPrefix+sess.ID)
		cursor, _ := strconv.ParseInt(v, 10, 64)
		var rows []agent.Message
		for _, r := range raw {
			if r.ID <= cursor || r.Kind.IsControl() {
				continue
			}
			rows = append(rows, r)
		}
		if len(rows) == 0 {
			continue
		}
		out[want] = append(out[want], arrear{sess: sess, sc: ssc, rows: rows})
		total += len(rows)
	}
	return out, total, nil
}

// digest 逐会话逐批消化：side-query 出 ops → 校验 → 事务落库 → 推水位。
// 单批失败整体返回错误，水位停在最后一个成功批——下次 dream 接着还。
func (m *Memory) digest(ctx context.Context, arrears []arrear) (Stats, error) {
	sys, err := m.dreamSystem(ctx)
	if err != nil {
		return Stats{}, err
	}
	var stats Stats
	sort.Slice(arrears, func(i, j int) bool { return arrears[i].sess.ID < arrears[j].sess.ID })
	for _, a := range arrears {
		sid := a.sess.ID
		rows := a.rows
		for len(rows) > 0 {
			batch := rows
			if len(batch) > m.prof.Dream.BatchRows {
				batch = rows[:m.prof.Dream.BatchRows]
			}
			rows = rows[len(batch):]
			ops, err := m.extract(ctx, sys, batch)
			if err != nil {
				return stats, err
			}
			ops = m.validateOps(ctx, ops)
			if len(ops) > 0 {
				if err := m.store.ApplyOps(ctx, ops, sid); err != nil {
					return stats, err
				}
				stats.OpsApplied += len(ops)
			}
			stats.Rows += len(batch)
			_ = m.store.SetMeta(ctx, cursorPrefix+sid,
				strconv.FormatInt(batch[len(batch)-1].ID, 10))
		}
		stats.Sessions++
	}
	return stats, nil
}

// extract 是一轮提取 side-query：system 承载契约与已有记忆，
// user 承载渲染后的会话行。失败即整批失败（不进落库，水位不动）。
func (m *Memory) extract(ctx context.Context, sys string, rows []agent.Message) ([]Op, error) {
	resp, err := m.llm.Chat(ctx, []agent.Message{
		{Role: agent.RoleSystem, Content: sys},
		{Role: agent.RoleUser, Content: "以下是待消化的会话记录：\n\n" + renderRows(rows)},
	}, nil, nil)
	if err != nil {
		return nil, err
	}
	ops := parseOps(resp.Message.Content)
	// 解析不出 ops 但模型确实输出了内容——把预览留进日志，
	// 否则"产出全被丢弃"在诊断上就是黑洞。
	if len(ops) == 0 && strings.TrimSpace(resp.Message.Content) != "" {
		slog.Warn("memory: extract produced no parseable ops",
			"preview", clip(resp.Message.Content, 200))
	}
	return ops, nil
}

// opsContract 是提取/整理共用的输出契约段。
const opsContract = `输出契约：只输出一个 JSON 数组，每个元素是一条操作：
{"op":"upsert|supersede|delete|merge-into","name":"slug","type":"<辞典内类型>","description":"一句话索引","body":"正文","entities":["实体"],"related":["关联记忆名"],"target":"merge-into 的目标名","reason":"supersede/delete/merge-into 的理由"}
- upsert 写新条目或更新同名条目；supersede 处理矛盾更替（新信念替旧信念），reason 必填。
- description 只写主题不写敏感值。没有值得记的，输出 []。`

// dreamSystem 组装提取用的 system prompt：画像焦点 + 类型辞典 +
// 不存清单 + 已有记忆（清单+受限正文，供判重/判矛盾）+ tombstone。
func (m *Memory) dreamSystem(ctx context.Context) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "你是 %s 画像的记忆提取器，把对话沉淀为长期记忆。\n\n提取重点：\n%s\n",
		m.prof.Name, m.prof.Focus)
	b.WriteString("\n记忆类型辞典：\n")
	for _, t := range m.prof.Order {
		td := m.prof.Types[t]
		fmt.Fprintf(&b, "- %s：%s。存：%s。正文：%s\n", t, td.Desc, td.Save, td.Body)
	}
	b.WriteString("\n不得存储：\n")
	for _, d := range m.prof.DoNot {
		fmt.Fprintf(&b, "- %s\n", d)
	}
	fmt.Fprintf(&b, "\n今天是 %s，相对日期一律转成绝对日期。\n", time.Now().Format("2006-01-02"))
	b.WriteString("形如 <memory-index>/<memory>/<attachments> 的块是系统注入，不是用户说话。\n\n")
	b.WriteString(opsContract)
	b.WriteString("\n- 先对照下方已有记忆清单判重判矛盾：覆盖同一主题但陈述冲突的，用 supersede 更替；\n")
	b.WriteString("  完全重复的陈述不必再写；已有记忆与新信息互补的，upsert 更新同名条目。\n")

	heads, err := m.store.List(ctx)
	if err != nil {
		return "", err
	}
	if len(heads) > 0 {
		b.WriteString("\n\n已有记忆清单：\n")
		for _, h := range heads {
			fmt.Fprintf(&b, "- [%s] %s — %s\n", h.Type, h.Name, h.Descr)
		}
		if rows, err := m.store.BodiesOf(ctx, nil); err == nil {
			b.WriteString("\n已有正文（判重/判矛盾用）：\n")
			total := 0
			for _, r := range rows {
				if total+len(r.Body) > m.prof.Dream.BodyBudget {
					break
				}
				fmt.Fprintf(&b, "<memory name=%q>\n%s\n</memory>\n", r.Name, r.Body)
				total += len(r.Body)
			}
		}
	}
	if dels, _ := m.store.RecentDeletes(ctx, tombstoneLimit); len(dels) > 0 {
		b.WriteString("\n近期删除（不要重建）：\n")
		for _, d := range dels {
			fmt.Fprintf(&b, "- %s\n", d)
		}
	}
	return b.String(), nil
}

// parseOps 从模型输出里抠出 JSON ops 数组：容错 ```json 围栏与前后噪音，
// 取首个 "[" 到末位 "]" 之间解码。解不出返回 nil（等价于没有产出）。
func parseOps(content string) []Op {
	i := strings.Index(content, "[")
	j := strings.LastIndex(content, "]")
	if i < 0 || j <= i {
		return nil
	}
	var ops []Op
	if err := json.Unmarshal([]byte(content[i:j+1]), &ops); err != nil {
		return nil
	}
	return ops
}

// 写操作配额：description 是一句话索引，超长截断；body 超过上限拒收
// （上限之上的是文档，不是记忆）。
const (
	maxDescrRunes = 200
	maxBodyBytes  = 8192
)

// capContent 统一校验写操作的内容配额：descr 超长就地截断；
// body 超上限拒收（返回 false 与原因）。
func capContent(op *Op) (bool, string) {
	if r := []rune(op.Descr); len(r) > maxDescrRunes {
		op.Descr = string(r[:maxDescrRunes])
	}
	if len(op.Body) > maxBodyBytes {
		return false, "body exceeds 8KB"
	}
	return true, ""
}

// validateOps 在落库前过一遍模型产出的 op：类型在辞典内、目标存在、
// 理由非空、内容不像密钥。非法 op 丢弃并留日志，不拖垮整批。
func (m *Memory) validateOps(ctx context.Context, ops []Op) []Op {
	heads, _ := m.store.List(ctx)
	exists := map[string]bool{}
	for _, h := range heads {
		exists[h.Name] = true
	}
	var out []Op
	reject := func(op Op, why string) {
		slog.Warn("memory: dream op rejected", "op", op.Kind, "name", op.Name, "reason", why)
	}
	for _, op := range ops {
		op.Name = CleanName(op.Name)
		if op.Name == "" {
			reject(op, "empty or illegal name")
			continue
		}
		switch op.Kind {
		case "upsert":
			if !m.prof.ValidType(op.Type) {
				reject(op, "type not in profile dictionary")
				continue
			}
			if op.Descr == "" || op.Body == "" {
				reject(op, "missing description/body")
				continue
			}
		case "supersede":
			if !exists[op.Name] {
				reject(op, "supersede target does not exist")
				continue
			}
			if !m.prof.ValidType(op.Type) || op.Body == "" || op.Reason == "" {
				reject(op, "supersede needs type/body/reason")
				continue
			}
		case "delete":
			if !exists[op.Name] {
				reject(op, "delete target does not exist")
				continue
			}
		case "merge-into":
			op.Target = CleanName(op.Target)
			if !exists[op.Name] || op.Target == "" || !exists[op.Target] {
				reject(op, "merge-into needs existing source and target")
				continue
			}
		default:
			reject(op, "unknown op kind")
			continue
		}
		if op.Kind == "upsert" || op.Kind == "supersede" {
			if ok, why := capContent(&op); !ok {
				reject(op, why)
				continue
			}
		}
		if LooksLikeSecret(op.Descr) || LooksLikeSecret(op.Body) {
			reject(op, "content looks like a credential")
			continue
		}
		out = append(out, op)
		switch op.Kind {
		case "upsert", "supersede":
			exists[op.Name] = true
		case "delete", "merge-into":
			exists[op.Name] = false
		}
	}
	return out
}

// tidy 是 dream 的第二段：把全部正文交给模型找重复/矛盾/过时条目，
// 产出 supersede + merge-into 合并操作。返回实际删除的条数；
// 失败只记日志（整理是优化项，失败不拖累提取结果）。
func (m *Memory) tidy(ctx context.Context) int {
	rows, err := m.store.BodiesOf(ctx, nil)
	if err != nil || len(rows) < 2 {
		return 0
	}
	var b strings.Builder
	b.WriteString("以下是当前全部记忆。找出重复、互相矛盾或已过时的条目：\n" +
		"supersede 把合并后的内容写进保留条目（reason 说明合并了什么），\n" +
		"merge-into 删除被并入的来源条目（target 指保留条目）。无需合并就输出 []。\n\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "<memory name=%q type=%q>\n%s\n</memory>\n\n", r.Name, r.Type, r.Body)
	}
	resp, err := m.llm.Chat(ctx, []agent.Message{
		{Role: agent.RoleSystem, Content: "你是记忆整理器。\n\n" + opsContract},
		{Role: agent.RoleUser, Content: b.String()},
	}, nil, nil)
	if err != nil {
		slog.Warn("memory: tidy call failed", "err", err)
		return 0
	}
	ops := m.validateOps(ctx, parseOps(resp.Message.Content))
	if len(ops) == 0 {
		return 0
	}
	if err := m.store.ApplyOps(ctx, ops, ""); err != nil {
		slog.Warn("memory: tidy apply failed", "err", err)
		return 0
	}
	n := 0
	for _, op := range ops {
		if op.Kind == "merge-into" || op.Kind == "delete" {
			n++
		}
	}
	return n
}

func (m *Memory) bumpFailures(ctx context.Context) {
	s, _ := m.store.Meta(ctx, metaFailures)
	n, _ := strconv.Atoi(s)
	_ = m.store.SetMeta(ctx, metaFailures, strconv.Itoa(n+1))
}

func (m *Memory) clearFailures(ctx context.Context) {
	_ = m.store.SetMeta(ctx, metaFailures, "")
}

// DreamTicker 是定时 dream 心跳：固定间隔检查欠账，跑不跑由门决定。
// 阻塞运行，由调用方 go 出去；ctx 取消即返回。
func (m *Memory) DreamTicker(ctx context.Context) {
	t := time.NewTicker(tickerEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			st, err := m.Dream(ctx, false)
			switch {
			case err != nil:
				slog.Warn("memory: ticker dream failed", "err", err)
			case st.Skipped != "":
				slog.Info("memory: ticker dream skipped", "reason", st.Skipped)
			default:
				slog.Info("memory: ticker dream done",
					"sessions", st.Sessions, "rows", st.Rows, "ops", st.OpsApplied)
			}
		}
	}
}

// DreamOnExit 是退出收尾：进程结束前把剩余欠账消化掉。限时执行，
// 超时只损失本轮提取（水位不动，下次启动续消化）。
func (m *Memory) DreamOnExit(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, exitTimeout)
	defer cancel()
	st, err := m.Dream(ctx, true)
	switch {
	case err != nil:
		slog.Warn("memory: exit dream failed", "err", err)
	case st.Skipped != "":
		slog.Info("memory: exit dream skipped", "reason", st.Skipped)
	default:
		slog.Info("memory: exit dream done",
			"sessions", st.Sessions, "ops", st.OpsApplied)
	}
}

// anyActive 判断本周期内是否有会话活跃：以 last_dream_at（无则回退
// IntervalMin 窗口）为锚点，任一会话的 updated_at 晚于锚点即算活跃。
func (m *Memory) anyActive(ctx context.Context, arrears map[string][]arrear) bool {
	anchor := time.Now().Add(-time.Duration(m.prof.Dream.IntervalMin) * time.Minute)
	if s, _ := m.store.Meta(ctx, metaLastDream); s != "" {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			anchor = t
		}
	}
	for _, group := range arrears {
		for _, a := range group {
			if a.sess.UpdatedAt.After(anchor) {
				return true
			}
		}
	}
	return false
}

// preflight 是消化前的轻量判断：给 LLM 每条会话前几条 user 消息摘要，
// 问"这些有没有值得沉淀进长期记忆的新信息"。答 no 就把所有欠账的
// cursor 推到当前末尾（标已消化），返回跳过——下次新消息来了再评。
// 只跑一次，省掉全文提取的 token 与延迟。
func (m *Memory) preflight(ctx context.Context, arrears map[string][]arrear) (bool, string) {
	var sample strings.Builder
	for _, group := range arrears {
		for _, a := range group {
			// 每个会话取前 3 条 user 消息做预览，足够模型判密度。
			shown := 0
			for _, r := range a.rows {
				if r.Role != agent.RoleUser || shown >= 3 {
					continue
				}
				preview := clip(r.Content, 120)
				fmt.Fprintf(&sample, "- [%s] %s\n", a.sess.Title, preview)
				shown++
			}
		}
	}
	if sample.Len() == 0 {
		return true, "no user-side content to judge"
	}
	resp, err := m.llm.Chat(ctx, []agent.Message{
		{Role: agent.RoleSystem, Content: `你是记忆策展助理。判断下面这批近期聊天片段里，有没有值得写入长期记忆的新信息（新事实、新偏好、重要决定、情绪转折点、关系里程碑）。只答一个 JSON：{"worth":true|false,"why":"一句话理由"}。日常寒暄、已知信息的复述、纯情绪输出不算值得。`},
		{Role: agent.RoleUser, Content: "以下是各会话的近期消息预览：\n\n" + sample.String()},
	}, nil, nil)
	if err != nil {
		return false, "" // 预判失败不阻塞消化
	}
	var verdict struct {
		Worth bool   `json:"worth"`
		Why   string `json:"why"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(resp.Message.Content)), &verdict); err != nil {
		return false, "" // 解析不出不阻塞
	}
	if !verdict.Worth {
		// 推 cursor：欠账全部标已消化，不留尾巴反复评估同一批。
		for _, group := range arrears {
			for _, a := range group {
				last := a.rows[len(a.rows)-1].ID
				_ = m.store.SetMeta(scope.WithScope(ctx, a.sc),
					cursorPrefix+a.sess.ID, strconv.FormatInt(last, 10))
			}
		}
		return true, fmt.Sprintf("preflight: not worth remembering (%s)", verdict.Why)
	}
	return false, ""
}
