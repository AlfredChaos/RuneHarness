package gateway

// PDF 附件入站：抽文字 + 给文字层不足的页附渲染图，展开成 Part 序列。
// 抽取细节与资源闸在 internal/pdfdoc；这里只管「放进消息时长什么样」与「失败怎么说」。

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"

	"runeharness/internal/agent"
	"runeharness/internal/pdfdoc"
)

// pdfExtract 是抽取入口，包级变量以便测试注入（避免网关测试依赖真 poppler）。
var pdfExtract = pdfdoc.Extract

// expandPDF 把一个 PDF 变成 [document 分片, 渲染页 image 分片...]。
//
// 可预期的失败（加密/损坏/超时/缺工具）不拒绝整条消息：返回一个只带说明文字的 document
// 分片，由模型在对话里如实告诉用户「这份文件打不开」——用户体验是熊的一句话，而不是
// App 里一个看不懂的 400。只有调用方取消（客户端已断开）才返回错误。
func expandPDF(ctx context.Context, data []byte, filename string) ([]agent.Part, error) {
	name := filename
	if name == "" {
		name = "document.pdf"
	}
	res, err := pdfExtract(ctx, data, name, pdfdoc.Options{})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		slog.Warn("pdf attachment unreadable", "file", name, "bytes", len(data), "err", err)
		return []agent.Part{{
			Kind: agent.PartDocument, MediaType: "application/pdf", Filename: name,
			Text: pdfdoc.Note(name, err),
		}}, nil
	}
	parts := make([]agent.Part, 0, 1+len(res.Scans))
	parts = append(parts, agent.Part{
		Kind: agent.PartDocument, MediaType: "application/pdf", Filename: name, Text: res.Text,
	})
	for _, s := range res.Scans {
		// 渲染图已是 ≤3Mpx 的 JPEG，不再过 normalizeImage（pdfdoc 的尺寸就是照着网关预算定的）。
		parts = append(parts, agent.Part{
			Kind: agent.PartImage, MediaType: "image/jpeg", Filename: name,
			DataB64: base64.StdEncoding.EncodeToString(s.JPEG),
		})
	}
	return parts, nil
}
