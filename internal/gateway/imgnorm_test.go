package gateway

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/jpeg"
	"image/png"
	"math/rand/v2"
	"testing"
)

// noiseRGBA 生成不可压缩的噪声图（模拟相机原图：大尺寸 + 大字节）。alpha 由调用方指定。
func noiseRGBA(w, h int, alpha byte) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	r := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i] = byte(r.IntN(256))
		img.Pix[i+1] = byte(r.IntN(256))
		img.Pix[i+2] = byte(r.IntN(256))
		img.Pix[i+3] = alpha
	}
	return img
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

// 大图：长边缩到 maxImageDim、重编码 JPEG，体积显著下降；这是真机"图片轮
// 120s 超时"的直接修复点。
func TestNormalizeImageDownscalesLarge(t *testing.T) {
	src := encodePNG(t, noiseRGBA(2400, 1800, 255))
	out, mt := normalizeImage(src, "image/png")
	if mt != "image/jpeg" {
		t.Fatalf("media type = %q, want image/jpeg", mt)
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("output is not a jpeg: %v", err)
	}
	if cfg.Width != 1600 || cfg.Height != 1200 {
		t.Fatalf("dims = %dx%d, want 1600x1200 (aspect preserved)", cfg.Width, cfg.Height)
	}
	if len(out) >= len(src)/2 {
		t.Fatalf("size %d vs source %d: expected at least 2x reduction", len(out), len(src))
	}
}

// 小图（尺寸与字节都在阈值内）原样放行：不重编码、不改媒体类型。
func TestNormalizeImagePassesSmallThrough(t *testing.T) {
	src := encodePNG(t, noiseRGBA(64, 64, 255))
	out, mt := normalizeImage(src, "image/png")
	if !bytes.Equal(out, src) || mt != "image/png" {
		t.Fatalf("small image was modified (mt=%q, same bytes=%v)", mt, bytes.Equal(out, src))
	}
}

// 解码失败（HEIC/损坏/非图）不拒绝，原样透传——网关不做格式校验。
func TestNormalizeImageUndecodablePassthrough(t *testing.T) {
	junk := bytes.Repeat([]byte{0xde, 0xad, 0xbe, 0xef}, 400*1024) // >maxImageBytes，强制走解码分支
	out, mt := normalizeImage(junk, "image/heic")
	if !bytes.Equal(out, junk) || mt != "image/heic" {
		t.Fatalf("undecodable input must pass through untouched")
	}
}

// 透明 PNG 转 JPEG 要铺白底：否则透明区变黑，截图类图片内容不可读。
// RGB 用噪声、alpha=0：PNG 不可压缩（走归一化分支且 JPEG 更小），渲染结果应是纯白。
func TestNormalizeImageFlattensTransparencyOnWhite(t *testing.T) {
	src := encodePNG(t, noiseRGBA(1700, 1700, 0))
	out, mt := normalizeImage(src, "image/png")
	if mt != "image/jpeg" {
		t.Fatalf("media type = %q, want image/jpeg (normalization branch not taken)", mt)
	}
	dec, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	r, g, b, _ := dec.At(10, 10).RGBA()
	if r>>8 < 240 || g>>8 < 240 || b>>8 < 240 {
		t.Fatalf("transparent pixel flattened to (%d,%d,%d), want near white", r>>8, g>>8, b>>8)
	}
}

// 接线：大图经 parseAttachments 后落到 Part 里的是缩小后的 JPEG，而不是入站原图；
// 校验（大小/总量）仍按入站字节，归一化只作用于通过校验之后。
func TestParseAttachmentsNormalizesLargeImage(t *testing.T) {
	src := encodePNG(t, noiseRGBA(1700, 1100, 255)) // ≈7.5MB：在 10MB 入站上限内，长边仍超归一化阈值
	parts, err := parseAttachments([]attachmentIn{{
		Kind: "image", MediaType: "image/png", DataB64: base64.StdEncoding.EncodeToString(src),
	}})
	if err != nil {
		t.Fatalf("parseAttachments: %v", err)
	}
	if len(parts) != 1 || parts[0].MediaType != "image/jpeg" {
		t.Fatalf("part media type = %+v, want single image/jpeg part", parts)
	}
	got, err := base64.StdEncoding.DecodeString(parts[0].DataB64)
	if err != nil {
		t.Fatalf("part payload is not valid base64: %v", err)
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(got))
	if err != nil || cfg.Width != 1600 {
		t.Fatalf("part payload dims = %+v err=%v, want 1600px-wide jpeg", cfg, err)
	}
}
