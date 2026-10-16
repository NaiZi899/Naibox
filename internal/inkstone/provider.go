// provider.go 把书生·端砚（Intern InkStone）适配为 gateway.Provider。
//
// # 本包在解耦链里的位置
//
// 与 loomy 同属"轻上游"：凭证是一个无 TTL 的凭据（Loomy=session，本包=API Key），
// 没有 refresh token，没有每日签到/福利（书生端砚的"领墨点"是每月一次性，不走每日）。
// 因此**刻意不实现**：CredentialRefresher / JobExt / DailyActionExt。
//
// 实现的扩展点：
//
//	Provider          铁 4 方法（Chat/Models/ID/Caps）
//	CredentialLoader(+Secret)   从 auths/inkstone 扫描 API Key 并入 pool
//	QuotaExt          按账号查墨点余额（需 user_token，缺省回落未知）
//	ErrorClassifier   把 invalid_api_key 等归类为会话失效/冷却
//	AccountColumnsExt 自报账号池列（provider/nickname/uid/quota/status）
package inkstone

import (
	"sync"

	"github.com/NaiZi899/omni2api/internal/gateway"
)

// providerID 上游标识（模型前缀 "inkstone/…"）。
const providerID = "inkstone"

// ProviderID 导出供装配层使用。
const ProviderID = providerID

// Config Provider 的装配参数。
type Config struct {
	// AuthDir 凭证目录（auths/inkstone）。装配层传入。
	AuthDir string
	// InferBase / MgmtBase 端点覆盖（一般用默认值）。
	InferBase string
	MgmtBase  string
}

// Provider 实现 gateway.Provider（及若干可选扩展点）。
type Provider struct {
	cfg Config

	// credentialSrc 从账号池取某 uid 的凭证（装配层注入，同 lobsterai 模式）。
	credentialSrc func(uid string) (gateway.Credential, bool)

	mu sync.Mutex
}

// New 构造 Provider（装配层或测试用）。
func New(cfg Config) *Provider {
	p := &Provider{cfg: cfg}
	if p.cfg.InferBase == "" {
		p.cfg.InferBase = DefaultInferBase
	}
	if p.cfg.MgmtBase == "" {
		p.cfg.MgmtBase = DefaultMgmtBase
	}
	return p
}

// NewWithConfig 与 New 同；为与其它上游命名一致保留。
func NewWithConfig(cfg Config) *Provider { return New(cfg) }

// NewProvider 契约测试用的无依赖构造（零值 Config，指向生产端点，无副作用可多次调用）。
func NewProvider() gateway.Provider { return New(Config{}) }

// SetCredentialSource 注入"按 uid 取凭证"的回调（装配层从 pool.SecretOf 接）。
func (p *Provider) SetCredentialSource(fn func(uid string) (gateway.Credential, bool)) {
	if fn == nil {
		return
	}
	p.mu.Lock()
	p.credentialSrc = fn
	p.mu.Unlock()
}

// credential 取某 uid 的凭证（含 Auth）。
func (p *Provider) credential(uid string) (*Auth, bool) {
	p.mu.Lock()
	fn := p.credentialSrc
	p.mu.Unlock()
	if fn == nil {
		// 未接线：回落到目录扫描（测试/独立部署时仍可用）。
		list, err := LoadDir(p.cfg.AuthDir)
		if err == nil {
			for _, a := range list {
				if a.UID == uid {
					return a, true
				}
			}
		}
		return nil, false
	}
	cred, ok := fn(uid)
	if !ok {
		return nil, false
	}
	a, ok := cred.Secret.(*Auth)
	if !ok || a == nil {
		return nil, false
	}
	return a, true
}

// ID 实现 gateway.Provider。
func (p *Provider) ID() string { return providerID }

// Caps 声明能力：对话 + 动态模型目录。
// ⚠ 不声明 CapQuotaProbe：它要求实现 AdminExt（管理端点），
// 而书生端砚的额度走 gateway.QuotaExt（RefreshQuota），不需要主动探测端点。
func (p *Provider) Caps() gateway.Capability {
	return gateway.CapChat | gateway.CapModels
}

// compile-time: Provider 必须满足 gateway.Provider。
var _ gateway.Provider = (*Provider)(nil)

// 其余方法（Chat / Models / 扩展点）见独立文件：chat.go / quota_ext.go / errorclassifier.go / loader.go。
