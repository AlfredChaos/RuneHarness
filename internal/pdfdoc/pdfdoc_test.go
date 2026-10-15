package pdfdoc

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"runeharness/internal/cmdrun"
)

// fakePoppler 按「命令名 → 响应」应答；记录调用，供断言「没多做事」。
type fakePoppler struct {
	pages int
	sizes string // 追加到 pdfinfo 输出
	texts []string
	imgs  string // pdfimages -list 输出
	fail  map[string]error
	calls []string
	// 渲染：pdftoppm 把这个内容写到 <prefix>.jpg
	jpeg []byte
}

func (f *fakePoppler) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if err := f.fail[name]; err != nil {
		return nil, err
	}
	switch name {
	case "pdfinfo":
		out := "Pages:           " + strconv.Itoa(f.pages) + "\n" + f.sizes
		return []byte(out), nil
	case "pdftotext":
		return []byte(strings.Join(f.texts, "\f") + "\f"), nil
	case "pdfimages":
		return []byte(f.imgs), nil
	case "pdftoppm":
		prefix := args[len(args)-1]
		return nil, os.WriteFile(prefix+".jpg", f.jpeg, 0o600)
	}
	return nil, errors.New("unexpected command " + name)
}

func newFake(f *fakePoppler) *Extractor {
	if f.jpeg == nil {
		f.jpeg = []byte("\xff\xd8\xff\xe0JFIF-fake")
	}
	return &Extractor{run: f.run, sem: make(chan struct{}, 2)}
}

func a4Sizes(pages int) string {
	var b strings.Builder
	for i := 1; i <= pages; i++ {
		b.WriteString("Page    " + strconv.Itoa(i) + " size:  595.304 x 841.89 pts (A4)\n")
	}
	return b.String()
}

const imgHeader = "page   num  type   width height color comp bpc  enc interp  object ID x-ppi y-ppi size ratio\n" +
	"--------------------------------------------------------------------------------------------\n"

func lorem(n int) string { return strings.Repeat("字", n) }

func (f *fakePoppler) count(name string) int {
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, name+" ") {
			n++
		}
	}
	return n
}

// 纯文字文档：只给文字，不渲染任何页——这是对「别把每份简历都变成图」的约束。
func TestTextOnlyDocumentRendersNothing(t *testing.T) {
	f := &fakePoppler{pages: 2, sizes: a4Sizes(2), texts: []string{lorem(400), lorem(300)}, imgs: imgHeader}
	res, err := newFake(f).Extract(context.Background(), []byte("%PDF"), "resume.pdf", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Scans) != 0 || f.count("pdftoppm") != 0 {
		t.Fatalf("text-only doc rendered %d pages (pdftoppm calls=%d), want 0", len(res.Scans), f.count("pdftoppm"))
	}
	if !strings.Contains(res.Text, "--- page 1 ---") || !strings.Contains(res.Text, "--- page 2 ---") {
		t.Fatalf("pages not labelled:\n%s", res.Text)
	}
}

// 扫描页（文字层近乎为空）→ 渲染；有文字的页不渲染。页码与图一一对应。
func TestScannedPagesAreRendered(t *testing.T) {
	f := &fakePoppler{pages: 3, sizes: a4Sizes(3), texts: []string{lorem(400), "", lorem(5)}, imgs: imgHeader}
	res, err := newFake(f).Extract(context.Background(), []byte("%PDF"), "scan.pdf", Options{})
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	for _, s := range res.Scans {
		got = append(got, s.Page)
	}
	if len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Fatalf("rendered pages = %v, want [2 3]", got)
	}
	if !strings.Contains(res.Text, "--- page 2 (little or no text layer; rendered image attached) ---") {
		t.Fatalf("scan page not annotated:\n%s", res.Text)
	}
}

// 回归（真实文档）：PPT 导出的 PDF 有文字层但整页是手机截图——位图覆盖率高也要渲染，
// 否则截图里的界面文字模型永远看不到。同时 2x2 的占位小图和蒙版不算内容位图。
func TestImageCoveragePagesAreRenderedDespiteTextLayer(t *testing.T) {
	imgs := imgHeader +
		// 页 1：一张 3in x 7in 的截图（A4 8.27x11.69in=96.7in² → 21/96.7≈22%）+ 一张同尺寸 → 合计 ≈43% ≥ 25%
		"   1     0 image     600  1400  rgb     3   8  jpeg   no        9  0   200   200 36.0K 3.5%\n" +
		"   1     1 image     600  1400  rgb     3   8  jpeg   no       10  0   200   200 36.0K 3.5%\n" +
		// 页 2：只有 2x2 占位图与蒙版——不应触发渲染
		"   2     2 image       2     2  index   1   1  image  no        22  0    12    10    2B   - \n" +
		"   2     3 smask     5000  5000  gray    1   8  image  no        22  0   100   100  35B 2.3%\n"
	f := &fakePoppler{pages: 2, sizes: a4Sizes(2), texts: []string{lorem(200), lorem(200)}, imgs: imgs}
	res, err := newFake(f).Extract(context.Background(), []byte("%PDF"), "ppt.pdf", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Scans) != 1 || res.Scans[0].Page != 1 {
		t.Fatalf("scans = %+v, want only page 1 (screenshot-heavy); page 2 has only placeholders/masks", res.Scans)
	}
}

// 渲染额度：超额的页不渲染，并在文字块里明说「没附上」——模型据此如实告诉用户，
// 而不是以为自己看过了。
func TestRenderQuotaIsDisclosed(t *testing.T) {
	f := &fakePoppler{pages: 5, sizes: a4Sizes(5), texts: []string{"", "", "", "", ""}, imgs: imgHeader}
	res, err := newFake(f).Extract(context.Background(), []byte("%PDF"), "big-scan.pdf", Options{MaxScanPages: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Scans) != 2 || f.count("pdftoppm") != 2 {
		t.Fatalf("rendered %d (pdftoppm=%d), want exactly the quota 2", len(res.Scans), f.count("pdftoppm"))
	}
	for _, want := range []string{"pages 3, 4, 5", "could not be attached", "page 3 (little or no text layer; image NOT attached)"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("text missing %q:\n%s", want, res.Text)
		}
	}
}

// 页数上限：只读前 MaxPages 页，并明说；pdftotext/pdfinfo 的 -l 也被限住（不让 poppler 处理 5000 页）。
func TestPageCapIsAppliedAndDisclosed(t *testing.T) {
	f := &fakePoppler{pages: 100, sizes: a4Sizes(3), texts: []string{lorem(100), lorem(100), lorem(100)}, imgs: imgHeader}
	res, err := newFake(f).Extract(context.Background(), []byte("%PDF"), "long.pdf", Options{MaxPages: 3})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "only the first 3 of 100 pages were read") {
		t.Fatalf("page cap not disclosed:\n%s", res.Text)
	}
	if strings.Contains(res.Text, "--- page 4 ---") {
		t.Fatalf("page 4 leaked past the cap")
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "pdftotext") && !strings.Contains(c, "-l 3 ") {
			t.Fatalf("pdftotext not bounded to the page cap: %s", c)
		}
	}
}

// 文字量上限：在页中间截断、丢弃其后各页，并说明截在哪一页——不能静默吞内容。
func TestTextCapTruncatesAndDisclosesWhere(t *testing.T) {
	f := &fakePoppler{pages: 3, sizes: a4Sizes(3), texts: []string{lorem(80), lorem(80), lorem(80)}, imgs: imgHeader}
	res, err := newFake(f).Extract(context.Background(), []byte("%PDF"), "t.pdf", Options{MaxTextRunes: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"text was cut off at 100 characters on page 2", "pages 3–3 are omitted", "[truncated]"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("missing %q:\n%s", want, res.Text)
		}
	}
	if strings.Contains(res.Text, "--- page 3 ---") {
		t.Errorf("page 3 should be dropped after the cap")
	}
	if n := strings.Count(res.Text, "字"); n != 100 {
		t.Errorf("kept %d characters, want exactly the cap 100", n)
	}
}

// 失败分类：加密/损坏/超时/缺工具各自可识别，且 Note 把原因写成给用户的话。
func TestFailureClassification(t *testing.T) {
	cases := []struct {
		name string
		fail error
		want error
		note string
	}{
		{"invalid", &cmdrun.CmdError{Name: "pdfinfo", Code: 1, Stderr: "Syntax Error: Couldn't read xref table"}, ErrInvalid, "damaged"},
		{"encrypted-by-password", &cmdrun.CmdError{Name: "pdfinfo", Code: 1, Stderr: "Command Line Error: Incorrect password"}, ErrEncrypted, "password-protected"},
		{"copy-restricted", &cmdrun.CmdError{Name: "pdfinfo", Code: 3}, ErrEncrypted, "password-protected"},
		{"tools-missing", ErrToolsMissing, ErrToolsMissing, "no PDF tools"},
		{"timeout", context.DeadlineExceeded, ErrTimeout, "too long"},
	}
	for _, c := range cases {
		f := &fakePoppler{pages: 1, fail: map[string]error{"pdfinfo": c.fail}}
		_, err := newFake(f).Extract(context.Background(), []byte("%PDF"), "x.pdf", Options{})
		if !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want errors.Is %v", c.name, err, c.want)
			continue
		}
		if note := Note("x.pdf", err); !strings.Contains(note, c.note) || !strings.Contains(note, "x.pdf") {
			t.Errorf("%s: note %q lacks %q", c.name, note, c.note)
		}
	}
}

// 位图覆盖率探测失败不致命：文字照给，只是少了一个判据。
func TestCoverageProbeFailureIsNotFatal(t *testing.T) {
	f := &fakePoppler{pages: 1, sizes: a4Sizes(1), texts: []string{lorem(200)}, fail: map[string]error{"pdfimages": errors.New("boom")}}
	res, err := newFake(f).Extract(context.Background(), []byte("%PDF"), "x.pdf", Options{})
	if err != nil || !strings.Contains(res.Text, "--- page 1 ---") {
		t.Fatalf("err=%v text=%q", err, res.Text)
	}
}

// 单页渲染失败不拖垮整份文档：该页记入「没附上」，其余照常。
func TestRenderFailureDegradesGracefully(t *testing.T) {
	f := &fakePoppler{pages: 2, sizes: a4Sizes(2), texts: []string{"", lorem(200)}, imgs: imgHeader,
		fail: map[string]error{"pdftoppm": errors.New("render boom")}}
	res, err := newFake(f).Extract(context.Background(), []byte("%PDF"), "x.pdf", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Scans) != 0 || !strings.Contains(res.Text, "page 1 (little or no text layer; image NOT attached)") {
		t.Fatalf("render failure not reflected: scans=%d\n%s", len(res.Scans), res.Text)
	}
}

// 并发闸：同时处理的文档数不超过 maxConcurrent——每个 poppler 进程 ~100MB，pod 只有 512Mi。
func TestConcurrencyIsBounded(t *testing.T) {
	var cur, peak atomic.Int32
	slow := func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "pdfinfo" {
			n := cur.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(40 * time.Millisecond)
			cur.Add(-1)
			return []byte("Pages: 1\n" + a4Sizes(1)), nil
		}
		if name == "pdftotext" {
			return []byte(lorem(100) + "\f"), nil
		}
		return []byte(imgHeader), nil
	}
	const limit = 2 // 测试自己定：与产品常量解耦，否则改常量时构造与断言同步漂移、测试永远绿
	e := &Extractor{run: slow, sem: make(chan struct{}, limit)}
	done := make(chan struct{})
	for range 8 {
		go func() { _, _ = e.Extract(context.Background(), []byte("%PDF"), "x.pdf", Options{}); done <- struct{}{} }()
	}
	for range 8 {
		<-done
	}
	if got := peak.Load(); got > limit {
		t.Fatalf("peak concurrent documents = %d, want <= %d: Extract is not taking the semaphore", got, limit)
	}
}

// 排队等并发闸时调用方取消：立刻返回，不占着闸、不去跑 poppler。
func TestCancelWhileQueued(t *testing.T) {
	e := &Extractor{run: (&fakePoppler{pages: 1}).run, sem: make(chan struct{}, 1)}
	e.sem <- struct{}{} // 闸被占满
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Extract(ctx, []byte("%PDF"), "x.pdf", Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestParseCoverageIgnoresMasksAndPlaceholders(t *testing.T) {
	sizes := map[int][2]float64{1: {8.27, 11.69}, 2: {8.27, 11.69}}
	list := imgHeader +
		"   1     0 image     826  1169  rgb     3   8  jpeg   no        9  0   100   100  36.0K 3.5%\n" + // 8.26x11.69in ≈ 全页
		"   1     1 smask    5000  5000  gray    1   8  image  no        9  0   100   100  35B 2.3%\n" + // 蒙版：type 不是 image，忽略
		// 页 2：type=image 但只有 2x2 像素、ppi 极低 → 显示面积被「放大」到 4in x 4in=16in²（≈17%/张）。
		// 过滤器若失效，这类占位小图会把覆盖率虚高；真实 PPT 里这种 2x2 index 图一页有十几张。
		"   2     2 image       2     2  index   1   1  image  no        22  0   0.5   0.5    2B   - \n" +
		"   2     3 image       2     2  index   1   1  image  no        23  0   0.5   0.5    2B   - \n" +
		"   2     4 image       2     2  index   1   1  image  no        24  0   0.5   0.5    2B   - \n"
	cov := parseCoverage(list, sizes)
	if c := cov[1]; c < 0.95 || c > 1 {
		t.Fatalf("page 1 coverage = %.3f, want ~1.0 from the single content bitmap only (clamped to 1)", c)
	}
	if c := cov[2]; c != 0 {
		t.Fatalf("page 2 coverage = %.3f, want 0: tiny placeholder bitmaps must not count", c)
	}
}

func TestCleanText(t *testing.T) {
	in := "行一  \r\n\r\n\r\n\r\n行二\x00\x07\xff末尾\t\n"
	got := cleanText(in)
	if got != "行一\n\n行二末尾" {
		t.Fatalf("cleanText = %q", got)
	}
}

// ---- 集成：真 poppler（本机/镜像里有才跑；CI 无 poppler 时跳过，不是静默通过）----

func needPoppler(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"pdfinfo", "pdftotext", "pdfimages", "pdftoppm"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("poppler not installed (%s missing)", bin)
		}
	}
}

// minimalPDF 手写一个单页、一行 Helvetica 文字的合法 PDF（无外部依赖）。
func minimalPDF(text string) []byte {
	var objs []string
	objs = append(objs, "<< /Type /Catalog /Pages 2 0 R >>")
	objs = append(objs, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	objs = append(objs, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>")
	stream := "BT /F1 24 Tf 72 700 Td (" + text + ") Tj ET"
	objs = append(objs, "<< /Length "+strconv.Itoa(len(stream))+" >>\nstream\n"+stream+"\nendstream")
	objs = append(objs, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	var b strings.Builder
	b.WriteString("%PDF-1.4\n")
	offs := make([]int, len(objs))
	for i, o := range objs {
		offs[i] = b.Len()
		b.WriteString(strconv.Itoa(i+1) + " 0 obj\n" + o + "\nendobj\n")
	}
	xref := b.Len()
	b.WriteString("xref\n0 " + strconv.Itoa(len(objs)+1) + "\n0000000000 65535 f \n")
	for _, o := range offs {
		b.WriteString(strings.Repeat("0", 10-len(strconv.Itoa(o))) + strconv.Itoa(o) + " 00000 n \n")
	}
	b.WriteString("trailer\n<< /Size " + strconv.Itoa(len(objs)+1) + " /Root 1 0 R >>\nstartxref\n" + strconv.Itoa(xref) + "\n%%EOF\n")
	return []byte(b.String())
}

func TestRealPopplerExtractsText(t *testing.T) {
	needPoppler(t)
	res, err := Extract(context.Background(), minimalPDF("Invoice 4711 total 99.50"), "inv.pdf", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "Invoice 4711 total 99.50") || !strings.Contains(res.Text, "1 page]") {
		t.Fatalf("real extraction wrong:\n%s", res.Text)
	}
	// 一行短文字（<30 非空白字符）→ 判为"几乎无文字层"，会附渲染图：验证真 pdftoppm 产物是 JPEG。
	if len(res.Scans) != 1 || len(res.Scans[0].JPEG) < 3 || res.Scans[0].JPEG[0] != 0xFF || res.Scans[0].JPEG[1] != 0xD8 {
		t.Fatalf("scans = %d, first bytes ok? want one real JPEG", len(res.Scans))
	}
}

func TestRealPopplerRejectsGarbage(t *testing.T) {
	needPoppler(t)
	_, err := Extract(context.Background(), []byte("%PDF-1.4\nnot really"), "bad.pdf", Options{})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestRealPopplerEncrypted(t *testing.T) {
	needPoppler(t)
	if _, err := exec.LookPath("qpdf"); err != nil {
		t.Skip("qpdf not installed; cannot build an encrypted fixture")
	}
	dir := t.TempDir()
	in, out := dir+"/in.pdf", dir+"/enc.pdf"
	if err := os.WriteFile(in, minimalPDF("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := exec.Command("qpdf", "--encrypt", "user-pw", "owner-pw", "256", "--", in, out).CombinedOutput(); err != nil {
		t.Skipf("qpdf encrypt failed: %v %s", err, b)
	}
	data, _ := os.ReadFile(out)
	_, err := Extract(context.Background(), data, "enc.pdf", Options{})
	if !errors.Is(err, ErrEncrypted) {
		t.Fatalf("err = %v, want ErrEncrypted", err)
	}
}
