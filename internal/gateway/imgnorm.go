package gateway

// 入站图片归一化：真机相机/截图原图动辄 3-8MB，base64 后 4-10MB，MiniMax
// 视觉端点处理不过来——首次调用就撞 120s requestTimeout 整轮 error（sessions.db
// 实证：763KB 的截图都要 42-75s）。网关在 Submit 之前把大图缩到视觉模型真正
// 用得上的分辨率，所有客户端（app/WS/HTTP）一并受益，不依赖客户端发版。
import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif" // 注册解码器：image.Decode 按魔数选格式
	"image/jpeg"
	_ "image/png"

	xdraw "golang.org/x/image/draw"
)

const (
	// 长边上限：1600px 足够读屏幕文字与照片内容，再大只增加上行与视觉 token。
	maxImageDim = 1600
	// 字节上限：已经不大的图（小 PNG 截图/图标）原样放行，保真。
	maxImageBytes = 800 * 1024
	// 重编码 JPEG 质量：80 对照片/截图的可读性影响可忽略，体积通常降一个数量级。
	jpegQuality = 80
)

// normalizeImage 返回适合上送模型的图片字节与媒体类型。
// 小于阈值（尺寸与字节双双达标）或解码失败（HEIC/损坏/未知格式）一律原样
// 返回——解码失败不在这里拒绝，交给下游模型自己报错，网关不充当格式校验器。
func normalizeImage(data []byte, mediaType string) ([]byte, string) {
	if len(data) <= maxImageBytes {
		cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil || (cfg.Width <= maxImageDim && cfg.Height <= maxImageDim) {
			return data, mediaType
		}
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return data, mediaType
	}

	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if long := max(w, h); long > maxImageDim {
		w, h = w*maxImageDim/long, h*maxImageDim/long
		w, h = max(w, 1), max(h, 1)
	}

	// JPEG 不支持透明：先铺白底再合成，否则透明 PNG 会变成黑底。
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	xdraw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, b, xdraw.Over, nil)

	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return data, mediaType
	}
	if out.Len() >= len(data) {
		return data, mediaType // 重编码反而更大（极小图）：不折腾，原样
	}
	return out.Bytes(), "image/jpeg"
}
