package gateway

// 入站图片归一化。目的只有一个：别把相机原图（~12MP、4-8MB）原样推给视觉模型。
//
// 预算按「像素总数」而不是「长边」定。MiniMax-M3 服务端自己有预算：图片 token 在
// ~3.8k 封顶（default detail），对 A4 竖页大约是长边 2000px 处——超出的像素进了
// 模型也会被它自己缩掉，只是白白多传、多解码。实测（api.minimaxi.com，A4 小字页）：
//
//	长边 1600px（旧策略） 2.5k token  小字识别 22/40
//	3.0 Mpx（本策略）     3.9k token  小字识别 33-38/40，与模型封顶一致
//
// 所以「长边 1600」对文字类图（截图/文档/白板）是过度压缩；而照片内容对分辨率不敏感，
// 3Mpx 也绰绰有余。预算贴着模型的 token 封顶走：再大没有收益，再小就是自己伤字。
import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif" // 注册解码器：image.Decode 按魔数选格式
	"image/jpeg"
	_ "image/png"
	"math"

	xdraw "golang.org/x/image/draw"
)

const (
	// 像素预算：超过才缩，缩到恰好 ≤ 该值（保持宽高比）。
	maxImagePixels = 3_000_000
	// 字节上限：像素已达标且体积不大的图原样放行，保真。
	maxImageBytes = 1 << 20
	// 解码前的像素硬上限：DecodeConfig 只读头，声明 20000x20000 的 138 字节 PNG 会让
	// image.Decode 分配 1.6GB（实测）。超过则不解码、原样透传，由下游模型端拒绝。
	// 注意：这是内存护栏，不是质量策略——相机原图 ≤ ~50MP，远低于它。
	maxDecodePixels = 64_000_000
	// 重编码 JPEG 质量。实测 q90 比 q80 小字识别更稳（35-38 vs 33-35 /40），体积 +25%。
	jpegQuality = 90
)

// normalizeImage 返回适合上送模型的图片字节与媒体类型。
//
// 原样透传（不动字节）的情形：像素与体积都达标；解码失败（HEIC/损坏/未知格式——
// 网关不充当格式校验器）；声明像素超过 maxDecodePixels；带 EXIF 方向的 JPEG
// 重编码后体积反而不降（见下）。
func normalizeImage(data []byte, mediaType string) ([]byte, string) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return data, mediaType
	}
	px := cfg.Width * cfg.Height
	if px > maxDecodePixels || px <= 0 {
		return data, mediaType
	}
	if px <= maxImagePixels && len(data) <= maxImageBytes {
		return data, mediaType
	}
	// EXIF 方向：stdlib 解码不应用旋转、重编码又会丢掉 EXIF——对「像素横着、EXIF 说竖着」
	// 的相机照片，缩放后方向标记没了，模型看到的是躺倒的图。手机相机直出多为此形态。
	// 只有 JPEG 可能带它；有方向标记就不重编码，原样透传（模型端按 EXIF 处理）。
	// 字节上限收紧到 8MB：200MB 相机原图带 EXIF 也得重编码，否则上游模型吃不下。
	if format == "jpeg" && hasExifOrientation(data) && len(data) <= 8<<20 {
		return data, mediaType
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return data, mediaType
	}

	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w*h > maxImagePixels {
		s := math.Sqrt(float64(maxImagePixels) / float64(w*h))
		w, h = max(int(float64(w)*s), 1), max(int(float64(h)*s), 1)
	}

	// JPEG 不支持透明：先铺白底再合成，否则透明 PNG 会变成黑底。
	// CatmullRom：文字边缘比 ApproxBiLinear 更锐（实测小字识别略优），且不会像
	// Lanczos 那样在高对比边缘出振铃。
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, b, xdraw.Over, nil)

	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return data, mediaType
	}
	if out.Len() >= len(data) {
		return data, mediaType // 重编码反而更大（极小图）：不折腾，原样
	}
	return out.Bytes(), "image/jpeg"
}

// hasExifOrientation 报告 JPEG 是否带「非默认」EXIF 方向（Orientation ≠ 1）。
// 只解析 APP1/Exif 段里的 IFD0 Orientation(0x0112)；任何结构异常一律按「无」处理——
// 误判成「无」的后果是照旧缩放，不比不判更糟。
func hasExifOrientation(d []byte) bool {
	if len(d) < 4 || d[0] != 0xFF || d[1] != 0xD8 {
		return false
	}
	for i := 2; i+4 <= len(d); {
		if d[i] != 0xFF {
			return false
		}
		marker, segLen := d[i+1], int(d[i+2])<<8|int(d[i+3])
		if marker == 0xDA || marker == 0xD9 { // SOS/EOI：元数据段已过
			return false
		}
		end := i + 2 + segLen
		if segLen < 2 || end > len(d) {
			return false
		}
		if marker == 0xE1 && segLen >= 16 && string(d[i+4:i+10]) == "Exif\x00\x00" {
			return exifOrientation(d[i+10:end]) > 1
		}
		i = end
	}
	return false
}

// exifOrientation 读 TIFF 头 + IFD0 里 tag 0x0112 的值；失败返回 0。
func exifOrientation(t []byte) int {
	if len(t) < 8 {
		return 0
	}
	var u16 func([]byte) int
	var u32 func([]byte) int
	switch string(t[:2]) {
	case "II":
		u16 = func(b []byte) int { return int(b[0]) | int(b[1])<<8 }
		u32 = func(b []byte) int { return int(b[0]) | int(b[1])<<8 | int(b[2])<<16 | int(b[3])<<24 }
	case "MM":
		u16 = func(b []byte) int { return int(b[0])<<8 | int(b[1]) }
		u32 = func(b []byte) int { return int(b[0])<<24 | int(b[1])<<16 | int(b[2])<<8 | int(b[3]) }
	default:
		return 0
	}
	ifd := u32(t[4:8])
	if ifd < 8 || ifd+2 > len(t) {
		return 0
	}
	n := u16(t[ifd : ifd+2])
	for k := range n {
		e := ifd + 2 + k*12
		if e+12 > len(t) {
			return 0
		}
		if u16(t[e:e+2]) == 0x0112 {
			return u16(t[e+8 : e+10])
		}
	}
	return 0
}
