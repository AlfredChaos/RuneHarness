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

func systemPrompt(extra string) string {
	p := fmt.Sprintf(summaryPrompt, summaryBudget)
	if extra != "" {
		p += "\n- " + extra
	}
	return p
}
