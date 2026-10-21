package custom

import (
	"net/http"
	"strings"
	"testing"
)

// spec 校验是启动期唯一的防线：配置写错时进程应当起不来、且报错能直接定位
// 到第几条、哪个字段。这组用例逐条钉住这些判据。

func ptr[T any](v T) *T { return &v }

func baseSpec() Spec {
	return Spec{ID: "my-openai", BaseURL: "https://api.example.com/v1", APIKey: "sk-test-123456"}
}

func TestNormalizeMultipleKeys(t *testing.T) {
	s := baseSpec()
	s.APIKey = ""
	s.APIKeys = []string{" sk-a-12345678 ", "sk-b-87654321", "sk-a-12345678"}
	if _, err := Normalize(s, nil); err == nil || !strings.Contains(err.Error(), "重复") {
		t.Fatalf("重复 key 应被拒: %v", err)
	}

	s.APIKeys = []string{"sk-a-12345678", "sk-b-87654321"}
	ep, err := Normalize(s, nil)
	if err != nil {
		t.Fatalf("多 key 被拒: %v", err)
	}
	if len(ep.Keys) != 2 || ep.APIKey != "sk-a-12345678" {
		t.Errorf("多 key 归一结果不对: %v", ep.Keys)
	}

	// 三种密钥形态不能混用。
	s.APIKey = "sk-single"
	if _, err := Normalize(s, nil); err == nil || !strings.Contains(err.Error(), "只能填一种") {
		t.Errorf("多 key 与单 key 混用应被拒: %v", err)
	}

	s = baseSpec()
	s.APIKey = ""
	s.APIKeys = []string{"sk-a", ""}
	if _, err := Normalize(s, nil); err == nil || !strings.Contains(err.Error(), "为空") {
		t.Errorf("空行应被拒: %v", err)
	}
}

func TestNormalizeDefaults(t *testing.T) {
	ep, err := Normalize(baseSpec(), nil)
	if err != nil {
		t.Fatalf("合法配置被拒: %v", err)
	}
	if ep.Name != "my-openai" {
		t.Errorf("Name 缺省应回落到 ID，得到 %q", ep.Name)
	}
	if ep.ChatPath != "/chat/completions" || ep.ModelsPath != "/models" {
		t.Errorf("路径默认值不对: %q %q", ep.ChatPath, ep.ModelsPath)
	}
	if ep.AuthHeader != "Authorization" || ep.AuthScheme != "Bearer" {
		t.Errorf("鉴权默认值不对: %q %q", ep.AuthHeader, ep.AuthScheme)
	}
	if ep.Timeout.Seconds() != 120 || ep.Shards != 1 {
		t.Errorf("超时/分片默认值不对: %v %d", ep.Timeout, ep.Shards)
	}
	if got := ep.ChatURL(); got != "https://api.example.com/v1/chat/completions" {
		t.Errorf("ChatURL 拼接错误: %s", got)
	}
}

func TestNormalizeAPIKeySources(t *testing.T) {
	s := baseSpec()
	s.APIKey = ""
	s.APIKeyEnv = "MY_KEY"
	ep, err := Normalize(s, func(k string) string {
		if k == "MY_KEY" {
			return "sk-from-env"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("env 形态被拒: %v", err)
	}
	if ep.APIKey != "sk-from-env" {
		t.Errorf("没有读到环境变量: %q", ep.APIKey)
	}

	if _, err := Normalize(s, func(string) string { return "" }); err == nil {
		t.Error("环境变量为空时应报错")
	} else if !strings.Contains(err.Error(), "MY_KEY") {
		t.Errorf("报错应点名环境变量: %v", err)
	}

	both := baseSpec()
	both.APIKeyEnv = "MY_KEY"
	if _, err := Normalize(both, func(string) string { return "x" }); err == nil {
		t.Error("api_key 与 api_key_env 同时填写时应报错")
	}

	none := baseSpec()
	none.APIKey = ""
	if _, err := Normalize(none, nil); err == nil {
		t.Error("两者都为空时应报错")
	}
}

func TestNormalizeIDRules(t *testing.T) {
	for _, id := range []string{"", "My-OpenAI", "1openai", "my_openai", "my openai", "a/b"} {
		s := baseSpec()
		s.ID = id
		if _, err := Normalize(s, nil); err == nil {
			t.Errorf("id %q 应被拒", id)
		}
	}
	for _, id := range []string{"workbuddy", "codearts", "qodercn"} {
		s := baseSpec()
		s.ID = id
		if _, err := Normalize(s, nil); err == nil {
			t.Errorf("id %q 与内置上游重名，应被拒", id)
		}
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"https://api.example.com/v1/", "https://api.example.com/v1", false},
		{"http://127.0.0.1:8000/v1", "http://127.0.0.1:8000/v1", false},
		{"https://api.example.com", "https://api.example.com", false},
		{"ftp://api.example.com", "", true},
		{"api.example.com/v1", "", true},
		{"https://user:pw@api.example.com/v1", "", true},
		{"https://api.example.com/v1?x=1", "", true},
		{"", "", true},
	}
	for _, c := range cases {
		s := baseSpec()
		s.BaseURL = c.in
		ep, err := Normalize(s, nil)
		if c.wantErr {
			if err == nil {
				t.Errorf("%q 应被拒", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q 被误拒: %v", c.in, err)
			continue
		}
		if ep.BaseURL != c.want {
			t.Errorf("%q 归一结果 %q，期望 %q", c.in, ep.BaseURL, c.want)
		}
	}
}

func TestNormalizePathsAndHeaders(t *testing.T) {
	s := baseSpec()
	s.ChatPath = "chat/completions"
	if _, err := Normalize(s, nil); err == nil {
		t.Error("相对路径应被拒")
	}
	s.ChatPath = "/chat/completions?x=1"
	if _, err := Normalize(s, nil); err == nil {
		t.Error("带查询串的路径应被拒")
	}

	for _, h := range []string{"Host", "Content-Length", "Transfer-Encoding", "Authorization"} {
		s = baseSpec()
		s.Headers = map[string]string{h: "x"}
		if _, err := Normalize(s, nil); err == nil {
			t.Errorf("不允许覆盖 %s", h)
		}
	}
	s = baseSpec()
	s.Headers = map[string]string{"X-Title": "a\r\nInjected: 1"}
	if _, err := Normalize(s, nil); err == nil {
		t.Error("头值含换行应被拒")
	}
	s = baseSpec()
	s.Headers = map[string]string{"X-Title": "NaiBox"}
	ep, err := Normalize(s, nil)
	if err != nil {
		t.Fatalf("合法附加头被拒: %v", err)
	}
	if ep.Headers["X-Title"] != "NaiBox" {
		t.Error("附加头丢失")
	}
}

func TestNormalizeAuthScheme(t *testing.T) {
	// 指针 nil = 不写 = Bearer
	ep, _ := Normalize(baseSpec(), nil)
	if ep.AuthScheme != "Bearer" {
		t.Errorf("缺省 scheme 应为 Bearer，得到 %q", ep.AuthScheme)
	}
	// 显式空串 = 裸 key
	s := baseSpec()
	s.AuthScheme = ptr("")
	ep, _ = Normalize(s, nil)
	if ep.AuthScheme != "" {
		t.Errorf("显式空串应为裸 key，得到 %q", ep.AuthScheme)
	}
	// 自定义方案
	s = baseSpec()
	s.AuthScheme = ptr("Token")
	ep, _ = Normalize(s, nil)
	if ep.AuthScheme != "Token" {
		t.Errorf("自定义 scheme 丢失: %q", ep.AuthScheme)
	}
}

func TestNormalizeLimitsAndModels(t *testing.T) {
	for _, tc := range []struct {
		timeout int
		shards  int
		bad     bool
	}{
		{0, 0, false}, {5, 1, false}, {600, 8, false}, {4, 1, true}, {601, 1, true}, {120, 0, false}, {120, 9, true},
	} {
		s := baseSpec()
		s.TimeoutSeconds, s.Shards = tc.timeout, tc.shards
		_, err := Normalize(s, nil)
		if tc.bad && err == nil {
			t.Errorf("timeout=%d shards=%d 应被拒", tc.timeout, tc.shards)
		}
		if !tc.bad && err != nil {
			t.Errorf("timeout=%d shards=%d 被误拒: %v", tc.timeout, tc.shards, err)
		}
	}

	s := baseSpec()
	s.Models = []string{" b ", "a", "a", ""}
	ep, _ := Normalize(s, nil)
	if len(ep.StaticModels) != 2 || ep.StaticModels[0] != "b" || ep.StaticModels[1] != "a" {
		t.Errorf("模型去重/去空结果不对: %#v", ep.StaticModels)
	}
}

func TestNormalizeAllSkipsDisabledAndRejectsDuplicates(t *testing.T) {
	garbage := Spec{ID: "Bad ID", BaseURL: "ftp://x"}
	garbage.Enabled = ptr(false)
	eps, err := NormalizeAll([]Spec{baseSpec(), garbage}, nil)
	if err != nil {
		t.Fatalf("停用条目不应影响启动: %v", err)
	}
	if len(eps) != 1 {
		t.Fatalf("只应有 1 个启用端点，得到 %d", len(eps))
	}

	dup := baseSpec()
	if _, err := NormalizeAll([]Spec{baseSpec(), dup}, nil); err == nil {
		t.Error("重复 id 应报错")
	} else if !strings.Contains(err.Error(), "custom_upstreams[1]") {
		t.Errorf("报错应带数组下标: %v", err)
	}

	// 停用条目也参与查重：两行同 id 时"另一行是否同一条"没有答案。
	off := baseSpec()
	off.Enabled = ptr(false)
	if _, err := NormalizeAll([]Spec{baseSpec(), off}, nil); err == nil {
		t.Error("停用条目与启用条目同 id 也应报错")
	}
}

func TestMaskKey(t *testing.T) {
	if got := MaskKey(""); got != "" {
		t.Errorf("空 key: %q", got)
	}
	if got := MaskKey("short"); got != "****" {
		t.Errorf("短 key: %q", got)
	}
	if got := MaskKey("sk-1234567890abcd"); got != "…abcd" {
		t.Errorf("长 key: %q", got)
	}
}

func TestApplyHeaders(t *testing.T) {
	s := baseSpec()
	s.Headers = map[string]string{"X-Title": "NaiBox"}
	s.AuthScheme = ptr("Token")
	ep, err := Normalize(s, nil)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, ep.ChatURL(), nil)
	ep.applyHeaders(req)
	if got := req.Header.Get("Authorization"); got != "Token sk-test-123456" {
		t.Errorf("鉴权头 = %q", got)
	}
	if got := req.Header.Get("X-Title"); got != "NaiBox" {
		t.Errorf("附加头 = %q", got)
	}
}
