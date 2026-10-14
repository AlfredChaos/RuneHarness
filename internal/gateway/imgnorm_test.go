package gateway

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/jpeg"
	"image/png"
	"math/rand/v2"
	"runtime"
	"testing"
)

// noiseGray 生成不可压缩的灰度噪声图：像素多、字节也多，但 PNG ≈ w*h 字节，
// 能在 10MB 入站上限内造出 >3Mpx 的「大图」。
func noiseGray(w, h int) *image.Gray {
	img := image.NewGray(image.Rect(0, 0, w, h))
	r := rand.New(rand.NewPCG(1, 2))
	for i := range img.Pix {
		img.Pix[i] = byte(r.IntN(256))
	}
	return img
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func dims(t *testing.T, b []byte) (int, int) {
	t.Helper()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("decode config: %v", err)
	}
	return cfg.Width, cfg.Height
}

// 超预算的大图：缩到恰好贴着像素预算（不过度缩小）、保持宽高比、重编码 JPEG、体积下降。
func TestNormalizeImageDownscalesToPixelBudget(t *testing.T) {
	src := encodePNG(t, noiseGray(2400, 1800)) // 4.32Mpx
	out, mt := normalizeImage(src, "image/png")
	if mt != "image/jpeg" {
		t.Fatalf("media type = %q, want image/jpeg", mt)
	}
	w, h := dims(t, out)
	if px := w * h; px > maxImagePixels || px < maxImagePixels*95/100 {
		t.Fatalf("output %dx%d = %d px, want within [95%%,100%%] of budget %d", w, h, px, maxImagePixels)
	}
	if got, want := float64(w)/float64(h), 2400.0/1800.0; got < want*0.995 || got > want*1.005 {
		t.Fatalf("aspect %.4f drifted from %.4f", got, want)
	}
	if len(out) >= len(src) {
		t.Fatalf("output %dB not smaller than source %dB", len(out), len(src))
	}
}

// 回归（用户诉求）：旧策略把长边 >1600 一律缩小，带文字的图被压糊。像素在预算内的图
// 即使长边 >1600 也不许缩尺寸——只允许在体积超限时重编码。
func TestNormalizeImageKeepsDimensionsWithinPixelBudget(t *testing.T) {
	src := encodePNG(t, noiseGray(1800, 1200)) // 2.16Mpx：长边 1800 > 旧上限 1600；2.16MB > 1MB 触发重编码
	out, mt := normalizeImage(src, "image/png")
	if mt != "image/jpeg" {
		t.Fatalf("media type = %q, want image/jpeg (re-encode branch)", mt)
	}
	if w, h := dims(t, out); w != 1800 || h != 1200 {
		t.Fatalf("dims = %dx%d, want 1800x1200 untouched (old policy shrank to 1600 long edge)", w, h)
	}
}

// 像素与体积都达标的图原样放行：不重编码、不改媒体类型、字节逐位相同。
// 1900x1400 的纯色 PNG 很小，是「大尺寸但低熵」的截图/文档的缩影。
func TestNormalizeImagePassesCompliantThrough(t *testing.T) {
	for name, img := range map[string]image.Image{
		"tiny":            noiseGray(64, 64),
		"large-low-ent":   image.NewGray(image.Rect(0, 0, 1900, 1400)),
		"exactly-1Mpx-ok": image.NewGray(image.Rect(0, 0, 1000, 1000)),
	} {
		src := encodePNG(t, img)
		out, mt := normalizeImage(src, "image/png")
		if !bytes.Equal(out, src) || mt != "image/png" {
			t.Errorf("%s: modified (mt=%q, same bytes=%v)", name, mt, bytes.Equal(out, src))
		}
	}
}

// 解码失败（HEIC/损坏/非图）不拒绝，原样透传——网关不做格式校验。
func TestNormalizeImageUndecodablePassthrough(t *testing.T) {
	junk := bytes.Repeat([]byte{0xde, 0xad, 0xbe, 0xef}, 400*1024)
	out, mt := normalizeImage(junk, "image/heic")
	if !bytes.Equal(out, junk) || mt != "image/heic" {
		t.Fatalf("undecodable input must pass through untouched")
	}
}

// 透明 PNG 转 JPEG 要铺白底：否则透明区变黑，截图类图片内容不可读。
// RGB 用噪声、alpha=0：PNG 体积大（走重编码分支），渲染结果应是纯白。
func TestNormalizeImageFlattensTransparencyOnWhite(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 1700, 1700))
	r := rand.New(rand.NewPCG(3, 4))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = byte(r.IntN(256)), byte(r.IntN(256)), byte(r.IntN(256)), 0
	}
	out, mt := normalizeImage(encodePNG(t, img), "image/png")
	if mt != "image/jpeg" {
		t.Fatalf("media type = %q, want image/jpeg (normalization branch not taken)", mt)
	}
	dec, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	r8, g8, b8, _ := dec.At(10, 10).RGBA()
	if r8>>8 < 240 || g8>>8 < 240 || b8>>8 < 240 {
		t.Fatalf("transparent pixel flattened to (%d,%d,%d), want near white", r8>>8, g8>>8, b8>>8)
	}
}

// exifAPP1 构造一个只含 IFD0.Orientation 的 APP1/Exif 段（byteOrder 为 "II" 或 "MM"）。
func exifAPP1(byteOrder string, orientation int) []byte {
	var bo binary.AppendByteOrder = binary.BigEndian
	if byteOrder == "II" {
		bo = binary.LittleEndian
	}
	tiff := []byte(byteOrder)
	tiff = bo.AppendUint16(tiff, 0x002A)
	tiff = bo.AppendUint32(tiff, 8) // IFD0 偏移
	tiff = bo.AppendUint16(tiff, 1) // 1 个条目
	tiff = bo.AppendUint16(tiff, 0x0112)
	tiff = bo.AppendUint16(tiff, 3) // SHORT
	tiff = bo.AppendUint32(tiff, 1)
	tiff = bo.AppendUint16(tiff, uint16(orientation))
	tiff = bo.AppendUint16(tiff, 0)
	tiff = bo.AppendUint32(tiff, 0) // 无下一个 IFD
	payload := append([]byte("Exif\x00\x00"), tiff...)
	seg := []byte{0xFF, 0xE1}
	seg = binary.BigEndian.AppendUint16(seg, uint16(len(payload)+2))
	return append(seg, payload...)
}

// withEXIF 在 JPEG 的 SOI 之后插入 APP1 段。orientation=0 表示不带 EXIF。
func withEXIF(jpg []byte, byteOrder string, orientation int) []byte {
	if orientation == 0 {
		return jpg
	}
	seg := exifAPP1(byteOrder, orientation)
	out := append([]byte{}, jpg[:2]...)
	out = append(out, seg...)
	return append(out, jpg[2:]...)
}

func TestHasExifOrientation(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want bool
	}{
		{"MM rotate90", append(append([]byte{0xFF, 0xD8}, exifAPP1("MM", 6)...), 0xFF, 0xD9), true},
		{"II rotate270", append(append([]byte{0xFF, 0xD8}, exifAPP1("II", 8)...), 0xFF, 0xD9), true},
		{"orientation 1 is the default, not a rotation", append(append([]byte{0xFF, 0xD8}, exifAPP1("MM", 1)...), 0xFF, 0xD9), false},
		{"no EXIF segment", []byte{0xFF, 0xD8, 0xFF, 0xD9}, false},
		{"not a jpeg", []byte("GIF89a....."), false},
		{"truncated segment", append([]byte{0xFF, 0xD8}, exifAPP1("MM", 6)[:12]...), false},
	}
	for _, c := range cases {
		if got := hasExifOrientation(c.in); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// 回归（EXIF）：手机相机直出的 JPEG 是「像素横着、EXIF 方向说竖着」。stdlib 解码不应用旋转、
// 重编码又丢 EXIF——缩放后模型看到的是躺倒的图。带非默认方向的 JPEG 必须原样透传；
// 同一张图不带方向则照常缩放（对照，证明透传来自 EXIF 守卫而不是别的条件）。
func TestNormalizeImageKeepsExifRotatedJPEGUntouched(t *testing.T) {
	var base bytes.Buffer
	if err := jpeg.Encode(&base, noiseGray(2400, 1800), &jpeg.Options{Quality: 85}); err != nil { // 4.32Mpx > 预算
		t.Fatalf("encode jpeg: %v", err)
	}

	rotated := withEXIF(base.Bytes(), "MM", 6)
	if out, mt := normalizeImage(rotated, "image/jpeg"); !bytes.Equal(out, rotated) || mt != "image/jpeg" {
		t.Fatalf("EXIF-rotated JPEG was re-encoded (same bytes=%v, mt=%q): orientation would be lost", bytes.Equal(out, rotated), mt)
	}

	plain := withEXIF(base.Bytes(), "MM", 0)
	out, _ := normalizeImage(plain, "image/jpeg")
	if w, h := dims(t, out); w*h > maxImagePixels {
		t.Fatalf("control: unrotated JPEG not downscaled (%dx%d)", w, h)
	}
}

// bombPNG 构造一个头里声明 w×h、实际只有一行数据的 PNG：几百字节，
// 但 image.Decode 会按声明尺寸分配整张画布。
func bombPNG(w, h uint32) []byte {
	chunk := func(typ string, data []byte) []byte {
		b := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
		b = append(b, typ...)
		b = append(b, data...)
		return binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(append([]byte(typ), data...)))
	}
	ihdr := binary.BigEndian.AppendUint32(nil, w)
	ihdr = binary.BigEndian.AppendUint32(ihdr, h)
	ihdr = append(ihdr, 8, 2, 0, 0, 0) // 8-bit RGB，无交错
	var idat bytes.Buffer
	zw := zlib.NewWriter(&idat)
	_, _ = zw.Write(make([]byte, 1+int(w)*3))
	_ = zw.Close()
	out := []byte("\x89PNG\r\n\x1a\n")
	out = append(out, chunk("IHDR", ihdr)...)
	out = append(out, chunk("IDAT", idat.Bytes())...)
	return append(out, chunk("IEND", nil)...)
}

// 回归（解码炸弹）：声明 20000×20000 的 PNG 在修复前让 normalizeImage 分配 ~1.6GB
// （一个请求 OOM 掉整个 pod）。超过解码像素硬上限必须不解码、原样透传。
func TestNormalizeImageRefusesToDecodeHugeDeclaredCanvas(t *testing.T) {
	bomb := bombPNG(20000, 20000)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	out, mt := normalizeImage(bomb, "image/png")
	runtime.ReadMemStats(&after)

	if !bytes.Equal(out, bomb) || mt != "image/png" {
		t.Fatalf("oversized canvas must pass through untouched")
	}
	if alloc := (after.TotalAlloc - before.TotalAlloc) >> 20; alloc > 100 {
		t.Fatalf("normalizeImage allocated %d MB for a %dB input: decode guard missing", alloc, len(bomb))
	}
}

// 接线：大图经 parseAttachments 后落到 Part 里的是缩小后的 JPEG，而不是入站原图；
// 校验（大小/总量）仍按入站字节，归一化只作用于通过校验之后。
func TestParseAttachmentsNormalizesLargeImage(t *testing.T) {
	src := encodePNG(t, noiseGray(2400, 1800)) // ≈4.3MB：在 10MB 入站上限内，像素超预算
	parts, err := parseAttachments(context.Background(), []attachmentIn{{
		Kind: "image", MediaType: "image/png", DataB64: base64.StdEncoding.EncodeToString(src),
	}})
	if err != nil {
		t.Fatalf("parseAttachments: %v", err)
	}
	if len(parts) != 1 || parts[0].MediaType != "image/jpeg" {
		t.Fatalf("part = %+v, want single image/jpeg part", parts)
	}
	got, err := base64.StdEncoding.DecodeString(parts[0].DataB64)
	if err != nil {
		t.Fatalf("part payload is not valid base64: %v", err)
	}
	if w, h := dims(t, got); w*h > maxImagePixels {
		t.Fatalf("part payload %dx%d exceeds pixel budget", w, h)
	}
}
