package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NaiZi899/Naibox/internal/auth"
	"github.com/NaiZi899/Naibox/internal/custom"
	"github.com/NaiZi899/Naibox/internal/gateway"
	"github.com/NaiZi899/Naibox/internal/pool"
)

// 这条测试把 custom.Provider 接进真实的出口层（不是 mock），验证三件
// 只有端到端才看得见的事：
//
//	① `<id>/<模型>` 前缀在出站前被剥掉（上游收到的是裸模型名）；
//	② 鉴权头带的是配置里的 key；
//	③ 流式响应逐字节到达客户端。
//
// 它也是"自定义上游不是特殊路径"的证明：走的是与 codearts 完全相同的
// Pool.PickFor → Credential → router.Chat 链路。

type customRouter struct {
	id   string
	prov *custom.Provider
	p    *pool.Pool
}

var _ ProviderRouter = (*customRouter)(nil)

func (r *customRouter) Has(id string) bool { return id == r.id }
func (r *customRouter) Default() string    { return r.id }
func (r *customRouter) Models(ctx context.Context, id string) ([]gateway.ModelInfo, bool) {
	if id != r.id {
		return nil, false
	}
	ms, err := r.prov.Models(ctx, gateway.Credential{})
	return ms, err == nil
}
func (r *customRouter) ModelMultipliers(context.Context, string) (map[string]float64, bool) {
	return nil, false
}
func (r *customRouter) Chat(ctx context.Context, id string, cred gateway.Credential, body []byte) (gateway.ChatStream, bool, error) {
	if id != r.id {
		return gateway.ChatStream{}, false, nil
	}
	cs, err := r.prov.Chat(ctx, cred, body)
	return cs, true, err
}
func (r *customRouter) Credential(id, uid string) (gateway.Credential, bool) {
	if id != r.id {
		return gateway.Credential{}, false
	}
	secret, ok := r.p.SecretOf(uid)
	if !ok || secret == nil {
		return gateway.Credential{}, false
	}
	return gateway.Credential{Provider: id, UID: uid, Secret: secret}, true
}
func (r *customRouter) RefreshCredential(context.Context, string, gateway.Credential) (bool, error) {
	return false, nil // 静态 key 没有续期
}
func (r *customRouter) Classify(_ string, status int, body string) (gateway.ErrorKind, bool) {
	return r.prov.Classify(status, body), true
}
func (r *customRouter) Explain(string, int, string) (gateway.ErrorExplain, bool) {
	return gateway.ErrorExplain{}, false
}
func (r *customRouter) RefreshSkew(string, gateway.Credential) (time.Duration, bool) {
	return 0, false
}
func (r *customRouter) ResetAt(string, gateway.Credential) (time.Time, bool) {
	return time.Time{}, false
}
func (r *customRouter) SoftRateReset(string, int, string) (time.Time, bool) {
	return time.Time{}, false
}

func TestChatRoutesToCustomProvider(t *testing.T) {
	var mu sync.Mutex
	var gotAuth, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		gotBody = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
	}))
	defer upstream.Close()

	ep, err := custom.Normalize(custom.Spec{
		ID: "custom-test", Name: "测试端点", BaseURL: upstream.URL, APIKey: "sk-custom-secret",
		Models: []string{"gpt-test"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	prov := custom.New(ep, nil)

	p := pool.New("")
	p.SetDefaultProvider("custom-test")
	p.SetMaxInFlight(10)
	creds, err := prov.LoadCredentialsWithSecrets("")
	if err != nil {
		t.Fatal(err)
	}
	auths := make([]*auth.Auth, 0, len(creds))
	secrets := map[string]any{}
	for _, c := range creds {
		auths = append(auths, &auth.Auth{UID: c.UID, Nickname: c.Nickname})
		secrets[c.UID] = c.Secret
	}
	p.SyncToDirWithSecrets("custom-test", auths, secrets)

	r := &customRouter{id: "custom-test", prov: prov, p: p}
	def := newFakeUpstream(t, func(string) (int, string, bool) {
		return 500, `{"error":"默认上游不该被自定义前缀的请求碰到"}`, false
	})
	h := NewHandler(Config{
		Pool: p, Upstream: def, Provider: r,
		DefaultProvider: "custom-test", OwnedBy: "workbuddy",
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"custom-test/gpt-test","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"content":"ok"`) {
		t.Errorf("流式响应没有原样到达: %s", rec.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if gotAuth != "Bearer sk-custom-secret" {
		t.Errorf("鉴权头 = %q，期望 Bearer sk-custom-secret", gotAuth)
	}
	var sent struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil {
		t.Fatalf("上游收到的不是 JSON: %v (%s)", err, gotBody)
	}
	if sent.Model != "gpt-test" {
		t.Errorf("上游收到的模型名 = %q，期望剥掉前缀后的 gpt-test", sent.Model)
	}
}

// TestCustomMultiKeyFailover 多 key 时 401 应换到下一把 key，而不是透传给客户端。
func TestCustomMultiKeyFailover(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	badKeySeen := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		auth := r.Header.Get("Authorization")
		if auth == "Bearer sk-bad-key-1111" {
			badKeySeen = true
		}
		mu.Unlock()
		if auth == "Bearer sk-bad-key-1111" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"bad key"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
	}))
	defer upstream.Close()

	ep, err := custom.Normalize(custom.Spec{
		ID: "multi-test", BaseURL: upstream.URL,
		APIKeys: []string{"sk-bad-key-1111", "sk-good-key-2222"},
		Models:  []string{"m1"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	prov := custom.New(ep, nil)
	p := pool.New("")
	p.SetDefaultProvider("multi-test")
	p.SetMaxInFlight(10)
	creds, _ := prov.LoadCredentialsWithSecrets("")
	auths := make([]*auth.Auth, 0, len(creds))
	secrets := map[string]any{}
	for _, c := range creds {
		auths = append(auths, &auth.Auth{UID: c.UID, Nickname: c.Nickname})
		secrets[c.UID] = c.Secret
	}
	p.SyncToDirWithSecrets("multi-test", auths, secrets)

	r := &customRouter{id: "multi-test", prov: prov, p: p}
	h := NewHandler(Config{
		Pool: p, Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 500, "{}", false }),
		Provider: r, DefaultProvider: "multi-test", OwnedBy: "workbuddy",
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"multi-test/m1","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"content":"ok"`) {
		t.Fatalf("多 key 请求没有成功: status=%d body=%s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if badKeySeen && hits < 2 {
		t.Errorf("坏 key 被拒后没有换号: hits=%d", hits)
	}
}

// TestCustomRateLimitPassthrough 单端点限流必须原样透传，且只打上游一次。
//
// 这是评审 I1 的回归用例：默认的"换号再试"对同一把 key 的多个分片毫无意义，
// 会把一次 429 放大成 N 次上游请求，最后还给客户端一个 503。
func TestCustomRateLimitPassthrough(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"slow down"}`)
	}))
	defer upstream.Close()

	ep, err := custom.Normalize(custom.Spec{
		ID: "rl-test", BaseURL: upstream.URL, APIKey: "sk-rl",
		Models: []string{"m1"}, Shards: 2,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	prov := custom.New(ep, nil)
	p := pool.New("")
	p.SetDefaultProvider("rl-test")
	p.SetMaxInFlight(10)
	creds, _ := prov.LoadCredentialsWithSecrets("")
	auths := make([]*auth.Auth, 0, len(creds))
	secrets := map[string]any{}
	for _, c := range creds {
		auths = append(auths, &auth.Auth{UID: c.UID, Nickname: c.Nickname})
		secrets[c.UID] = c.Secret
	}
	p.SyncToDirWithSecrets("rl-test", auths, secrets)

	r := &customRouter{id: "rl-test", prov: prov, p: p}
	h := NewHandler(Config{
		Pool: p, Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 500, "{}", false }),
		Provider: r, DefaultProvider: "rl-test", OwnedBy: "workbuddy",
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"rl-test/m1","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d，期望 429（body=%s）", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Errorf("上游被请求 %d 次，期望 1（分片不能放大同 key 的重试）", hits)
	}
	if !strings.Contains(rec.Body.String(), "slow down") {
		t.Errorf("上游错误正文未透传: %s", rec.Body.String())
	}
}
