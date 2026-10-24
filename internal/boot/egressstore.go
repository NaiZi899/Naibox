// egressstore.go 管理台「出口线路」的宿主实现。
//
// # 为什么探活**不复用**装配好的 Transport
//
// 装配期把每条线路编译成 `upstream.Client.Egress[id]` 的两个 http.Client。
// 那个表是**只读**的（运行期替换 map 会与并发请求构成数据竞争），
// 所以保存新配置后必须重启才生效。
//
// 但探活不该被这个限制拖住：它只关心"这条 proxy URL 现在能不能出去、
// 出口是哪个 IP"，完全可以现场搭一个一次性 Transport 去试。
// 于是用户在**重启之前**就能验证配置对不对 —— 这正是探活最大价值所在。
//
// 数据竞争在这里不存在：一次性 client 只被一次探测使用，用完即弃。
package boot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/NaiZi899/Naibox/internal/admin"
	"github.com/NaiZi899/Naibox/internal/auth"
	"github.com/NaiZi899/Naibox/internal/pool"
	"github.com/NaiZi899/Naibox/internal/upstream"
)

// egressStore 线路表 + 探活缓存 + 绑定。
type egressStore struct {
	path   string
	pool   *pool.Pool
	prober *upstream.EgressProber
	logf   func(string, ...any)

	mu  sync.Mutex
	cfg *Config
}

// newEgressStore 构造。探活缓存 10 分钟（见 upstream.EgressProber）。
func newEgressStore(cfgPath string, cfg *Config, p *pool.Pool, logf func(string, ...any)) *egressStore {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &egressStore{
		path:   cfgPath,
		pool:   p,
		prober: upstream.NewEgressProber(0),
		logf:   logf,
		cfg:    cfg,
	}
}

// current 取当前生效配置（Save 后会替换，因此每次都要重新取）。
func (s *egressStore) current() *Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// bindCounts 每条线上的账号数（运行时以池为准，不读磁盘）。
func (s *egressStore) bindCounts(excludeUID string) map[string]int {
	auths := s.allAuths()
	return CountEgressBindings(auths, excludeUID)
}

// allAuths 从池里取全部凭证对象。
//
// 为什么不直接扫磁盘：池里那份是**当前生效**的（可能刚被管理台改过绑定），
// 而磁盘扫描会漏掉内存里尚未 Flush 的改动 —— 那会让"上限已满"的判断
// 在两次点击之间跳来跳去。
func (s *egressStore) allAuths() []*auth.Auth {
	if s.pool == nil {
		return nil
	}
	list := s.pool.List()
	out := make([]*auth.Auth, 0, len(list))
	for _, st := range list {
		if a := s.pool.AuthByUID(st.UID); a != nil {
			out = append(out, a)
		}
	}
	return out
}

// List 线路表 + 账号投影（探活只读缓存，不打网络）。
func (s *egressStore) List(ctx context.Context) ([]admin.EgressView, []admin.EgressAccountView) {
	cfg := s.current()
	if cfg == nil {
		return nil, nil
	}
	counts := s.bindCounts("")
	lines := make([]admin.EgressView, 0, len(cfg.Egress))
	for _, e := range cfg.Egress {
		v := admin.EgressView{
			ID: e.ID, Proxy: redactProxy(e.Proxy), Realm: e.Realm, Region: e.Region,
			MaxAccounts: e.MaxAccounts, ProbeURL: e.ProbeURL, Bound: counts[e.ID],
		}
		if r, ok := s.prober.Cached(e.ID); ok {
			v.Probe = probeView(r)
		}
		lines = append(lines, v)
	}
	var accounts []admin.EgressAccountView
	for _, a := range s.allAuths() {
		v := admin.EgressAccountView{UID: a.UID, Nickname: a.Nickname, Channel: a.Channel, Egress: a.EgressID}
		if s.pool != nil {
			for _, st := range s.pool.List() {
				if st.UID == a.UID {
					v.Provider = st.Provider
					break
				}
			}
		}
		accounts = append(accounts, v)
	}
	return lines, accounts
}

// Probe 探活。ids 为空 = 全部线路；force=true 忽略缓存。
func (s *egressStore) Probe(ctx context.Context, ids []string, force bool) ([]admin.EgressView, error) {
	cfg := s.current()
	if cfg == nil {
		return nil, errors.New("配置尚未加载")
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[strings.TrimSpace(id)] = true
	}
	counts := s.bindCounts("")
	var out []admin.EgressView
	var firstErr error
	for _, e := range cfg.Egress {
		if len(want) > 0 && !want[e.ID] {
			continue
		}
		hc, err := probeHTTPClient(e.Proxy)
		if err != nil {
			// 一条线路的 proxy URL 配坏了不该让整页探活失败：
			// 把它如实记在这条线路上，其余线路照常探。
			if firstErr == nil {
				firstErr = fmt.Errorf("线路 %s: %w", e.ID, err)
			}
			out = append(out, admin.EgressView{
				ID: e.ID, Proxy: redactProxy(e.Proxy), Realm: e.Realm, Region: e.Region,
				MaxAccounts: e.MaxAccounts, ProbeURL: e.ProbeURL, Bound: counts[e.ID],
				Probe: &admin.EgressProbeView{Error: err.Error(), At: time.Now().Format(time.RFC3339)},
			})
			continue
		}
		res := s.prober.Get(ctx, e.ID, hc, e.ProbeURL, force)
		out = append(out, admin.EgressView{
			ID: e.ID, Proxy: redactProxy(e.Proxy), Realm: e.Realm, Region: e.Region,
			MaxAccounts: e.MaxAccounts, ProbeURL: e.ProbeURL, Bound: counts[e.ID],
			Probe: probeView(res),
		})
	}
	// 全部线路都探不了时才把错误抛上去（此时页面上什么有用信息都没有）。
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// Save 保存 egress 整表：校验 → 写回 config.json → 标记需重启。
func (s *egressStore) Save(raw json.RawMessage) (applied, needRestart []string, err error) {
	lines, err := decodeEgressPatch(raw)
	if err != nil {
		return nil, nil, err
	}
	// 空 proxy = **沿用同 id 旧线路的 proxy**。
	//
	// # 为什么必须有这条（否则这个功能是坏的）
	//
	// HTTP 接口下发的 proxy 是**脱敏值**（socks5://***@host）。若让前端把
	// 展示值原样提交回来，`***` 就会被当成真的用户名写进 config.json ——
	// 那是一条不可逆的破坏（下次请求直接 407）。而 PUT 又是**整表覆盖**，
	// 用户只想改一条线的 region 时，其余线路只能提交它们手里的值 ——
	// 也就是脱敏值。
	//
	// 所以约定与「自定义上游的 api_key」完全一致：空串 = 沿用旧值。
	// 新增的线路没有旧值可继承，proxy 仍为空 → normalizeEgress 会明确拒绝。
	cfgNow := s.current()
	if cfgNow != nil && len(cfgNow.Egress) > 0 {
		prev := make(map[string]string, len(cfgNow.Egress))
		for _, e := range cfgNow.Egress {
			prev[e.ID] = e.Proxy
		}
		for i := range lines {
			if strings.TrimSpace(lines[i].Proxy) == "" {
				lines[i].Proxy = prev[lines[i].ID]
			}
		}
	}
	// 用一份临时 Config 跑同一条 normalize（判据只有一份）。
	tmp := Config{Egress: lines}
	if err := tmp.normalizeEgress(); err != nil {
		return nil, nil, err
	}
	// 绑定一致性在**保存时**就要提示（装配期只喊日志，那时已经晚了）：
	// 例如用户删掉了一条还有账号绑着的线路。
	cfg := s.current()
	var auths []*auth.Auth
	if cfg != nil {
		auths = s.allAuths()
	}
	byID := map[string]bool{}
	for _, e := range tmp.Egress {
		byID[e.ID] = true
	}
	orphan := 0
	for _, a := range auths {
		if a != nil && a.EgressID != "" && !byID[a.EgressID] {
			orphan++
		}
	}

	payload, err := json.Marshal(tmp.Egress)
	if err != nil {
		return nil, nil, err
	}
	if err := writeConfigKey(s.path, "egress", payload); err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	if s.cfg != nil {
		s.cfg.Egress = tmp.Egress
	}
	s.mu.Unlock()
	// URL 可能变了：缓存全废（否则页面会继续显示旧 IP）。
	s.prober.InvalidateAll()

	if orphan > 0 {
		s.logf("⚠ egress 已保存：有 %d 个账号绑定的线路已不存在（它们会回落共用出口）", orphan)
	}
	s.logf("egress 已保存：%d 条线路（重启网关后才对出站生效；探活可先验证）", len(tmp.Egress))
	return []string{"egress"}, []string{"egress"}, nil
}

// Bind 给账号绑线路（空串 = 解绑）。
func (s *egressStore) Bind(uid, egressID string) error {
	if s.pool == nil {
		return errors.New("账号池未接线")
	}
	a := s.pool.AuthByUID(uid)
	if a == nil {
		return fmt.Errorf("账号 %s 不在池里", uid)
	}
	cfg := s.current()
	var lines []EgressConfig
	if cfg != nil {
		lines = cfg.Egress
	}
	// 判据只有一份：与装配期同一个 CheckEgressBinding。
	if err := CheckEgressBinding(lines, a.Channel, egressID, s.bindCounts(uid)[egressID]); err != nil {
		return classOf(err)
	}
	if egressID == "" {
		// 解绑也要落盘：否则重启后又回到那条线上。
	}
	a.Lock()
	a.EgressID = egressID
	a.Unlock()
	if err := a.SaveAtomic(); err != nil {
		return fmt.Errorf("写入凭证失败（uid=%s）: %w", uid, err)
	}
	if !s.pool.SetEgress(uid, egressID) {
		// 池里没有这个 uid（并发删号）：文件已经落盘，如实说明即可。
		return fmt.Errorf("账号 %s 已不在池里（凭证已落盘）", uid)
	}
	if egressID == "" {
		s.logf("账号 %s 解绑出站线路（回到共用出口）", uid)
	} else {
		s.logf("账号 %s 绑定出站线路 %q", uid, egressID)
	}
	return nil
}

// classOf 把 boot 的判据错误翻成 admin 的哨兵（HTTP 状态码映射在 admin 层）。
func classOf(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrEgressFull):
		return fmt.Errorf("%w：%s", admin.ErrEgressFull, err.Error())
	case errors.Is(err, ErrEgressUnknown):
		return fmt.Errorf("%w：%s", admin.ErrEgressUnknown, err.Error())
	case errors.Is(err, ErrEgressRealmMismatch):
		return fmt.Errorf("%w：%s", admin.ErrEgressRealmMismatch, err.Error())
	default:
		return err
	}
}

// probeHTTPClient 现场搭一个只用于探活的 client。
//
// 超时取 15s：探活是"人在等答案"的动作，比它慢的线路本身就说明有问题。
func probeHTTPClient(proxyURL string) (*http.Client, error) {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("proxy URL 解析失败: %w", err)
	}
	if u.Host == "" {
		return nil, errors.New("proxy URL 缺少主机名")
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyURL(u),
		MaxIdleConns:          2,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
	}
	return &http.Client{Timeout: 15 * time.Second, Transport: tr}, nil
}

// probeView 结果 → 下发形态。
func probeView(r upstream.ProbeResult) *admin.EgressProbeView {
	at := ""
	if !r.At.IsZero() {
		at = r.At.Format(time.RFC3339)
	}
	return &admin.EgressProbeView{
		OK: r.OK(), IP: r.IP, Country: r.Country, LatencyMS: r.LatencyMS, At: at, Error: r.Err,
	}
}

// decodeEgressPatch 接受两种请求体：裸数组，或 {"lines":[...]}。
//
// 两种都收是因为"整表保存"这件事在别处（custom_upstreams）用的是对象包一层。
// 与其让前端猜，不如两边都认 —— 解析成本为零，误用成本不低。
func decodeEgressPatch(raw json.RawMessage) ([]EgressConfig, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var lines []EgressConfig
		if err := json.Unmarshal(raw, &lines); err != nil {
			return nil, fmt.Errorf("线路表不是合法数组: %w", err)
		}
		return lines, nil
	}
	var obj struct {
		Lines []EgressConfig `json:"lines"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("线路表不是合法 JSON: %w", err)
	}
	return obj.Lines, nil
}
