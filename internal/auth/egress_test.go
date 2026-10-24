// egress_test.go 出站线路绑定（egressId）的凭证往返。
//
// # 这个文件守的是一个安静的数据丢失
//
// MarshalNested 是**逐字段白名单**写回的（只写它知道的键）。所以任何一个
// 新增字段只要漏进白名单，表现就是：token 刷新一次 → 该字段从磁盘消失
// → 下次启动读回来是空值 → "绑定莫名丢了"。
//
// deviceToken 曾经踩过这个坑（见 MarshalNested 的注释），egressId 是同一个
// 形状的字段，所以同样用往返测试钉住。
package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestEgressIDParsedFromNestedForm 嵌套形（插件 OAuth 输出）认 account.egressId。
func TestEgressIDParsedFromNestedForm(t *testing.T) {
	raw := []byte(`{
	  "auth": {"accessToken":"at","refreshToken":"rt","expiresAt":1,
	           "domain":"www.workbuddy.ai","channel":"intl"},
	  "account": {"uid":"u1","nickname":"n","deviceToken":"dt","egressId":"intl-01"}
	}`)
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.EgressID != "intl-01" {
		t.Errorf("嵌套形 egressId 未解析：%q", a.EgressID)
	}
	if a.DeviceToken != "dt" {
		t.Errorf("deviceToken 不能因为新增字段而丢：%q", a.DeviceToken)
	}
}

// TestEgressIDParsedFromFlatForm 扁平形（手写凭证）认顶层 egressId。
func TestEgressIDParsedFromFlatForm(t *testing.T) {
	raw := []byte(`{"accessToken":"at","uid":"u2","egressId":"cn-home-01"}`)
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.EgressID != "cn-home-01" {
		t.Errorf("扁平形 egressId 未解析：%q", a.EgressID)
	}
}

// TestEgressIDSurvivesSaveAtomic 写回（token 刷新路径）必须保留 egressId。
func TestEgressIDSurvivesSaveAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "u1.json")
	raw := []byte(`{"accessToken":"at0","uid":"u1","deviceToken":"dt","egressId":"line-a"}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	a.FilePath = path
	// 模拟一次刷新：改 token 后落盘。
	a.AccessToken = "at1"
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(written)
	if err != nil {
		t.Fatalf("回读 Parse: %v", err)
	}
	if back.EgressID != "line-a" {
		t.Errorf("刷新后出站线路绑定丢失（egressId=%q）—— "+
			"表现是\"刷新一次之后所有账号又回到共用出口\"", back.EgressID)
	}
	if back.AccessToken != "at1" {
		t.Errorf("token 未更新：%q", back.AccessToken)
	}

	// 键名也要对：嵌套形 account.egressId（与 Parse 的读取位置一致）。
	var doc struct {
		Account struct {
			EgressID string `json:"egressId"`
		} `json:"account"`
	}
	if err := json.Unmarshal(written, &doc); err != nil {
		t.Fatalf("写回的凭证不是合法 JSON: %v", err)
	}
	if doc.Account.EgressID != "line-a" {
		t.Errorf("写回位置不对（应在 account.egressId）：%q", doc.Account.EgressID)
	}
}
