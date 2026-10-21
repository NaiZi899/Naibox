package boot

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// 管理页 store 的判据：只动 custom_upstreams 一个键、坏输入不落盘、
// 编辑时按 id 继承密钥（列表不回显明文）。

func TestCustomStoreReplacePreservesOtherKeys(t *testing.T) {
	path := writeTempConfig(t, `{"listen":"127.0.0.1:0","api_key":"keep-me","custom_upstreams":[]}`)
	cfg := &Config{}
	store := newCustomStore(path, cfg)

	items := json.RawMessage(`[{"id":"my-openai","base_url":"https://api.example.com/v1","api_key":"sk-12345678","models":["m1"]}]`)
	need, err := store.ReplaceCustomUpstreams(items)
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if len(need) != 1 || need[0] != "custom_upstreams" {
		t.Errorf("need_restart 不符: %v", need)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if string(doc["api_key"]) != `"keep-me"` {
		t.Errorf("其它顶层键被改动: %s", doc["api_key"])
	}
	if !strings.Contains(string(doc["custom_upstreams"]), `"my-openai"`) {
		t.Errorf("custom_upstreams 未写入: %s", doc["custom_upstreams"])
	}

	list, err := store.ListCustomUpstreams()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].KeyTail != "…5678" || list[0].ModelsCount != 1 {
		t.Errorf("列表投影不符: %+v", list)
	}
}

func TestCustomStoreInheritsKeyWhenEditing(t *testing.T) {
	path := writeTempConfig(t, `{"listen":"127.0.0.1:0","custom_upstreams":[
	  {"id":"my-openai","name":"旧名字","base_url":"https://api.example.com/v1","api_key":"sk-old-secret-1234","models":["m1"]}
	]}`)
	cfg := &Config{}
	if err := json.Unmarshal([]byte(`[{"id":"my-openai","name":"旧名字","base_url":"https://api.example.com/v1","api_key":"sk-old-secret-1234","models":["m1"]}]`), &cfg.CustomUpstreams); err != nil {
		t.Fatal(err)
	}
	store := newCustomStore(path, cfg)

	// 列表编辑不会回显明文 key：提交里 api_key 为空是正常形态。
	items := json.RawMessage(`[{"id":"my-openai","name":"新名字","base_url":"https://api.example.com/v1","api_key":"","models":["m1"]}]`)
	if _, err := store.ReplaceCustomUpstreams(items); err != nil {
		t.Fatalf("编辑（不带 key）应被接受: %v", err)
	}
	if cfg.CustomUpstreams[0].APIKey != "sk-old-secret-1234" {
		t.Errorf("内存配置没有继承密钥: %q", cfg.CustomUpstreams[0].APIKey)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "sk-old-secret-1234") {
		t.Errorf("落盘时丢了继承来的密钥: %s", raw)
	}
	if !strings.Contains(string(raw), "新名字") {
		t.Errorf("改名没有落盘: %s", raw)
	}
}

func TestCustomStoreRejectsInvalidWithoutWriting(t *testing.T) {
	original := `{"listen":"127.0.0.1:0","custom_upstreams":[]}`
	path := writeTempConfig(t, original)
	store := newCustomStore(path, &Config{})

	_, err := store.ReplaceCustomUpstreams(json.RawMessage(`[{"id":"bad id","base_url":"https://x","api_key":"k"}]`))
	if err == nil {
		t.Fatal("非法 id 应被拒")
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != original {
		t.Errorf("校验失败时不应写盘，文件已变：%s", raw)
	}
}

func TestCustomStoreClearKeyOnDisabledEntry(t *testing.T) {
	path := writeTempConfig(t, `{"listen":"127.0.0.1:0","custom_upstreams":[
	  {"id":"a","enabled":false,"base_url":"https://x","api_key":"sk-old-secret"}
	]}`)
	cfg := &Config{}
	if err := json.Unmarshal([]byte(`[{"id":"a","enabled":false,"base_url":"https://x","api_key":"sk-old-secret"}]`), &cfg.CustomUpstreams); err != nil {
		t.Fatal(err)
	}
	store := newCustomStore(path, cfg)

	// 列表不回显密钥；要"清空"必须显式带上 clear_api_key（否则会被当成沿用）。
	items := json.RawMessage(`[{"id":"a","enabled":false,"base_url":"https://x","api_key":"","clear_api_key":true}]`)
	if _, err := store.ReplaceCustomUpstreams(items); err != nil {
		t.Fatalf("清空密钥应被接受: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "sk-old-secret") {
		t.Errorf("清空后仍把旧密钥写回: %s", raw)
	}
	if strings.Contains(string(raw), "clear_api_key") {
		t.Errorf("语义位不应落盘: %s", raw)
	}
}

func TestCustomStoreListMasksKeyFromEnv(t *testing.T) {
	path := writeTempConfig(t, `{"listen":"127.0.0.1:0","custom_upstreams":[
	  {"id":"env-one","base_url":"https://x","api_key_env":"NB_TEST_MASK","enabled":false}
	]}`)
	t.Setenv("NB_TEST_MASK", "sk-very-secret-abcd")
	cfg := &Config{}
	// 直接塞进 cfg，模拟启动后管理页读到的内存配置。
	if err := json.Unmarshal([]byte(`[{"id":"env-one","base_url":"https://x","api_key_env":"NB_TEST_MASK","enabled":false}]`), &cfg.CustomUpstreams); err != nil {
		t.Fatal(err)
	}
	store := newCustomStore(path, cfg)
	list, err := store.ListCustomUpstreams()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("列表条数 = %d", len(list))
	}
	if list[0].KeyTail != "…abcd" {
		t.Errorf("env 密钥尾号投影不符: %q", list[0].KeyTail)
	}
	if list[0].Enabled {
		t.Error("disabled 条目的 Enabled 投影错误")
	}
}
