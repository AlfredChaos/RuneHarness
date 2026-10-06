package compact

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"runeharness/internal/agent"
	"runeharness/internal/scope"
	"runeharness/internal/session"
	"runeharness/internal/session/sqlite"
)

// newTestStore 开一个内存库并建会话，返回 store 与带该会话 scope 的 ctx。
func newTestStore(t *testing.T) (*sqlite.Store, context.Context) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	base := scope.WithScope(context.Background(), scope.Scope{TenantID: "t", Workspace: t.TempDir()})
	ssn, err := st.CreateSession(base, session.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	return st, scope.WithSession(base, ssn.ID)
}

func newTestCompactor(t *testing.T, llm agent.LLM) (*Compactor, *sqlite.Store, context.Context) {
	t.Helper()
	st, ctx := newTestStore(t)
	c, err := New(llm, st, Config{Window: 128_000, MaxOutput: 8192, Auto: true})
	if err != nil {
		t.Fatal(err)
	}
	return c, st, ctx
}

func call(name string) agent.ToolCall {
	return agent.ToolCall{ID: "c-" + name, Name: name, Arguments: json.RawMessage(`{}`)}
}

func toolMsg(content string) agent.Message {
	return agent.Message{Role: agent.RoleTool, Content: content}
}

// blobRef 从 persisted-output 指针里取出 ref。
func blobRef(t *testing.T, content string) string {
	t.Helper()
	_, rest, ok := strings.Cut(content, `src="blob://`)
	if !ok {
		t.Fatalf("no blob pointer in %q", content)
	}
	ref, _, _ := strings.Cut(rest, `"`)
	return ref
}

func TestBackfillEmptyMarkers(t *testing.T) {
	c, _, ctx := newTestCompactor(t, nil)
	out, err := c.Backfill(ctx, []agent.ToolCall{call("run_command"), call("run_command")},
		[]agent.Message{toolMsg(" \n"), {Role: agent.RoleTool, IsError: true}})
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Content != EmptyOutput || out[1].Content != EmptyErrorOutput {
		t.Fatalf("markers = %q, %q", out[0].Content, out[1].Content)
	}
}

// 300KB 命令输出：消息落头尾预览 + 指针，blob 存全文；30KB 结果原样进上下文；
// read_file 超长也不 spill（请求侧限额）。
func TestBackfillSpill(t *testing.T) {
	c, st, ctx := newTestCompactor(t, nil)
	big := "HEAD" + strings.Repeat("x", 300_000) + "TAIL-ERROR"
	mid := strings.Repeat("m", 30_000)
	read := strings.Repeat("r", 60_000)
	out, err := c.Backfill(ctx,
		[]agent.ToolCall{call("run_command"), call("run_command"), call("read_file")},
		[]agent.Message{toolMsg(big), toolMsg(mid), toolMsg(read)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out[0].Content, "HEAD") || !strings.Contains(out[0].Content, "TAIL-ERROR") ||
		len(out[0].Content) > 3_000 {
		t.Fatalf("spilled preview = %d chars", len(out[0].Content))
	}
	full, err := st.LoadBlob(ctx, blobRef(t, out[0].Content))
	if err != nil || full != big {
		t.Fatalf("blob full text mismatch: %d chars, %v", len(full), err)
	}
	if out[1].Content != mid {
		t.Fatal("30KB result should go into context as-is")
	}
	if out[2].Content != read {
		t.Fatal("read_file is request-limited and must not spill")
	}
}

// 整批合计超 200K：从最大的开始落，直到达标；硬上限 400KB 截断。
func TestBackfillBatchBudgetAndHardCap(t *testing.T) {
	c, _, ctx := newTestCompactor(t, nil)
	r40, r45 := strings.Repeat("a", 40_000), strings.Repeat("b", 45_000)
	var calls []agent.ToolCall
	var res []agent.Message
	for i := 0; i < 5; i++ { // 5 × ~45K ≈ 225K > 200K
		calls = append(calls, call("run_command"))
		if i == 2 {
			res = append(res, toolMsg(r45+r45[:4_000]))
		} else {
			res = append(res, toolMsg(r40+r40[:i*1000]))
		}
	}
	out, err := c.Backfill(ctx, calls, res)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out[2].Content, "<persisted-output") {
		t.Fatal("largest result should spill first")
	}
	for i, m := range out {
		if i != 2 && strings.HasPrefix(m.Content, "<persisted-output") {
			t.Fatalf("result %d spilled although batch is under budget after one spill", i)
		}
	}

	huge := strings.Repeat("z", 500_000)
	out, err = c.Backfill(ctx, []agent.ToolCall{call("read_file")}, []agent.Message{toolMsg(huge)})
	if err != nil || len(out[0].Content) > hardCap+100 || !strings.Contains(out[0].Content, "truncated at") {
		t.Fatalf("hard cap: %d chars, %v", len(out[0].Content), err)
	}
}
