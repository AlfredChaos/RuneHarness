// Package pdfdoc 把 PDF 变成模型能吃的两种东西：文字（poppler pdftotext）与
// 页面渲染图（pdftoppm）。
//
// 为什么两种都要：只抽文字，扫描件（B 超报告、拍照合同）是零字节；只发图，
// 文字型文档白白多花 token 且小字不稳。实测的两份真实文档也说明单靠文字层
// 不够——PPT 导出的 PDF 里，标题里的「90天」「HSBC HK App」是矢量描边、没有文字层，
// 三张手机截图里的界面文字只存在于位图里。所以按页判定：
//
//   - 文字：所有页都抽（便宜），按页标注，供模型引用页码。
//   - 渲染图：只给「文字层不足」或「被大图覆盖」的页（有上限）。
//
// 所有资源都有闸：页数、文字量、渲染页数、单进程时限、总时限、并发数、输出缓冲。
// PDF 是不可信输入，poppler 是 C++ 解析器，进程隔离 + 限时是主要的防线。
package pdfdoc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"runeharness/internal/cmdrun"
)

// 失败分类。调用方据此渲染面向用户的说明（Note），而不是让整条消息失败：
// 用户发来一个加密 PDF，应该由熊在对话里告诉他"这份文件打不开"，而不是弹 HTTP 400。
var (
	ErrInvalid      = errors.New("not a readable PDF")
	ErrEncrypted    = errors.New("PDF is password-protected or restricts text access")
	ErrToolsMissing = cmdrun.ErrToolsMissing // 别名：缺 poppler 二进制
	ErrTimeout      = cmdrun.ErrTimeout      // 别名：整条抽取超时
)

// Options 是单个文档的处理上限；零值取默认。
type Options struct {
	MaxPages     int // 最多读取的页数（文字与渲染候选都只在这范围内）
	MaxScanPages int // 最多渲染成图的页数
	MaxTextRunes int // 文字总量上限（按字符计，超出截断）
}

const (
	defaultMaxPages     = 60
	defaultMaxScanPages = 8
	defaultMaxTextRunes = 60_000

	// 判定一页要不要附渲染图。
	minTextRunes      = 30   // 非空白字符少于此：视为无文字层（扫描页/海报/封面）
	coverageThreshold = 0.25 // 页面里「位图显示面积」占比达到此：截图/照片为主，文字层不全

	// 渲染：长边 2000px、JPEG q85。A4 竖页 ≈ 1414x2000 = 2.8Mpx，正好落在网关
	// 3.0Mpx 预算内（见 gateway/imgnorm.go），不会被二次压缩。
	renderLongSide = 2000
	renderQuality  = "85"

	cmdTimeout    = 20 * time.Second
	totalTimeout  = 60 * time.Second
	maxConcurrent = 2 // 同时处理的文档数：每个 poppler 进程峰值 ~100MB，pod 内存有限
)

func (o Options) withDefaults() Options {
	if o.MaxPages <= 0 {
		o.MaxPages = defaultMaxPages
	}
	if o.MaxScanPages <= 0 {
		o.MaxScanPages = defaultMaxScanPages
	}
	if o.MaxTextRunes <= 0 {
		o.MaxTextRunes = defaultMaxTextRunes
	}
	return o
}

// Scan 是一页的渲染图（JPEG）。
type Scan struct {
	Page int
	JPEG []byte
}

// Result 是一个文档的处理结果：Text 是整份文档的文字块（含页码标注与说明），
// Scans 是需要随文字一并发给模型的页面图，按页码升序。
type Result struct {
	Text  string
	Scans []Scan
}

// runFunc 执行外部命令；可注入，测试用假实现。
type runFunc = cmdrun.RunFunc

// Extractor 持有并发闸与命令执行器。
type Extractor struct {
	run runFunc
	sem chan struct{}
}

// New 返回用真实 poppler 的 Extractor。
func New() *Extractor {
	return &Extractor{run: cmdrun.Exec, sem: make(chan struct{}, maxConcurrent)}
}

var defaultExtractor = New()

// Extract 用进程级默认 Extractor 处理一个 PDF。name 只用于展示。
func Extract(ctx context.Context, pdf []byte, name string, opt Options) (Result, error) {
	return defaultExtractor.Extract(ctx, pdf, name, opt)
}

type page struct {
	n        int
	text     string
	runes    int
	coverage float64
	render   bool // 判定要渲染
	scanned  bool // 渲染成功
}

// Extract 处理一个 PDF。失败返回上面的哨兵错误之一（可 errors.Is）；成功时 Result 总是
// 有可用内容（至少文字块）。
func (e *Extractor) Extract(ctx context.Context, pdf []byte, name string, opt Options) (Result, error) {
	opt = opt.withDefaults()
	select {
	case e.sem <- struct{}{}:
		defer func() { <-e.sem }()
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, totalTimeout)
	defer cancel()

	dir, err := os.MkdirTemp("", "pdfdoc-")
	if err != nil {
		return Result{}, fmt.Errorf("pdfdoc: temp dir: %w", err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "in.pdf")
	if err := os.WriteFile(path, pdf, 0o600); err != nil {
		return Result{}, fmt.Errorf("pdfdoc: write temp: %w", err)
	}

	total, sizes, err := e.info(ctx, path, opt.MaxPages)
	if err != nil {
		return Result{}, classify(ctx, err)
	}
	read := min(total, opt.MaxPages)

	texts, err := e.text(ctx, path, read)
	if err != nil {
		return Result{}, classify(ctx, err)
	}

	pages := make([]page, read)
	for i := range pages {
		pages[i] = page{n: i + 1, text: texts[i], runes: nonSpaceRunes(texts[i])}
	}
	truncAt := applyTextCap(pages, opt.MaxTextRunes)
	pages = pages[:truncAt.keep]

	// 位图覆盖率：失败不致命（只是少一个判据）。
	if cov, err := e.coverage(ctx, path, len(pages), sizes); err == nil {
		for i := range pages {
			pages[i].coverage = cov[pages[i].n]
		}
	}
	renderQuota := opt.MaxScanPages
	for i := range pages {
		p := &pages[i]
		p.render = p.runes < minTextRunes || p.coverage >= coverageThreshold
	}
	var scans []Scan
	var skipped []int // 想渲染但没渲染成（超额或渲染失败）
	for i := range pages {
		p := &pages[i]
		if !p.render {
			continue
		}
		if renderQuota <= 0 {
			skipped = append(skipped, p.n)
			continue
		}
		jpg, err := e.renderPage(ctx, dir, path, p.n)
		if err != nil {
			if ctx.Err() != nil { // 总时限到了：停止渲染，已有的照常返回
				skipped = append(skipped, p.n)
				continue
			}
			skipped = append(skipped, p.n)
			continue
		}
		renderQuota--
		p.scanned = true
		scans = append(scans, Scan{Page: p.n, JPEG: jpg})
	}

	return Result{Text: format(name, total, read, pages, truncAt, scans, skipped), Scans: scans}, nil
}

// Note 把 Extract 的失败渲染成放进消息的说明文字（模型会在对话里转告用户）。
func Note(name string, err error) string {
	var why string
	switch {
	case errors.Is(err, ErrEncrypted):
		why = "the file is password-protected or does not allow text access"
	case errors.Is(err, ErrInvalid):
		why = "the file is damaged or is not a valid PDF"
	case errors.Is(err, ErrTimeout):
		why = "processing took too long (the file may be too large or complex)"
	case errors.Is(err, ErrToolsMissing):
		why = "the server has no PDF tools installed"
	default:
		why = "an internal error occurred"
	}
	return fmt.Sprintf("[PDF attachment: %s — could not be read: %s. Tell the user and ask for another copy or a screenshot of the pages.]", name, why)
}

// ---- 外部命令封装 ----

var (
	rePages    = regexp.MustCompile(`(?m)^Pages:\s+(\d+)`)
	rePageSize = regexp.MustCompile(`(?m)^Page\s+(\d+)\s+size:\s+([\d.]+)\s+x\s+([\d.]+)\s+pts`)
)

// info 返回总页数与前 n 页的页面尺寸（英寸）。
func (e *Extractor) info(ctx context.Context, path string, n int) (int, map[int][2]float64, error) {
	out, err := e.run(ctx, "pdfinfo", "-f", "1", "-l", strconv.Itoa(n), path)
	if err != nil {
		return 0, nil, err
	}
	m := rePages.FindSubmatch(out)
	if m == nil {
		return 0, nil, fmt.Errorf("%w: pdfinfo reported no page count", ErrInvalid)
	}
	total, _ := strconv.Atoi(string(m[1]))
	if total <= 0 {
		return 0, nil, fmt.Errorf("%w: zero pages", ErrInvalid)
	}
	sizes := map[int][2]float64{}
	for _, s := range rePageSize.FindAllSubmatch(out, -1) {
		pg, _ := strconv.Atoi(string(s[1]))
		w, _ := strconv.ParseFloat(string(s[2]), 64)
		h, _ := strconv.ParseFloat(string(s[3]), 64)
		sizes[pg] = [2]float64{w / 72, h / 72}
	}
	return total, sizes, nil
}

// text 一次抽前 n 页，按 \f 切页。默认阅读顺序（不用 -layout）：实测 PPT 三栏页
// 用 -layout 会把三栏并成同一行，默认模式则按栏顺序输出；同时少 ~40% 空白 token。
// -nodiag 丢弃斜向文字（水印/印章）。
func (e *Extractor) text(ctx context.Context, path string, n int) ([]string, error) {
	out, err := e.run(ctx, "pdftotext", "-q", "-nodiag", "-f", "1", "-l", strconv.Itoa(n), path, "-")
	if err != nil {
		return nil, err
	}
	segs := strings.Split(string(out), "\f")
	texts := make([]string, n)
	for i := range texts {
		if i < len(segs) {
			texts[i] = cleanText(segs[i])
		}
	}
	return texts, nil
}

// coverage 返回每页「位图显示面积 / 页面面积」。只算真正的内容位图：
// 排除蒙版（smask/mask/stencil）和 <64px 的占位小图（实测 PPT 里一堆 2x2 index 图）。
func (e *Extractor) coverage(ctx context.Context, path string, n int, sizes map[int][2]float64) (map[int]float64, error) {
	out, err := e.run(ctx, "pdfimages", "-list", "-f", "1", "-l", strconv.Itoa(n), path)
	if err != nil {
		return nil, err
	}
	return parseCoverage(string(out), sizes), nil
}

func parseCoverage(list string, sizes map[int][2]float64) map[int]float64 {
	area := map[int]float64{}
	for i, line := range strings.Split(list, "\n") {
		if i < 2 { // 表头两行
			continue
		}
		f := strings.Fields(line)
		// page num type width height color comp bpc enc interp object ID x-ppi y-ppi size ratio
		if len(f) < 15 || f[2] != "image" {
			continue
		}
		pg, _ := strconv.Atoi(f[0])
		w, _ := strconv.ParseFloat(f[3], 64)
		h, _ := strconv.ParseFloat(f[4], 64)
		xppi, _ := strconv.ParseFloat(f[12], 64)
		yppi, _ := strconv.ParseFloat(f[13], 64)
		if w < 64 || h < 64 || xppi <= 0 || yppi <= 0 {
			continue
		}
		area[pg] += (w / xppi) * (h / yppi)
	}
	cov := map[int]float64{}
	for pg, a := range area {
		sz, ok := sizes[pg]
		if !ok || sz[0]*sz[1] <= 0 {
			continue
		}
		cov[pg] = min(a/(sz[0]*sz[1]), 1) // 图片可重叠，面积和可超过 100%
	}
	return cov
}

func (e *Extractor) renderPage(ctx context.Context, dir, path string, n int) ([]byte, error) {
	prefix := filepath.Join(dir, "p"+strconv.Itoa(n))
	ns := strconv.Itoa(n)
	if _, err := e.run(ctx, "pdftoppm", "-jpeg", "-jpegopt", "quality="+renderQuality,
		"-scale-to", strconv.Itoa(renderLongSide), "-f", ns, "-l", ns, "-singlefile", path, prefix); err != nil {
		return nil, err
	}
	return os.ReadFile(prefix + ".jpg")
}

// ---- 文字处理与格式化 ----

var reBlankRuns = regexp.MustCompile(`\n{3,}`)

// cleanText 规整一页文字：统一换行、去行尾空白、去控制字符与非法 UTF-8、折叠连续空行。
func cleanText(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, strings.ReplaceAll(s, "\r\n", "\n"))
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRightFunc(l, unicode.IsSpace)
	}
	return strings.TrimSpace(reBlankRuns.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

func nonSpaceRunes(s string) int {
	n := 0
	for _, r := range s {
		if !unicode.IsSpace(r) {
			n++
		}
	}
	return n
}

type truncation struct {
	keep      int // 保留的页数（含被截断的那一页）
	cutPage   int // 被截断的页码；0 = 未截断
	total     int // 超限时的字符上限，用于说明
	omittedTo int // 被整页丢弃的最后页码（>= cutPage+1 时有意义）
}

// applyTextCap 按页累计字符，超过上限时截断当页并丢弃其后各页。原地修改 pages[i].text。
func applyTextCap(pages []page, limit int) truncation {
	used := 0
	for i := range pages {
		r := utf8.RuneCountInString(pages[i].text)
		if used+r <= limit {
			used += r
			continue
		}
		remain := limit - used
		pages[i].text = string([]rune(pages[i].text)[:max(remain, 0)]) + "\n…[truncated]"
		pages[i].runes = nonSpaceRunes(pages[i].text)
		return truncation{keep: i + 1, cutPage: pages[i].n, total: limit, omittedTo: pages[len(pages)-1].n}
	}
	return truncation{keep: len(pages)}
}

func format(name string, total, read int, pages []page, tr truncation, scans []Scan, skipped []int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[PDF attachment: %s — %d page%s]\n", name, total, plural(total))
	if read < total {
		fmt.Fprintf(&b, "Note: only the first %d of %d pages were read.\n", read, total)
	}
	if tr.cutPage > 0 {
		fmt.Fprintf(&b, "Note: text was cut off at %d characters on page %d", tr.total, tr.cutPage)
		if tr.omittedTo > tr.cutPage {
			fmt.Fprintf(&b, "; pages %d–%d are omitted", tr.cutPage+1, tr.omittedTo)
		}
		b.WriteString(".\n")
	}
	if len(scans) > 0 {
		nums := make([]int, len(scans))
		for i, s := range scans {
			nums[i] = s.Page
		}
		fmt.Fprintf(&b, "Note: page%s %s %s also attached as rendered image%s after this text, in page order.\n",
			plural(len(nums)), joinInts(nums), isAre(len(nums)), plural(len(nums)))
	}
	if len(skipped) > 0 {
		fmt.Fprintf(&b, "Note: page%s %s contain%s images or scanned content that could not be attached (render limit reached or render failed).\n",
			plural(len(skipped)), joinInts(skipped), map[bool]string{true: "", false: "s"}[len(skipped) != 1])
	}
	for _, p := range pages {
		b.WriteString("\n--- page " + strconv.Itoa(p.n))
		switch {
		case p.scanned && p.runes < minTextRunes:
			b.WriteString(" (little or no text layer; rendered image attached)")
		case p.scanned:
			b.WriteString(" (rendered image attached)")
		case p.render && p.runes < minTextRunes:
			b.WriteString(" (little or no text layer; image NOT attached)")
		}
		b.WriteString(" ---\n")
		if p.text != "" {
			b.WriteString(p.text + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

func joinInts(ns []int) string {
	s := make([]string, len(ns))
	for i, n := range ns {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, ", ")
}

// ---- 命令执行：共享实现见 internal/cmdrun ----

// classify 把命令错误映射成哨兵错误。poppler 退出码：1 打开失败、2 输出失败、
// 3 权限限制（拷贝被禁）、99 其他；加密且需口令时 stderr 含 "Incorrect password"。
func classify(ctx context.Context, err error) error {
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
		if ce.Code == 3 || strings.Contains(ce.Stderr, "Incorrect password") {
			return fmt.Errorf("%w: %s", ErrEncrypted, strings.TrimSpace(ce.Stderr))
		}
		return fmt.Errorf("%w: %s", ErrInvalid, strings.TrimSpace(ce.Stderr))
	}
	return fmt.Errorf("pdfdoc: %w", err)
}
