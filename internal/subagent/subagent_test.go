package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"runeharness/internal/agent"
	"runeharness/internal/compact"
	"runeharness/internal/scope"
	"runeharness/internal/session"
	"runeharness/internal/tools"
)

// stubLLM 按脚本依次返回 Response，并记录每次收到的 history 与 specs。
type stubLLM struct {
	script    []agent.Response
	calls     int
	histories [][]agent.Message
	specs     [][]tools.Spec
}

func (s *stubLLM) Chat(_ context.Context, history []agent.Message, specs []tools.Spec, _ func(agent.Partial)) (agent.Response, error) {
	s.histories = append(s.histories, append([]agent.Message(nil), history...))
	s.specs = append(s.specs, append([]tools.Spec(nil), specs...))
	r := s.script[s.calls]
	s.calls++
	return r, nil
}

type errLLM struct{ err error }

func (e errLLM) Chat(context.Context, []agent.Message, []tools.Spec, func(agent.Partial)) (agent.Response, error) {
	return agent.Response{}, e.err
}

// blockLLM 阻塞到 ctx 结束，模拟挂住的模型调用。
type blockLLM struct{}

func (blockLLM) Chat(ctx context.Context, _ []agent.Message, _ []tools.Spec, _ func(agent.Partial)) (agent.Response, error) {
	<-ctx.Done()
	return agent.Response{}, ctx.Err()
}

// echoTool 返回固定文本，用于断言子代理确实拿到了基础工具。
type echoTool struct{ out string }

func (e echoTool) Spec() tools.Spec {
	return tools.Spec{
		Name:       "echo",
		Parameters: map[string]any{"type": "object"},
	}
}
func (e echoTool) Run(context.Context, json.RawMessage) (string, error) { return e.out, nil }

func taskCall(desc, typ string) agent.ToolCall {
	args := `{"description":` + mustJSON(desc) + `,"subagent_type":` + mustJSON(typ) + `}`
	return agent.ToolCall{ID: "t1", Name: ToolName, Arguments: json.RawMessage(args)}
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func toolCallResp(calls ...agent.ToolCall) agent.Response {
	return agent.Response{
		Message:      agent.Message{Role: agent.RoleAssistant, ToolCalls: calls},
		FinishReason: agent.FinishReasonToolCalls,
	}
}

func stopResp(text string) agent.Response {
	return agent.Response{
		Message:      agent.Message{Role: agent.RoleAssistant, Content: text},
		FinishReason: agent.FinishReasonStop,
	}
}

func specNames(specs []tools.Spec) []string {
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return out
}

// 主代理调用 task(general_purpose)：子代理应拿到全新上下文
// [system, user(desc)]，结论回传为 tool 结果。
func TestGeneralPurposeFreshContext(t *testing.T) {
	sub := &stubLLM{script: []agent.Response{stopResp("conclusion")}}
	main := &stubLLM{script: []agent.Response{
		toolCallResp(taskCall("find test framework", "general_purpose")),
		stopResp("done"),
	}}
	base := []tools.Tool{echoTool{out: "ok"}}
	reg := tools.NewRegistry(append(slices.Clone(base), wired(sub, base, "SUB PROMPT", false, &fakeStore{}))...)
	a := agent.New(main, reg, 10)

	hist, err := a.Run(parentCtx(), []agent.Message{
		{Role: agent.RoleSystem, Content: "MAIN PROMPT"},
		{Role: agent.RoleUser, Content: "go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sub.histories) != 1 {
		t.Fatalf("sub llm called %d times, want 1", sub.calls)
	}
	got := sub.histories[0]
	if len(got) != 2 || got[0].Role != agent.RoleSystem || got[0].Content != "SUB PROMPT" ||
		got[1].Role != agent.RoleUser || got[1].Content != "find test framework" {
		t.Fatalf("sub history = %+v, want [system SUB PROMPT, user desc]", got)
	}
	// tool 结果应是子代理的结论
	var res string
	for _, m := range hist {
		if m.Role == agent.RoleTool {
			res = m.Content
		}
	}
	if res != "conclusion" {
		t.Fatalf("task result = %q, want 'conclusion'", res)
	}
}

// 主代理调用 task(normal)：子代理应继承父历史（含 system 提示词），
// 任务描述作为新的 user 消息追加。
func TestNormalInheritsContext(t *testing.T) {
	sub := &stubLLM{script: []agent.Response{stopResp("ok")}}
	main := &stubLLM{script: []agent.Response{
		toolCallResp(taskCall("continue it", "normal")),
		stopResp("done"),
	}}
	reg := tools.NewRegistry(wired(sub, nil, "SUB PROMPT", false, &fakeStore{}))
	a := agent.New(main, reg, 10)

	parent := []agent.Message{
		{Role: agent.RoleSystem, Content: "MAIN PROMPT"},
		{Role: agent.RoleUser, Content: "hello"},
		{Role: agent.RoleAssistant, Content: "hi there"},
		{Role: agent.RoleUser, Content: "do X"},
	}
	if _, err := a.Run(parentCtx(), parent); err != nil {
		t.Fatal(err)
	}
	got := sub.histories[0]
	// 期望 = 父历史（不含触发 task 的 assistant 消息）+ user(continue it)
	wantLen := len(parent) + 1
	if len(got) != wantLen {
		t.Fatalf("sub history len = %d, want %d: %+v", len(got), wantLen, got)
	}
	for i, m := range parent {
		if got[i].Role != m.Role || got[i].Content != m.Content {
			t.Fatalf("sub history[%d] = %+v, want %+v", i, got[i], m)
		}
	}
	if last := got[len(got)-1]; last.Role != agent.RoleUser || last.Content != "continue it" {
		t.Fatalf("sub last msg = %+v, want user 'continue it'", last)
	}
}

// 嵌套开关：nest=false 时子代理不持有 task；nest=true 时持有但下一层不再持有。
func TestNestingDepthCap(t *testing.T) {
	// nest=false：depth1 的 specs 不应含 task
	sub := &stubLLM{script: []agent.Response{stopResp("ok")}}
	main := &stubLLM{script: []agent.Response{
		toolCallResp(taskCall("x", "general_purpose")), stopResp("done"),
	}}
	reg := tools.NewRegistry(wired(sub, nil, "S", false, &fakeStore{}))
	a := agent.New(main, reg, 10)
	if _, err := a.Run(parentCtx(), []agent.Message{{Role: agent.RoleUser, Content: "go"}}); err != nil {
		t.Fatal(err)
	}
	for _, name := range specNames(sub.specs[0]) {
		if name == ToolName {
			t.Fatalf("nest=false 但子代理拿到了 task: %v", specNames(sub.specs[0]))
		}
	}

	// nest=true：depth1 持有 task，并可 spawn depth2；depth2 不应再有 task。
	// 同一个 stubLLM 服务两个子层：消费顺序为
	//   depth1 chat#1 → task → depth2 chat#1 → stop → depth1 chat#2 → stop
	sub2 := &stubLLM{script: []agent.Response{
		toolCallResp(taskCall("deeper", "general_purpose")), // depth1 决定再委派
		stopResp("deep result"),                             // depth2 完成
		stopResp("sub done"),                                // depth1 收尾
	}}
	main2 := &stubLLM{script: []agent.Response{
		toolCallResp(taskCall("x", "general_purpose")), stopResp("done"),
	}}
	reg2 := tools.NewRegistry(wired(sub2, nil, "S", true, &fakeStore{}))
	a2 := agent.New(main2, reg2, 10)
	if _, err := a2.Run(parentCtx(), []agent.Message{{Role: agent.RoleUser, Content: "go"}}); err != nil {
		t.Fatal(err)
	}
	if sub2.calls != 3 {
		t.Fatalf("sub llm called %d times, want 3", sub2.calls)
	}
	// depth1 的两轮调用都应带 task；depth2 的调用不应再有 task
	if !hasSpec(sub2.specs[0], ToolName) {
		t.Fatalf("nest=true 但 depth1 没有 task: %v", specNames(sub2.specs[0]))
	}
	if hasSpec(sub2.specs[1], ToolName) {
		t.Fatalf("depth2 不应再有 task: %v", specNames(sub2.specs[1]))
	}
	if !hasSpec(sub2.specs[2], ToolName) {
		t.Fatalf("depth1 第二轮仍应持有 task: %v", specNames(sub2.specs[2]))
	}
}

func hasSpec(specs []tools.Spec, name string) bool {
	for _, s := range specs {
		if s.Name == name {
			return true
		}
	}
	return false
}

// 子代理失败时 task 返回 error（由 Registry.Call 转成回填字符串）。
func TestSubagentError(t *testing.T) {
	sub := errLLM{err: errors.New("boom")}
	tool := wired(sub, nil, "S", false, &fakeStore{})
	_, err := tool.Run(parentCtx(), json.RawMessage(`{"description":"x"}`))
	if err == nil || err.Error() != "boom" {
		t.Fatalf("err = %v, want 'boom'", err)
	}
}

// 子代理整轮运行受 Timeout 兜底：挂住时按时返回超时错误而非无限阻塞。
func TestSubagentTimeout(t *testing.T) {
	tool := wired(blockLLM{}, nil, "S", false, &fakeStore{})
	tool.Timeout = 50 * time.Millisecond

	start := time.Now()
	_, err := tool.Run(parentCtx(), json.RawMessage(`{"description":"x"}`))
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("took %v, Timeout not applied", elapsed)
	}
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want timeout error", err)
	}
}

// 参数校验与默认类型。
func TestArgValidation(t *testing.T) {
	tool := wired(&stubLLM{script: []agent.Response{stopResp("ok")}}, nil, "S", false, &fakeStore{})
	if _, err := tool.Run(parentCtx(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("empty description should error")
	}
	if _, err := tool.Run(parentCtx(), json.RawMessage(`{"description":"x","subagent_type":"bogus"}`)); err == nil {
		t.Fatal("unknown subagent_type should error")
	}
	// 省略 subagent_type → 默认 general_purpose（全新上下文）
	if _, err := tool.Run(parentCtx(), json.RawMessage(`{"description":"x"}`)); err != nil {
		t.Fatal(err)
	}
	sub := tool.llm.(*stubLLM)
	got := sub.histories[0]
	if got[0].Role != agent.RoleSystem || got[0].Content != "S" {
		t.Fatalf("default type should use genPrompt, got %+v", got[0])
	}
}

// fakeStore 记录建会话元数据与每次 Append 的归属会话，验证 M7 的
// 父子建链与会话隔离。blob/状态/留痕走内存，满足压缩器依赖。
type fakeStore struct {
	metas   []session.Meta
	nextID  int
	appends []recordedAppend
	blobs   map[string]string
}

type recordedAppend struct {
	session string
	msg     agent.Message
}

func (f *fakeStore) CreateSession(_ context.Context, meta session.Meta) (session.Session, error) {
	f.nextID++
	f.metas = append(f.metas, meta)
	return session.Session{ID: "child-" + string(rune('0'+f.nextID))}, nil
}

func (f *fakeStore) Append(ctx context.Context, msg agent.Message) (int64, error) {
	sc, err := scope.FromContext(ctx)
	if err != nil {
		return 0, err
	}
	f.appends = append(f.appends, recordedAppend{session: sc.SessionID, msg: msg})
	return int64(len(f.appends)), nil
}

func (f *fakeStore) AppendBlob(_ context.Context, ref, content string) error {
	if f.blobs == nil {
		f.blobs = map[string]string{}
	}
	f.blobs[ref] = content
	return nil
}

func (f *fakeStore) GetState(context.Context) (json.RawMessage, error)  { return nil, nil }
func (f *fakeStore) PutState(context.Context, json.RawMessage) error    { return nil }
func (f *fakeStore) LogRequest(context.Context, agent.RequestLog) error { return nil }

// wired 建一个带 Store 与压缩配置的 task 工具（D11：Store 必填）。
func wired(llm agent.LLM, base []tools.Tool, prompt string, nest bool, store *fakeStore) *Tool {
	task := New(llm, base, prompt, nest)
	task.Store = store
	task.CompactCfg = compact.Config{Window: 128_000, Auto: true}
	return task
}

func parentCtx() context.Context {
	return scope.WithScope(context.Background(),
		scope.Scope{TenantID: "t", SessionID: "parent-1", Workspace: "/w"})
}

// general_purpose 子代理：spawn 建 kind=subagent/depth=1/parent=当前会话
// 的子会话，初始历史（system+task）与 loop 消息全部落在子会话名下。
func TestSubagentSessionPersistence(t *testing.T) {
	store := &fakeStore{}
	sub := &stubLLM{script: []agent.Response{stopResp("conclusion")}}
	main := &stubLLM{script: []agent.Response{
		toolCallResp(taskCall("find test framework", "general_purpose")),
		stopResp("done"),
	}}
	base := []tools.Tool{echoTool{out: "ok"}}
	task := wired(sub, base, "SUB PROMPT", false, store)
	reg := tools.NewRegistry(append(slices.Clone(base), task)...)
	a := agent.New(main, reg, 10)

	if _, err := a.Run(parentCtx(), []agent.Message{
		{Role: agent.RoleSystem, Content: "MAIN"},
		{Role: agent.RoleUser, Content: "go"},
	}); err != nil {
		t.Fatal(err)
	}
	if len(store.metas) != 1 {
		t.Fatalf("created %d sessions, want 1", len(store.metas))
	}
	m := store.metas[0]
	if m.ParentID != "parent-1" || m.Kind != session.KindSubagent || m.Depth != 1 || m.Workspace != "/w" {
		t.Fatalf("child meta = %+v", m)
	}
	for _, ap := range store.appends {
		if ap.session != "child-1" {
			t.Fatalf("append landed in session %q, want child-1", ap.session)
		}
	}
	if len(store.appends) < 3 ||
		store.appends[0].msg.Role != agent.RoleSystem ||
		store.appends[1].msg.Role != agent.RoleUser {
		t.Fatalf("first child appends should be system+task, got %+v", store.appends)
	}
}

// normal 子代理不落全量父快照：只落 fork 控制行 + task，防嵌套 O(n²)。
// fork 行的 meta 记父会话与水印，读侧回放 = 父视图 + 子会话行（plan §4.9）。
func TestSubagentNormalInheritsMarker(t *testing.T) {
	store := &fakeStore{}
	sub := &stubLLM{script: []agent.Response{stopResp("conclusion")}}
	main := &stubLLM{script: []agent.Response{
		toolCallResp(taskCall("summarize", "normal")),
		stopResp("done"),
	}}
	task := wired(sub, nil, "SUB PROMPT", false, store)
	reg := tools.NewRegistry(task)
	a := agent.New(main, reg, 10)

	if _, err := a.Run(parentCtx(), []agent.Message{
		{Role: agent.RoleSystem, Content: "MAIN"},
		{Role: agent.RoleUser, Content: "go"},
	}); err != nil {
		t.Fatal(err)
	}
	if len(store.appends) < 2 {
		t.Fatalf("appends = %+v", store.appends)
	}
	fork := store.appends[0].msg
	if fork.Kind != agent.KindFork {
		t.Fatalf("first child message should be a fork control row, got kind=%q content=%q",
			fork.Kind, fork.Content)
	}
	var fm session.ForkMeta
	if err := json.Unmarshal([]byte(fork.Content), &fm); err != nil || fm.ParentSessionID != "parent-1" {
		t.Fatalf("fork meta = %+v, %v", fm, err)
	}
	if store.appends[1].msg.Content != "summarize" {
		t.Fatalf("second child message should be task, got %q",
			store.appends[1].msg.Content)
	}
}

// Store 未配置：spawn 直接报错（D11：Store 必填）。
func TestSubagentWithoutStoreErrors(t *testing.T) {
	task := New(&stubLLM{script: []agent.Response{stopResp("x")}}, nil, "S", false)
	task.CompactCfg = compact.Config{Window: 128_000}
	if _, err := task.Run(parentCtx(), json.RawMessage(`{"description":"x"}`)); err == nil {
		t.Fatal("want store-required error")
	}
}

// ctx 缺 scope：spawn 必须报错而非静默跳过落库。
func TestSubagentStoreWithoutScopeErrors(t *testing.T) {
	task := wired(&stubLLM{script: []agent.Response{stopResp("x")}}, nil, "S", false, &fakeStore{})
	_, err := task.Run(context.Background(), json.RawMessage(`{"description":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "scope") {
		t.Fatalf("err = %v, want scope error", err)
	}
}
