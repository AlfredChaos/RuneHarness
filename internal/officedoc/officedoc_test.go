package officedoc

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"runeharness/internal/cmdrun"
)

// zipFixture 在内存里打包 zip。OOXML = zip，测试夹具不必真实办公文件，
// 结构对了就行——解析器只看条目路径与 XML。
func zipFixture(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(files[n])); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

var ole2 = []byte("\xD0\xCF\x11\xE0\xA1\xB1\x1A\xE1" + strings.Repeat("\x00", 512))

func TestDetect(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
		want string
	}{
		{"a.docx", []byte("PK\x03\x04..."), "docx"},
		{"b.XLSX", []byte("PK\x03\x04..."), "xlsx"},
		{"c.pptx", []byte("PK\x03\x04..."), "pptx"},
		{"d.doc", ole2, "doc"},
		{"e.xls", ole2, "xls"},
		{"f.ppt", ole2, "ppt"},
		{"g.html", []byte("<html><body>x</body></html>"), "html"},
		{"h.htm", []byte("<p>x</p>"), "html"},
		// 后缀与内容不符：.doc 却不是 OLE2 → 拒绝（多半是错后缀或坏文件）
		{"i.doc", []byte("PK\x03\x04..."), ""},
		{"j.htm", ole2, ""}, // OLE2 却叫 .html → 拒绝
		{"k.bin", ole2, ""},
		{"l.docx", []byte("not zip at all"), "docx"}, // 后缀仍然定路由，由 zip 层报 ErrInvalid/ErrEncrypted
	} {
		if got := detect(tc.name, tc.data); got != tc.want {
			t.Errorf("%s: detect = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestDocxExtractsParagraphs(t *testing.T) {
	data := zipFixture(t, map[string]string{
		"[Content_Types].xml": `<?xml version="1.0"?><Types/>`,
		"word/document.xml": `<?xml version="1.0"?>
<w:document xmlns:w="w"><w:body>
  <w:p><w:r><w:t>第一段：购销合同</w:t></w:r></w:p>
  <w:p><w:r><w:t>第二段 金额 88,000 元</w:t><w:br/><w:t>换行后的同段</w:t></w:r></w:p>
  <w:p><w:r><w:t></w:t></w:r></w:p>
  <w:p><w:r><w:t>尾段</w:t></w:r></w:p>
</w:body></w:document>`,
	})
	text, err := Extract(context.Background(), data, "contract.docx")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"第一段：购销合同", "第二段 金额 88,000 元", "换行后的同段", "尾段", "[Document attachment: contract.docx — docx]"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if strings.Count(text, "第一段") != 1 {
		t.Errorf("paragraphs mangled:\n%s", text)
	}
}

func TestXlsxSharedAndInlineCells(t *testing.T) {
	data := zipFixture(t, map[string]string{
		"xl/sharedStrings.xml": `<?xml version="1.0"?>
<sst><si><t>北京</t></si><si><t>上海</t></si><si><r><t>深</t></r><r><t>圳</t></r></si></sst>`,
		"xl/worksheets/sheet1.xml": `<?xml version="1.0"?>
<worksheet><sheetData>
  <row><c t="s"><v>0</v></c><c><v>1024</v></c><c t="inlineStr"><is><t>备注一</t></is></c></row>
  <row><c t="s"><v>2</v></c><c><v>2048</v></c></row>
</sheetData></worksheet>`,
		"xl/worksheets/sheet2.xml": `<?xml version="1.0"?>
<worksheet><sheetData>
  <row><c t="s"><v>1</v></c></row>
</sheetData></worksheet>`,
	})
	text, err := Extract(context.Background(), data, "report.xlsx")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"北京\t1024\t备注一", "深圳\t2048", "上海", "--- sheet 1 ---", "--- sheet 2 ---"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
}

func TestPptxSlideOrder(t *testing.T) {
	data := zipFixture(t, map[string]string{
		"ppt/slides/slide1.xml":  `<p:sld xmlns:a="a"><a:p><a:t>第一页</a:t></a:p><a:p><a:t>第二行</a:t></a:p></p:sld>`,
		"ppt/slides/slide2.xml":  `<p:sld xmlns:a="a"><a:p><a:t>第二页</a:t></a:p></p:sld>`,
		"ppt/slides/slide10.xml": `<p:sld xmlns:a="a"><a:p><a:t>第十页</a:t></a:p></p:sld>`,
	})
	text, err := Extract(context.Background(), data, "deck.pptx")
	if err != nil {
		t.Fatal(err)
	}
	i1 := strings.Index(text, "第一页")
	i2 := strings.Index(text, "第二页")
	i10 := strings.Index(text, "第十页")
	if i1 < 0 || i2 < 0 || i10 < 0 || !(i1 < i2 && i2 < i10) {
		t.Fatalf("slide order wrong (numeric sort required):\n%s", text)
	}
}

// 加密 OOXML：扩展名是 .docx 但魔数是 OLE2 —— Office 加密文档的真实形态。
func TestEncryptedOOXMLIsNotInvalid(t *testing.T) {
	_, err := Extract(context.Background(), ole2, "locked.docx")
	if !errors.Is(err, ErrEncrypted) {
		t.Fatalf("err = %v, want ErrEncrypted (OLE2 + ooxml ext = encrypted Office)", err)
	}
}

func TestZipBombGuard(t *testing.T) {
	// 成员数超限：600 个条目直接拒
	files := map[string]string{}
	for i := 0; i < 600; i++ {
		files["member-"+strconv.Itoa(i)+".xml"] = "x"
	}
	if _, err := Extract(context.Background(), zipFixture(t, files), "bomb.docx"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid for >512 members", err)
	}
	// 不是 zip 却叫 .docx → ErrInvalid
	if _, err := Extract(context.Background(), []byte("random garbage"), "x.docx"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid for non-zip docx", err)
	}
}

// legacy 路由：.doc/.xls/.ppt 走对应工具；stderr 里的 password 归 ErrEncrypted。
func TestLegacyRoutingAndErrors(t *testing.T) {
	var gotTool string
	old := runCmd
	runCmd = func(_ context.Context, name string, _ ...string) ([]byte, error) {
		gotTool = name
		return []byte("DOC BODY 正文"), nil
	}
	t.Cleanup(func() { runCmd = old })

	text, err := Extract(context.Background(), ole2, "memo.doc")
	if err != nil || gotTool != "catdoc" {
		t.Fatalf("doc: text=%q err=%v tool=%q", text, err, gotTool)
	}
	if !strings.Contains(text, "DOC BODY 正文") {
		t.Fatalf("catdoc output must pass through, got %q", text)
	}
	if _, err = Extract(context.Background(), ole2, "book.xls"); err != nil || gotTool != "xls2csv" {
		t.Fatalf("xls: err=%v tool=%q", err, gotTool)
	}
	if _, err = Extract(context.Background(), ole2, "deck.ppt"); err != nil || gotTool != "catppt" {
		t.Fatalf("ppt: err=%v tool=%q", err, gotTool)
	}

	// 工具报错里的 password → ErrEncrypted（密码保护的 OLE 文件形态之一）
	runCmd = func(context.Context, string, ...string) ([]byte, error) {
		return nil, &cmdrun.CmdError{Name: "catdoc", Code: 1, Stderr: "File is password protected"}
	}
	if _, err := Extract(context.Background(), ole2, "locked.doc"); !errors.Is(err, ErrEncrypted) {
		t.Fatalf("err = %v, want ErrEncrypted", err)
	}
	runCmd = func(context.Context, string, ...string) ([]byte, error) {
		return nil, &cmdrun.CmdError{Name: "catdoc", Code: 1, Stderr: "not a Word document"}
	}
	if _, err := Extract(context.Background(), ole2, "bad.doc"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestHTMLStripsToText(t *testing.T) {
	page := []byte(`<!DOCTYPE html><html><head><title>标题页</title>
<style>body{color:red}</style></head><body>
<script>evil()</script>
<h1>季度总结</h1><p>营收 &amp; 利润增长 30%</p>
<table><tr><td>北京</td><td>1024</td></tr><tr><td>上海</td><td>2048</td></tr></table>
<svg><rect/></svg>
<div>结尾 &lt;ok&gt;</div></body></html>`)
	text, err := Extract(context.Background(), page, "report.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"季度总结", "营收 & 利润增长 30%", "北京", "1024", "结尾 <ok>"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	for _, bad := range []string{"evil()", "color:red", "<rect"} {
		if strings.Contains(text, bad) {
			t.Errorf("htmlText leaked %q:\n%s", bad, text)
		}
	}
}

// 超长文本截断并标注；无损调用者判断总长。
func TestTextCapDisclosed(t *testing.T) {
	big := strings.Repeat("很长的一段话。", 20_000) // ~14万字符 > 60k 上限
	text, err := Extract(context.Background(), []byte("<p>"+big+"</p>"), "huge.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "cut off at 60000 characters") {
		t.Fatalf("truncation not disclosed:\n%.200s", text)
	}
}
