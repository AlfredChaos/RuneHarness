package compact

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"runeharness/internal/agent"
	"runeharness/internal/tools"
)

// 回填层数值（plan §4.2），三者对齐 CC。
const (
	spillThreshold = 50_000  // 单条超过即 spill（字符）
	batchBudget    = 200_000 // 整批合计预算（字符）
	hardCap        = 400_000 // 保险丝：超过即截断，属于异常输出
	previewEdge    = 1_000   // spill 预览的头部、尾部各取多少字符
)

// 空结果的固定标记（plan §4.2 第 1 步）：不以空形态写回，否则部分端点报 400，
// 模型也可能误判"没有进展"而结束本轮。
const (
	EmptyOutput      = "(no output)"
	EmptyErrorOutput = "error: tool failed with no output"
)

// requestLimited 是在请求侧限额的工具：一次读多少由参数决定，不进 spill——
// 模型点名要读的内容先落盘再让它重读是纯亏；read_file 读 blob:// 的结果再被
// spill 还会形成循环。
var requestLimited = map[string]bool{"read_file": true, "list_dir": true}

// Backfill 规范化一批工具结果：空结果写固定标记，超过硬上限的截断，单条超过
// 50K 或整批合计超过 200K 的结果原文落 blob、history 只留头尾预览与指针。
// 指针形态在追加时定型，之后每轮请求看到的都是同一份（prefix 稳定）。
func (c *Compactor) Backfill(ctx context.Context, calls []agent.ToolCall, results []agent.Message) ([]agent.Message, error) {
	out := slices.Clone(results)
	spilled := make([]bool, len(out))
	spillable := func(i int) bool {
		return !spilled[i] && i < len(calls) && !requestLimited[calls[i].Name] && calls[i].Name != agent.CompactToolName
	}
	spill := func(i int) error {
		ref, err := newRef("res")
		if err != nil {
			return err
		}
		if err := c.store.AppendBlob(ctx, ref, out[i].Content); err != nil {
			return fmt.Errorf("spill %s result: %w", calls[i].Name, err)
		}
		out[i].Content = persistedPreview(ref, out[i].Content)
		spilled[i] = true
		return nil
	}

	total := 0
	for i := range out {
		switch {
		case strings.TrimSpace(out[i].Content) == "" && out[i].IsError:
			out[i].Content = EmptyErrorOutput
		case strings.TrimSpace(out[i].Content) == "":
			out[i].Content = EmptyOutput
		case len(out[i].Content) > hardCap:
			out[i].Content = cutRunes(out[i].Content, hardCap) +
				fmt.Sprintf("\n[output truncated at %d chars]", hardCap)
		}
		if spillable(i) && len(out[i].Content) > spillThreshold {
			if err := spill(i); err != nil {
				return nil, err
			}
		}
		total += len(out[i].Content)
	}
	// 整批超预算：从最大的开始逐条落，直到达标或无可落的结果。
	for total > batchBudget {
		big := -1
		for i := range out {
			if spillable(i) && (big < 0 || len(out[i].Content) > len(out[big].Content)) {
				big = i
			}
		}
		if big < 0 {
			break
		}
		before := len(out[big].Content)
		if err := spill(big); err != nil {
			return nil, err
		}
		total -= before - len(out[big].Content)
	}
	return out, nil
}

// persistedPreview 渲染 spill 后留在 history 里的形态：头尾预览 + 取回指针。
// 预览取头部与尾部——命令的报错通常在末尾。
func persistedPreview(ref, content string) string {
	head, tail := cutRunes(content, previewEdge), cutRunesFromEnd(content, previewEdge)
	return fmt.Sprintf("<persisted-output src=\"%s%s\" size=\"%s\">\n"+
		"Output too large for context; the full text is stored. "+
		"Read it with read_file path=\"%s%s\" (use offset/limit to page).\n"+
		"Preview (head):\n%s\n...\nPreview (tail):\n%s\n</persisted-output>",
		tools.BlobScheme, ref, FormatSize(len(content)), tools.BlobScheme, ref, head, tail)
}

// newRef 生成 blob 引用：tool_call id 由模型/端点生成，部分兼容端点会跨轮
// 重复，不能当唯一键。
func newRef(prefix string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("blob ref: %w", err)
	}
	return prefix + "-" + hex.EncodeToString(b[:]), nil
}

// FormatSize 把字节数渲染成 "12.3KB" 形态。
func FormatSize(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%dB", n)
	}
	return fmt.Sprintf("%.1fKB", float64(n)/1024)
}

// cutRunes 把 s 截到不超过 n 字节，不切断 UTF-8 字符。
func cutRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// cutRunesFromEnd 取 s 末尾不超过 n 字节的部分，不切断 UTF-8 字符。
func cutRunesFromEnd(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}
