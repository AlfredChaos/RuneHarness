// Package cron 提供定时任务调度：五段式 cron 表达式按任务时区求值，
// 触发的工作经 pending 队列由 TUI 在 agent 空闲时交付为新一轮。
// 调度与执行解耦：本包只负责"到点入队"，不调用模型。
package cron

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Schedule 是解析后的五段式 cron 表达式；各字段为升序去重的取值集合。
// 支持 *、*/N、N、N-M、N-M/S、N,M 列表；不支持 L/W/? 与名字别名。
// 时间按任务自带时区解释——"0 9 * * 1-5" 在该时区的工作日 9:00 触发。
type Schedule struct {
	minute []int // 0-59
	hour   []int // 0-23
	dom    []int // 1-31
	month  []int // 1-12
	dow    []int // 0-6，0=周日（7 解析时归一为 0）
	expr   string
}

// fieldRange 是一个字段的合法取值区间。
type fieldRange struct{ min, max int }

var fieldRanges = []fieldRange{
	{0, 59}, // minute
	{0, 23}, // hour
	{1, 31}, // day-of-month
	{1, 12}, // month
	{0, 6},  // day-of-week（解析时接受 7 作周日别名）
}

// Parse 解析五段式 cron 表达式；非法返回 error（供工具层直接回报模型）。
func Parse(expr string) (*Schedule, error) {
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return nil, fmt.Errorf("expected 5 fields (minute hour day-of-month month day-of-week), got %d", len(parts))
	}
	s := &Schedule{expr: strings.Join(parts, " ")}
	out := make([][]int, 5)
	for i, p := range parts {
		v, err := expandField(p, fieldRanges[i])
		if err != nil {
			return nil, fmt.Errorf("field %d (%q): %w", i+1, p, err)
		}
		out[i] = v
	}
	s.minute, s.hour, s.dom, s.month, s.dow = out[0], out[1], out[2], out[3], out[4]
	return s, nil
}

// expandField 把单字段展开为升序取值集合；支持通配、步进、区间、列表。
func expandField(field string, r fieldRange) ([]int, error) {
	isDow := r.min == 0 && r.max == 6
	set := map[int]bool{}
	for _, part := range strings.Split(field, ",") {
		switch {
		case part == "*" || strings.HasPrefix(part, "*/"):
			step := 1
			if part != "*" {
				n, err := strconv.Atoi(part[2:])
				if err != nil || n < 1 {
					return nil, fmt.Errorf("invalid step %q", part)
				}
				step = n
			}
			for i := r.min; i <= r.max; i += step {
				set[i] = true
			}
		case strings.Contains(part, "-"):
			loStr, rest, _ := strings.Cut(part, "-")
			hiStr, stepStr, hasStep := strings.Cut(rest, "/")
			lo, err1 := strconv.Atoi(loStr)
			hi, err2 := strconv.Atoi(hiStr)
			step := 1
			var err3 error
			if hasStep {
				step, err3 = strconv.Atoi(stepStr)
			}
			effMax := r.max
			if isDow {
				effMax = 7 // dow 区间允许 7 作周日别名（如 5-7 = 五,六,日）
			}
			if err1 != nil || err2 != nil || err3 != nil ||
				lo > hi || step < 1 || lo < r.min || hi > effMax {
				return nil, fmt.Errorf("invalid range %q", part)
			}
			for i := lo; i <= hi; i += step {
				if isDow && i == 7 {
					set[0] = true // 7 归一为周日
					break
				}
				set[i] = true
			}
		default:
			n, err := strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("invalid value %q", part)
			}
			if isDow && n == 7 {
				n = 0
			}
			if n < r.min || n > r.max {
				return nil, fmt.Errorf("value %d out of range [%d,%d]", n, r.min, r.max)
			}
			set[n] = true
		}
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("empty field %q", field)
	}
	out := make([]int, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Ints(out)
	return out, nil
}

// maxLookAheadDays 是 Next 的搜索上限：8 年覆盖最宽的合法命中间隔
// （2096-02-29 → 2104-02-29 跨世纪非闰年的 8 年闰周期间隔）；8 年
// 无匹配的表达式可判定为永不命中（如 "0 0 31 2 *"）。
const maxLookAheadDays = 8 * 366

// Next 返回严格晚于 from 的下一个匹配时刻，按 loc 时区求值
// （loc 为 nil 时用本地时区）。maxLookAheadDays 内无匹配返回 false。
//
// 逐段跳跃（月→日→时→分）而非逐分钟遍历。DOM 与 DOW 同时受限时任一
// 命中即可（标准 cron 的 OR 语义）。DST：固定时刻落在春季拨快的空档里
// 当天跳过；秋季回拨只触发一次（步进越过重复区间）。与 vixie-cron 一致。
func (s *Schedule) Next(from time.Time, loc *time.Location) (time.Time, bool) {
	if loc == nil {
		loc = time.Local
	}
	minuteSet := intSet(s.minute)
	hourSet := intSet(s.hour)
	domSet := intSet(s.dom)
	monthSet := intSet(s.month)
	dowSet := intSet(s.dow)
	domWild := len(s.dom) == 31
	dowWild := len(s.dow) == 7

	// 严格晚于 from：向上取整到下一分钟起点。
	t := from.In(loc).Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < maxLookAheadDays*24*60; i++ {
		if !monthSet[int(t.Month())] {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc)
			continue
		}
		if !dayOK(domSet, dowSet, domWild, dowWild, t.Day(), t.Weekday()) {
			t = stepTime(t, time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc), 24*time.Hour)
			continue
		}
		if !hourSet[t.Hour()] {
			t = stepTime(t, time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, loc), time.Hour)
			continue
		}
		if !minuteSet[t.Minute()] {
			t = stepTime(t, time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute()+1, 0, 0, loc), time.Minute)
			continue
		}
		return t, true
	}
	return time.Time{}, false
}

// dayOK 判定某天的日匹配：DOM/DOW 双受限时任一命中（标准 cron OR
// 语义），单侧受限按该侧，两侧皆通配恒真。
func dayOK(domSet, dowSet map[int]bool, domWild, dowWild bool, day int, wd time.Weekday) bool {
	switch {
	case domWild && dowWild:
		return true
	case domWild:
		return dowSet[int(wd)]
	case dowWild:
		return domSet[day]
	default:
		return domSet[day] || dowSet[int(wd)]
	}
}

// stepTime 保证 Next 的步进单调前进：time.Date 在 DST 春季空档上会把
// 不存在的时刻归一回更早的本地时间（如纽约 3/10 02:00→01:00 EST），
// 不用守卫会把循环卡死在同一时刻；此时改用绝对时间 fallback 跨过空档。
func stepTime(from, candidate time.Time, fallback time.Duration) time.Time {
	if candidate.After(from) {
		return candidate
	}
	return from.Add(fallback)
}

// Match 报告 t（按 loc 求值）是否命中表达式；调度器测试与调试用。
func (s *Schedule) Match(t time.Time, loc *time.Location) bool {
	if loc == nil {
		loc = time.Local
	}
	t = t.In(loc)
	if !intSet(s.month)[int(t.Month())] || !intSet(s.hour)[t.Hour()] || !intSet(s.minute)[t.Minute()] {
		return false
	}
	return dayOK(intSet(s.dom), intSet(s.dow), len(s.dom) == 31, len(s.dow) == 7,
		t.Day(), t.Weekday())
}

// Expr 返回规范化后的表达式原文（字段间单空格）。
func (s *Schedule) Expr() string { return s.expr }

// intSet 把取值列表转查重 map；字段最多 60 个值，构建开销可忽略。
func intSet(vs []int) map[int]bool {
	m := make(map[int]bool, len(vs))
	for _, v := range vs {
		m[v] = true
	}
	return m
}

// ── Humanize ────────────────────────────────────────────────────────
// 刻意收窄：覆盖常见模式（每 N 分钟、整点、每日/周几几点、工作日），
// 其余原样回显 cron 串。仅供展示，不参与调度。

var dowNames = []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}

// Humanize 把 cron 表达式译成简短英文描述；无法归类时返回原串。
func Humanize(expr string) string {
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return expr
	}
	minute, hour, dom, month, dow := parts[0], parts[1], parts[2], parts[3], parts[4]
	wild := dom == "*" && month == "*" && dow == "*"

	if wild && hour == "*" {
		if minute == "*" {
			return "every minute"
		}
		if n, ok := stepOf(minute); ok {
			if n == 1 {
				return "every minute"
			}
			return fmt.Sprintf("every %d minutes", n)
		}
	}
	if m, ok := numOf(minute); ok && wild {
		if hour == "*" {
			if m == 0 {
				return "every hour"
			}
			return fmt.Sprintf("every hour at :%02d", m)
		}
		if n, ok := stepOf(hour); ok {
			suffix := ""
			if m != 0 {
				suffix = fmt.Sprintf(" at :%02d", m)
			}
			if n == 1 {
				return "every hour" + suffix
			}
			return fmt.Sprintf("every %d hours%s", n, suffix)
		}
	}
	m, mok := numOf(minute)
	h, hok := numOf(hour)
	if !mok || !hok {
		return expr
	}
	at := fmt.Sprintf("at %02d:%02d", h, m)
	switch {
	case dom == "*" && month == "*" && dow == "*":
		return "every day " + at
	case dom == "*" && month == "*" && dow == "1-5":
		return "weekdays " + at
	case dom == "*" && month == "*" && len(dow) == 1:
		if d, err := strconv.Atoi(dow); err == nil && d >= 0 && d <= 7 {
			return "every " + dowNames[d%7] + " " + at
		}
	}
	return expr
}

func numOf(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// stepOf 解析 "*/N"，返回 N；非步进形式返回 false。
func stepOf(s string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimPrefix(s, "*/"))
	if err != nil || !strings.HasPrefix(s, "*/") || n < 1 {
		return 0, false
	}
	return n, true
}
