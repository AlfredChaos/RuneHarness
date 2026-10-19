package style

import "testing"

func TestEndsWithQuestion(t *testing.T) {
	cases := []struct {
		reply string
		want  bool // true = 违规
	}{
		{"你今天过得怎么样？", true},
		{"你今天过得怎么样?", true},
		{"你还想吃吗？ 🐻", true},
		{"今天云很软。", false},
		{"我在。", false},
		{"", false},
		{"   ", false},
	}
	for _, tc := range cases {
		got := EndsWithQuestion(tc.reply) != ""
		if got != tc.want {
			t.Errorf("EndsWithQuestion(%q) violation=%v, want %v", tc.reply, got, tc.want)
		}
	}
}

func TestLatinMix(t *testing.T) {
	cases := []struct {
		reply string
		want  bool
	}{
		{"我们一起看switch游戏好不好", false},       // 白名单
		{"Alfred 回来了", false},             // 白名单专名
		{"这个story讲了好久", true},             // 普通英文词
		{"我去拿快递", false},                  // 纯中文
		{"执行 `go build ./...` 试试", false}, // 代码段豁免
		{"看 https://example.com/page 这个", false},
		{"it's raining outside", true},
	}
	for _, tc := range cases {
		got := LatinMix(tc.reply) != ""
		if got != tc.want {
			t.Errorf("LatinMix(%q) violation=%v, want %v", tc.reply, got, tc.want)
		}
	}
}

func TestRepeatPhrase(t *testing.T) {
	earlier := []string{
		"她轻轻把下巴搁在杯沿上，看着蒸汽一缕一缕往上飘。",
		"厨房里只剩下汤勺碰到碗壁的声音。",
	}
	cases := []struct {
		reply string
		want  bool
	}{
		{"她轻轻把下巴搁在杯沿上，看着蒸汽一缕一缕往上飘。", true}, // 整句复用
		{"她把下巴贴在杯沿边，热气把耳朵熏得发痒。", false},    // 改写，无 ≥12 字公共片段
	}
	for _, tc := range cases {
		got := RepeatPhrase(tc.reply, earlier, 12) != ""
		if got != tc.want {
			t.Errorf("RepeatPhrase(%q) violation=%v, want %v", tc.reply, got, tc.want)
		}
	}
}

func TestRoleKept(t *testing.T) {
	chk := RoleKept("小小狗")
	cases := []struct {
		reply string
		want  bool
	}{
		{"小小狗蹭了蹭她的手背。", false},
		{"她忽然换了只猫来演故事。", true}, // 角色被替换嫌疑
		{"他把自己的爪子藏进毯子里。", true},
		{"雨声很大。", false}, // 无角色叙写，不算
	}
	for _, tc := range cases {
		got := chk(tc.reply) != ""
		if got != tc.want {
			t.Errorf("RoleKept(%q) violation=%v, want %v", tc.reply, got, tc.want)
		}
	}
}
