package agent

import "testing"

// ViewHash 必须覆盖附件分片（§7a 决策）：换图、去图都要让哈希漂移，
// 否则重放核对发现不了附件差异。序列化按结构体字段声明序输出，字节稳定。
func TestViewHashCoversParts(t *testing.T) {
	base := Message{Role: RoleUser, Content: "看图"}
	withParts := base
	withParts.Parts = []Part{{Kind: PartImage, MediaType: "image/png", DataB64: "aGVsbG8="}}
	altered := base
	altered.Parts = []Part{{Kind: PartImage, MediaType: "image/jpeg", DataB64: "aGVsbG8="}}

	if ViewHash([]Message{base}) == ViewHash([]Message{withParts}) {
		t.Fatal("hash ignores parts")
	}
	if ViewHash([]Message{withParts}) == ViewHash([]Message{altered}) {
		t.Fatal("hash ignores part payload")
	}
	if ViewHash([]Message{withParts}) != ViewHash([]Message{withParts}) {
		t.Fatal("hash not deterministic for identical input")
	}
}
