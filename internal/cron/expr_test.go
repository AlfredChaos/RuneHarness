package cron

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, expr string) *Schedule {
	t.Helper()
	s, err := Parse(expr)
	if err != nil {
		t.Fatalf("Parse(%q): %v", expr, err)
	}
	return s
}

func TestParseRejects(t *testing.T) {
	for _, expr := range []string{
		"",             // 空
		"* * * *",      // 少一段
		"* * * * * *",  // 多一段
		"60 * * * *",   // 分钟越界
		"* 24 * * *",   // 小时越界
		"* * 0 * *",    // dom 下限
		"* * * 13 *",   // 月份越界
		"* * * * 8",    // dow 越界（7 之后无别名）
		"*/0 * * * *",  // 步进 0
		"*/* * * * *",  // 步进非数字
		"a * * * *",    // 非数字
		"1-b * * * *",  // 区间端点非数字
		"5-1 * * * *",  // 区间倒置
		"* * * * MON",  // 名字别名不支持
		"0 9 1-32 * *", // dom 区间上界越界
		"* * * * 1-8",  // dow 区间上界越界（7 是上限）
	} {
		if _, err := Parse(expr); err == nil {
			t.Fatalf("Parse(%q) expected error", expr)
		}
	}
}

func TestParseNormalize(t *testing.T) {
	s := mustParse(t, "  0   9 * * 1-5  ")
	if s.Expr() != "0 9 * * 1-5" {
		t.Fatalf("normalized expr = %q", s.Expr())
	}
	// dow=7 归一为周日（0）
	s = mustParse(t, "0 9 * * 7")
	if len(s.dow) != 1 || s.dow[0] != 0 {
		t.Fatalf("dow=7 should normalize to Sunday, got %v", s.dow)
	}
	// 区间含 7：5-7 = 五,六,日
	s = mustParse(t, "0 9 * * 5-7")
	if len(s.dow) != 3 || s.dow[0] != 0 || s.dow[1] != 5 || s.dow[2] != 6 {
		t.Fatalf("dow 5-7 should be {0,5,6}, got %v", s.dow)
	}
	// 全周区间不被提前截断（回归：dow 归一不得污染循环变量）
	s = mustParse(t, "0 9 * * 0-6")
	if len(s.dow) != 7 {
		t.Fatalf("dow 0-6 should be all 7 days, got %v", s.dow)
	}
	// 逗号列表 + 区间步进（步进只挂 * 或 N-M，不挂列表元素）
	s = mustParse(t, "0,30 9-17/2 * * 1,3,5")
	if len(s.minute) != 2 || len(s.hour) != 5 || len(s.dow) != 3 {
		t.Fatalf("list/step parse wrong: minute=%v hour=%v dow=%v", s.minute, s.hour, s.dow)
	}
}

func TestMatchWeekdayAndTime(t *testing.T) {
	loc := time.UTC
	s := mustParse(t, "0 9 * * 1-5")
	cases := []struct {
		at   time.Time
		want bool
	}{
		{time.Date(2024, 1, 15, 9, 0, 0, 0, loc), true},  // 周一 9:00
		{time.Date(2024, 1, 15, 9, 1, 0, 0, loc), false}, // 周一 9:01
		{time.Date(2024, 1, 14, 9, 0, 0, 0, loc), false}, // 周日 9:00
		{time.Date(2024, 1, 19, 9, 0, 0, 0, loc), true},  // 周五 9:00
		{time.Date(2024, 1, 20, 9, 0, 0, 0, loc), false}, // 周六 9:00
	}
	for _, c := range cases {
		if got := s.Match(c.at, loc); got != c.want {
			t.Fatalf("Match(%v) = %v, want %v", c.at, got, c.want)
		}
	}
}

func TestMatchDomDowOr(t *testing.T) {
	loc := time.UTC
	// DOM 与 DOW 同时受限 → OR：每月 1 号或每周一
	s := mustParse(t, "0 0 1 * 1")
	if !s.Match(time.Date(2024, 3, 1, 0, 0, 0, 0, loc), loc) { // 3/1 周五，dom 命中
		t.Fatal("dom hit should match")
	}
	if !s.Match(time.Date(2024, 3, 4, 0, 0, 0, 0, loc), loc) { // 3/4 周一，dow 命中
		t.Fatal("dow hit should match")
	}
	if s.Match(time.Date(2024, 3, 5, 0, 0, 0, 0, loc), loc) { // 3/5 周二，都不命中
		t.Fatal("neither dom nor dow should match")
	}
	// 单方受限时无 OR：dow=* 时只看 dom
	s = mustParse(t, "0 0 1 * *")
	if s.Match(time.Date(2024, 3, 4, 0, 0, 0, 0, loc), loc) { // 3/4 周一但不是 1 号
		t.Fatal("dom-only constraint must not match on weekday")
	}
}

func TestNextStrictlyAfterFrom(t *testing.T) {
	loc := time.UTC
	s := mustParse(t, "30 14 * * *")
	next, ok := s.Next(time.Date(2024, 1, 15, 14, 29, 0, 0, loc), loc)
	if !ok || !next.Equal(time.Date(2024, 1, 15, 14, 30, 0, 0, loc)) {
		t.Fatalf("next = %v ok=%v", next, ok)
	}
	// 恰好在触发点：严格晚于 from → 次日
	next, ok = s.Next(time.Date(2024, 1, 15, 14, 30, 0, 0, loc), loc)
	if !ok || !next.Equal(time.Date(2024, 1, 16, 14, 30, 0, 0, loc)) {
		t.Fatalf("next at-exact = %v ok=%v", next, ok)
	}
	// 每周一 9:00：from 周一 9:00 应给下周一
	s = mustParse(t, "0 9 * * 1")
	next, ok = s.Next(time.Date(2024, 1, 15, 9, 0, 0, 0, loc), loc)
	if !ok || !next.Equal(time.Date(2024, 1, 22, 9, 0, 0, 0, loc)) {
		t.Fatalf("next weekday = %v ok=%v", next, ok)
	}
	// 跨年：12/31 之后 → 次年
	s = mustParse(t, "0 0 1 1 *")
	next, ok = s.Next(time.Date(2024, 12, 31, 0, 0, 0, 0, loc), loc)
	if !ok || !next.Equal(time.Date(2025, 1, 1, 0, 0, 0, 0, loc)) {
		t.Fatalf("next cross-year = %v ok=%v", next, ok)
	}
}

func TestNextTimezone(t *testing.T) {
	sh, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skip("Asia/Shanghai tzdata unavailable")
	}
	// 上海 9:00 = UTC 1:00。from = UTC 2024-01-15 16:00 → 上海 1/16 00:00；
	// 下一个上海 9:00 是 1/16 9:00 = UTC 1/16 01:00。
	s := mustParse(t, "0 9 * * *")
	next, ok := s.Next(time.Date(2024, 1, 15, 16, 0, 0, 0, time.UTC), sh)
	if !ok {
		t.Fatal("no next")
	}
	wantUTC := time.Date(2024, 1, 16, 1, 0, 0, 0, time.UTC)
	if !next.Equal(wantUTC) {
		t.Fatalf("next = %v, want %v", next, wantUTC)
	}
	// 验证返回时刻确实落在上海时区 9:00
	inSH := next.In(sh)
	if inSH.Hour() != 9 || inSH.Day() != 16 {
		t.Fatalf("next in Shanghai = %v, want day16 9:00", inSH)
	}
}

func TestNextDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("America/New_York tzdata unavailable")
	}
	// 2024-03-10 春拨快：02:30 不存在（2:00→3:00）→ 当天跳过到 3/11
	s := mustParse(t, "30 2 * * *")
	next, ok := s.Next(time.Date(2024, 3, 9, 12, 0, 0, 0, ny), ny)
	if !ok {
		t.Fatal("no next")
	}
	if next.In(ny).Day() != 11 {
		t.Fatalf("spring-forward: next = %v, want Mar 11 02:30", next.In(ny))
	}
	// 2024-11-03 秋回拨：1:30 出现两次（EDT 与 EST）→ 只触发一次
	s = mustParse(t, "30 1 * * *")
	next, ok = s.Next(time.Date(2024, 11, 3, 0, 0, 0, 0, ny), ny)
	if !ok {
		t.Fatal("no next")
	}
	got := next.In(ny)
	if got.Day() != 3 || got.Hour() != 1 || got.Minute() != 30 {
		t.Fatalf("fall-back: next = %v, want Nov 3 01:30", got)
	}
	// 触发一次后下一个应是 11/4，不是第二个 1:30
	next2, _ := s.Next(next, ny)
	if next2.In(ny).Day() != 4 {
		t.Fatalf("fall-back second next = %v, want Nov 4", next2.In(ny))
	}
}

func TestNextImpossible(t *testing.T) {
	// 2 月 31 日永不命中 → 8 年搜索界遍历后 false（dow 受限同理可救，
	// 这里 dow=* 无 OR 逃生口）
	s := mustParse(t, "0 0 31 2 *")
	if _, ok := s.Next(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), time.UTC); ok {
		t.Fatal("Feb 31 must never match")
	}
}

func TestNextLeapDayBeyondOneYear(t *testing.T) {
	// 2/29 任务创建在非闰年：下一个匹配是三年后（2024→2028 需跨
	// 两个闰年窗口），一年搜索界会误判为永不命中而拒绝注册。
	s := mustParse(t, "0 9 29 2 *")
	next, ok := s.Next(time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC), time.UTC)
	if !ok {
		t.Fatal("Feb 29 schedule must find a match")
	}
	if !next.Equal(time.Date(2028, 2, 29, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("next = %v, want 2028-02-29 09:00", next)
	}
}

func TestHumanize(t *testing.T) {
	for expr, want := range map[string]string{
		"* * * * *":    "every minute",
		"*/5 * * * *":  "every 5 minutes",
		"0 * * * *":    "every hour",
		"0 9 * * *":    "every day at 09:00",
		"0 9 * * 1-5":  "weekdays at 09:00",
		"0 9 * * 1":    "every Monday at 09:00",
		"30 14 28 2 *": "30 14 28 2 *", // 无法归类时原样
		"0,30 9 * * *": "0,30 9 * * *",
	} {
		if got := Humanize(expr); got != want {
			t.Fatalf("Humanize(%q) = %q, want %q", expr, got, want)
		}
	}
}
