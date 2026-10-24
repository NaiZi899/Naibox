// egress_test.go 按账号选出口：两个号走两条线，第三个号走默认链路。
//
// # 这条测试守的东西
//
// 改造前所有账号共用一个出口（upstream.New() 的 Transport 没有 Proxy）。
// 上游按"账号背后的设备 + 出口 IP"认定风控，共用出口等于把整池绑在一根
// 绳上 —— 2026-10-15/16 之后 11140 的连坐形态就是这么来的。
//
// 所以这里必须证明：**同一个进程里**，不同账号的出站请求确实从不同的
// 代理出去了，且未绑定的账号不受影响（回落默认链路，与改造前一致）。
package upstream

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/NaiZi899/Naibox/internal/auth"
)

// recordingProxy 一个最小的"转发型" HTTP 代理，记录每个请求的目标 host。
//
// 目标上游是**明文 HTTP**，所以 Go 的 Transport 走的是 absolute-form
// （`GET http://host/path HTTP/1.1`），不需要实现 CONNECT：
// 这对本测试足够 —— 我们要证明的是"请求从哪条线路出去"，不是隧道本身。
type recordingProxy struct {
	mu    sync.Mutex
	hosts []string
}

func (p *recordingProxy) wrap(direct *http.Transport) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.URL.Host
		if host == "" {
			host = r.Host
		}
		p.mu.Lock()
		p.hosts = append(p.hosts, host)
		p.mu.Unlock()

		out := r.Clone(r.Context())
		out.RequestURI = ""
		resp, err := direct.RoundTrip(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})
}

func (p *recordingProxy) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.hosts)
}

// newProxyServer 起一个代理，返回它的 URL。
func newProxyServer(t *testing.T, p *recordingProxy) string {
	t.Helper()
	// 直连用的 Transport：Proxy 留空（nil）—— 否则代理会把自己当上游，
	// 变成无限递归。
	direct := &http.Transport{}
	srv := httptest.NewServer(p.wrap(direct))
	t.Cleanup(srv.Close)
	return srv.URL
}

// egressClientFor 按代理 URL 造一条线路的两个 client（与装配层同形）。
func egressClientFor(t *testing.T, proxyURL string) *EgressClient {
	t.Helper()
	u, err := url.Parse(proxyURL)
	if err != nil {
		t.Fatalf("代理 URL 解析失败: %v", err)
	}
	tr := &http.Transport{Proxy: http.ProxyURL(u)}
	return &EgressClient{
		HTTP: &http.Client{Transport: tr},
		Chat: &http.Client{Timeout: 0, Transport: tr},
	}
}

// TestEgressRoutesPerAccount 三个账号 → 三条不同去向。
func TestEgressRoutesPerAccount(t *testing.T) {
	// 假上游：任何路径都返回业务成功的信封（doJSON 只认 code==0）。
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
	}))
	defer upstreamSrv.Close()

	proxyA, proxyB := &recordingProxy{}, &recordingProxy{}
	urlA, urlB := newProxyServer(t, proxyA), newProxyServer(t, proxyB)

	c := New()
	// 两个 base 都指到假上游（DailyCheckin 走 billing 域）。
	c.ChatBaseCN = upstreamSrv.URL
	c.BillingBaseCN = upstreamSrv.URL
	c.Egress = map[string]*EgressClient{
		"line-a": egressClientFor(t, urlA),
		"line-b": egressClientFor(t, urlB),
	}

	upstreamHost := mustHost(t, upstreamSrv.URL)

	// A 走 line-a。
	if err := c.DailyCheckin(&auth.Auth{UID: "uA", AccessToken: "at", EgressID: "line-a"}); err != nil {
		t.Fatalf("A 的请求失败: %v", err)
	}
	if proxyA.count() != 1 {
		t.Fatalf("A 应恰好经过 line-a 一次，实际 %d 次", proxyA.count())
	}
	if proxyB.count() != 0 {
		t.Fatalf("A 不该经过 line-b，实际 %d 次", proxyB.count())
	}

	// B 走 line-b。
	if err := c.DailyCheckin(&auth.Auth{UID: "uB", AccessToken: "at", EgressID: "line-b"}); err != nil {
		t.Fatalf("B 的请求失败: %v", err)
	}
	if proxyB.count() != 1 {
		t.Fatalf("B 应恰好经过 line-b 一次，实际 %d 次", proxyB.count())
	}
	if proxyA.count() != 1 {
		t.Errorf("B 不该再经过 line-a（应仍为 1），实际 %d", proxyA.count())
	}

	// 两个代理记到的目标 host 必须是同一个上游（证明"换的是出口，不是目标"）。
	if got := proxyA.hosts[0]; got != upstreamHost {
		t.Errorf("line-a 的目标 host = %q，期望 %q", got, upstreamHost)
	}

	// C 未绑定：两条线都不该看到它（走默认链路直连）。
	if err := c.DailyCheckin(&auth.Auth{UID: "uC", AccessToken: "at"}); err != nil {
		t.Fatalf("C 的请求失败: %v", err)
	}
	if proxyA.count() != 1 || proxyB.count() != 1 {
		t.Errorf("未绑定线路的账号不得经过任何线路：a=%d b=%d", proxyA.count(), proxyB.count())
	}
}

// TestEgressUnknownIDFallsBackToDefault 绑了一个不存在的 id → 回落默认链路。
//
// # 为什么是回落而不是报错
//
// 装配期已经为这种情况打了日志（boot.validateEgressBindings）；运行期
// 若因此让账号直接不可用，一次配置笔误就会让整个号从池子里"消失"。
// 回落让"能用"和"看得见的告警"同时成立。
func TestEgressUnknownIDFallsBackToDefault(t *testing.T) {
	c := New()
	c.Egress = map[string]*EgressClient{"line-a": {}}

	a := &auth.Auth{UID: "u1", AccessToken: "at", EgressID: "no-such-line"}
	if got := c.httpFor(a); got != c.HTTP {
		t.Error("未知线路必须回落默认 HTTP client")
	}
	if got := c.chatHTTPFor(a); got != c.chatHTTP() {
		t.Error("未知线路必须回落默认聊天 client")
	}
}

// TestEgressDisabledIsByteIdentical 没有线路时行为与改造前完全一致。
func TestEgressDisabledIsByteIdentical(t *testing.T) {
	c := New()
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	if got := c.httpFor(a); got != c.HTTP {
		t.Error("未配置 egress 时普通请求必须走 c.HTTP")
	}
	if got := c.chatHTTPFor(a); got != c.chatHTTP() {
		t.Error("未配置 egress 时聊天请求必须走 chatHTTP()")
	}
	// nil 账号（部分诊断路径会传 nil）同样不得 panic。
	if got := c.httpFor(nil); got != c.HTTP {
		t.Error("nil 账号必须回落默认链路")
	}
}

// TestSetResponseHeaderTimeoutCoversEveryTransport 首字节上限必须铺到每条线路。
//
// 守的是一个静默失败：改造前装配层用 `up.ChatHTTP.Transport.(*http.Transport)`
// 直接赋值，引入线路后那句只改默认链路，线路上的上限会悄悄退回零值
// （= 不设上限），表现是"配了线路的部署聊天卡住不再被掐断"。
func TestSetResponseHeaderTimeoutCoversEveryTransport(t *testing.T) {
	c := New()
	trA := &http.Transport{}
	trB := &http.Transport{}
	c.Egress = map[string]*EgressClient{
		"a": {HTTP: &http.Client{Transport: trA}, Chat: &http.Client{Transport: trA}},
		"b": {HTTP: &http.Client{Transport: trB}, Chat: &http.Client{Transport: trB}},
	}
	c.SetResponseHeaderTimeout(42 * 1e9) // 42s

	base, ok := c.ChatHTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatal("默认聊天 Transport 类型不符")
	}
	if base.ResponseHeaderTimeout != 42*1e9 {
		t.Errorf("默认链路未被设置: %v", base.ResponseHeaderTimeout)
	}
	for name, tr := range map[string]*http.Transport{"a": trA, "b": trB} {
		if tr.ResponseHeaderTimeout != 42*1e9 {
			t.Errorf("线路 %s 的 ResponseHeaderTimeout=%v，期望 42s",
				name, tr.ResponseHeaderTimeout)
		}
	}
}

// mustHost 取出 URL 的 host:port。
func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("解析 URL 失败: %v", err)
	}
	return u.Host
}
