package boot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NaiZi899/Naibox/internal/custom"
	"github.com/NaiZi899/Naibox/internal/gateway"
	"github.com/NaiZi899/Naibox/internal/pool"
)

// customEndpointsForTest 造两条规范化后的端点（不依赖配置文件）。
func customEndpointsForTest(t *testing.T) ([]custom.Endpoint, error) {
	t.Helper()
	return custom.NormalizeAll([]custom.Spec{{
		ID: "my-openai", Name: "我的 OpenAI 反代",
		BaseURL: "https://api.example.com/v1", APIKey: "sk-test-123456",
		Models: []string{"gpt-4o-mini", "deepseek-chat"}, Shards: 2,
	}}, os.Getenv)
}

// writeTempConfig 写一份临时配置（管理页 store 的用例也用它）。
func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// 这组用例覆盖配置解析（含 disabled 跳过）与装配（合成账号入池）。
// 管理页 store 的读写在 customstore_test.go。

func TestLoadCustomUpstreams(t *testing.T) {
	cfgPath := writeTempConfig(t, `{
	  "listen": "127.0.0.1:0",
	  "custom_upstreams": [
	    {"id":"my-openai","base_url":"https://api.example.com/v1","api_key":"sk-abcdefgh"},
	    {"id":"disabled-one","enabled":false,"base_url":"ftp://bad"},
	    {"id":"env-one","base_url":"http://127.0.0.1:8000/v1","api_key_env":"NB_TEST_CUSTOM_KEY","shards":3}
	  ]
	}`)
	t.Setenv("NB_TEST_CUSTOM_KEY", "sk-from-env-1234")

	cfg, err := LoadAt(cfgPath, t.TempDir())
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if len(cfg.CustomEndpoints) != 2 {
		t.Fatalf("启用端点 = %d，期望 2（disabled 应被跳过）", len(cfg.CustomEndpoints))
	}
	byID := map[string]bool{}
	for _, ep := range cfg.CustomEndpoints {
		byID[ep.ID] = true
	}
	if !byID["my-openai"] || !byID["env-one"] || byID["disabled-one"] {
		t.Fatalf("端点集合不对: %+v", byID)
	}
}

func TestLoadCustomUpstreamsRejectsBadEnabledEntry(t *testing.T) {
	cfgPath := writeTempConfig(t, `{
	  "listen": "127.0.0.1:0",
	  "custom_upstreams": [
	    {"id":"a","base_url":"ftp://x","api_key":"k"},
	    {"id":"b","base_url":"https://x","api_key":"k"}
	  ]
	}`)
	if _, err := LoadAt(cfgPath, t.TempDir()); err == nil {
		t.Fatal("启用的非法条目应让启动失败")
	} else if !strings.Contains(err.Error(), "custom_upstreams[0]") {
		t.Errorf("报错应带条目下标: %v", err)
	}
}

// fakeProvider 只为把 registry 填满（默认上游的对账逻辑需要它）。
type fakeProvider struct{ id string }

func (f fakeProvider) ID() string               { return f.id }
func (f fakeProvider) Caps() gateway.Capability { return gateway.CapChat }
func (f fakeProvider) Chat(context.Context, gateway.Credential, []byte) (gateway.ChatStream, error) {
	return gateway.ChatStream{}, nil
}
func (f fakeProvider) Models(context.Context, gateway.Credential) ([]gateway.ModelInfo, error) {
	return nil, nil
}

func TestRegisterCustomUpstreams(t *testing.T) {
	// 直接用已规范化的 Endpoint 构造配置，避免把配置解析再测一遍。
	eps, err := customEndpointsForTest(t)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{CustomEndpoints: eps}

	reg := gateway.NewRegistry()
	if err := reg.Register(fakeProvider{id: "workbuddy"}); err != nil {
		t.Fatal(err)
	}
	p := pool.New(filepath.Join(t.TempDir(), "state.json"))
	p.SetDefaultProvider("workbuddy")

	if err := registerCustomUpstreams(cfg, reg, p, func(string, ...any) {}); err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	if _, ok := reg.Get("my-openai"); !ok {
		t.Fatal("自定义上游没有注册进 registry")
	}
	// shards=2 → 池里应有 2 个合成账号。
	if got := len(p.ListFor("my-openai")); got != 2 {
		t.Errorf("合成账号数 = %d，期望 2", got)
	}
	for _, st := range p.ListFor("my-openai") {
		if st.Nickname == "" {
			t.Errorf("账号 %s 缺少展示名", st.UID)
		}
	}
	// 静态模型目录应当能直接回答（不需要网络）。
	r := registryRouter{reg: reg, p: p}
	ms, ok := r.Models(context.Background(), "my-openai")
	if !ok || len(ms) != 2 || ms[0].ID != "gpt-4o-mini" {
		t.Fatalf("模型目录不符: ok=%v ms=%+v", ok, ms)
	}
}
