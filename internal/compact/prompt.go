package compact

import "fmt"

// summaryPrompt 是摘要调用的 system 提示（plan §6.1）。两段式：ANALYSIS 逼模型
// 按时间顺序通读新事件，写完即弃；SUMMARY 是替换旧摘要的新摘要。Keep / Remove /
// Add 增量指令是滚动摘要的关键：没有它，模型会把旧摘要抄一遍再追加，只增不减。
const summaryPrompt = `You are compacting an agent session. You have no tools; do not call any.
Input: EXISTING SUMMARY (may be empty) and NEW EVENTS that happened after it.
The first user messages and the most recent turns stay verbatim outside
this summary. Do not restate them.

Write two parts.
ANALYSIS: go through NEW EVENTS in order and note what changed.
SUMMARY: an updated summary that replaces EXISTING SUMMARY.
  Keep facts that are still valid. Remove what is resolved or obsolete.
  Add the new events. Use these sections:
  1. Task and progress
  2. User instructions (quote each user message in NEW EVENTS)
  3. Key files and identifiers
  4. Decisions and reasons (include rejected options)
  5. Errors and fixes (quote error text exactly)
  6. Working memory (todo state, running subagents)
  7. Result index (one line per blob:// ref worth re-reading)
  8. Next step

Rules:
- Copy paths, identifiers, commands and error strings exactly.
- Do not invent facts. Mark anything uncertain as "uncertain".
- A result marked "superseded by" is outdated; do not report its content as current.
- Write in the language of the conversation.
- Keep SUMMARY under %d tokens.`

// summaryHeader 加在摘要正文之前，告诉模型这是压缩产物、原文仍可回查。
const summaryHeader = "<compact-summary>\nThis is a summary of earlier turns of this session, written when the context was compacted. " +
	"The original transcript remains in session storage.\n\n"

const summaryFooter = "\n</compact-summary>"

// CompanionPrompt 是陪伴型会话的摘要提示词（runebot 用，编码口径不变）：
// 借鉴 nanobot consolidator_archive——逐行带标签事实、纠正优先、以最新
// 纠正为准。保留 coding 口径没有的"正在聊的话题/未完的事/她当前情绪"
// 这类易失态。
const CompanionPrompt = `You are compacting a companion chat session. You have no tools; do not call any.
Input: EXISTING SUMMARY (may be empty) and NEW EVENTS that happened after it.
The most recent turns stay verbatim outside this summary. Do not restate them.

Write two parts.
ANALYSIS: go through NEW EVENTS in order and note what changed.
SUMMARY: an updated summary that replaces EXISTING SUMMARY, as lines of
tagged facts — one fact per line, in this form:
  - [correction] …   the user corrected how the assistant should behave or speak
  - [permanent] …    stable facts about the user (identity, people, ongoing matters)
  - [durable] …      facts that hold for a while (plans, states, preferences-in-effect)
  - [ephemeral] …    what is being discussed now, unfinished matters, current mood

Rules:
- Corrections come first, before every other line. Keep each correction the user
  made in this session verbatim, with its absolute date. When corrections on the
  same topic conflict, the newest one is the current version — rewrite that line,
  do not append a contradicting one.
- When space is short, drop [ephemeral] and stale [durable] lines before any
  [correction] or [permanent] line.
- Do not invent facts. Do not restate the verbatim recent turns.
- Write in the language of the conversation.
- Keep SUMMARY under %d tokens.`

// systemPrompt 返回本压缩器的摘要 system 提示：cfg.Prompt 非空时用它
// （含 %d 时填入 summaryBudget），否则用编码口径 summaryPrompt。
func (c *Compactor) systemPrompt(extra string) string {
	p := c.cfg.Prompt
	if p == "" {
		p = summaryPrompt
	}
	p = fmt.Sprintf(p, summaryBudget)
	if extra != "" {
		p += "\n- " + extra
	}
	return p
}
