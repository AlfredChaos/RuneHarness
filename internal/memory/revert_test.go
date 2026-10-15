package memory

import (
	"strings"
	"testing"
)

// ctxFor/ctxSub 定义在 memory_test.go / memory_edge_test.go。

func TestRevertOneStep(t *testing.T) {
	sess := &fakeSess{}
	llm := &fakeLLM{}
	m, err := New(sess, llm, Config{DBPath: ":memory:", Profile: Lookup("companion")})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx := ctxSub("bear", "app", "s1")

	// 建一条、改一次、再改一次——流水 3 条。
	save := func(body string) {
		t.Helper()
		if err := m.store.ApplyOps(ctx, []Op{{Kind: "upsert", Name: "note", Body: body, Type: "bond"}}, "sess"); err != nil {
			t.Fatal(err)
		}
	}
	save("v1")
	save("v2")
	save("v3")

	// 回退一步 → 应回 v2。
	if err := m.store.Revert(ctx, "note", 0); err != nil {
		t.Fatal(err)
	}
	got, err := m.store.Get(ctx, "note")
	if err != nil || got.Body != "v2" {
		t.Fatalf("after revert: body=%q err=%v", got.Body, err)
	}
	// revert 自己也进流水，能再回退。
	if err := m.store.Revert(ctx, "note", 0); err != nil {
		t.Fatal(err)
	}
	got, _ = m.store.Get(ctx, "note")
	if got.Body != "v3" {
		t.Fatalf("revert of revert should restore v3, got %q", got.Body)
	}
}

func TestRevertToLogID(t *testing.T) {
	sess := &fakeSess{}
	m, err := New(sess, &fakeLLM{}, Config{DBPath: ":memory:", Profile: Lookup("companion")})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx := ctxSub("bear", "app", "s1")
	for _, b := range []string{"v1", "v2", "v3"} {
		if err := m.store.ApplyOps(ctx, []Op{{Kind: "upsert", Name: "note", Body: b, Type: "bond"}}, "s"); err != nil {
			t.Fatal(err)
		}
	}
	hist, _ := m.store.History(ctx, "note")
	if len(hist) != 3 {
		t.Fatalf("hist=%v", hist)
	}
	// hist 新在前：hist[2] 是第一条（v1）。
	if err := m.store.Revert(ctx, "note", hist[2].ID); err != nil {
		t.Fatal(err)
	}
	got, _ := m.store.Get(ctx, "note")
	if got.Body != "v1" {
		t.Fatalf("revert to first log should restore v1, got %q", got.Body)
	}
}

func TestRevertDeleted(t *testing.T) {
	sess := &fakeSess{}
	m, err := New(sess, &fakeLLM{}, Config{DBPath: ":memory:", Profile: Lookup("companion")})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx := ctxSub("bear", "app", "s1")
	_ = m.store.ApplyOps(ctx, []Op{{Kind: "upsert", Name: "note", Body: "hello", Type: "bond"}}, "s")
	_ = m.store.ApplyOps(ctx, []Op{{Kind: "delete", Name: "note"}}, "s")
	if _, err := m.store.Get(ctx, "note"); err == nil {
		t.Fatal("should be deleted")
	}
	// 回退 delete → 恢复 body。
	if err := m.store.Revert(ctx, "note", 0); err != nil {
		t.Fatal(err)
	}
	got, err := m.store.Get(ctx, "note")
	if err != nil || got.Body != "hello" {
		t.Fatalf("undelete should restore body, got %q err=%v", got.Body, err)
	}
}

func TestRevertTool(t *testing.T) {
	sess := &fakeSess{}
	m, err := New(sess, &fakeLLM{}, Config{DBPath: ":memory:", Profile: Lookup("companion")})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx := ctxSub("bear", "app", "s1")
	tool := memTool{m: m}
	if _, err := tool.Run(ctx, []byte(`{"op":"save","name":"note","type":"bond","description":"d","body":"v1"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Run(ctx, []byte(`{"op":"save","name":"note","type":"bond","description":"d","body":"v2"}`)); err != nil {
		t.Fatal(err)
	}
	out, err := tool.Run(ctx, []byte(`{"op":"revert","name":"note"}`))
	if err != nil || !strings.Contains(out, "reverted") {
		t.Fatalf("revert tool: %q err=%v", out, err)
	}
	got, _ := m.store.Get(ctx, "note")
	if got.Body != "v1" {
		t.Fatalf("after tool revert: body=%q", got.Body)
	}
	// history 输出要带 log id（revert 锚点可见）。
	hist, _ := tool.Run(ctx, []byte(`{"op":"history","name":"note"}`))
	if !strings.Contains(hist, "#") {
		t.Fatalf("history should print log ids, got %q", hist)
	}
}
