package gateway

// parseAttachments 的 PDF 分支测试。抽取本身用注入的 pdfExtract 打桩——
// poppler 的行为在 internal/pdfdoc 测，这里测的是网关策略：kind/类型校验、
// Part 形态、失败变成分片说明而不是拒绝整轮。

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"runeharness/internal/agent"
	"runeharness/internal/pdfdoc"
)

func stubExtract(t *testing.T, fn func(context.Context, []byte, string, pdfdoc.Options) (pdfdoc.Result, error)) {
	t.Helper()
	old := pdfExtract
	pdfExtract = fn
	t.Cleanup(func() { pdfExtract = old })
}

func pdfIn(t *testing.T) attachmentIn {
	t.Helper()
	return attachmentIn{Kind: agent.PartDocument, MediaType: "application/pdf",
		Filename: "report.pdf", DataB64: base64.StdEncoding.EncodeToString([]byte("%PDF-fake"))}
}

// 正常路径：document 分片带抽取文字，渲染页紧随其后成为 image/jpeg 分片。
func TestParseAttachmentsPDFExpands(t *testing.T) {
	stubExtract(t, func(_ context.Context, data []byte, name string, _ pdfdoc.Options) (pdfdoc.Result, error) {
		if string(data) != "%PDF-fake" || name != "report.pdf" {
			t.Fatalf("extractor got wrong input: %q %q", data[:8], name)
		}
		return pdfdoc.Result{
			Text:  "[PDF attachment: report.pdf — 2 pages]\n--- page 1 ---\n体检报告正文",
			Scans: []pdfdoc.Scan{{Page: 2, JPEG: []byte("\xff\xd8jpeg")}},
		}, nil
	})
	parts, err := parseAttachments(context.Background(), []attachmentIn{pdfIn(t)})
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want document+scan = 2", len(parts))
	}
	doc := parts[0]
	if doc.Kind != agent.PartDocument || doc.MediaType != "application/pdf" || doc.Filename != "report.pdf" {
		t.Fatalf("doc part malformed: %+v", doc)
	}
	if !strings.Contains(doc.Text, "体检报告正文") {
		t.Fatalf("doc part must carry extracted text, got %q", doc.Text)
	}
	scan := parts[1]
	if scan.Kind != agent.PartImage || scan.MediaType != "image/jpeg" || scan.Filename != "report.pdf" {
		t.Fatalf("scan part malformed: %+v", scan)
	}
	if got := scan.DataB64; got != base64.StdEncoding.EncodeToString([]byte("\xff\xd8jpeg")) {
		t.Fatalf("scan b64 wrong: %q", got)
	}
}

// 加密 PDF 不 400：变成一条 model 可见的分片说明。调用方取消才返回错误。
func TestParseAttachmentsPDFFailureBecomesNote(t *testing.T) {
	stubExtract(t, func(context.Context, []byte, string, pdfdoc.Options) (pdfdoc.Result, error) {
		return pdfdoc.Result{}, pdfdoc.ErrEncrypted
	})
	parts, err := parseAttachments(context.Background(), []attachmentIn{pdfIn(t)})
	if err != nil {
		t.Fatalf("readable-failure must not reject the turn: %v", err)
	}
	if len(parts) != 1 || parts[0].Kind != agent.PartDocument {
		t.Fatalf("want single document note part, got %+v", parts)
	}
	if !strings.Contains(parts[0].Text, "password-protected") {
		t.Fatalf("note missing reason: %q", parts[0].Text)
	}

	stubExtract(t, func(context.Context, []byte, string, pdfdoc.Options) (pdfdoc.Result, error) {
		return pdfdoc.Result{}, context.Canceled
	})
	if _, err := parseAttachments(context.Background(), []attachmentIn{pdfIn(t)}); !errors.Is(err, context.Canceled) {
		t.Fatalf("caller cancellation must propagate as error, got %v", err)
	}
}

// 校验顺序：document 只认 application/pdf；其它类型、其它 kind 一概拒绝。
func TestParseAttachmentsDocumentValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   attachmentIn
		want string
	}{
		{"doc wrong media type", attachmentIn{Kind: agent.PartDocument, MediaType: "application/msword", Filename: "a.doc"}, "application/pdf"},
		{"doc pdf but empty b64", attachmentIn{Kind: agent.PartDocument, MediaType: "application/pdf", Filename: "a.pdf", DataB64: "!!!"}, "invalid base64"},
		{"unknown kind", attachmentIn{Kind: "video", MediaType: "video/mp4"}, "unsupported"},
		{"image mislabeled pdf", attachmentIn{Kind: agent.PartImage, MediaType: "application/pdf"}, "image/*"},
	} {
		if _, err := parseAttachments(context.Background(), []attachmentIn{tc.in}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want error containing %q, got %v", tc.name, tc.want, err)
		}
	}
}

// 图片路径不受影响：image 走 normalizeImage，不触发抽取器。
func TestParseAttachmentsImagePathUntouched(t *testing.T) {
	called := false
	stubExtract(t, func(context.Context, []byte, string, pdfdoc.Options) (pdfdoc.Result, error) {
		called = true
		return pdfdoc.Result{}, nil
	})
	src := encodePNG(t, noiseGray(8, 8))
	parts, err := parseAttachments(context.Background(), []attachmentIn{{
		Kind: "image", MediaType: "image/png", DataB64: base64.StdEncoding.EncodeToString(src)}})
	if err != nil || called {
		t.Fatalf("image path must not reach the extractor: err=%v called=%v", err, called)
	}
	if len(parts) != 1 || parts[0].Kind != agent.PartImage {
		t.Fatalf("image part malformed: %+v", parts)
	}
}
