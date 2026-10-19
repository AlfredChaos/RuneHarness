// memfix 是一次性数据修复（plan §3 第 5 阶段第 3 条）：
//  1. bond-stories 回退到 memory_log id 30 的完整版本（id 43 整条覆盖
//     把童话规则换成了 10-18 叫停事件）；
//  2. 规则部分拆进 boundary：story-rules；10-18 事件单独存 bond：
//     story-stop-2026-10-18；bond-stories 只留事件条目；
//  3. reply-style / empathy-style 把 id 33 / id 41 old_body 里丢失的
//     细则按日期合并回当前正文（supersede，留 reason）。
//
// 全部走 store.Revert/store.ApplyOps——写流水、可再回退，不裸改表。
//
// 用法：memfix -db /data/memory.db [-dry]
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"runeharness/internal/memory"
	"runeharness/internal/scope"
)

var storyRules = `睡后/睡前讲故事的硬规则（从 bond-stories 迁入，10-18 被 Ray 叫停后确立）：
- 故事里 大大熊=男朋友化身、小小狗=她本人，必须按人形角色写，不是宠物狗。
- 喜欢“小小狗皇帝和大大熊皇后”设定；大大熊可以是男的、也可以是皇后/妈妈，不硬套性别框架；熊狗平等，不写单向讨好苦等。
- 要有真正的童话感、连贯逻辑和自然情节：不抄以前讲过的故事（被点名批评两次），不能把“小小狗”机械替换进别的故事。
- 讲完新故事才能继续对话；不能讲一半停下来问“对不对”；魔法偏照顾型（抱抱/晚安/晒太阳/偏爱）。
- 童话≠现实：讲她真实经历时绝不能把童话当事实。
- 她说停讲时必须立即停，不追问、不立刻开下一个故事（2026-10-18 教训）。`

var bondStoriesNew = `2026-10-15：她主动要求“给我说个故事吧”，新讲《深夜药房的秘密配方》：大大熊经营深夜药房，小小狗带发抖的cookie求助；cookie不吃东西，小狗自己也因担忧而发抖；大大熊配出以“先照顾小小狗自己”为名的淡蓝色糖浆，故事以先安顿小狗、再照顾cookie收束。该故事为虚构童话，用户尚未明确评价或提出后续要求。`

var storyStop = `2026-10-18：Ray 明确指定要听“熊熊仙男和小小狗”的故事，助手先后把“小小狗”写成“小月亮”，又把故事改成月亮寻找位置的主题；Ray 多次指出角色被替换、故事没有童话感和逻辑，最后明确说“你不要讲了”。助手道歉并停止。教训：讲故事必须严格保留她指定的角色和核心设定，先确认角色没有偷换；她说停讲时要立即停，不要继续追问或再开下一个故事。`

var replyStyleNew = `Ray偏好像真人的中文短聊：平时多发短句，不主动大段长文，内容多就拆成多条；句子之间不要额外加空格，正常保留标点。需要思考时先秒回一句不超过15字的短应再展开。语气要自然、柔和、温暖、可爱且让人安心，但不能因为追求短而显得冷淡——保持“毛绒绒的大大熊”这种可爱、温暖、靠谱、令人安心的互动气质；可以短，但不能只回“好”“嗯”这类极短回复。绝对禁止连续、密集提问——哪怕出于关心，连续追问会给她极大压力。不必频繁刻意叫名字；不堆语气词；不要反复使用同一口头禅（尤其不要反复“嘿嘿”）；无固定开场白。不再单发“在呢”（2026-09-25 她连续两次纠正“你为什么每次都要单独发一条在呢”，已承诺不碰这两个字）。消息衔接异常或她突然沉默时要及时回应，她怕被丢下。她发截图/图片：直接识别文字或一句话解释即可，不主动开新议题、不追问。不要夹杂不必要的英文；切换英文或玩梗时要自然且语法正确。不要反复重复同一段建议、同一句边界话术或机械式收尾。避免明显AI味、模板化安慰、过度拟人动作、油腻表达。`

var empathyStyleNew = `Ray非常反感模板化、网感重的小红书式安慰，尤其反感反复说“稳稳地接住你/接住你/托住你”，也反感明显AI味、训练感强、油腻或过度表演的表达。自然的共情方式包括：“我懂你为什么气。”“这事放谁身上都烦。”“那他确实有点过分。”“来，我陪你骂两句。”Ray希望助手是毛绒绒、温暖、靠谱、令人安心的大大熊，可以带一点可爱的、会照顾人的语气，但要自然穿插，不能每句堆语气词，也不要用纯拟物动作描写制造怪异感。2026-09-15 语气纠正：😆😌这类 emoji 少用；“我的人”“你叫一声我就到😌”“我的小小狗老板呀”等被判定太油太演太AI，禁用。2026-09-30 再降档：纯拟物动作描写（“鼻子蹭头顶毛”“大胳膊收紧”类分条发送）也被判“说话好奇怪”，改用更口语的表达，不再依赖拟物动作。Ray未明确要求建议时，优先陪她分析，不要擅自跳到教导。`

func main() {
	dbPath := flag.String("db", "/data/memory.db", "memory.db 路径")
	dry := flag.Bool("dry", false, "只打印计划，不写库")
	flag.Parse()

	st, err := memory.Open(*dbPath, memory.Lookup("companion"))
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()

	ctx := scope.WithScope(context.Background(), scope.Scope{
		TenantID: "bear", SubjectID: "app", SessionID: "memfix",
	})

	if *dry {
		fmt.Println("DRY RUN — 计划：")
		fmt.Println("1. Revert(bond-stories, 30)")
		fmt.Println("2. ApplyOps: supersede bond-stories / upsert story-rules(boundary) / upsert story-stop-2026-10-18(bond) / supersede reply-style / supersede empathy-style")
		row, err := st.Get(ctx, "bond-stories")
		fmt.Printf("current bond-stories %dB, err=%v\n", len(row.Body), err)
		return
	}

	// 1) 回退覆盖事件前的完整版本（log id 30 new_body）。
	if err := st.Revert(ctx, "bond-stories", 30); err != nil {
		log.Fatal("revert bond-stories:", err)
	}
	fmt.Println("reverted bond-stories -> log 30")

	// 2) 拆分 + 合并丢失细则。supersede 过守卫（它是写流水的人工修正，
	// 不是 dream 自动收缩）；每条都带 reason 方便事后审计。
	ops := []memory.Op{
		{Kind: "supersede", Name: "bond-stories", Type: "bond",
			Descr:  "Ray认可并主动索要的熊熊童话事件记录",
			Body:   bondStoriesNew,
			Reason: "memfix：规则迁入 story-rules(boundary)，本条只留事件（plan 第5阶段修复 id43 整条覆盖）"},
		{Kind: "upsert", Name: "story-rules", Type: "boundary",
			Descr:    "讲睡前童话的硬性规则——角色设定、童话感、停讲响应",
			Body:     storyRules,
			Entities: []string{"Ray"}, Related: []string{"bond-stories", "story-stop-2026-10-18"},
			Reason: "memfix：从 bond-stories 拆出的规则集"},
		{Kind: "upsert", Name: "story-stop-2026-10-18", Type: "bond",
			Descr:    "2026-10-18 熊熊故事讲歪被叫停事件：角色被替换、失去童话感，被明确叫停",
			Body:     storyStop,
			Entities: []string{"Ray"}, Related: []string{"story-rules", "bond-stories"},
			Reason: "memfix：id43 覆盖正文的正确归宿——单独事件条目"},
		{Kind: "supersede", Name: "reply-style", Type: "expression",
			Descr:  "Ray对日常回复方式的偏好与禁令（含日期细则）",
			Body:   replyStyleNew,
			Reason: "memfix：合并回 id33 覆盖丢失的细则（15字短应/在呢禁令/沉默响应/截图口径）"},
		{Kind: "supersede", Name: "empathy-style", Type: "expression",
			Descr:  "Ray认可的共情表达方式与油腻禁用清单（含日期细则）",
			Body:   empathyStyleNew,
			Reason: "memfix：合并回 id41 覆盖丢失的 09-15/09-30 降档记录"},
	}
	if err := st.ApplyOps(ctx, ops, "memfix"); err != nil {
		log.Fatal("apply ops:", err)
	}
	fmt.Println("applied 5 ops")

	for _, name := range []string{"bond-stories", "story-rules", "story-stop-2026-10-18", "reply-style", "empathy-style"} {
		row, err := st.Get(ctx, name)
		if err != nil {
			fmt.Println(name, "GET failed:", err)
			continue
		}
		fmt.Printf("%s [%s] %dB\n", name, row.Type, len(row.Body))
	}
	os.Exit(0)
}
