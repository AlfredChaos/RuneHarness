package session

import (
	"testing"

	"runeharness/internal/agent"
)

func TestFoldRepairsDanglingToolCalls(t *testing.T) {
	rows := []agent.Message{
		{ID: 1, Role: agent.RoleUser, Content: "q"},
		{ID: 2, Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{
			{ID: "c1", Name: "read_file"}, {ID: "c2", Name: "run_command"},
		}},
		{ID: 3, Role: agent.RoleTool, ToolCallID: "c1", Content: "file body"},
		// c2 的结果没落库（进程死在执行途中）
	}
	view, err := Fold(rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(view) != 4 {
		t.Fatalf("view len = %d, want 4: %+v", len(view), view)
	}
	last := view[3]
	if last.Role != agent.RoleTool || last.ToolCallID != "c2" || !last.IsError {
		t.Fatalf("last = %+v, want synthesized placeholder for c2", last)
	}
	if last.Content != "error: execution interrupted" {
		t.Fatalf("placeholder content = %q", last.Content)
	}
	if last.ID != 0 {
		t.Fatalf("synthesized row should have no DB id, got %d", last.ID)
	}
}

func TestFoldPairedCallsUntouched(t *testing.T) {
	rows := []agent.Message{
		{ID: 1, Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{{ID: "c1", Name: "x"}}},
		{ID: 2, Role: agent.RoleTool, ToolCallID: "c1", Content: "ok"},
	}
	view, err := Fold(rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(view) != 2 {
		t.Fatalf("view len = %d, want 2 (no synthesis)", len(view))
	}
}

func TestFoldKeepsOrphanToolRow(t *testing.T) {
	// 坏数据：tool 行没有对应 assistant 认领——不丢，原位保留
	rows := []agent.Message{
		{ID: 1, Role: agent.RoleUser, Content: "q"},
		{ID: 2, Role: agent.RoleTool, ToolCallID: "ghost", Content: "leftover"},
		{ID: 3, Role: agent.RoleAssistant, Content: "done"},
	}
	view, err := Fold(rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(view) != 3 {
		t.Fatalf("view len = %d, want 3 (orphan kept)", len(view))
	}
	if view[1].ToolCallID != "ghost" {
		t.Fatalf("orphan row displaced: %+v", view[1])
	}
}
