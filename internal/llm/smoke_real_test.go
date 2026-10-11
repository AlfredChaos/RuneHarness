package llm

// 真实端点附件冒烟测试：验证图片 Part 经 base64 走 MiniMax 视觉链路，
// 模型返回对图片的真实描述。门控与 internal/memory/e2e_real_test.go 同约定：
// RUNE_SMOKE=1 才跑，常规 go test 跳过（真端点、手工运行）。需要在仓库根
// 有 .env（OPENAI_API_KEY/BASE_URL/MODEL）。
//
//	RUNE_SMOKE=1 go test ./internal/llm/ -run RealImageSmoke -v
import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/joho/godotenv"

	"runeharness/internal/agent"
	"runeharness/internal/config"
)

func TestRealImageSmoke(t *testing.T) {
	if os.Getenv("RUNE_SMOKE") != "1" {
		t.Skip("set RUNE_SMOKE=1 to run the real-endpoint image smoke test")
	}
	// .env 在仓库根；测试 cwd 是 internal/llm，显式按相对路径加载。
	if err := godotenv.Overload(filepath.Join("..", "..", ".env")); err != nil {
		t.Skipf("no .env at repo root: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Skipf("config: %v", err)
	}

	// 64x64 纯红 PNG，stdlib 生成，无需外部素材。
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	red := color.RGBA{R: 255, A: 255}
	for y := range 64 {
		for x := range 64 {
			img.Set(x, y, red)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	b64 := base64.StdEncoding.EncodeToString(buf.Bytes())

	client := NewClient(cfg.APIKey, cfg.BaseURL, cfg.Model)
	resp, err := client.Chat(context.Background(),
		[]agent.Message{{
			Role:    agent.RoleUser,
			Content: "Describe this image in one short sentence. Reply in English.",
			Parts: []agent.Part{{
				Kind:      agent.PartImage,
				MediaType: "image/png",
				Filename:  "smoke.png",
				DataB64:   b64,
			}},
		}},
		nil, nil)
	if err != nil {
		t.Fatalf("real endpoint chat with image failed: %v", err)
	}
	if resp.Message.Content == "" {
		t.Fatal("real endpoint returned empty content for image message")
	}
	t.Logf("model description of solid-red image:\n%s", resp.Message.Content)
}
