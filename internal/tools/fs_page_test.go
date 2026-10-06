package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func numberedLines(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return b.String()
}

// 超过 limit 的文件：返回本页内容 + 续读坐标；按坐标续读拿到剩余部分。
func TestReadFilePagesByLines(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "big.txt"), []byte(numberedLines(5)), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := scopedCtx(ws)
	out, err := ReadFile{}.Run(ctx, json.RawMessage(`{"path":"big.txt","limit":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "line 1\nline 2\n") || !strings.Contains(out, "showing lines 1-2 of 5; continue with offset=3") {
		t.Fatalf("page 1 = %q", out)
	}
	out, err = ReadFile{}.Run(ctx, json.RawMessage(`{"path":"big.txt","offset":4}`))
	if err != nil || out != "line 4\nline 5\n" {
		t.Fatalf("last page = %q, %v", out, err)
	}
	if _, err := (ReadFile{}).Run(ctx, json.RawMessage(`{"path":"big.txt","offset":9}`)); err == nil {
		t.Fatal("offset beyond end should error")
	}
}

// 默认上限按字符封顶：50K 字符之后给续读坐标，不再整文件塞进上下文。
func TestReadFileCharCap(t *testing.T) {
	ws := t.TempDir()
	line := strings.Repeat("x", 999) + "\n" // 1000 字符/行
	if err := os.WriteFile(filepath.Join(ws, "wide.txt"), []byte(strings.Repeat(line, 80)), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := ReadFile{}.Run(scopedCtx(ws), json.RawMessage(`{"path":"wide.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "showing lines 1-50 of 80; continue with offset=51") {
		t.Fatalf("char cap note missing: %q", out[len(out)-80:])
	}
}

type fakeBlobs map[string]string

func (f fakeBlobs) LoadBlob(_ context.Context, ref string) (string, error) {
	if s, ok := f[ref]; ok {
		return s, nil
	}
	return "", errors.New("not found")
}

// blob:// 走 Store 查询并同样分页；没有 BlobReader 时报错。
func TestReadFileBlobScheme(t *testing.T) {
	ctx := scopedCtx(t.TempDir())
	r := ReadFile{Blobs: fakeBlobs{"r1": numberedLines(3)}}
	out, err := r.Run(ctx, json.RawMessage(`{"path":"blob://r1","offset":2,"limit":1}`))
	if err != nil || !strings.HasPrefix(out, "line 2\n") || !strings.Contains(out, "offset=3") {
		t.Fatalf("blob page = %q, %v", out, err)
	}
	if _, err := r.Run(ctx, json.RawMessage(`{"path":"blob://missing"}`)); err == nil {
		t.Fatal("missing blob should error")
	}
	if _, err := (ReadFile{}).Run(ctx, json.RawMessage(`{"path":"blob://r1"}`)); err == nil {
		t.Fatal("blob:// without BlobReader should error")
	}
}
