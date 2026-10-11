package config

import "testing"

// 工作窗口解析：显式覆盖 > 模型表前缀（最长匹配优先）> 200K 回落。
func TestContextWindowResolution(t *testing.T) {
	cases := []struct {
		env, model string
		want       int
	}{
		{"96000", "anything", 96_000}, // 显式覆盖最高优先
		{"", "MiniMax-M3", 1_000_000}, // 百万家族
		{"", "minimax-m1", 1_000_000}, // 大小写不敏感
		{"", "deepseek-chat", 128_000},
		{"", "gpt-4.1-mini", 1_000_000}, // 长前缀优先于 gpt-4o
		{"", "gpt-4o", 128_000},
		{"", "qwen3-max", 262_144},
		{"", "some-new-model", 200_000}, // 未命中回落 200K
		{"", "", 200_000},
	}
	for _, c := range cases {
		got, err := contextWindow(c.env, c.model)
		if err != nil || got != c.want {
			t.Errorf("contextWindow(%q,%q) = %d,%v; want %d", c.env, c.model, got, err, c.want)
		}
	}
	if _, err := contextWindow("abc", "x"); err == nil {
		t.Error("non-numeric override should error")
	}
}

// 视觉能力表：命中前缀即图片输入，未命中一律纯文本（默认 false）。
func TestModelVision(t *testing.T) {
	cases := map[string]bool{
		"minimax-m3":       true,  // 主用端点，已确认视觉
		"MiniMax-M3":       true,  // 大小写不敏感
		"minimax-m3-flash": true,  // 家族前缀
		"minimax-m2":       false, // 旧家族未确认，不放行
		"gpt-4o":           true,
		"gpt-4.1-mini":     true,
		"gpt-5":            true,
		"o4-mini":          true,
		"claude-sonnet-4":  true,
		"gemini-2.0-flash": true,
		"grok-4":           true,
		"grok-2-vision":    true,
		"deepseek-chat":    false,
		"qwen3-max":        false,
		"":                 false,
	}
	for model, want := range cases {
		if got := ModelVision(model); got != want {
			t.Errorf("ModelVision(%q) = %v, want %v", model, got, want)
		}
	}
}
