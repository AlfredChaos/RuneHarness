package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"runeharness/internal/agent"
)

// 间隔格式化：分钟/小时/天三档口语量级。
func TestFormatGap(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "1 分钟"},
		{3 * time.Minute, "3 分钟"},
		{59 * time.Minute, "59 分钟"},
		{5 * time.Hour, "5 小时"},
		{26 * time.Hour, "1 天"},
		{3 * 24 * time.Hour, "3 天"},
	}
	for _, tc := range cases {
		if got := formatGap(tc.d); got != tc.want {
			t.Fatalf("formatGap(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// 运行时上下文块：附在当前用户消息末尾，含当前时间；
// 有更早的真实用户消息时含"距上一条消息 N"——解决"隔了 3 天还问洗完了吗"。
func TestRuntimeDecorateAppendsContext(t *testing.T) {
	old := agent.Message{Role: agent.RoleUser, Content: "我去洗澡",
		CreatedAt: time.Now().Add(-3 * 24 * time.Hour).UnixMilli()}
	cur := agent.Message{Role: agent.RoleUser, Content: "回来啦",
		CreatedAt: time.Now().UnixMilli()}
	view := []agent.Message{
		{Role: agent.RoleSystem, Content: "sys"},
		old,
		{Role: agent.RoleAssistant, Content: "去吧"},
		cur,
	}
	out := runtimeDecorate(context.Background(), view)
	last := out[len(out)-1].Content
	if !strings.Contains(last, "<runtime-context>当前时间") {
		t.Fatalf("runtime context missing: %q", last)
	}
	if !strings.Contains(last, "距上一条消息 3 天") {
		t.Fatalf("gap should be surfaced: %q", last)
	}
	// 只改请求副本：入参消息不动。
	if strings.Contains(view[3].Content, "runtime-context") {
		t.Fatal("decorate must not mutate the input view")
	}
}

// 无更早用户消息时只有时间、没有间隔；无真实用户消息时原样返回。
func TestRuntimeDecorateEdgeCases(t *testing.T) {
	one := []agent.Message{{Role: agent.RoleUser, Content: "hi", CreatedAt: time.Now().UnixMilli()}}
	out := runtimeDecorate(context.Background(), one)
	if strings.Contains(out[0].Content, "距上一条") {
		t.Fatal("first message has no previous gap")
	}
	empty := runtimeDecorate(context.Background(), nil)
	if empty != nil {
		t.Fatal("empty view stays empty")
	}
	// inject 行不算真实用户消息。
	inj := []agent.Message{{Role: agent.RoleUser, Kind: agent.KindInject, Content: "nag"}}
	if got := runtimeDecorate(context.Background(), inj); got[0].Content != "nag" {
		t.Fatal("inject rows are not decorated")
	}
}
