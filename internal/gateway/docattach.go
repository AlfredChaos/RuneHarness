package gateway

// 文档附件入站：PDF 走 pdfdoc（抽文字 + 扫描页渲染图），Office/HTML 走
// officedoc（抽纯文本）。一个入站附件可能展开成多个 Part。校验用入站字节
// （DoS 防线），落库/上送用处理后的结果；文档二进制本身不入库，库里只留
// 抽出的文字（PDF 外加渲染图）。

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"runeharness/internal/agent"
	"runeharness/internal/officedoc"
	"runeharness/internal/pdfdoc"
)

// pdfExtract / officeExtract 是抽取入口，包级变量以便测试注入。
var (
	pdfExtract    = pdfdoc.Extract
	officeExtract = officedoc.Extract
)

// docMediaOK 是 kind=document 的 media_type 白名单。客户端 MIME 探测常给
// application/octet-stream 或干脆缺失，真正的类型判定由 officedoc.detect
// 按文件名扩展名 + 魔数复核做；这里只挡明确不是文档的类型（如 video/*）。
func docMediaOK(mt string) bool {
	switch mt {
	case "application/pdf", "application/octet-stream", "text/html",
		"application/msword", "application/vnd.ms-excel", "application/vnd.ms-powerpoint",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation":
		return true
	}
	return false
}

// expandDocument 把一个文档附件变成 [document 分片, 渲染页 image 分片…]（PDF 才有后者）。
//
// 可预期的失败（加密/损坏/超时/缺工具）不拒绝整条消息：返回一个只带说明文字的
// document 分片，由模型在对话里如实告诉用户「这份文件打不开」。只有调用方取消
// （客户端已断开）才返回错误。
func expandDocument(ctx context.Context, data []byte, filename string) ([]agent.Part, error) {
	name := filename
	if name == "" {
		name = "document"
	}
	if isPDFFile(name) {
		return expandPDF(ctx, data, name)
	}
	text, err := officeExtract(ctx, data, name)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		slog.Warn("document attachment unreadable", "file", name, "bytes", len(data), "err", err)
		return []agent.Part{{
			Kind: agent.PartDocument, MediaType: "application/octet-stream", Filename: name,
			Text: officedoc.Note(name, err),
		}}, nil
	}
	return []agent.Part{{
		Kind: agent.PartDocument, MediaType: "application/octet-stream", Filename: name, Text: text,
	}}, nil
}

// expandPDF 把 PDF 变成 [document 分片（抽出的文字）, 渲染页 image 分片…]。
func expandPDF(ctx context.Context, data []byte, name string) ([]agent.Part, error) {
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
			Kind: agent.PartImage, MediaType: "image/jpeg", Filename: fmt.Sprintf("%s p.%d", name, s.Page),
			DataB64: base64.StdEncoding.EncodeToString(s.JPEG),
		})
	}
	return parts, nil
}

func isPDFFile(name string) bool {
	return strings.HasSuffix(strings.ToLower(name), ".pdf")
}
