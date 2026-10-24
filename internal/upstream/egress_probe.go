package upstream

// egress_probe.go 出口自检：回答"这条线路现在到底是哪个 IP / 哪个地区"。
//
// # 为什么它是**按需**的，而不是启动时全量探一遍
//
// config 里 `probe_url` 的注释已经把代价写清楚了：给上游多一条固定心跳。
// 一个 8 账号 × 8 条线路的部署，启动即探测等于每次重启都朝 8 个第三方
// IP 服务发一轮请求 —— 那是**我们自己**制造的规律性流量。所以：
//
//	启动         不探测（只在校验阶段喊配置错误，见 boot.validateEgressBindings）
//	管理台按需   点「探活」才打，结果缓存 10 分钟
//	绑定后       不探测（绑定本身不依赖出口可用性，探活是解释性信息）
//
// # 为什么带单飞（single-flight）
//
// 管理台一次「全部探活」会对每条线路各打一次；而 10 分钟缓存过期后，
// 用户很可能连点几下。没有单飞时，同一条线路会被并发打 3 次同一次探测，
// 三个请求的结果还可能不一致（代理池按会话粘性的产品尤其明显）——
// 页面上就会看到数字跳来跳去。单飞保证"同一时刻最多一个在飞"，其余等它。
import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// defaultProbeTTL 探活结果的缓存时长。
//
// 10 分钟是"够用来回答此刻是哪条出口"与"不给第三方制造心跳"的折中：
// 出口 IP 的粘性通常以分钟到小时计，更长会开始骗人，更短则等于心跳。
const defaultProbeTTL = 10 * time.Minute

// probeBodyLimit 探活响应的读取上限。IP 自检端点的响应都是几十字节，
// 4 KB 已经足够；上限的意义是防止一个配错的 URL 把整页 HTML 读进内存。
const probeBodyLimit = 4 << 10

// ProbeResult 一次出口自检的结果。
type ProbeResult struct {
	// IP 出口 IP（解析不出来时为空）。
	IP string `json:"ip,omitempty"`
	// Country 出口地区（端点给了才有；只有 IP 的端点留空）。
	Country string `json:"country,omitempty"`
	// LatencyMS 往返耗时。
	LatencyMS int64 `json:"latency_ms"`
	// At 取得这个结果的时刻（缓存命中时是**当初探测的时刻**，不是现在）。
	At time.Time `json:"at"`
	// Err 失败原因（空 = 成功）。
	Err string `json:"error,omitempty"`
}

// OK 是否成功。
func (r ProbeResult) OK() bool { return r.Err == "" }

// ProbeEgress 用**指定线路的 client** 请求 probeURL。
//
// hc 为 nil 或 probeURL 为空时直接返回带原因的结果 —— 不 panic、不吞掉原因：
// 管理台要把"为什么没探"如实显示出来，否则用户会以为线路坏了。
func ProbeEgress(ctx context.Context, hc *http.Client, probeURL string) ProbeResult {
	now := time.Now()
	if strings.TrimSpace(probeURL) == "" {
		return ProbeResult{At: now, Err: "该线路没有配置 probe_url（不探测）"}
	}
	if hc == nil {
		return ProbeResult{At: now, Err: "该线路没有可用的 HTTP client（未装配）"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		return ProbeResult{At: now, Err: "probe_url 非法: " + err.Error()}
	}
	// 探活不该被上游的 UA 伪装牵连：这是打给第三方 IP 服务的请求，
	// 用最小 UA 反而更像"正常工具"。
	req.Header.Set("User-Agent", "curl/8.0")
	start := time.Now()
	resp, err := hc.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return ProbeResult{At: now, LatencyMS: latency, Err: "请求失败: " + err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, probeBodyLimit))
	if resp.StatusCode/100 != 2 {
		return ProbeResult{At: now, LatencyMS: latency,
			Err: fmt.Sprintf("端点返回 %s: %s", resp.Status, snippetOf(body))}
	}
	out := parseProbeBody(body)
	out.LatencyMS = latency
	out.At = now
	if out.IP == "" {
		out.Err = "响应里读不出出口 IP（把 probe_url 指向一个返回纯 IP 或 {\"ip\":\"…\"} 的端点）"
	}
	return out
}

// parseProbeBody 解析常见的 IP 自检响应。
//
// 覆盖三种真实形态（用户随手填的端点就落在这三种里）：
//
//	{"ip":"1.2.3.4","country":"CN"}   ipify / ip-api 的 JSON
//	1.2.3.4                           ifconfig.me / icanhazip 的纯文本
//	ip=1.2.3.4                        cloudflare cdn-cgi/trace 的多行 key=value
func parseProbeBody(body []byte) ProbeResult {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ProbeResult{}
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(trimmed), &doc); err == nil {
		out := ProbeResult{}
		for _, k := range []string{"ip", "query", "origin"} {
			if v, ok := doc[k].(string); ok && looksLikeIP(v) {
				out.IP = v
				break
			}
		}
		for _, k := range []string{"country", "country_code", "code"} {
			if v, ok := doc[k].(string); ok && v != "" && len(v) <= 8 {
				out.Country = v
				break
			}
		}
		if out.IP != "" || out.Country != "" {
			return out
		}
	}
	// key=value 多行（cdn-cgi/trace）
	out := ProbeResult{}
	for _, line := range strings.Split(trimmed, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "ip":
			if looksLikeIP(v) {
				out.IP = v
			}
		case "loc":
			if v != "" {
				out.Country = v
			}
		}
	}
	if out.IP != "" || out.Country != "" {
		return out
	}
	// 纯文本 IP
	if first := strings.Fields(trimmed); len(first) > 0 && looksLikeIP(first[0]) {
		return ProbeResult{IP: first[0]}
	}
	return ProbeResult{}
}

// looksLikeIP 极简判定（不引入 net.ParseIP 的全部形态：端点给的通常是 v4/v6 字面量）。
func looksLikeIP(s string) bool {
	if s == "" || len(s) > 45 {
		return false
	}
	digits, dots, colons := 0, 0, 0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case r == '.':
			dots++
		case r == ':':
			colons++
		case r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
			// v6 的十六进制段
		default:
			return false
		}
	}
	return digits > 0 && (dots > 0 || colons > 0)
}

func snippetOf(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	if s == "" {
		return "(空响应体)"
	}
	return s
}

// EgressProber 带缓存与单飞的探活器。
//
// 键是线路 id（不是 URL）：同一条线路换了 probe_url 也还是"这条线"，
// 缓存维度与用户看到的概念一致。
type EgressProber struct {
	ttl  time.Duration
	now  func() time.Time
	mu   sync.Mutex
	data map[string]ProbeResult
	// calls 记录在飞的探测：同 id 的后来者等同一个结果。
	calls map[string]*probeCall
}

type probeCall struct {
	done chan struct{}
	res  ProbeResult
}

// NewEgressProber 构造。ttl<=0 用默认 10 分钟。
func NewEgressProber(ttl time.Duration) *EgressProber {
	if ttl <= 0 {
		ttl = defaultProbeTTL
	}
	return &EgressProber{ttl: ttl, now: time.Now, data: map[string]ProbeResult{}, calls: map[string]*probeCall{}}
}

// SetClock 测试注入时间源。
func (p *EgressProber) SetClock(fn func() time.Time) {
	if fn != nil {
		p.now = fn
	}
}

// Cached 只读缓存（不触发探测）。force=false 且缓存新鲜时直接用缓存。
func (p *EgressProber) Cached(id string) (ProbeResult, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.data[id]
	if !ok {
		return ProbeResult{}, false
	}
	if p.now().Sub(r.At) > p.ttl {
		return ProbeResult{}, false
	}
	return r, true
}

// Get 取一条线路的探活结果。
//
// force=true 时忽略缓存直接探（管理台「重新探活」按钮）。
// 缓存新鲜时**不打网络**；同一 id 并发进来时只有一个真的在飞。
func (p *EgressProber) Get(ctx context.Context, id string, hc *http.Client, probeURL string, force bool) ProbeResult {
	if !force {
		if r, ok := p.Cached(id); ok {
			return r
		}
	}
	p.mu.Lock()
	if call, ok := p.calls[id]; ok {
		p.mu.Unlock()
		select {
		case <-call.done:
			return call.res
		case <-ctx.Done():
			return ProbeResult{At: p.now(), Err: "等待探活结果超时: " + ctx.Err().Error()}
		}
	}
	call := &probeCall{done: make(chan struct{})}
	p.calls[id] = call
	p.mu.Unlock()

	res := ProbeEgress(ctx, hc, probeURL)
	// 失败的探测也缓存：否则一条坏线路会被每次刷页面重试一遍，
	// 而"坏"通常不会在 10 分钟内自己好。
	call.res = res
	p.mu.Lock()
	p.data[id] = res
	delete(p.calls, id)
	p.mu.Unlock()
	close(call.done)
	return res
}

// Invalidate 丢掉一条线路的缓存（保存配置后调用：URL 可能变了）。
func (p *EgressProber) Invalidate(id string) {
	p.mu.Lock()
	delete(p.data, id)
	p.mu.Unlock()
}

// InvalidateAll 全部作废。
func (p *EgressProber) InvalidateAll() {
	p.mu.Lock()
	p.data = map[string]ProbeResult{}
	p.mu.Unlock()
}
