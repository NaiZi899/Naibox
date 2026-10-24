package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestParseProbeBody 三种真实端点形态都要能读出来。
//
// 用户随手填的 probe_url 落在这三种里：ipify 的 JSON、ifconfig.me 的纯文本、
// cloudflare cdn-cgi/trace 的 key=value。少支持一种，那条线路就会显示
// "读不出出口 IP"，看起来像线路坏了。
func TestParseProbeBody(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantIP   string
		wantCtry string
	}{
		{"ipify JSON", `{"ip":"1.2.3.4"}`, "1.2.3.4", ""},
		{"ip-api JSON", `{"query":"5.6.7.8","country":"Germany","countryCode":"DE"}`, "5.6.7.8", "Germany"},
		{"纯文本", "  9.9.9.9\n", "9.9.9.9", ""},
		{"cdn-cgi/trace", "fl=123\nh=www.cloudflare.com\nip=203.0.113.7\nts=1\nloc=CN\n", "203.0.113.7", "CN"},
		{"IPv6 纯文本", "2001:db8::1\n", "2001:db8::1", ""},
		{"空响应", "", "", ""},
		{"HTML 错误页", "<html><body>blocked</body></html>", "", ""},
	}
	for _, c := range cases {
		got := parseProbeBody([]byte(c.body))
		if got.IP != c.wantIP || got.Country != c.wantCtry {
			t.Errorf("%s: 得到 ip=%q country=%q，want ip=%q country=%q",
				c.name, got.IP, got.Country, c.wantIP, c.wantCtry)
		}
	}
}

// TestProbeEgressNoProbeURL 没配 probe_url 时如实说"不探测"，而不是报成"线路坏了"。
func TestProbeEgressNoProbeURL(t *testing.T) {
	res := ProbeEgress(context.Background(), http.DefaultClient, "")
	if res.OK() {
		t.Fatal("没配 probe_url 不该报成功")
	}
	if !contains(res.Err, "probe_url") {
		t.Fatalf("错误信息应当指向 probe_url，实际 %q", res.Err)
	}
}

// TestProbeEgressHTTP 真打一次：状态码/延迟/IP 都要对上。
func TestProbeEgressHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ip":"198.51.100.9","country":"CN"}`))
	}))
	defer srv.Close()
	res := ProbeEgress(context.Background(), srv.Client(), srv.URL)
	if !res.OK() {
		t.Fatalf("探活应当成功: %+v", res)
	}
	if res.IP != "198.51.100.9" || res.Country != "CN" {
		t.Fatalf("解析结果不符: %+v", res)
	}
	if res.At.IsZero() {
		t.Fatal("必须带时刻（缓存与页面都靠它判断新旧）")
	}
}

// TestProbeEgressNon2xx 非 2xx 要把状态码与片段带出来（排障第一现场）。
func TestProbeEgressNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("blocked by provider"))
	}))
	defer srv.Close()
	res := ProbeEgress(context.Background(), srv.Client(), srv.URL)
	if res.OK() {
		t.Fatal("403 不该报成功")
	}
	if !contains(res.Err, "403") || !contains(res.Err, "blocked") {
		t.Fatalf("错误信息应当带状态码与片段，实际 %q", res.Err)
	}
}

// TestEgressProberCacheAndSingleFlight 缓存与单飞：
//   - 并发 3 个请求只打 1 次网络
//   - 缓存期内再问仍是 1 次
//   - force=true 才再打一次
func TestEgressProberCacheAndSingleFlight(t *testing.T) {
	var hits int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		<-release // 卡住，让并发请求真正重叠
		w.Write([]byte(`{"ip":"10.0.0.1"}`))
	}))
	defer srv.Close()

	p := NewEgressProber(time.Minute)
	ctx := context.Background()
	var wg sync.WaitGroup
	results := make([]ProbeResult, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = p.Get(ctx, "line-a", srv.Client(), srv.URL, false)
		}(i)
	}
	// 给三个 goroutine 一点时间全部进入 Get（否则可能只有一个在飞）
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("并发 3 次探活应当只打 1 次网络（单飞），实际 %d 次", got)
	}
	for i, r := range results {
		if r.IP != "10.0.0.1" {
			t.Fatalf("第 %d 个结果不对: %+v", i, r)
		}
	}

	// 缓存命中：不打网络
	if _, ok := p.Cached("line-a"); !ok {
		t.Fatal("刚探过就该有缓存")
	}
	p.Get(ctx, "line-a", srv.Client(), srv.URL, false)
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("缓存期内不该再打网络，实际 %d 次", got)
	}

	// force 强制重探
	release2 := make(chan struct{})
	close(release2)
	p.Get(ctx, "line-a", srv.Client(), srv.URL, true)
	// 注意：handler 仍在等 release（已关闭，所以立刻返回）
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("force=true 应当重探一次，实际 %d 次", got)
	}
}

// TestEgressProberInvalidate 保存配置后缓存必须作废（否则页面继续显示旧 IP）。
func TestEgressProberInvalidate(t *testing.T) {
	p := NewEgressProber(time.Minute)
	p.mu.Lock()
	p.data["x"] = ProbeResult{IP: "1.1.1.1", At: time.Now()}
	p.mu.Unlock()
	if _, ok := p.Cached("x"); !ok {
		t.Fatal("前提不成立：缓存没写进去")
	}
	p.Invalidate("x")
	if _, ok := p.Cached("x"); ok {
		t.Fatal("Invalidate 之后不该还命中")
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
