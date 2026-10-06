package memory

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// maxBodyClip 是单条常驻正文注入的字节上限。
const maxBodyClip = 4096

// indexMarker 是索引块的起始标签；回放与提取输入都靠它辨认系统注入。
const indexMarker = "<memory-index"

// renderInject 渲染注入块：索引（按画像类型分组、updated_at 倒序、
// 配额截断）+ AlwaysInject 类型的完整正文。空间为空时返回空串——
// 不注入任何块。
func (m *Memory) renderInject(ctx context.Context) (string, error) {
	heads, err := m.store.List(ctx)
	if err != nil || len(heads) == 0 {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<memory-index updated=%q>\n", time.Now().Format(time.RFC3339))
	// 画像序在前，辞典外类型兜底排尾。
	ordered := append([]string{}, m.prof.Order...)
	for _, h := range heads {
		if !slices.Contains(ordered, h.Type) {
			ordered = append(ordered, h.Type)
		}
	}
	truncated := false
	for _, typ := range ordered {
		for _, h := range heads {
			if h.Type != typ {
				continue
			}
			line := fmt.Sprintf("- [%s] %s — %s\n", h.Type, h.Name, h.Descr)
			if b.Len()+len(line) > m.prof.Inject.MaxIndexBytes {
				truncated = true
				break
			}
			b.WriteString(line)
		}
		if truncated {
			break
		}
	}
	if truncated {
		b.WriteString("- …[index truncated, use memory list/read for full view]\n")
	}
	b.WriteString("</memory-index>")

	// 常驻正文：AlwaysInject 类型整块带上，单条 4KB、总量封顶。
	var pinned []string
	for _, typ := range m.prof.Order {
		if td := m.prof.Types[typ]; td.AlwaysInject {
			pinned = append(pinned, typ)
		}
	}
	if len(pinned) > 0 {
		rows, err := m.store.BodiesOf(ctx, pinned)
		if err != nil {
			return "", err
		}
		total := 0
		for _, r := range rows {
			body := r.Body
			if len(body) > maxBodyClip {
				body = clip(body, maxBodyClip)
			}
			if total+len(body) > m.prof.Inject.MaxBodyBytes {
				break
			}
			fmt.Fprintf(&b, "\n<memory name=%q type=%q>\n%s\n</memory>", r.Name, r.Type, body)
			total += len(body)
		}
	}
	return b.String(), nil
}
