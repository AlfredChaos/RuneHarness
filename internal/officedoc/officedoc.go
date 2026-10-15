// Package officedoc 把 Office 文档与 HTML 抽成纯文本，随 document 分片发给模型。
//
// 两类实现路径：
//   - OOXML（.docx/.xlsx/.pptx）：本质是 zip + xml，用标准库流式解包，
//     不引入新二进制；解析只取正文相关条目（document.xml、sharedStrings、
//     各 worksheet/slide），大小与成员数都有硬上限（防 zip 炸弹）。
//   - Legacy（.doc/.xls/.ppt）与 HTML：老 OLE2 走 catdoc 家族
//     （catdoc/xls2csv/catppt，镜像里 ~200KB），HTML 用自带标签剥离器
//     （跳过 script/style 等非正文节点，实体解码，折叠空白）。
//
// 与 pdfdoc 同一套约定：失败归一为哨兵错误，由网关转成「文件打不开」的
// 分片说明，不拒绝整条消息；并发与超时护栏在 internal/cmdrun。
package officedoc

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"runeharness/internal/cmdrun"
)

var (
	ErrInvalid      = errors.New("not a readable document")
	ErrEncrypted    = errors.New("document is password-protected or restricts access")
	ErrToolsMissing = cmdrun.ErrToolsMissing // 别名：缺 catdoc 家族
	ErrTimeout      = cmdrun.ErrTimeout      // 别名：整条抽取超时
)

const (
	maxTextRunes   = 60_000 // 与 pdfdoc.MaxTextRunes 同档：单文档文字上限
	totalTimeout   = 60 * time.Second
	maxZipMembers  = 512
	maxMemberBytes = 32 << 20 // 单条解压上限
	maxRunnerJobs  = 2        // catdoc 系列并发上限（per-runner）
)

// Extract 把一份文档变成放进消息的纯文本块（带头部标注与截断说明）。
// name 决定格式路由（扩展名）；二进制本身用来做魔数复核。
// 可预期失败（损坏/加密/超时/缺工具）返回哨兵错误，调用方转 Note。
func Extract(ctx context.Context, data []byte, name string) (string, error) {
	kind := detect(name, data)
	if kind == "" {
		return "", fmt.Errorf("%w: cannot tell document type from %q", ErrInvalid, name)
	}
	var text string
	var err error
	switch {
	case kind == "html":
		text = htmlText(data)
	case kind == "docx" || kind == "xlsx" || kind == "pptx":
		// 加密 OOXML 实际是 OLE2 容器（不是 zip）：魔数复核在此拦截。
		if !bytes.HasPrefix(data, []byte("PK\x03\x04")) {
			if isOLE2(data) {
				return "", fmt.Errorf("%w: %s is an OLE2 container, likely password-protected Office file", ErrEncrypted, kind)
			}
			return "", fmt.Errorf("%w: %s is not a zip package", ErrInvalid, kind)
		}
		text, err = ooxmlText(data, kind)
	default: // doc/xls/ppt：catdoc 家族
		text, err = legacyText(ctx, data, name, kind)
	}
	if err != nil {
		return "", err
	}
	return format(name, kind, text), nil
}

// Note 把 Extract 的失败渲染成放进消息的说明文字（模型会在对话里转告用户）。
func Note(name string, err error) string {
	var why string
	switch {
	case errors.Is(err, ErrEncrypted):
		why = "the file is password-protected or access-restricted"
	case errors.Is(err, ErrInvalid):
		why = "the file is damaged or in a format the server cannot read"
	case errors.Is(err, ErrTimeout):
		why = "processing took too long (the file may be too large or complex)"
	case errors.Is(err, ErrToolsMissing):
		why = "the server has no converter installed for this file type"
	default:
		why = "an internal error occurred"
	}
	return fmt.Sprintf("[Document attachment: %s — could not be read: %s. Tell the user and ask for another copy or a screenshot of the pages.]", name, why)
}

// ---- 格式识别 ----

var extKind = map[string]string{
	"docx": "docx", "docm": "docx", "xlsx": "xlsx", "xlsm": "xlsx",
	"pptx": "pptx", "pptm": "pptx",
	"doc": "doc", "xls": "xls", "ppt": "ppt",
	"html": "html", "htm": "html", "xhtml": "html",
}

func detect(name string, data []byte) string {
	ext := ""
	if i := strings.LastIndexByte(name, '.'); i >= 0 && i < len(name)-1 {
		ext = strings.ToLower(name[i+1:])
	}
	kind := extKind[ext]
	// 魔数复核：声称是 OOXML 却不是 zip、声称是 html 却像 OLE2，按可疑处理。
	switch kind {
	case "docx", "xlsx", "pptx":
		// 非 zip 会在 Extract 里归为加密或损坏，这里不误判。
	case "doc", "xls", "ppt":
		if !isOLE2(data) {
			return "" // .doc 却不是 OLE2：多半是错后缀的新格式或坏文件
		}
	case "html":
		if isOLE2(data) || bytes.HasPrefix(data, []byte("PK\x03\x04")) {
			return ""
		}
	}
	return kind
}

func isOLE2(data []byte) bool {
	return bytes.HasPrefix(data, []byte("\xD0\xCF\x11\xE0\xA1\xB1\x1A\xE1"))
}

// ---- OOXML（zip + xml，纯标准库） ----

// zip 炸弹护栏：成员数、单条解压、全包解压都封顶；只读正文相关条目。
func ooxmlText(data []byte, kind string) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("%w: bad zip: %v", ErrInvalid, err)
	}
	if len(zr.File) > maxZipMembers {
		return "", fmt.Errorf("%w: zip has %d members (cap %d)", ErrInvalid, len(zr.File), maxZipMembers)
	}
	switch kind {
	case "docx":
		body, err := zipMemberText(zr, "word/document.xml")
		if err != nil {
			return "", err
		}
		return docxText(body), nil
	case "xlsx":
		ss, _ := zipMemberText(zr, "xl/sharedStrings.xml") // 允许没有 sharedStrings（全内联）
		sheets := zipNames(zr, `^xl/worksheets/sheet(\d+)\.xml$`)
		if len(sheets) == 0 {
			return "", fmt.Errorf("%w: no worksheets in package", ErrInvalid)
		}
		var b strings.Builder
		for i, p := range sheets {
			raw, err := zipMemberText(zr, p)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&b, "--- sheet %d ---\n%s\n", i+1, xlsxText(raw, ss))
		}
		return b.String(), nil
	default: // pptx
		slides := zipNames(zr, `^ppt/slides/slide(\d+)\.xml$`)
		if len(slides) == 0 {
			return "", fmt.Errorf("%w: no slides in package", ErrInvalid)
		}
		var b strings.Builder
		for i, p := range slides {
			raw, err := zipMemberText(zr, p)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&b, "--- slide %d ---\n%s\n", i+1, pptxText(raw))
		}
		return b.String(), nil
	}
}

// zipMemberText 顺序读一条成员，带解压上限。
func zipMemberText(zr *zip.Reader, want string) ([]byte, error) {
	for _, f := range zr.File {
		if f.Name != want {
			continue
		}
		if f.UncompressedSize64 > maxMemberBytes {
			return nil, fmt.Errorf("%w: %s expands to %d bytes (cap %d)", ErrInvalid, want, f.UncompressedSize64, maxMemberBytes)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("%w: open %s: %v", ErrInvalid, want, err)
		}
		defer rc.Close()
		return io.ReadAll(io.LimitReader(rc, maxMemberBytes+1))
	}
	return nil, fmt.Errorf("%w: missing %s", ErrInvalid, want)
}

// zipNames 按数字后缀排序返回匹配成员名（sheet10 排在 sheet9 后）。
func zipNames(zr *zip.Reader, pat string) []string {
	re := regexp.MustCompile(pat)
	type hit struct {
		name string
		n    int
	}
	var hits []hit
	for _, f := range zr.File {
		m := re.FindStringSubmatch(f.Name)
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		hits = append(hits, hit{f.Name, n})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].n < hits[j].n })
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.name
	}
	return out
}

// docxText：<w:p> 段落换行，<w:t>/<w:instrText> 是文字，<w:br>/<w:cr> 换行，
// <w:tab> 转空格。token 流式走，不建 DOM。
func docxText(data []byte) string {
	d := xml.NewDecoder(bytes.NewReader(data))
	var b strings.Builder
	inPara := false
	for {
		tok, err := d.Token()
		if err != nil {
			break // io.EOF 或语法损坏：已收集的部分照用（截断胜过全丢）
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "p":
				if inPara {
					b.WriteByte('\n')
				}
				inPara = true
			case "br", "cr":
				if inPara {
					b.WriteByte('\n')
				}
			case "tab":
				if inPara {
					b.WriteByte('\t')
				}
			}
		case xml.EndElement:
			if t.Name.Local == "p" && inPara {
				b.WriteByte('\n')
				inPara = false
			}
		case xml.CharData:
			if inPara {
				b.Write(t)
			}
		}
	}
	return b.String()
}

// xlsxText：逐行输出 `<c>` 单元格文本，tab 分隔。t="s" 查共享串，
// t="inlineStr" 取 <is> 内文，其余取 <v>。
func xlsxText(sheet []byte, shared []byte) string {
	ss := parseSharedStrings(shared)
	d := xml.NewDecoder(bytes.NewReader(sheet))
	var b strings.Builder
	var cellText strings.Builder
	inCell, inValue, inInline, isShared := false, false, false, false
	flushCell := func() {
		if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
			b.WriteByte('\t')
		}
		if isShared {
			if idx, err := strconv.Atoi(strings.TrimSpace(cellText.String())); err == nil && idx >= 0 && idx < len(ss) {
				b.WriteString(ss[idx])
			}
		} else {
			b.WriteString(cellText.String())
		}
		cellText.Reset()
		isShared = false
	}
	for {
		tok, err := d.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "c":
				inCell = true
				for _, a := range t.Attr {
					if a.Name.Local == "t" && a.Value == "s" {
						isShared = true
					}
					if a.Name.Local == "t" && a.Value == "inlineStr" {
						inInline = true
					}
				}
			case "v":
				inValue = true
			case "is":
				inInline = true
			case "row":
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "v":
				inValue = false
			case "is":
				inInline = false
			case "c":
				if inCell {
					flushCell()
					inCell = false
				}
			}
		case xml.CharData:
			if inCell && (inValue || inInline) {
				cellText.Write(t)
			}
		}
	}
	return b.String()
}

func parseSharedStrings(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	d := xml.NewDecoder(bytes.NewReader(data))
	var out []string
	var cur strings.Builder
	inSI, inT := false, false
	for {
		tok, err := d.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "si":
				inSI = true
				cur.Reset()
			case "t":
				inT = true
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "si":
				if inSI {
					out = append(out, cur.String())
					inSI = false
				}
			case "t":
				inT = false
			}
		case xml.CharData:
			if inSI && inT {
				cur.Write(t)
			}
		}
	}
	return out
}

// pptxText：每张幻灯片把 <a:t> 文本串起来，<a:p> 换行。
func pptxText(data []byte) string {
	d := xml.NewDecoder(bytes.NewReader(data))
	var b strings.Builder
	inP := false
	for {
		tok, err := d.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "p" {
				if inP {
					b.WriteByte('\n')
				}
				inP = true
			}
		case xml.EndElement:
			if t.Name.Local == "p" && inP {
				b.WriteByte('\n')
				inP = false
			}
		case xml.CharData:
			// a:t 的文字直接收：命名空间不一写死 local name 判断即可
			if inP {
				b.Write(t)
			}
		}
	}
	return b.String()
}

// ---- legacy 二进制（catdoc 家族） ----

var defaultRunner = cmdrun.New(maxRunnerJobs)

// runCmd 是 legacy 工具的执行入口，包级变量以便测试注入。
var runCmd = defaultRunner.Run

// legacyText 调 catdoc / xls2csv / catppt 抽正文。这些工具对损坏文件通常
// 非零退出或 stderr 报警，统一归入 ErrInvalid；含 password 字样归 ErrEncrypted。
func legacyText(ctx context.Context, data []byte, name, kind string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, totalTimeout)
	defer cancel()
	dir, err := os.MkdirTemp("", "officedoc-*")
	if err != nil {
		return "", fmt.Errorf("officedoc: tmpdir: %w", err)
	}
	defer os.RemoveAll(dir)
	p := filepath.Join(dir, safeName(name, kind))
	if err := os.WriteFile(p, data, 0o600); err != nil {
		return "", fmt.Errorf("officedoc: write: %w", err)
	}
	tool := map[string]string{"doc": "catdoc", "xls": "xls2csv", "ppt": "catppt"}[kind]
	out, err := runCmd(ctx, tool, "-d", "utf-8", p)
	if err != nil {
		return "", classifyErr(ctx, err)
	}
	return string(out), nil
}

func classifyErr(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, ErrToolsMissing), errors.Is(err, ErrInvalid), errors.Is(err, ErrEncrypted), errors.Is(err, ErrTimeout):
		return err
	case errors.Is(ctx.Err(), context.DeadlineExceeded), errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: %v", ErrTimeout, err)
	case errors.Is(ctx.Err(), context.Canceled):
		return ctx.Err()
	}
	var ce *cmdrun.CmdError
	if errors.As(err, &ce) {
		if strings.Contains(strings.ToLower(ce.Stderr), "password") || strings.Contains(strings.ToLower(ce.Stderr), "encrypt") {
			return fmt.Errorf("%w: %s", ErrEncrypted, strings.TrimSpace(ce.Stderr))
		}
		return fmt.Errorf("%w: %s", ErrInvalid, strings.TrimSpace(ce.Stderr))
	}
	return fmt.Errorf("officedoc: %w", err)
}

// safeName 只留扩展名语义，剥离路径与可疑字符（名字只用于落盘与展示）。
func safeName(name, kind string) string {
	base := path.Base(name)
	base = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, base)
	if base == "" || base == "." {
		base = "document." + kind
	}
	return base
}

// ---- HTML ----

// htmlText 去标签取正文：script/style/noscript/svg/template/head 整块跳过，
// 块级闭合标签换行，实体解码，空白折叠。
func htmlText(data []byte) string {
	s := string(data)
	var b strings.Builder
	skipDepth := 0 // script/style 嵌套深度
	i := 0
	for i < len(s) {
		c := s[i]
		if c != '<' {
			j := strings.IndexByte(s[i:], '<')
			if j < 0 {
				j = len(s) - i
			}
			if skipDepth == 0 {
				b.WriteString(html.UnescapeString(s[i : i+j]))
			}
			i += j
			continue
		}
		// <!-- 注释 -->
		if strings.HasPrefix(s[i:], "<!--") {
			if e := strings.Index(s[i+4:], "-->"); e >= 0 {
				i += 4 + e + 3
				continue
			}
			i = len(s)
			continue
		}
		// <!DOCTYPE / <?xml / 处理指令
		if i+1 < len(s) && (s[i+1] == '!' || s[i+1] == '?') {
			if e := strings.IndexByte(s[i:], '>'); e >= 0 {
				i += e + 1
				continue
			}
			i = len(s)
			continue
		}
		// 普通标签
		j := i + 1
		closing := j < len(s) && s[j] == '/'
		if closing {
			j++
		}
		start := j
		for j < len(s) && (s[j] == ':' || unicode.IsLetter(rune(s[j])) || unicode.IsDigit(rune(s[j])) || s[j] == '-') {
			j++
		}
		tag := strings.ToLower(s[start:j])
		// 找到 '>'（引号内的 > 也算，保守处理：大多数网页可接受）
		for j < len(s) && s[j] != '>' {
			j++
		}
		if j < len(s) {
			j++ // 吃掉 '>'
		}
		switch tag {
		case "script", "style", "noscript", "template", "svg", "iframe", "object":
			if !closing {
				skipDepth++
			} else if skipDepth > 0 {
				skipDepth--
			}
		case "br", "p", "div", "li", "tr", "table", "section", "article",
			"header", "footer", "main", "aside", "blockquote", "pre",
			"h1", "h2", "h3", "h4", "h5", "h6", "hr", "ul", "ol", "dl", "dd", "dt":
			// 块级闭合/自闭标签都换成换行
			b.WriteByte('\n')
		}
		i = j
	}
	return b.String()
}

// ---- 输出整形 ----

var reBlankRuns = regexp.MustCompile(`\n{3,}`)
var reSpaceRuns = regexp.MustCompile(`[ \t]{2,}`)

// format 输出最终文本块：头部标注 + 正文清理 + 截断说明。
func format(name, kind, text string) string {
	text = strings.ToValidUTF8(text, "")
	text = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, strings.ReplaceAll(text, "\r\n", "\n"))
	text = reSpaceRuns.ReplaceAllString(text, " ")
	text = strings.TrimSpace(reBlankRuns.ReplaceAllString(text, "\n\n"))
	truncated := false
	if utf8.RuneCountInString(text) > maxTextRunes {
		text = string([]rune(text)[:maxTextRunes])
		truncated = true
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[Document attachment: %s — %s]\n", name, kind)
	if truncated {
		fmt.Fprintf(&b, "Note: text was cut off at %d characters.\n", maxTextRunes)
	}
	if text != "" {
		b.WriteString(text + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
