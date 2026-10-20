// provider.go 把 基元律动（TokenRhythm）适配为 gateway.Provider。
//
// # 本包在解耦链里的位置
//
// 与 loomy / inkstone 同属"轻上游"：凭证是一把无 TTL 的 key，
// 没有 refresh token、没有每日签到玩法。
// 因此**刻意不实现**：CredentialRefresher / JobExt / DailyActionExt /
// DailyCheckinExt / QuotaExt（理由逐条见 credential.go 的包注释）。
//
// 实现的扩展点：
//
//	Provider                      铁 4 方法（ID/Caps/Chat/Models）
//	AuthDirExt                    凭证目录（auths/jiyuan）
//	CredentialLoader(+Secret)     扫描凭证入池
//	AccountImportExt              批量粘贴 key（本上游唯一的"上号"入口）
//	DisplayNameExt                控制台分组中文名
//	ErrorClassifier               余额不足/失效 key 的分类
package jiyuan

import (
	"strings"
	"sync"

	"github.com/NaiZi899/Naibox/internal/gateway"
)

// providerID 上游标识。它同时决定模型目录的前缀：
// 核心在合并目录时统一加 `<providerID>/`，所以本上游的模型对外
// 呈现为 `jiyuan/deepseek-flash`、`jiyuan/glm-5.3` 这样的形态。
const providerID = "jiyuan"

// ProviderID 导出供装配层使用。
const ProviderID = providerID

// Config Provider 的装配参数。
type Config struct {
	// AuthDir 凭证目录（auths/jiyuan）。装配层传入。
	AuthDir string
	// BaseURL 端点覆盖（一般留空，用 DefaultBaseURL）。
	BaseURL string
	// LiveModels 是否允许 Models() 实时拉取上游目录（默认 false）。
	//
	// # 为什么做成显式开关而不是"永远实时拉 + 失败兜底"
	//
	// `Models()` 会被**契约测试**调用（`verifyModelsNonEmpty`），而契约
	// 用的是伪造凭证 —— 永远实时拉会让 `go test ./internal/jiyuan/`
	// 每次都打一次真实网络。单元测试不该依赖外网：离线/CI 里会变成
	// 随机失败，而"随机失败"最终会让所有人开始忽略红灯。
	//
	// 装配层（cmd/server）置 true —— 生产要的是"上游上了新模型能立刻看到"。
	LiveModels bool
	// MgmtBase 管理面（`/api/*`，余额所在）基址覆盖。空 = 官方域名。
	//
	// # 为什么与 BaseURL 分开
	//
	// 很多反代只转发 `/v1`（推理面），不转发 `/api`（管理面）。那种部署下
	// 聊天走代理、余额必须直连官方域名。基址只有一个时只能二选一：
	//
	//	用代理 → 余额 404/401，界面恒显示 `—`，且**没有任何报错**
	//	用官方 → 聊天绕过代理，代理等于白配
	//
	// 所以拆成两个。形态校验在装配层做（`http/https` + 主机名），
	// 因为那里才知道"启动参数非法该不该拦"。空串走 DefaultMgmtBase。
	MgmtBase string
}

// Provider 实现 gateway.Provider（及若干可选扩展点）。
type Provider struct {
	cfg Config

	// credentialSrc 从账号池取某 uid 的凭证（装配层注入，同 inkstone 模式）。
	credentialSrc func(uid string) (gateway.Credential, bool)

	mu sync.Mutex
}

// New 构造 Provider（装配层或测试用）。
func New(cfg Config) *Provider {
	p := &Provider{cfg: cfg}
	if strings.TrimSpace(p.cfg.BaseURL) == "" {
		p.cfg.BaseURL = DefaultBaseURL
	}
	// 管理面基址：**只有非空时才设置**。
	//
	// ⚠ 这里不能无条件调 setMgmtBase(p.cfg.MgmtBase)：空串在该函数里会
	// 回落成 DefaultMgmtBase，而 New 常被"已经指向本地上游"的调用方使用
	// （测试就是），无条件设置会把它刚设好的地址冲掉 —— 表现是所有管理面
	// 请求静默打到官方域名，用例报"凭证无效"而不是它真正要验的东西。
	//
	// 空 = 沿用当前值（进程启动时即 DefaultMgmtBase），与"留空用官方域名"
	// 的配置语义一致。
	if strings.TrimSpace(p.cfg.MgmtBase) != "" {
		setMgmtBase(p.cfg.MgmtBase)
	}
	return p
}

// NewWithConfig 与 New 同；为与其它上游命名一致保留。
func NewWithConfig(cfg Config) *Provider { return New(cfg) }

// NewProvider 契约测试用的无依赖构造。
//
// 零值 Config、指向生产端点、无副作用、可多次调用 ——
// 契约测试会拿它跑 ID/Caps/Chat/Models 的行为断言。
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
//
// 未接线时回落到目录扫描：这样"独立部署 / 测试"与"网关内"两条路径
// 都能取到 key，不会因为装配层漏接一行而整个上游不可用。
func (p *Provider) credential(uid string) (*Auth, bool) {
	p.mu.Lock()
	fn := p.credentialSrc
	p.mu.Unlock()

	if fn == nil {
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

// Caps 声明能力：对话 + 动态模型目录 + 额度探测 + 批量导入。
//
// # 为什么现在声明 CapQuotaProbe（改造前刻意不声明）
//
// 当时的理由是"本上游没有可用的额度接口，余额只认 session cookie"。
// 那个理由在 quota_ext.go 落地后已经不成立：额度确实读得到，只是入口
// 与推理面不同。
//
// ⚠ 但声明它有一条**硬前提**：契约要求同时实现 QuotaExt 与 AdminExt
// （管理端点 + 非空路由）。只声明不实现会被 `RunProviderContract` 挡下 ——
// inkstone 第一版就是这么被抓的。本包两者都有：
//
//	quota_ext.go    QuotaExt（读余额）
//	adminroute.go   AdminExt（POST /admin/jiyuan/quota-probe）
//
// ⚠ 不声明 CapCheckin：基元律动没有每日签到玩法（不像 workbuddy/loomy）。
func (p *Provider) Caps() gateway.Capability {
	return gateway.CapChat | gateway.CapModels | gateway.CapQuotaProbe | gateway.CapImport
}

// compile-time: Provider 必须满足 gateway.Provider。
var _ gateway.Provider = (*Provider)(nil)

// 其余方法见独立文件：
//
//	chat.go            Chat
//	models.go          Models
//	import.go          ImportCredentials
//	loader.go          AuthDir / LoadCredentials(+Secrets) / DisplayName
//	errorclassifier.go Classify
