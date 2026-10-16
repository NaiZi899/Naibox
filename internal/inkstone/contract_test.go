// contract_test.go 书生·端砚的契约测试接入。
//
// 一行接入 RunProviderContract：它检查行为契约（ID 合法且稳定、Caps 必含 CapChat、
// 不 panic、声明的能力真可用、Chat 流可 Close、ctx 取消被尊重）。
// 详情见 internal/gateway/contract.go。
package inkstone

import (
	"context"
	"os"
	"testing"

	"github.com/NaiZi899/omni2api/internal/gateway"
)

// TestContract 跑 gateway 契约。
//
// 用伪造凭证跑：真人 key 未注入时，Chat 会因凭证无效返回错误 —— 契约把
// "凭证不对"与"没实现"区分不了，但它仍校验"不 panic / 流可 Close / ctx 取消"。
func TestContract(t *testing.T) {
	gateway.RunProviderContract(t, NewProvider)
}

// TestContractAgainstRealKey 有真实 API Key（环境变量 INKSTONE_TEST_KEY）时，
// 对真实上游做一次完整行为验证（Models 能拉、Chat 能通）。
func TestContractAgainstRealKey(t *testing.T) {
	key := os.Getenv("INKSTONE_TEST_KEY")
	if key == "" {
		t.Skip("INKSTONE_TEST_KEY 未设置，跳过对真实上游的契约验证")
	}
	cred := gateway.Credential{
		Provider: ProviderID,
		UID:      "test-uid",
		Nickname: "contract-real",
		Secret: &Auth{
			APIKey: key,
			UID:    "test-uid",
		},
	}
	p := New(Config{})
	ctx := context.Background()
	models, err := p.Models(ctx, cred)
	if err != nil {
		t.Fatalf("Models() 错误: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("Models() 返回空目录")
	}
	body := []byte(`{"model":"deepseek-v4-flash-0731","messages":[{"role":"user","content":"reply exactly OK"}]}`)
	cs, err := p.Chat(ctx, cred, body)
	if err != nil {
		t.Fatalf("Chat() 错误: %v", err)
	}
	if cs.Body != nil {
		_ = cs.Body.Close()
	}
}

// TestCredentialDeriveUID deterministic：同一 key 派生相同 uid；不同 key 不同。
func TestCredentialDeriveUID(t *testing.T) {
	k1 := "sk-a1b2c3d4e5f6a7b8"
	a1 := NewAuth(k1, "", "")
	a2 := NewAuth(k1, "", "")
	if a1.UID == "" || a1.UID != a2.UID {
		t.Fatalf("同一 key 派生 uid 不稳定: %q vs %q", a1.UID, a2.UID)
	}
	if a1.UID == deriveUID("sk-ddff000000000000") {
		t.Fatalf("不同 key 派生相同 uid: %q", a1.UID)
	}
}

// TestParseAvailableCredits 解析墨点字符串。
func TestParseAvailableCredits(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"10.000000", 10, true},
		{"8.5", 8, true},
		{"0.000000", 0, true},
		{"3", 3, true},
		{"", 0, false},
		{"abc", 0, false},
	}
	for _, c := range cases {
		got, err := ParseAvailableCredits(c.in)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("ParseAvailableCredits(%q) = %d,%v want %d,%v", c.in, got, err, c.want, c.ok)
		}
	}
}
