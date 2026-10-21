package custom

import (
	"testing"
)

// 多 key 的账号生成：每把 key × shards，uid 可区分、secret 带各自 key。
func TestLoadCredentialsMultipleKeys(t *testing.T) {
	s := baseSpec()
	s.APIKey = ""
	s.APIKeys = []string{"sk-key-one-1111", "sk-key-two-2222"}
	s.Shards = 2
	ep := mustEndpoint(t, s)
	p := New(ep, nil)

	creds, err := p.LoadCredentialsWithSecrets("")
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 4 {
		t.Fatalf("账号数 = %d，期望 2 key × 2 shards = 4", len(creds))
	}
	seen := map[string]bool{}
	keys := map[string]bool{}
	for _, c := range creds {
		if seen[c.UID] {
			t.Errorf("uid 重复: %s", c.UID)
		}
		seen[c.UID] = true
		secret, ok := c.Secret.(*Endpoint)
		if !ok || secret == nil {
			t.Fatalf("%s 的 secret 类型不对: %T", c.UID, c.Secret)
		}
		if len(secret.Keys) != 0 {
			t.Errorf("%s 的 secret 不应带完整 key 列表", c.UID)
		}
		if secret.APIKey == "" {
			t.Errorf("%s 的 secret 没有 key", c.UID)
		}
		keys[secret.APIKey] = true
	}
	if !keys["sk-key-one-1111"] || !keys["sk-key-two-2222"] {
		t.Errorf("secret 没有覆盖两把 key: %v", keys)
	}

	proj, _ := p.LoadCredentials("")
	if len(proj) != 4 || proj[0].UID != "my-openai-k1s1" {
		t.Errorf("投影不符: %+v", proj)
	}
}

func TestUIDsKeepLegacyShapeForSingleKey(t *testing.T) {
	ep := mustEndpoint(t, baseSpec())
	p := New(ep, nil)
	if got := p.UIDs(); len(got) != 1 || got[0] != "my-openai-s1" {
		t.Errorf("单 key uid 应保持旧形状: %v", got)
	}
}
