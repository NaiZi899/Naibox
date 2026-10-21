// provider.go —— 把一条 Endpoint 变成 gateway.Provider。
//
// # 为什么“端点”与“Provider”是两个类型
//
// Spec/Endpoint 是**配置**：用户写的、可热改的、要落盘的。
// Provider 是**运行期对象**：注册进 Registry、挂在账号池上、暴露管理端点。
// 混成一个类型会让“改配置”与“改活着的连接池”纠缠在一起 —— 配置里改一行
// 超时，已经在跑的流式请求就该被掐掉吗？分开之后这个问题不存在：
// 新配置生成新 Provider，旧 Provider 只服务完手上的请求。
//
// # 为什么 Provider 自己还要存一份 ep
//
// Chat 每次都从凭证的 secret 里取回**调用方选中的那个**端点（池子里每条账号
// 都是自己的副本），这样热改配置不会悄悄改掉老账号的行为。
// 但 Models() 与 Probe() 这两条路径**不需要账号**：模型目录是端点级事实，
// 管理台的探测更是“这台机器能不能连上”。它们用构造时那份 ep 当兜底，
// 于是没有凭证（或凭证 secret 形态不对）时仍然可用 —— 契约测试与
// /v1/models 的合并都走这条路。
package custom

import (
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/NaiZi899/Naibox/internal/gateway"
)

// 各条路径的时限。集中在这里是为了让“哪个超时管哪段”可一屏读完：
//
//	dial 10s / TLS 握手 10s      —— 建连阶段（防半开连接把请求槽占死）
//	ResponseHeaderTimeout        —— 只到响应头为止，**不含**响应体
//	models 10s                   —— 拉一次目录是启动期/管理动作，慢就等于失败
//	probe 5s                     —— 面板上一个按钮，等超过 5 秒用户会以为页面卡了
const (
	dialTimeout         = 10 * time.Second
	tlsHandshakeTimeout = 10 * time.Second
	modelsTimeout       = 10 * time.Second
	probeTimeout        = 5 * time.Second
	// modelsTTL 模型目录缓存时长：目录变化是“人改配置/换模型”级别的事件，
	// 10 分钟足够新；而每次 /v1/models 都打上游会让面板每次刷新都多一次往返。
	modelsTTL = 10 * time.Minute
	// probeTTL 探测结果缓存时长。面板的“刷新”按钮会被连点，
	// 15 秒内复用上一次结果可以避免把上游当成靶子打。
	probeTTL = 15 * time.Second
	// defaultResponseHeaderTimeout 端点没带 Timeout 时的兜底（与 spec.go 的默认 120s 对齐）。
	defaultResponseHeaderTimeout = 120 * time.Second
	// maxRedirects 同主机重定向上限（与 net/http 的默认值一致，显式写出来便于读者对账）。
	maxRedirects = 10
)

// 编译期断言：这六个接口是核心发现本上游的**唯一**通道。
//
// 为什么要写成编译期断言而不是靠测试：接口改名/方法签名漂移时，
// 测试可能因为“根本没跑到那条断言”而全绿，编译期断言则直接编译失败。
// 尤其是 DisplayNameExt / CredentialLoader / CredentialSecretLoader 这类
// 可选扩展点 —— 它们不参与 Provider 接口匹配，写错了不会报错，只会静默失效
// （见 gateway/contract.go 里 verifyExtensionsDiscoverable 记的那次事故）。
var (
	_ gateway.Provider               = (*Provider)(nil)
	_ gateway.AdminExt               = (*Provider)(nil)
	_ gateway.CredentialLoader       = (*Provider)(nil)
	_ gateway.CredentialSecretLoader = (*Provider)(nil)
	_ gateway.ErrorClassifier        = (*Provider)(nil)
	_ gateway.DisplayNameExt         = (*Provider)(nil)
)

// modelsCache 模型目录缓存。
type modelsCache struct {
	list []gateway.ModelInfo
	at   time.Time
	// errLogged 保证“拉取失败、沿用旧目录”这件事只记一次日志。
	// 不设它的话，端点长期不可达会把日志刷满（每个 /v1/models 一次）。
	errLogged bool
}

// probeCache 探测结果缓存。
type probeCache struct {
	res ProbeResult
	at  time.Time
}

// Provider 一条自定义上游。
type Provider struct {
	ep     Endpoint
	log    *log.Logger
	client *http.Client

	// mu 保护 cache，同时充当模型拉取的**串行化闸门**：并发进来的 N 个
	// /v1/models 只会有一个真的发请求，其余等锁后直接读缓存。
	mu    sync.Mutex
	cache modelsCache

	// probeMu 是**另一把**锁：探测由管理台发起、模型目录由出口发起，
	// 共用一把锁会让“点一下探测”卡住整条 /v1/models（最多 5 秒）。
	probeMu    sync.Mutex
	probeCache probeCache
}

// New 构造一个 Provider。
//
// logger 允许为 nil（测试里就是 nil）：内部会换成一个丢弃写入的 logger，
// 这样调用方不必在每个测试里建一个 log.New。
func New(ep Endpoint, logger *log.Logger) *Provider {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &Provider{
		ep:     ep,
		log:    logger,
		client: newHTTPClient(ep),
	}
}

// logf 写一条日志；Logger 缺失时静默丢弃（永不 panic，契约要求）。
func (p *Provider) logf(format string, args ...any) {
	if p == nil || p.log == nil {
		return
	}
	p.log.Printf(format, args...)
}

// newHTTPClient 构造该端点的 HTTP 客户端。
//
// # 为什么必须自己拼 Transport 而不是用 http.DefaultClient
//
//  1. 每个端点的 TLS 校验策略可能不同（自签端点要 InsecureSkipVerify），
//     共用一个 Transport 迟早串味；
//  2. 要按端点覆盖 ResponseHeaderTimeout（配置里的 timeout_seconds）。
//
// # 为什么**不设** client.Timeout
//
// client.Timeout 覆盖“从发出请求到读完响应体”的**全过程**。自定义上游是
// 流式端点，一次长回复跑十几分钟是正常的 —— 设了总体超时就会把长流从中间切断，
// 表现为“前端答到一半突然没了”，而且日志里看不出是超时（连接是被本地掐的）。
// 真正需要防的是“连不上/不回头”这两种，对应 DialContext 与
// ResponseHeaderTimeout：它们只管到响应头为止，之后交给调用方的 ctx 管。
//
// # CheckRedirect 为什么必须拦跨主机
//
// 鉴权头（Authorization / X-Api-Key …）是端点级密钥。若上游（或链路上的
// 中间人）回一个 302 到别的主机，Go 的 http.Client 会**自动跟随**。
// 跨主机的 Authorization 在 Go 里会被剥掉一部分，但自定义头（auth_header 可配成
// 任意名字）不在它的敏感头名单里 —— 那等于把密钥送给了第三方主机。
// 这里直接拒绝：宁可报错，也不发一个把密钥送到别处的请求。
func newHTTPClient(ep Endpoint) *http.Client {
	var tr *http.Transport
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = base.Clone()
	} else {
		// DefaultTransport 被替换过（少见）：退回到一个最小可用配置，
		// 而不是冒险断言失败后 panic。
		tr = &http.Transport{Proxy: http.ProxyFromEnvironment}
	}

	// Clone 出来的 TLSClientConfig 可能是 nil，也可能是共享指针；
	// 两种都先拿到自己的副本再改，避免改到别的 Transport。
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		tr.TLSClientConfig = tr.TLSClientConfig.Clone()
	}
	tr.TLSClientConfig.InsecureSkipVerify = ep.InsecureSkipVerify // #nosec G402 —— 自签端点是显式配置项

	// 建连超时：没有它，被丢包的端点会让请求一直挂在 SYN 阶段，
	// 账号池的在途额度被无声占满。
	tr.DialContext = (&net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: 30 * time.Second,
	}).DialContext
	tr.TLSHandshakeTimeout = tlsHandshakeTimeout

	headerTimeout := ep.Timeout
	if headerTimeout <= 0 {
		headerTimeout = defaultResponseHeaderTimeout
	}
	tr.ResponseHeaderTimeout = headerTimeout

	return &http.Client{
		Transport:     tr,
		CheckRedirect: sameHostRedirect,
	}
}

// sameHostRedirect 只允许同主机内的重定向。
//
// 判据是“目标主机与**原始**请求主机相同”（Host 含端口，逐字比较、忽略大小写）。
// 用原始请求而不是上一次跳转，是为了防住 a → b → a 这种绕一圈把头带出去的链路：
// 第一次 a → b 就已经被拒了。
func sameHostRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		// 返回错误而不是 http.ErrUseLastResponse：后者会把最后一个 3xx 当成
		// **正常响应**交给上层，出口层只判 >=400，于是 302 的正文被当作
		// 200 的 SSE 流写给客户端。重定向环路必须显式失败。
		return fmt.Errorf("重定向超过 %d 跳（起点 %s）", maxRedirects, via[0].URL.Host)
	}
	if len(via) == 0 {
		return nil
	}
	orig := via[0].URL
	if !strings.EqualFold(orig.Host, req.URL.Host) {
		return &redirectError{from: orig.Host, to: req.URL.Host}
	}
	return nil
}

// redirectError 跨主机重定向的错误。
//
// 单独一个类型是为了让上层（以及排查的人）一眼看出“这不是网络故障，
// 是上游想把我引到别的主机去”。文案里刻意不带 URL 的路径与查询串。
type redirectError struct {
	from string
	to   string
}

func (e *redirectError) Error() string {
	return "拒绝跨主机重定向（" + e.from + " → " + e.to + "）：避免把鉴权头带到另一个主机"
}

// ID 上游标识（同时是模型前缀）。
func (p *Provider) ID() string { return p.ep.ID }

// DisplayName 给人看的名字。
//
// 配置里没填 name 时 Normalize 已经把 ID 抄了过去，所以这里直接返回即可，
// 不需要再判空 —— 前端看到的永远是“配置里的名字”。
func (p *Provider) DisplayName() string { return p.ep.Name }

// Caps 能力声明。
//
// 自定义端点是一个“薄”上游：只有对话与模型目录。
// 不声明 CapQuotaProbe / CapCheckin / CapGrowth / CapWelfare / CapTravel 是
// **刻意的**：这些能力都需要上游专有的账号生命周期端点，OpenAI 兼容协议里
// 没有它们。声明了却没有实现，契约测试会直接判不合格（这是好事）。
func (p *Provider) Caps() gateway.Capability {
	return gateway.CapChat | gateway.CapModels
}

// endpointOf 取出这次调用应该用的端点。
//
// 生产路径上池子总会把 *Endpoint 塞进 secret（见 credentials.go）；
// 契约测试与管理台的“无凭证”路径则没有 secret，这里回落到构造时那份。
// 两种情况都是**确定的**行为，不存在“用哪个都行”的模糊地带 —— 后者与
// 构造 Provider 时传进来的端点逐字节相同。
func (p *Provider) endpointOf(cred gateway.Credential) Endpoint {
	if ep, ok := cred.Secret.(*Endpoint); ok && ep != nil {
		return *ep
	}
	return p.ep
}

// clone 复制一份端点，用于分给各个合成账号。
//
// 为什么要深拷贝 map/slice：Headers 与 StaticModels 是引用类型，
// 直接把同一个 map 分给 8 个账号，将来任何一处“就地补一个头”都会同时改到
// 全部账号，且故障现象无法复现（取决于哪个账号先跑）。
func (e Endpoint) clone() Endpoint {
	out := e
	if e.Headers != nil {
		out.Headers = make(map[string]string, len(e.Headers))
		for k, v := range e.Headers {
			out.Headers[k] = v
		}
	}
	if e.StaticModels != nil {
		out.StaticModels = append([]string(nil), e.StaticModels...)
	}
	return out
}

// shardCount 需要几个合成账号（配置已经过校验，这里只兜 0 值）。
func (p *Provider) shardCount() int {
	n := p.ep.Shards
	if n < 1 {
		n = 1
	}
	return n
}
