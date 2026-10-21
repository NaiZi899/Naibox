package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 这组用例钉住三件事：契约形状、原始 JSON 透传（校验只在宿主一处）、
// 以及未接线时的降级。真实校验由 internal/boot 的 store 单测覆盖。

type fakeCustomStore struct {
	list      []CustomUpstreamView
	replaced  json.RawMessage
	replaceFn func(json.RawMessage) ([]string, error)
	tested    json.RawMessage
	probe     CustomProbeView
}

func (f *fakeCustomStore) ListCustomUpstreams() ([]CustomUpstreamView, error) {
	return f.list, nil
}

func (f *fakeCustomStore) ReplaceCustomUpstreams(items json.RawMessage) ([]string, error) {
	f.replaced = append(json.RawMessage(nil), items...)
	if f.replaceFn != nil {
		return f.replaceFn(items)
	}
	return []string{"custom_upstreams"}, nil
}

func (f *fakeCustomStore) TestCustomUpstream(spec json.RawMessage) (CustomProbeView, error) {
	f.tested = append(json.RawMessage(nil), spec...)
	return f.probe, nil
}

func customReq(method, path, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.RemoteAddr = "127.0.0.1:12345"
	return r
}

func TestCustomUpstreamsList(t *testing.T) {
	store := &fakeCustomStore{list: []CustomUpstreamView{{
		ID: "my-openai", Name: "我的 OpenAI 反代", BaseURL: "https://api.example.com/v1",
		Enabled: true, ModelsCount: 12, Shards: 1, KeyTail: "…abcd",
		Probe: CustomProbeView{OK: true, Status: 200, LatencyMS: 96, ModelsCount: 12},
	}}}
	rec := httptest.NewRecorder()
	New(Config{CustomUpstreams: store}).ServeHTTP(rec, customReq(http.MethodGet, "/admin/custom-upstreams", ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Items []CustomUpstreamView `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 1 || got.Items[0].ID != "my-openai" || got.Items[0].KeyTail != "…abcd" {
		t.Fatalf("列表内容不符: %+v", got.Items)
	}
	// 完整密钥绝不能出现在回执里（这里用子串粗检，字段层面由 View 结构保证）。
	if strings.Contains(rec.Body.String(), "api_key") {
		t.Error("回执里出现了 api_key 字段")
	}
}

func TestCustomUpstreamsReplacePassesRawJSON(t *testing.T) {
	store := &fakeCustomStore{list: []CustomUpstreamView{}}
	body := `{"items":[{"id":"a","base_url":"https://x","api_key":"k"}]}`
	rec := httptest.NewRecorder()
	New(Config{CustomUpstreams: store}).ServeHTTP(rec, customReq(http.MethodPut, "/admin/custom-upstreams", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(string(store.replaced), `"id":"a"`) {
		t.Errorf("store 没收到原始 items: %s", store.replaced)
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["saved"] != true {
		t.Errorf("缺少 saved=true: %v", got)
	}
	if nr, _ := got["need_restart"].([]any); len(nr) != 1 || nr[0] != "custom_upstreams" {
		t.Errorf("need_restart 不符: %v", got["need_restart"])
	}
}

func TestCustomUpstreamsReplaceRejectsEmptyAndStoreErrors(t *testing.T) {
	store := &fakeCustomStore{}
	rec := httptest.NewRecorder()
	New(Config{CustomUpstreams: store}).ServeHTTP(rec, customReq(http.MethodPut, "/admin/custom-upstreams", `{}`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("缺 items 应为 400，得到 %d", rec.Code)
	}

	store = &fakeCustomStore{replaceFn: func(json.RawMessage) ([]string, error) {
		return nil, errors.New("custom_upstreams[0]（a）的 base_url 非法：只支持 http/https")
	}}
	rec = httptest.NewRecorder()
	New(Config{CustomUpstreams: store}).ServeHTTP(rec, customReq(http.MethodPut, "/admin/custom-upstreams", `{"items":[]}`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("校验失败应为 400，得到 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "base_url") {
		t.Errorf("错误信息应透传: %s", rec.Body.String())
	}
}

func TestCustomUpstreamsTest(t *testing.T) {
	store := &fakeCustomStore{probe: CustomProbeView{OK: true, Status: 200, LatencyMS: 12, ModelsCount: 3}}
	rec := httptest.NewRecorder()
	New(Config{CustomUpstreams: store}).ServeHTTP(rec,
		customReq(http.MethodPost, "/admin/custom-upstreams/test", `{"id":"draft","base_url":"https://x","api_key":"k"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(string(store.tested), `"id":"draft"`) {
		t.Errorf("store 没收到草稿: %s", store.tested)
	}
	if !strings.Contains(rec.Body.String(), `"models_count":3`) {
		t.Errorf("探测结果未回传: %s", rec.Body.String())
	}
}

func TestCustomUpstreamsNotWired(t *testing.T) {
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/admin/custom-upstreams", ""},
		{http.MethodPut, "/admin/custom-upstreams", `{"items":[]}`},
		{http.MethodPost, "/admin/custom-upstreams/test", `{}`},
	} {
		rec := httptest.NewRecorder()
		New(Config{}).ServeHTTP(rec, customReq(tc.method, tc.path, tc.body))
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s %s: status = %d，期望 501", tc.method, tc.path, rec.Code)
		}
	}
}
