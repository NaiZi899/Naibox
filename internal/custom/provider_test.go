package custom

// provider_test.go —— Chat / Models / Classify / Probe / Credentials 的行为钉死。
//
// 这组用例全部跑在 httptest 假上游上（没有外网、没有真凭证）：
// 本包的契约是“OpenAI 兼容协议”，那么“发出去的字节长什么样、
// 拿回来的字节怎么交回去”就应该能在这台机器上说清楚。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NaiZi899/Naibox/internal/gateway"
)

// testKey 必须长于 8 位，否则 MaskKey 会整体变成 ****，尾号断言就没意义了。
const testKey = "sk-test-abcdefgh1234"

// mustEndpoint 用 Normalize 造一个端点（默认值也由它补）。
//
// 刻意不用手写 Endpoint 字面量：字面量会绕过校验（例如 AuthScheme 漏填就
// 悄悄变成“裸 key”，请求能发出去但鉴权头是错的），于是测试全绿而生产配置被拒。
func mustEndpoint(t *testing.T, s Spec) Endpoint {
	t.Helper()
	ep, err := Normalize(s, nil)
	if err != nil {
		t.Fatalf("Normalize(%s) 失败: %v", s.ID, err)
	}
	return ep
}

// specFor 一条最小合法配置，baseURL 由调用方给（通常是假上游的地址）。
func specFor(baseURL string) Spec {
	return Spec{ID: "my-openai", BaseURL: baseURL, APIKey: testKey}
}

// chatCred 造一个带 secret 的凭证（生产路径的形态）。
func chatCred(ep Endpoint) gateway.Credential {
	return gateway.Credential{Provider: ep.ID, UID: ep.ID + "-s1", Secret: &ep}
}

// TestChatForwardsRequestAndReturnsStream 一条完整请求的逐项断言：
// 方法、路径、鉴权头、附加头、Content-Type、Accept、请求体原样、
// 以及返回的流能读完并 Close。
func TestChatForwardsRequestAndReturnsStream(t *testing.T) {
	const sse = "data: {\"id\":\"1\"}\n\ndata: [DONE]\n\n"

	var gotMethod, gotPath, gotBody string
	var gotHeader http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotMethod, gotPath, gotBody = r.Method, r.URL.Path, string(b)
		gotHeader = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, sse)
	}))
	defer srv.Close()

	s := specFor(srv.URL + "/v1")
	s.Headers = map[string]string{"X-Title": "NaiBox"}
	ep := mustEndpoint(t, s)
	p := New(ep, nil)

	body := []byte(`{"model":"gpt-4o","stream":true}`)
	cs, err := p.Chat(context.Background(), chatCred(ep), body)
	if err != nil {
		t.Fatalf("Chat 报错: %v", err)
	}
	if cs.Status != http.StatusOK {
		t.Errorf("Status = %d，期望 200", cs.Status)
	}
	if cs.Body == nil {
		t.Fatal("Body 为 nil —— 调用方没有东西可读")
	}
	out, rerr := io.ReadAll(cs.Body)
	cerr := cs.Body.Close()
	if rerr != nil {
		t.Errorf("读流失败: %v", rerr)
	}
	if cerr != nil {
		t.Errorf("Close 报错: %v（连接会泄漏）", cerr)
	}
	if string(out) != sse {
		t.Errorf("SSE 字节被改动：\n got %q\nwant %q", string(out), sse)
	}

	if gotMethod != http.MethodPost || gotPath != "/v1/chat/completions" {
		t.Errorf("请求行 = %s %s，期望 POST /v1/chat/completions", gotMethod, gotPath)
	}
	if v := gotHeader.Get("Authorization"); v != "Bearer "+testKey {
		t.Errorf("Authorization = %q，期望 Bearer <key>", v)
	}
	if v := gotHeader.Get("X-Title"); v != "NaiBox" {
		t.Errorf("附加头 X-Title = %q", v)
	}
	if v := gotHeader.Get("Content-Type"); v != "application/json" {
		t.Errorf("Content-Type = %q", v)
	}
	if v := gotHeader.Get("Accept"); v != "text/event-stream" {
		t.Errorf("Accept = %q（缺了它有的网关会回整包 JSON）", v)
	}
	if gotBody != string(body) {
		t.Errorf("请求体被改动：\n got %s\nwant %s", gotBody, body)
	}
}

// TestChatAuthHeaderVariants 鉴权头的三种配置形态。
//
// 这三条是配置界面上最容易写错的字段（头名 / 方案 / 裸 key），
// 所以逐字钉住“最终发出去的头值”。
func TestChatAuthHeaderVariants(t *testing.T) {
	cases := []struct {
		name       string
		header     string
		scheme     *string
		wantHeader string
		wantValue  string
	}{
		{"默认 Bearer", "", nil, "Authorization", "Bearer " + testKey},
		{"自定义头名 + 裸 key", "X-Api-Key", ptr(""), "X-Api-Key", testKey},
		{"自定义方案", "", ptr("Token"), "Authorization", "Token " + testKey},
		{"自定义头名 + 自定义方案", "x-api-key", ptr("Token"), "X-Api-Key", "Token " + testKey},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotHeader, gotValue string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotHeader = c.wantHeader
				gotValue = r.Header.Get(c.wantHeader)
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			s := specFor(srv.URL + "/v1")
			s.AuthHeader, s.AuthScheme = c.header, c.scheme
			ep := mustEndpoint(t, s)

			if _, err := New(ep, nil).Chat(context.Background(), chatCred(ep), []byte(`{}`)); err != nil {
				t.Fatalf("Chat 报错: %v", err)
			}
			if gotValue != c.wantValue {
				t.Errorf("头 %s = %q，期望 %q", gotHeader, gotValue, c.wantValue)
			}
		})
	}
}

// TestChatReturnsNon2xxAsStream 非 2xx 也必须以 ChatStream 返回。
//
// 这条是 gateway.Provider 的核心约定：出口层靠 Status + 响应体做分类
// （冷却 / 换号）。若在这里改成一个 error，上游的业务错误码就丢了。
func TestChatReturnsNon2xxAsStream(t *testing.T) {
	const errBody = `{"error":{"message":"invalid api key","type":"auth_error"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, errBody)
	}))
	defer srv.Close()

	ep := mustEndpoint(t, specFor(srv.URL+"/v1"))
	cs, err := New(ep, nil).Chat(context.Background(), chatCred(ep), []byte(`{}`))
	if err != nil {
		t.Fatalf("非 2xx 不该返回 error，得到: %v", err)
	}
	if cs.Status != http.StatusUnauthorized {
		t.Errorf("Status = %d，期望 401", cs.Status)
	}
	if cs.Body == nil {
		t.Fatal("非 2xx 的响应体也必须交回来（分类要看正文）")
	}
	defer func() { _ = cs.Body.Close() }()
	out, _ := io.ReadAll(cs.Body)
	if string(out) != errBody {
		t.Errorf("错误体被改动: %q", string(out))
	}
}

// TestChatTransportError 连不上才返回 error，且文案里有 URL。
func TestChatTransportError(t *testing.T) {
	ep := mustEndpoint(t, specFor("http://127.0.0.1:1/v1"))
	_, err := New(ep, nil).Chat(context.Background(), chatCred(ep), []byte(`{}`))
	if err == nil {
		t.Fatal("连不上时应返回 error")
	}
	if !strings.Contains(err.Error(), ep.ChatURL()) {
		t.Errorf("错误文案应包含被打的 URL %s，得到: %v", ep.ChatURL(), err)
	}
	if strings.Contains(err.Error(), testKey) {
		t.Errorf("错误文案里出现了完整密钥: %v", err)
	}
}

// TestChatRejectsCrossHostRedirect 跨主机重定向必须被拒，且第二台主机
// **一个请求都收不到**。
//
// 为什么这条这么重要：auth_header 可以配成任意头名，而 Go 的 http.Client
// 只对 Authorization 做跨域剥离 —— 自定义头名会连值一起送给第三方主机。
func TestChatRejectsCrossHostRedirect(t *testing.T) {
	var secondHit atomic.Bool
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHit.Store(true)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: leaked\n\n")
	}))
	defer second.Close()

	var firstAuth atomic.Value
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstAuth.Store(r.Header.Get("Authorization"))
		http.Redirect(w, r, second.URL+"/v1/chat/completions", http.StatusFound)
	}))
	defer first.Close()

	ep := mustEndpoint(t, specFor(first.URL+"/v1"))
	_, err := New(ep, nil).Chat(context.Background(), chatCred(ep), []byte(`{}`))
	if err == nil {
		t.Fatal("跨主机重定向应返回 error")
	}
	if !strings.Contains(err.Error(), "重定向") {
		t.Errorf("错误文案应说明是重定向被拒，得到: %v", err)
	}
	if secondHit.Load() {
		t.Error("第二台主机收到了请求 —— 鉴权头已经泄漏出去了")
	}
	if v, _ := firstAuth.Load().(string); v != "Bearer "+testKey {
		t.Errorf("第一跳没有带上鉴权头（%q），说明这条用例没测到真实路径", v)
	}
}

// TestModelsStaticNoNetwork 静态目录一次网络都不发。
//
// 用“指向关闭端口”的基址来证明：若实现偷偷发了请求，这里必然报错。
func TestModelsStaticNoNetwork(t *testing.T) {
	s := specFor("http://127.0.0.1:1/v1")
	s.Models = []string{" m1 ", "m2", "m1"}
	ep := mustEndpoint(t, s)

	ms, err := New(ep, nil).Models(context.Background(), gateway.Credential{})
	if err != nil {
		t.Fatalf("静态目录不该依赖网络: %v", err)
	}
	if len(ms) != 2 || ms[0].ID != "m1" || ms[1].ID != "m2" {
		t.Fatalf("目录内容/顺序不对: %#v（应保持配置顺序并去重）", ms)
	}
	for _, m := range ms {
		if m.ContextWindow != 0 {
			t.Errorf("未知的上下文窗口应留 0，得到 %d", m.ContextWindow)
		}
	}
}

// TestModelsLiveParseAndCache 现场解析 {data:[…]}，且两次调用只打一次上游。
func TestModelsLiveParseAndCache(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path != "/v1/models" {
			t.Errorf("路径 = %s，期望 /v1/models", r.URL.Path)
		}
		if v := r.Header.Get("Authorization"); v != "Bearer "+testKey {
			t.Errorf("模型目录请求也要带鉴权头，得到 %q", v)
		}
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"gpt-4o"},{"id":"deepseek-chat"}]}`)
	}))
	defer srv.Close()

	p := New(mustEndpoint(t, specFor(srv.URL+"/v1")), nil)
	for i := 0; i < 2; i++ {
		ms, err := p.Models(context.Background(), gateway.Credential{})
		if err != nil {
			t.Fatalf("第 %d 次 Models 报错: %v", i+1, err)
		}
		if len(ms) != 2 || ms[0].ID != "gpt-4o" || ms[1].ID != "deepseek-chat" {
			t.Fatalf("第 %d 次目录内容是 %#v", i+1, ms)
		}
	}
	if hits != 1 {
		t.Errorf("上游被打了 %d 次，期望 1 次（10 分钟缓存没生效）", hits)
	}
}

// TestModelsTopLevelArray 兼容直接回数组的自建服务。
func TestModelsTopLevelArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `[{"id":"a"},{"id":""},{"id":"b"}]`)
	}))
	defer srv.Close()

	ms, err := New(mustEndpoint(t, specFor(srv.URL+"/v1")), nil).Models(context.Background(), gateway.Credential{})
	if err != nil {
		t.Fatalf("数组形状被拒: %v", err)
	}
	if len(ms) != 2 || ms[0].ID != "a" || ms[1].ID != "b" {
		t.Fatalf("空 id 应被跳过，得到 %#v", ms)
	}
}

// TestModelsFallsBackToCache 缓存过期 + 上游报错 ⇒ 沿用旧目录，且不返回 error。
//
// 手法：把缓存时间戳改成“已经过期”，这样才走得到拉取失败那条分支
// （否则 10 分钟缓存会把第二次调用直接挡在缓存里）。
func TestModelsFallsBackToCache(t *testing.T) {
	var hits int
	var broken atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if broken.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"boom"}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"keep-me"}]}`)
	}))
	defer srv.Close()

	p := New(mustEndpoint(t, specFor(srv.URL+"/v1")), nil)
	if _, err := p.Models(context.Background(), gateway.Credential{}); err != nil {
		t.Fatalf("首次拉取失败: %v", err)
	}

	broken.Store(true)
	p.mu.Lock()
	p.cache.at = time.Now().Add(-modelsTTL - time.Minute) // 强制过期
	p.mu.Unlock()

	ms, err := p.Models(context.Background(), gateway.Credential{})
	if err != nil {
		t.Fatalf("有旧目录时不该返回 error: %v", err)
	}
	if len(ms) != 1 || ms[0].ID != "keep-me" {
		t.Fatalf("应沿用旧目录，得到 %#v", ms)
	}
	if hits != 2 {
		t.Errorf("上游被打了 %d 次，期望 2 次（第二次应当真的试过了）", hits)
	}
}

// TestModelsReturnsCopy 目录必须是副本：出口层会就地做前缀标注。
func TestModelsReturnsCopy(t *testing.T) {
	s := specFor("http://127.0.0.1:1/v1")
	s.Models = []string{"m1"}
	p := New(mustEndpoint(t, s), nil)

	first, err := p.Models(context.Background(), gateway.Credential{})
	if err != nil {
		t.Fatalf("静态目录失败: %v", err)
	}
	first[0].ID = "mutated"
	second, _ := p.Models(context.Background(), gateway.Credential{})
	if second[0].ID != "m1" {
		t.Errorf("调用方改动污染了目录: %q", second[0].ID)
	}
}

// TestClassify 状态码 → ErrorKind 的映射表（必须与 brief/契约逐项一致）。
func TestClassify(t *testing.T) {
	p := New(mustEndpoint(t, specFor("http://127.0.0.1:1/v1")), nil)
	cases := []struct {
		status int
		want   gateway.ErrorKind
	}{
		{200, gateway.ErrKindNone},
		{400, gateway.ErrKindNone},
		// 401/403/429 原样透传：同一把 key 的其它分片只会得到同一个答案，
		// 换号重试等于把同一个失败再打 N 次（见 classify.go 的文件头）。
		{401, gateway.ErrKindPassthrough},
		{403, gateway.ErrKindPassthrough},
		{404, gateway.ErrKindClient},
		{429, gateway.ErrKindPassthrough},
		{500, gateway.ErrKindServer},
		{503, gateway.ErrKindServer},
	}
	for _, c := range cases {
		if got := p.Classify(c.status, `{"error":"whatever"}`); got != c.want {
			t.Errorf("Classify(%d) = %v，期望 %v", c.status, got, c.want)
		}
	}
}

// TestClassifyMultiKey 多 key 时 401/429 必须换号（下一把 key 可能是好的）。
func TestClassifyMultiKey(t *testing.T) {
	s := specFor("http://127.0.0.1:1/v1")
	s.APIKey = ""
	s.APIKeys = []string{"sk-a-12345678", "sk-b-87654321"}
	p := New(mustEndpoint(t, s), nil)
	if got := p.Classify(401, ""); got != gateway.ErrKindAuth {
		t.Errorf("多 key 401 = %v，期望 Auth（换号）", got)
	}
	if got := p.Classify(429, ""); got != gateway.ErrKindNone {
		t.Errorf("多 key 429 = %v，期望 None（换号不罚）", got)
	}
}

// TestProbeOKCountsModels 2xx 且形状正确：OK + 条数。
func TestProbeOKCountsModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"id":"a"},{"id":"b"},{"id":"c"}]}`)
	}))
	defer srv.Close()

	res := New(mustEndpoint(t, specFor(srv.URL+"/v1")), nil).Probe(context.Background())
	if !res.OK || res.Status != http.StatusOK {
		t.Fatalf("探测应成功: %+v", res)
	}
	if res.ModelsCount != 3 {
		t.Errorf("ModelsCount = %d，期望 3", res.ModelsCount)
	}
	if res.Error != "" {
		t.Errorf("成功时 Error 应为空，得到 %q", res.Error)
	}
	if res.At.IsZero() {
		t.Error("At 不应是零值（面板要显示探测时刻）")
	}
}

// TestProbeAuthErrorMessage 401/403 ⇒ “密钥无效”，且点名状态码。
func TestProbeAuthError(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			_, _ = io.WriteString(w, `{"error":"bad key"}`)
		}))

		res := New(mustEndpoint(t, specFor(srv.URL+"/v1")), nil).Probe(context.Background())
		srv.Close()

		if res.OK {
			t.Errorf("HTTP %d 不该判成 OK", code)
		}
		if !strings.Contains(res.Error, "密钥无效") {
			t.Errorf("HTTP %d 的文案应以“密钥无效”开头，得到 %q", code, res.Error)
		}
		if !strings.Contains(res.Error, strconv.Itoa(code)) {
			t.Errorf("HTTP %d 的文案应点名状态码，得到 %q", code, res.Error)
		}
		if res.Status != code {
			t.Errorf("Status = %d，期望 %d", res.Status, code)
		}
	}
}

// TestProbeNotFoundMessage 404 ⇒ “端点可达，但没有模型目录”。
//
// 这条与“连不上”必须能分开：前者是路径写错，后者是地址/网络写错。
func TestProbeNotFoundMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"no such route"}`)
	}))
	defer srv.Close()

	res := New(mustEndpoint(t, specFor(srv.URL+"/v1")), nil).Probe(context.Background())
	if res.OK || res.Status != http.StatusNotFound {
		t.Fatalf("探测结果不对: %+v", res)
	}
	if !strings.Contains(res.Error, "端点可达，但没有模型目录（HTTP 404）") {
		t.Errorf("文案 = %q", res.Error)
	}
}

// TestProbeConnectionRefusedMessage 连不上时文案里要有**真实 URL** 与底层错误。
func TestProbeConnectionRefusedMessage(t *testing.T) {
	ep := mustEndpoint(t, specFor("http://127.0.0.1:1/v1"))
	res := New(ep, nil).Probe(context.Background())
	if res.OK {
		t.Fatal("关闭端口不该探测成功")
	}
	if res.Status != 0 {
		t.Errorf("没连上时 Status 应为 0，得到 %d", res.Status)
	}
	if !strings.Contains(res.Error, ep.ModelsURL()) {
		t.Errorf("文案应包含 URL %s，得到 %q", ep.ModelsURL(), res.Error)
	}
	if len(res.Error) <= len("无法连接 "+ep.ModelsURL()+"：") {
		t.Errorf("文案里没有底层错误: %q", res.Error)
	}
}

// TestProbeNeverLeaksKey 上游在正文里回显密钥时，探测文案必须已脱敏。
//
// 用 502（走“普通失败”分支，正文会被带出来）来证明脱敏真的执行了：
// 401/404 两个分支的文案是固定句子，测不到这条路径。
func TestProbeNeverLeaksKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":"upstream rejects key `+testKey+` for this route"}`)
	}))
	defer srv.Close()

	res := New(mustEndpoint(t, specFor(srv.URL+"/v1")), nil).Probe(context.Background())
	if res.OK {
		t.Fatal("502 不该判成 OK")
	}
	if strings.Contains(res.Error, testKey) {
		t.Errorf("完整密钥出现在探测文案里: %q", res.Error)
	}
	if !strings.Contains(res.Error, MaskKey(testKey)) {
		t.Errorf("应显示脱敏后的尾号 %q，得到 %q", MaskKey(testKey), res.Error)
	}
	if len([]rune(res.Error)) > probeErrorBodyChars+200 {
		t.Errorf("文案过长（正文没被截断）: %d 字符", len([]rune(res.Error)))
	}
}

// TestProbeCachesAndProbeNow 15 秒缓存生效，ProbeNow 绕过它。
func TestProbeCachesAndProbeNow(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = io.WriteString(w, `{"data":[{"id":"a"}]}`)
	}))
	defer srv.Close()

	p := New(mustEndpoint(t, specFor(srv.URL+"/v1")), nil)
	if res := p.Probe(context.Background()); !res.OK {
		t.Fatalf("首次探测失败: %+v", res)
	}
	_ = p.Probe(context.Background())
	if hits != 1 {
		t.Errorf("两次 Probe 打了 %d 次上游，期望 1 次（缓存没生效）", hits)
	}
	if res := p.ProbeNow(context.Background()); !res.OK {
		t.Fatalf("ProbeNow 失败: %+v", res)
	}
	if hits != 2 {
		t.Errorf("ProbeNow 应绕过缓存，实际打了 %d 次", hits)
	}
}

// TestProbeSpec 未保存的草稿也能探测；校验失败时前缀是“配置非法：”。
func TestProbeSpec(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"id":"a"},{"id":"b"}]}`)
	}))
	defer srv.Close()

	good := ProbeSpec(context.Background(), specFor(srv.URL+"/v1"), nil)
	if !good.OK || good.ModelsCount != 2 {
		t.Fatalf("草稿探测失败: %+v", good)
	}

	bad := specFor("ftp://nope/v1")
	bad.ID = "Bad ID"
	res := ProbeSpec(context.Background(), bad, nil)
	if res.OK {
		t.Error("非法配置不该探测成功")
	}
	if !strings.HasPrefix(res.Error, "配置非法：") {
		t.Errorf("文案前缀 = %q", res.Error)
	}
	if !strings.Contains(res.Error, "custom_upstreams") && !strings.Contains(res.Error, "id") {
		t.Errorf("文案应点名哪个字段非法: %q", res.Error)
	}
}

// TestProbeSpecResolvesEnvKey 草稿里用 api_key_env 时，密钥由 getenv 解析。
func TestProbeSpecResolvesEnvKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := r.Header.Get("Authorization"); v != "Bearer from-env" {
			t.Errorf("环境变量里的密钥没被用上: %q", v)
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"a"}]}`)
	}))
	defer srv.Close()

	s := Spec{ID: "env-endpoint", BaseURL: srv.URL + "/v1", APIKeyEnv: "CUSTOM_TEST_KEY"}
	res := ProbeSpec(context.Background(), s, func(k string) string {
		if k == "CUSTOM_TEST_KEY" {
			return "from-env"
		}
		return ""
	})
	if !res.OK {
		t.Fatalf("探测失败: %+v", res)
	}
}

// TestCredentialsShards 合成账号的数量 / uid / 昵称 / secret。
func TestCredentialsShards(t *testing.T) {
	s := specFor("http://127.0.0.1:1/v1")
	s.Shards = 3
	ep := mustEndpoint(t, s)
	p := New(ep, nil)

	wantUIDs := []string{"my-openai-s1", "my-openai-s2", "my-openai-s3"}
	if got := p.UIDs(); len(got) != 3 || got[0] != wantUIDs[0] || got[2] != wantUIDs[2] {
		t.Fatalf("UIDs = %v，期望 %v", got, wantUIDs)
	}

	// dir 必须被忽略：给一个不存在的路径也不影响结果。
	creds, err := p.LoadCredentials("/nonexistent/custom/auths")
	if err != nil {
		t.Fatalf("LoadCredentials 报错: %v", err)
	}
	if len(creds) != 3 {
		t.Fatalf("凭证条数 = %d，期望 3", len(creds))
	}
	for i, c := range creds {
		if c.Provider != ep.ID {
			t.Errorf("[%d] Provider = %q", i, c.Provider)
		}
		if c.UID != wantUIDs[i] {
			t.Errorf("[%d] UID = %q，期望 %q", i, c.UID, wantUIDs[i])
		}
		if c.Nickname != "my-openai #"+strconv.Itoa(i+1) {
			t.Errorf("[%d] Nickname = %q", i, c.Nickname)
		}
	}

	secs, err := p.LoadCredentialsWithSecrets("")
	if err != nil {
		t.Fatalf("LoadCredentialsWithSecrets 报错: %v", err)
	}
	if len(secs) != 3 {
		t.Fatalf("带 secret 的条数 = %d，期望 3", len(secs))
	}
	for i, cs := range secs {
		got, ok := cs.Secret.(*Endpoint)
		if !ok || got == nil {
			t.Fatalf("[%d] Secret 不是 *Endpoint: %#v", i, cs.Secret)
		}
		if got.ID != ep.ID || got.APIKey != ep.APIKey {
			t.Errorf("[%d] secret 内容不对: %+v", i, *got)
		}
		if cs.UID != wantUIDs[i] {
			t.Errorf("[%d] uid = %q", i, cs.UID)
		}
	}
	// 分片之间不能共享同一个端点指针（改一个会影响全部）。
	a, _ := secs[0].Secret.(*Endpoint)
	b, _ := secs[1].Secret.(*Endpoint)
	if a == b {
		t.Error("两个分片共享同一个 *Endpoint 指针，热改会串味")
	}
	a.InsecureSkipVerify = true
	if b.InsecureSkipVerify {
		t.Error("改第一个分片影响到了第二个")
	}
}

// TestAdminRoutesAndHandlers 两条管理路由的路径、可见性与返回体。
func TestAdminRoutesAndHandlers(t *testing.T) {
	s := specFor("http://127.0.0.1:1/v1") // 探测必然失败，但 handler 不许 panic
	s.Models = []string{"m1", "m2"}
	ep := mustEndpoint(t, s)
	p := New(ep, nil)

	routes := p.AdminRoutes()
	if len(routes) != 2 {
		t.Fatalf("路由数 = %d，期望 2", len(routes))
	}
	info, models := routes[0], routes[1]
	if info.Method != http.MethodGet || info.Path != "/admin/custom/my-openai" {
		t.Errorf("信息路由 = %s %s", info.Method, info.Path)
	}
	if info.Title != "端点信息" || info.Hidden != false || info.Capability != gateway.CapModels {
		t.Errorf("信息路由元数据不对: %+v", info)
	}
	if models.Path != "/admin/custom/my-openai/models" || !models.Hidden || models.Title != "模型目录" {
		t.Errorf("目录路由元数据不对: %+v", models)
	}

	// 信息路由：静态目录 ⇒ models_source=config，且绝不回显完整密钥。
	rec := httptest.NewRecorder()
	info.Handler(rec, httptest.NewRequest(http.MethodGet, info.Path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("信息路由 HTTP %d", rec.Code)
	}
	raw := rec.Body.String()
	if strings.Contains(raw, testKey) {
		t.Errorf("信息路由回显了完整密钥: %s", raw)
	}
	var infoBody struct {
		ID           string      `json:"id"`
		Name         string      `json:"name"`
		BaseURL      string      `json:"base_url"`
		ModelsSource string      `json:"models_source"`
		ModelsCount  int         `json:"models_count"`
		Shards       int         `json:"shards"`
		KeyTail      string      `json:"key_tail"`
		Probe        ProbeResult `json:"probe"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &infoBody); err != nil {
		t.Fatalf("信息路由不是合法 JSON: %v", err)
	}
	if infoBody.ID != ep.ID || infoBody.Name != ep.Name || infoBody.BaseURL != ep.BaseURL {
		t.Errorf("身份字段不对: %+v", infoBody)
	}
	if infoBody.ModelsSource != "config" || infoBody.ModelsCount != 2 {
		t.Errorf("models_source/count = %q/%d，期望 config/2", infoBody.ModelsSource, infoBody.ModelsCount)
	}
	if infoBody.Shards != 1 {
		t.Errorf("shards = %d", infoBody.Shards)
	}
	if infoBody.KeyTail != MaskKey(testKey) {
		t.Errorf("key_tail = %q，期望 %q", infoBody.KeyTail, MaskKey(testKey))
	}
	if infoBody.Probe.OK {
		t.Error("端点不可达时 probe.ok 应为 false")
	}

	// 目录路由：静态目录 ⇒ 带前缀的 title。
	rec2 := httptest.NewRecorder()
	models.Handler(rec2, httptest.NewRequest(http.MethodGet, models.Path, nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("目录路由 HTTP %d", rec2.Code)
	}
	var dirBody struct {
		Items []struct {
			Title string `json:"title"`
			Model string `json:"model"`
		} `json:"items"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &dirBody); err != nil {
		t.Fatalf("目录路由不是合法 JSON: %v", err)
	}
	if len(dirBody.Items) != 2 || dirBody.Items[0].Title != "my-openai/m1" || dirBody.Items[0].Model != "m1" {
		t.Errorf("目录内容不对: %+v", dirBody.Items)
	}
}

// TestAdminModelsShowsError 目录拉取失败时仍然是 200 + 空 items + 原因。
func TestAdminModelsShowsError(t *testing.T) {
	// 没有静态目录，且基址指向关闭端口 ⇒ Models 必然失败。
	ep := mustEndpoint(t, specFor("http://127.0.0.1:1/v1"))
	p := New(ep, nil)
	routes := p.AdminRoutes()

	rec := httptest.NewRecorder()
	routes[1].Handler(rec, httptest.NewRequest(http.MethodGet, routes[1].Path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("失败时也要回 200（面板要显示空列表+原因），得到 %d", rec.Code)
	}
	var body struct {
		Items []any  `json:"items"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("不是合法 JSON: %v", err)
	}
	if body.Items == nil || len(body.Items) != 0 {
		t.Errorf("items 应为空数组而不是 null: %#v", body.Items)
	}
	if body.Error == "" {
		t.Error("失败时必须给出原因")
	}
	if strings.Contains(body.Error, testKey) {
		t.Errorf("原因里出现了完整密钥: %q", body.Error)
	}
}

// TestAdminInfoModelsSourceCache 拉过一次目录后，来源应显示 cache。
func TestAdminInfoModelsSourceCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"id":"a"},{"id":"b"},{"id":"c"},{"id":"d"}]}`)
	}))
	defer srv.Close()

	ep := mustEndpoint(t, specFor(srv.URL+"/v1"))
	p := New(ep, nil)
	if _, err := p.Models(context.Background(), gateway.Credential{}); err != nil {
		t.Fatalf("拉目录失败: %v", err)
	}
	source, count := p.modelsSource()
	if source != "cache" || count != 4 {
		t.Errorf("来源 = %q/%d，期望 cache/4", source, count)
	}
}

// TestIdentityAndCaps ID / DisplayName / Caps 的三条最小事实。
//
// 单列出来是因为它们服务于**核心**（模型前缀、分组标题、能力位），
// 写错时不会报错，只会让界面显示错误的东西。
func TestIdentityAndCaps(t *testing.T) {
	s := specFor("http://127.0.0.1:1/v1")
	s.Name = "我的中转"
	ep := mustEndpoint(t, s)
	p := New(ep, nil)

	if p.ID() != "my-openai" {
		t.Errorf("ID = %q", p.ID())
	}
	// 配置里没写 name 时，Normalize 会回落成 ID —— 所以 DisplayName 永远非空。
	if p.DisplayName() != "我的中转" {
		t.Errorf("DisplayName = %q", p.DisplayName())
	}
	if got := New(mustEndpoint(t, specFor("http://127.0.0.1:1/v1")), nil).DisplayName(); got != "my-openai" {
		t.Errorf("没填 name 时应显示 ID，得到 %q", got)
	}

	caps := p.Caps()
	if !caps.Has(gateway.CapChat) || !caps.Has(gateway.CapModels) {
		t.Errorf("Caps = %v，必须含 CapChat|CapModels", caps.Names())
	}
	// 不声明额度/签到类能力：本包没有账号生命周期，声明了就是假声明。
	for _, c := range []gateway.Capability{gateway.CapCheckin, gateway.CapGrowth, gateway.CapTravel, gateway.CapWelfare, gateway.CapQuotaProbe} {
		if caps.Has(c) {
			t.Errorf("不该声明 %v（自定义端点没有对应端点）", c)
		}
	}
}

// TestSameHostRedirectAllowed 同主机重定向必须照常跟随。
//
// 这条与 TestChatRejectsCrossHostRedirect 是一对：只拦跨主机，
// 而不是把所有重定向都当故障 —— 有些网关会用 302 补尾斜杠，那是正常的。
func TestSameHostRedirectAllowed(t *testing.T) {
	var secondPath atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			http.Redirect(w, r, "/v1/chat/completions/", http.StatusFound)
			return
		}
		secondPath.Store(r.URL.Path)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: ok\n\n")
	}))
	defer srv.Close()

	ep := mustEndpoint(t, specFor(srv.URL+"/v1"))
	cs, err := New(ep, nil).Chat(context.Background(), chatCred(ep), []byte(`{}`))
	if err != nil {
		t.Fatalf("同主机重定向被误拒: %v", err)
	}
	if cs.Body != nil {
		_ = cs.Body.Close()
	}
	if cs.Status != http.StatusOK {
		t.Errorf("Status = %d，期望跟随重定向后的 200", cs.Status)
	}
	if v, _ := secondPath.Load().(string); v != "/v1/chat/completions/" {
		t.Errorf("第二跳路径 = %q，说明重定向没被跟随", v)
	}
}

// TestEndpointCloneIsDeep secret 必须是深拷贝。
//
// 为什么值得单独钉：合成账号共享同一个 Headers map 时，任何一处“就地补个头”
// 都会同时改到全部分片，而症状取决于哪个分片先跑 —— 无法复现的那类 bug。
func TestEndpointCloneIsDeep(t *testing.T) {
	s := specFor("http://127.0.0.1:1/v1")
	s.Headers = map[string]string{"X-Title": "NaiBox"}
	s.Models = []string{"m1"}
	p := New(mustEndpoint(t, s), nil)

	secs, err := p.LoadCredentialsWithSecrets("")
	if err != nil {
		t.Fatalf("LoadCredentialsWithSecrets 报错: %v", err)
	}
	ep, ok := secs[0].Secret.(*Endpoint)
	if !ok {
		t.Fatalf("Secret 不是 *Endpoint: %#v", secs[0].Secret)
	}
	ep.Headers["X-Title"] = "hijacked"
	ep.StaticModels[0] = "hijacked"

	if p.ep.Headers["X-Title"] != "NaiBox" {
		t.Errorf("改 secret 的 Headers 影响到了 Provider 本体: %q", p.ep.Headers["X-Title"])
	}
	if p.ep.StaticModels[0] != "m1" {
		t.Errorf("改 secret 的 StaticModels 影响到了 Provider 本体: %q", p.ep.StaticModels[0])
	}
}

// TestShardCountFloor 手写的 Endpoint（Shards=0）也必须给一个账号。
//
// 生产路径上 Normalize 已经补了默认 1，但 New 的参数是公开的 ——
// shards=0 时若返回空账号列表，池子会以为这个端点没有可用凭证。
func TestShardCountFloor(t *testing.T) {
	p := New(Endpoint{ID: "raw", Name: "raw", Shards: 0}, nil)
	if got := p.UIDs(); len(got) != 1 || got[0] != "raw-s1" {
		t.Errorf("UIDs = %v，期望 1 条 raw-s1", got)
	}
	creds, err := p.LoadCredentials("")
	if err != nil || len(creds) != 1 {
		t.Errorf("凭证 = %v / err=%v，期望 1 条", creds, err)
	}
}

// TestLogfOnZeroValueProvider 零值 Provider 的日志入口不得 panic。
//
// 契约要求“任何方法都不得 panic”，而 logf 是唯一一个可能因为
// logger 为 nil 而崩的入口（New 里已兜住，这里锁住兜底本身）。
func TestLogfOnZeroValueProvider(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("logf 在零值 Provider 上 panic: %v", r)
		}
	}()
	var p Provider
	p.logf("不该炸 %d", 1)
}
