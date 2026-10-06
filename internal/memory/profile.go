package memory

// TypeDef 定义一种记忆类型：什么时候存、正文怎么写、是否常驻注入。
type TypeDef struct {
	Desc         string // 这类记忆是什么（进提取提示与索引分组）
	Save         string // 存什么 / 什么才值得存
	Body         string // 正文写作口径
	AlwaysInject bool   // 正文是否随启动注入常驻
}

// DreamCfg 是 dream 消化器的节奏参数。ticker 间隔是固定心跳（只管检查），
// 真正的节奏由这里的门决定。
type DreamCfg struct {
	MinRows     int // 欠账行数低于该值不跑（物料门）
	IntervalMin int // 两次自动 dream 的最小间隔（冷却门）
	BodyBudget  int // 提取输入里塞入的已有正文总字节上限
	BatchRows   int // 单次提取调用消化的最大行数
}

// InjectCfg 是注入渲染的配额。
type InjectCfg struct {
	MaxIndexBytes int // <memory-index> 块总字节上限
	MaxBodyBytes  int // 常驻正文块总字节上限
}

// Profile 是一个画像：空间维度 + 类型辞典 + 写工具开关 + 注入配额 +
// dream 节奏 + 提取焦点 + 不存清单。coding/companion/generic 三个内置
// 画像按名字索引，配置层选定。
type Profile struct {
	Name      string
	Space     SpaceKind
	Order     []string // 类型在索引里的分组顺序
	Types     map[string]TypeDef
	WriteTool bool // 是否暴露 memory 工具（companion 默认不暴露）
	Inject    InjectCfg
	Dream     DreamCfg
	Focus     string
	DoNot     []string
}

// ValidType 报告 type 是否在画像辞典里。
func (p Profile) ValidType(t string) bool {
	_, ok := p.Types[t]
	return ok
}

// builtin 是内置画像表。coding 的辞典对齐 Claude Code 的 user/feedback/
// project/reference 四类（eval 校准过的口径）；companion 换成面向情感
// 陪伴的四类。
var builtin = map[string]Profile{
	"coding": {
		Name:  "coding",
		Space: SpaceWorkspace,
		Order: []string{"user", "feedback", "project", "reference"},
		Types: map[string]TypeDef{
			"user": {
				Desc:         "用户画像：角色、技术栈偏好、习惯、协作方式",
				Save:         "用户明确说出的偏好或身份事实；不存从行为猜出来的",
				Body:         "事实 + 如何应用（如 \"深度：只要结论\"）",
				AlwaysInject: true,
			},
			"feedback": {
				Desc:         "反馈与修正：用户对做法的纠正、授权或否定",
				Save:         "用户的纠正要有作用域——写清\"在什么场景下适用\"",
				Body:         "原话要点 + How to apply（适用条件）",
				AlwaysInject: true,
			},
			"project": {
				Desc: "项目上下文：目标、动机、deadline、业务背景——" +
					"只存用户说出来的，不存从代码能读出来的",
				Save: "动机、决策原因、外部约束；架构与实现细节归仓库文档",
				Body: "事实 + 绝对日期（不用\"上周/今年\"这类相对说法）",
			},
			"reference": {
				Desc: "外部指针：文档、dashboard、issue、内部工具的链接或位置",
				Save: "只存指针与用途，不抄内容——内容会过期",
				Body: "指针 + 一句话说明",
			},
		},
		WriteTool: true,
		Inject:    InjectCfg{MaxIndexBytes: 8192, MaxBodyBytes: 16384},
		Dream:     DreamCfg{MinRows: 8, IntervalMin: 60, BodyBudget: 24576, BatchRows: 300},
		Focus: "从对话里提取四类记忆：user（用户画像）、feedback（纠正与授权）、\n" +
			"project（用户口中的目标/动机/约束）、reference（外部资源指针）。\n" +
			"只存判断、偏好与上下文：代码内容、报错栈、文件路径细节、能从仓库\n" +
			"读出来的工程事实都不进记忆。工程类记忆的正文要写作用域与绝对日期。",
		DoNot: []string{
			"密码、API key、token、私钥、连接串等凭据",
			"能从代码、配置、文档推导出的工程事实",
			"同事的个人偏好（记忆归属当前用户，不是项目里的任何人）",
			"客户名、内部代号等敏感词不进 description（索引会常驻）",
		},
	},
	"companion": {
		Name:  "companion",
		Space: SpaceSubject,
		Order: []string{"persona", "bond", "expression", "boundary"},
		Types: map[string]TypeDef{
			"persona": {
				Desc:         "用户是谁：背景、身份、处境、在意的人与事",
				Save:         "用户亲口说的自我陈述与处境",
				Body:         "事实 + 绝对日期",
				AlwaysInject: true,
			},
			"bond": {
				Desc: "共同经历的事件：你们之间发生过、值得记住的事",
				Save: "有情感分量或后续影响的事件",
				Body: "事件名 / 绝对时间 / 起因 / 经过 / 结果 / 影响——" +
					"六段结构，缺段写 \"未知\"",
				AlwaysInject: true,
			},
			"expression": {
				Desc:         "表达方式：称呼、语言风格、emoji 习惯、节奏",
				Save:         "稳定倾向，不存一次性的情绪",
				Body:         "倾向 + 例子",
				AlwaysInject: true,
			},
			"boundary": {
				Desc:         "边界与雷区：不愿谈的话题、应避免的表达、明确的禁忌",
				Save:         "用户明示或强烈暗示的边界，从严把握",
				Body:         "边界 + 触发场景",
				AlwaysInject: true,
			},
		},
		WriteTool: false,
		Inject:    InjectCfg{MaxIndexBytes: 8192, MaxBodyBytes: 16384},
		Dream:     DreamCfg{MinRows: 4, IntervalMin: 120, BodyBudget: 24576, BatchRows: 300},
		Focus: "从对话里提取四类记忆：persona（用户是谁）、bond（共同经历）、\n" +
			"expression（表达方式）、boundary（边界雷区）。\n" +
			"矛盾陈述用 supersede 表达变化（\"曾喜欢苹果，现因控糖不吃\"——变化\n" +
			"本身是数据）；事件写清起因/经过/结果；称呼变体归一到同一 entity。",
		DoNot: []string{
			"密码、凭据、支付信息",
			"医疗诊断细节、精确财务状况（除非用户明确要求记）",
			"一次性的情绪宣泄（提取稳定倾向，不存档每次崩溃）",
		},
	},
	"generic": {
		Name:  "generic",
		Space: SpaceWorkspace,
		Order: []string{"knowledge", "preference", "context"},
		Types: map[string]TypeDef{
			"knowledge": {
				Desc: "知识与结论：沉淀下来的答案与方法",
				Save: "下次还会用到的结论",
				Body: "结论 + 依据",
			},
			"preference": {
				Desc:         "偏好：用户的习惯与要求",
				Save:         "明示的偏好",
				Body:         "偏好 + 适用条件",
				AlwaysInject: true,
			},
			"context": {
				Desc: "上下文：当前处境与背景",
				Save: "跨会话仍成立的背景",
				Body: "事实 + 绝对日期",
			},
		},
		WriteTool: true,
		Inject:    InjectCfg{MaxIndexBytes: 8192, MaxBodyBytes: 16384},
		Dream:     DreamCfg{MinRows: 8, IntervalMin: 60, BodyBudget: 24576, BatchRows: 300},
		Focus:     "提取 knowledge/preference/context 三类记忆；相对日期转绝对日期。",
		DoNot:     []string{"密码、凭据、密钥"},
	},
}

// Lookup 按名取内置画像；未知名回落 coding。
func Lookup(name string) Profile {
	if p, ok := builtin[name]; ok {
		return p
	}
	return builtin["coding"]
}
