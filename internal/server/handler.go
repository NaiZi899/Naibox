// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + 转发到上游。
//
// # 本包**不依赖任何具体上游**（架构判据 3）
//
// 它认识的是：
//
//	gateway.Provider      契约（经 cfg.Provider / ProviderRouter 注入）
//	DefaultUpstream       默认上游客户端（窄接口，见其注释）
//	DefaultClassifier     默认上游的错误分类器（函数值）
//	wire.*                中立线协议工具（SSE 读写、usage 抽取、限流解析）
//
// 它**不认识** workbuddy / codearts / … 中的任何一个包。
// 这条边界由 arch_test.go 的 TestCoreDoesNotDependOnUpstreams 强制。
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/NaiZi899/Naibox/internal/apikey"
	"github.com/NaiZi899/Naibox/internal/auth"
	"github.com/NaiZi899/Naibox/internal/gateway"
	"github.com/NaiZi899/Naibox/internal/logbuf"
	"github.com/NaiZi899/Naibox/internal/pool"
	"github.com/NaiZi899/Naibox/internal/prompt"
	"github.com/NaiZi899/Naibox/internal/session"
	"github.com/NaiZi899/Naibox/internal/wire"
)

// Config handler 依赖。
type Config struct {
	Pool     *pool.Pool
	Upstream DefaultUpstream
	APIKey   string // 空 = 不鉴权
	// DefaultClassifier 默认上游的错误分类器（装配层注入）。
	//
	// # 为什么由装配层注入而不是核心自己调
	//
	// 分类判据（`hardMarkers` / `sessionDeadMarkers` / `12153`）是
	// **workbuddy 的上游知识**。核心包不得认识任何具体上游
	//（arch_test 判据 3 强制），所以核心只收一个函数值。
	//
	// 单上游部署（`Provider == nil`）与"该上游没实现 ErrorClassifier"
	// 两种情形都回落到它 —— 语义上就是"用默认上游的判据"，
	// 与改造前逐字节一致。
	//
	// nil 时回落 `gateway.DefaultErrorKind`（只按状态码）—— 那是**明确的降级**。
	DefaultClassifier func(status int, body string) gateway.ErrorKind
	// APIKeys 多 API key 管理（对标 new-api 令牌体系）：config.api_key 是
	// 管理钥匙（不限额），普通 key 走 apikey.Store 的额度/限速/用量。
	// nil = 不启用多 key 管理（旧行为）。
	APIKeys   *apikey.Store
	MaxRotate int // 单请求最多换号次数，默认 3

	// MaxBodyMB 出站前允许的最大请求体（MiB），<=0 回落 8。
	//
	// 见 cmd/server/config.go 的 Server.MaxBodyMB 注释：这是把
	// "静默截断"换成"明确 413"的开关。默认值与改造前硬编码的 8<<20 一致，
	// 因此既有部署的行为只在"超限"这一条路径上发生变化。
	MaxBodyMB int

	// StreamMax 流式请求的**总时长上限**；<=0 回落 defaultStreamMaxSeconds。
	//
	// 锚点是"请求进入 handler 的时刻"，且一次请求内的换号重试**共享同一份预算**
	// （见 newStreamWatchdog）—— 否则 MaxRotate 次重试会把上限乘 3，
	// 正是"挂到天荒地老"的形态。
	//
	// ≤0 表示"不设闸门"这个语义刻意**不提供**：不设就是改造前的挂死形态，
	// 没有哪个部署需要它。要放宽就把值调大。
	StreamMax time.Duration

	// StreamFirstByte 上游建连后等待首个内容帧的上限；<=0 回落 defaultStreamFirstByteSeconds。
	//
	// 与 StreamMax 的分工见 chatwatchdog.go 的文件头：一个管总预算，
	// 一个管"连上了但一个字都不吐"。已有产出即永久放行，所以慢而正常的
	// 长回答不会被它掐掉。
	StreamFirstByte time.Duration

	// PromptGate 内容拦截降级状态机（可为 nil）。
	//
	// # 为什么它必须与出站客户端**共用同一个实例**
	//
	// 出站循环在这里 Trigger()，出站客户端在每次发请求前读 Active()。
	// 两个不同的实例会让机制完全失效：handler 触发了降级，
	// 客户端却还在用正常提示词 → 每次重试都先撞一次 400。
	//
	// nil 表示本部署未接提示词体系：内容拦截退化为"只换号不罚号"
	// （保守但正确 —— 不会误伤账号，只是会多跑几次往返）。
	PromptGate *prompt.Gate
	// Session 会话粘性路由器（可选；nil = 关闭粘性，纯 Pick 轮换）。
	Session *session.Router
	// StickyCount 返回当前粘性会话绑定数（供 /status）；nil 时报告 0。
	StickyCount func() int
	// RedisMode 观测字段（"upstash" / "noop"），供 /status 透出。
	RedisMode    string
	SoftCooldown time.Duration // 429 冷却，默认 60s
	// RefreshSkew token 提前刷新窗口，默认 10m
	RefreshSkew time.Duration

	// ServiceName 本实例的身份标识；空 = 回落包级默认常量 ServiceName。
	//
	// # 为什么要可配置（T8 / 评审 F5）
	//
	// 原先它是编译期常量，于是控制台顶栏那个"服务名"是**前端硬编码**的
	// （3 处），后端改名前端不跟随 —— 那就是 B8。
	// 多实例部署时同名的另一个后果是宿主认不出"这是不是我管的那一个"。
	ServiceName string

	// NextResetAt 返回"额度耗尽的账号下次可用的时刻"，**按上游分派**。
	//
	// # 为什么做成可注入的回调（而不是出口层自己算）
	//
	// 早先出口层直接调 pool.CooldownUntilTomorrow4AM —— 那是把
	// **workbuddy 的策略**（次日 04:00，等 09:00/21:00 的签到恢复）
	// 写死在核心。codearts 没有签到，次日 4 点对它毫无意义。
	//
	// 现在由上游提供：workbuddy 给"次日 04:00"，codearts 给"配额窗口重置时刻"。
	// nil 或 ok=false 时回落到一个通用的保守值（1 小时），保证没有 Provider 也能跑。
	//
	// # ⚠ 为什么参数是 providerID 而不是没有参数（P2 修复）
	//
	// 旧签名是 `func() time.Time`。它**没有参数**，于是签名本身就排除了
	// "按上游分派"的可能 —— 无论注释怎么写，实现永远只能返回**同一个**上游的答案。
	// 装配层注入的是 `wb.NextResetAt`（workbuddy 的次日 04:00），于是
	// **任何上游**的账号在 ErrHardCredit 时都被冷到 workbuddy 的次日 04:00：
	//
	//	codearts 明明已恢复却还冷到次日凌晨 → 白白闲置近 24h
	//	或按 04:00 解冻而实际未恢复        → 又撞一次硬错误
	//
	// providerID 由 applyErrorPolicy / nextResetAt 从账号池的归属反查得到
	// （见 Pool.ProviderOf），与 Chat / Credential / RefreshCredential /
	// RefreshSkew 四条路径**同一形状**：出口层只按 ID 问，不做判断。
	//
	// # ok 的语义
	//
	//	ok=true   该上游上报了自己的恢复排程 → 用它的时刻
	//	ok=false  该上游**没有**这个信息     → 核心回落通用保守值
	//
	// 与 RefreshSkew 的 ok=false 语义逐字一致（见 ProviderRouter.RefreshSkew），
	// 也与 gateway.ResetPolicyExt 的返回值同形。
	NextResetAt func(providerID string) (until time.Time, ok bool)

	// OwnedBy 填进 /v1/models 的 `owned_by` 字段。
	//
	// 早先这里硬编码 "workbuddy" —— 上游名字写死在核心出口层。
	// 现在由 cmd/server 注入（单上游时就是该上游的 ID）。
	// 空值时回落到 "local"，**不假装知道**是哪个上游。
	//
	// 多上游之后它是**默认上游**的 owned_by；其余上游按各自的 Provider.ID()
	// 填（见 Provider 字段与 modelEntries）。
	OwnedBy string

	// Provider 多上游路由的接缝（可选；nil = 单上游模式，行为与改造前一致）。
	//
	// # 为什么做成接口而不是直接持有 gateway.Registry
	//
	// server 已经 import gateway（用 SplitModel），再持有 Registry 也不成环。
	// 但这里只声明**它实际需要的四个动作**，原因是：
	//   - 上游目录是**可能失败**的（无凭证/无账号），而 Registry 只有 Get，
	//     拿不到"这个上游现在能不能出目录"的答案；
	//   - 全部可缺省：nil 时整条路径退化成改造前的单上游行为（本字段是
	//     向后兼容的关键 —— 既有测试与既有部署都不注入它）。
	//
	// 装配层（cmd/server）用 gateway.Registry + 各 Provider 实现它。
	Provider ProviderRouter

	// DefaultProvider 默认上游标识：裸模型名（"auto"）与无前缀请求走它。
	//
	// 空串与 Provider 为 nil 两种情形下的语义都是"没有多上游概念"，
	// 此时 /v1/models 与选号都保持单上游行为。
	DefaultProvider string

	// DisableChatTools 关闭「上游工具自动注入」。
	//
	// 默认 false（开启）—— 用户要求「我要生图的话，它自动调用生图模型」，
	// 而且希望**客户端零改动**（普通聊天客户端不懂工具协议）。
	//
	// 关掉它的场景：某个客户端虽然不带 tools，但对响应形状极敏感
	// （例如自己解析 message 的每个字段）—— 那种客户端遇到工具循环
	// 产生的响应会不适应。留这个开关让运维能局部退回改造前的行为。
	//
	// ⚠ 它只影响**注入**；客户端自己带 tools 时本就不接管，与此开关无关。
	DisableChatTools bool
	// Admin 管理台子树（挂在 /admin/，由 internal/admin 提供）。nil = 不注册该子树。
	Admin http.Handler
	// ModelCatalog 供管理台取模型目录快照（成本系数用）。nil = 本实例不提供该能力。
	//
	// 为什么是函数而不是把 *Handler 传过去：admin 已经在用 `ResetModelsCache func()`
	// 这种「由 server 注入闭包」的方向；反向传 *Handler 会让 admin → server 形成
	// 编译期依赖，而 server 反过来 import admin 是为了挂 /admin/ 子树 —— 一反向就是
	// import cycle。传函数既避开环，也让 admin 不必知道缓存住在哪个包。
	ModelCatalog func() *gateway.ModelCatalog
	// ModelCatalogState 只读地报告模型目录缓存状态（ok/stale/unavailable）。
	// 语义见包级 ModelCatalogState —— 它绝不触发上游请求。nil = 未接线。
	ModelCatalogState func() CatalogState
}

// ServiceName 网关身份标识。经 /healthz 响应体 service 字段与 X-Service 头同时透出：
// 宿主（如 workbuddy-switch 托管网关子进程）探测同端口的旧服务/其他服务时，对方即使
// 返回 2xx 也不带本标识，宿主据此可识别"假成功"。
const ServiceName = "omni2api"

// Handler 主路由。
type Handler struct {
	cfg Config
	mux *http.ServeMux
	// apiKey 当前生效的管理钥匙（config api_key，支持运行时轮换）。
	// 与 cfg.APIKey 解耦：轮换写回 config 后调用 SetAPIKey 立即生效，无需重启。
	apiKeyMu sync.RWMutex
	apiKey   string
}

// SetAPIKey 运行时更新管理钥匙（管理台「轮换管理密钥」用）。
func (h *Handler) SetAPIKey(k string) {
	h.apiKeyMu.Lock()
	h.apiKey = k
	h.apiKeyMu.Unlock()
}

// currentAPIKey 返回当前生效的管理钥匙。
func (h *Handler) currentAPIKey() string {
	h.apiKeyMu.RLock()
	defer h.apiKeyMu.RUnlock()
	return h.apiKey
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 60 * time.Second
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	if cfg.MaxBodyMB <= 0 {
		cfg.MaxBodyMB = defaultMaxBodyMB
	}
	// 流式两道闸门的缺省。**必须在这里兜底而不是指望装配层**：
	// 漏设的后果是回到"没有总时长"的挂死形态，那是本次要修的病。
	if cfg.StreamMax <= 0 {
		cfg.StreamMax = defaultStreamMaxSeconds * time.Second
	}
	if cfg.StreamFirstByte <= 0 {
		cfg.StreamFirstByte = defaultStreamFirstByteSeconds * time.Second
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux(), apiKey: cfg.APIKey}
	registerCatalogHost(h) // 让包级 ModelCatalog/ModelCatalogState 能找到本实例的缓存
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	// 生图出口（OpenAI Images API 形状）。
	//
	// 与 chat 同一条鉴权（同一个 Bearer）：出口层多一条路径不该多一套凭据
	// —— 调用方已经把 api_key 配在 chat 上了，生图再要一个会让人以为
	// 这是两个服务。额度/限速也复用同一套 apikey 判定。
	h.mux.HandleFunc("POST /v1/images/generations", h.withAuth(h.imagesGenerations))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	// 单数别名：部分客户端/探针按 /v1/model 探测，与 /v1/models 同源同响应。
	h.mux.HandleFunc("GET /v1/model", h.withAuth(h.models))
	// 内嵌本地控制台（同源，无需 CORS）。
	h.mux.HandleFunc("GET /ui", h.ui)
	// 带尾斜杠的 /ui/ 也接受（用户习惯打 `http://host:port/ui/?token=...`）：
	// 302 到无斜杠的规范路径，查询参数（如 ?token=）原样保留。
	h.mux.HandleFunc("GET /ui/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui?"+r.URL.RawQuery, http.StatusFound)
	})
	h.mux.HandleFunc("GET /favicon.ico", h.favicon)
	h.mux.HandleFunc("GET /favicon.svg", h.favicon)
	h.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui", http.StatusFound)
	})
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	// 管理台子树：自身做本机校验与鉴权，这里只做挂载（不带 withAuth，否则浏览器拿不到）。
	if cfg.Admin != nil {
		h.mux.Handle("/admin/", cfg.Admin)
	}
	return h
}

// ResetModelsCache 清空动态模型缓存，让下一次 /v1/models 强制回源上游。
// 正缓存 1h / 负缓存 5min 都清掉——用户点「刷新模型」的意图是立刻看到最新目录。
//
// 模型目录（成本系数）缓存一并清空：两者是同一个上游账号在同一次「刷新」里的期望产物，
// 只清一个会出现「模型列表变了但系数还是旧的」这种半刷新状态。
//
// # ⚠ 三份缓存必须一起清（漏了会"刷新不彻底")
//
// /v1/models 的目录来自**三条互不相通的路径**，各自带一份缓存：
//
//	默认上游        dynamicModelsCache        ← 本函数第一个块
//	其余上游        providerModelsCache       ← resetProviderModelsCaches()
//	成本系数目录    modelCatalogCache         ← 本函数第三个块
//
// 漏掉第二条时的表现**不报错**：默认上游与倍率立刻更新，而 jiyuan/loomy/trae
// 的目录仍走 1h 缓存 —— 用户看到"刷新了但那些上游的模型没变"，原因却是
// 三份缓存里只清了另外两份。
func ResetModelsCache() {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()

	// 其余上游的按上游目录缓存（见 models_endpoint_cache.go）。
	resetProviderModelsCaches()

	modelCatalogCache.Lock()
	modelCatalogCache.entries = nil // 全上游一起清：刷新意图是"立刻看到最新目录"
	modelCatalogCache.Unlock()
}

// ChatLogRing 返回请求日志环形缓冲（/admin/logs 的数据源）。
func ChatLogRing() *logbuf.Ring { return chatLogRing }

// chatLogRing 进程内请求日志缓冲：与 stdout 表格日志同源，容量 2000 条。
var chatLogRing = logbuf.New(logbuf.DefaultCapacity)

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// apikeyCtxKey 请求上下文里存放"本次鉴权命中的 API key id"的键。
type apikeyCtxKeyType struct{}

var apikeyCtxKey apikeyCtxKeyType

// validBearer 报告请求是否携带正确凭据；未配置任何鉴权时恒真。
//
// 鉴权优先级（对标 new-api 的令牌体系，config.api_key 是管理钥匙）：
//
//	config.api_key 非空且匹配       → 管理 key，通过（不限额不限速）
//	apikey.Store 非空且 Bearer 命中 → 普通 key，通过（额度/限速校验见下）
//	两者都未配置                     → 不鉴权（旧行为）
//
// 普通 key 的错误映射：未知/禁用 → 401；额度用尽 → 402；超速 → 429。
// 抽成独立方法供 /ui 的非本机分支复用（那里要出 401 + 纯文本，而不是 OpenAI 错误信封）。
func (h *Handler) validBearer(r *http.Request) bool {
	return h.validCredential(bearerOf(r))
}

// validCredential 报告凭据字符串是否有效（不依赖请求上下文）。
//
// 与 validBearer 同一套判定，只是入参是裸字符串 —— 供 /ui 的
// `?token=` 查询参数分支复用（远程浏览器无法自定义请求头，
// 查询参数是它唯一能携带凭据的地方）。
func (h *Handler) validCredential(cred string) bool {
	ak := h.currentAPIKey()
	if ak == "" && h.cfg.APIKeys == nil {
		return true
	}
	if ak != "" && cred == ak {
		return true
	}
	if h.cfg.APIKeys != nil && cred != "" {
		_, err := h.cfg.APIKeys.Validate(cred)
		// 额度/限速错误由 withAuth 映射 402/429；这里只回答"凭证是否有效"。
		return err == nil
	}
	return false
}

// bearerOf 取出请求 Authorization 头里的 Bearer 凭据（无则空串）。
func bearerOf(r *http.Request) string {
	if authz := r.Header.Get("Authorization"); strings.HasPrefix(authz, "Bearer ") {
		return strings.TrimPrefix(authz, "Bearer ")
	}
	return ""
}

// authResult 鉴权结果：命中的 apikey id（空 = 管理 key / 无多 key）与错误。
type authResult struct {
	apikeyID string
	err      error // nil = 通过；否则为 apikey 包的哨兵错误
}

// authorize 完整鉴权判定（含普通 key 的额度/限速）。
func (h *Handler) authorize(r *http.Request) authResult {
	bearer := ""
	if authz := r.Header.Get("Authorization"); strings.HasPrefix(authz, "Bearer ") {
		bearer = strings.TrimPrefix(authz, "Bearer ")
	}
	ak := h.currentAPIKey()
	// 未配置任何鉴权 → 恒通过（旧行为）。
	if ak == "" && h.cfg.APIKeys == nil {
		return authResult{}
	}
	// 管理 key 优先。
	if ak != "" && bearer == ak {
		return authResult{}
	}
	// 普通 key。
	if h.cfg.APIKeys != nil && bearer != "" {
		id, err := h.cfg.APIKeys.Validate(bearer)
		if err == nil {
			return authResult{apikeyID: id}
		}
		return authResult{err: err}
	}
	return authResult{err: apikey.ErrUnknown}
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ar := h.authorize(r)
		if ar.err != nil {
			switch {
			case errors.Is(ar.err, apikey.ErrQuota):
				writeOpenAIError(w, http.StatusPaymentRequired, "insufficient_quota", "API key 额度已用完")
			case errors.Is(ar.err, apikey.ErrRateLimit):
				writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "请求过于频繁（触发该 API key 的每分钟上限）")
			default:
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			}
			return
		}
		if ar.apikeyID != "" {
			r = r.WithContext(context.WithValue(r.Context(), apikeyCtxKey, ar.apikeyID))
		}
		// panic → 可诊断的 500（而不是静默断连）。见 harden.go 的文件头。
		// 放在 withAuth 里而不是各个注册点：加新端点时不会漏。
		h.panicRecovery(next)(w, r)
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	total, healthy, _, _, _ := h.cfg.Pool.CountsDetailed()
	// 用 ServableNow 判定：healthy>0 但全占满在途时 chat 会 503，探活必须同口径，
	// 否则负载均衡器会把流量持续打进无法受理的实例。
	status := http.StatusOK
	if !h.cfg.Pool.ServableNow() {
		status = http.StatusServiceUnavailable
	}
	// 恒无鉴权（负载均衡/编排探活只需 2xx/503 语义），身份靠 service 字段 + X-Service 头双保险。
	svc := h.serviceName()
	w.Header().Set("X-Service", svc)
	writeJSON(w, status, map[string]any{
		"healthy": healthy,
		"total":   total,
		"service": svc,
	})
}

// serviceName 返回本实例的身份标识。
//
// 优先用配置里显式给的值（见 cmd/server.Config.ServiceName），
// 空则回落编译期默认值 —— 老配置无需任何改动即可继续工作。
func (h *Handler) serviceName() string {
	if h.cfg.ServiceName != "" {
		return h.cfg.ServiceName
	}
	return ServiceName
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := h.cfg.Pool.CountsDetailed()
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}
	redisMode := h.cfg.RedisMode
	if redisMode == "" {
		redisMode = "noop"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":        h.cfg.Pool.List(),
		"total":           total,
		"healthy":         healthy,
		"cooling":         cooling,
		"disabled":        disabled,
		"in_flight_full":  inFlightFull,
		"sticky_sessions": sticky,
		"redis_mode":      redisMode,
	})
}

const defaultMaxBodyMB = 8

// 流式出站的两道时间闸门缺省值（见 chatwatchdog.go 的文件头）。
//
// 放在 Config 的零值兜底里（与 MaxBodyMB 同一手法），而不是包在 const 里
// 让调用方各自记得设：漏设的后果是"闸门不装"——也就是改造前的形态，
// 而这个缺省的**升级方向必须更安全**（380s 挂死 → 15min 上限）。
//
// ⚠ 这两条只管**流式**。非流式路径本来就被 http.Client 的
// upstream.timeout_seconds 约束着，不需要第二套总时长。
const (
	// defaultStreamMaxSeconds 流式总时长上限：15 分钟。
	//
	// 为什么是这个量级：它是**天花板**不是目标（正常回答几秒到几分钟）。
	// 取 15min 是为了不误杀"长思考 + 长正文"的正常慢回答，
	// 同时把实测的 380s / 325s 这种挂死挡在 15 分钟以内（比 6 分钟更强，见下）。
	//
	// 为什么不是 6 分钟（= 实测最长 380s 之上留一点）：与 idle_timeout_seconds
	// 的 300s 挨得太近，两个数会被误当成同一个东西调；15min 明确是"绝对上限"，
	// 空闲检测那 300s 才是"卡住就断"的日常闸门。两者职责不重叠：一个管总预算，
	// 一个管"两次读之间"。
	defaultStreamMaxSeconds = 900
	// defaultStreamFirstByteSeconds 首内容帧看门狗：120 秒。
	//
	// 与 upstream.header_timeout_seconds 的默认值同为 120s，但**管的阶段不同**：
	// 那个是"响应头还没回来"，这个是"响应头回来了、连接已建立，却一直不产出内容"。
	// 实测的 380s/325s/120s 挂死都落在这一段（status 已是 200，流却是空的）。
	defaultStreamFirstByteSeconds = 120
)

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	// MaxBytesReader 而不是 LimitReader：超限时 Read 返回 *http.MaxBytesError，
	// 我们据此回 413；LimitReader 只会静默截断，把"太大"伪装成"JSON 畸形"。
	//
	// 传 w 是为了让 net/http 在超限时标记连接不可复用（避免残留未读字节
	// 污染同一 keep-alive 连接上的下一个请求）。
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(h.cfg.MaxBodyMB)<<20))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			// 报错文案必须给出**可执行的两条出路**：缩小请求 / 调大配置。
			// 只说"too large"会让用户去猜上限是多少、在哪改。
			writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large",
				fmt.Sprintf("请求体超过上限 %d MiB（可在 config.json 的 server.max_body_mb 调整）；"+
					"多图/长上下文会话请缩小 messages 或提高该上限", h.cfg.MaxBodyMB))
			return
		}
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)

	// 选号要按**请求的模型**取额度（见 pool.PickForModel），
	// 并按 **provider 前缀**限定上游（见 pool.PickFor）。
	//
	// 为什么在这里做而不是只在池内：只有出口层知道客户端要哪个模型。
	// 按模型额度的上游（codearts）里，一个账号可能对 gpt-5.5 零额度、
	// 对别的模型额度很高 —— 用标量选号会优先选中它，然后白跑一次。
	//
	// 剥掉 provider 前缀（"workbuddy/auto" → "auto"）：额度表按上游模型名建，
	// 前缀是网关的路由记号，不属于上游模型名。
	//
	// 未知前缀立刻回 400：那是客户端写错了，让它马上知道，
	// 而不是把 "codearts/GLM-5.2" 当模型名发给默认上游再收一个莫名的 400。
	reqProvider, reqModel, perr := h.providerFor(peek.Model)
	if perr != nil {
		writeOpenAIError(w, http.StatusBadRequest, "unknown_provider", perr.Error())
		return
	}
	// 出站请求体里必须写回**剥掉前缀**的模型名：前缀是网关的路由记号，
	// 上游不认识 "workbuddy/auto"（实测：带着前缀发过去，上游按未知模型拒绝，
	// 表现为一次白跑的 503 + 换号）。这一条与"按前缀选号"是一体两面 ——
	// 选号看前缀，发出去看裸名。
	//
	// 只在模型名真的变了时才重新编码：不带前缀的请求（既有客户端）
	// 保持**原始字节原样转发**，行为与改造前逐字节一致。
	outBody := body
	if hasPrefixIn(peek.Model) && reqModel != peek.Model {
		if rewritten, ok := rewriteModel(body, reqModel); ok {
			outBody = rewritten
		}
		// 重写失败（应当不可能：body 刚被 JSON 解析过）时保留原始 body ——
		// 宁可让上游按原样拒绝并给出它的错误，也不要在这里造一个假错误。
	}

	// 请求级统计：出口即打一行表格日志（任何路径都会走到）。
	st := newChatStat(time.Now(), body, peek.Stream)
	st.provider = reqProvider
	// API key 用量记录：命中普通 key 时在出口把 token/成败记给该 key。
	if kid, ok := r.Context().Value(apikeyCtxKey).(string); ok && kid != "" && h.cfg.APIKeys != nil {
		st.apikeyID = kid
		st.bump = func(toks int, ok bool) { h.cfg.APIKeys.BumpUsage(kid, int64(toks), ok) }
	}
	defer st.done()

	// ctx 供出站调用使用（Provider.Chat / RefreshCredential 都收 ctx）。
	//
	// # 为什么挂在 r.Context() 上
	//
	// 客户端断开时 r.Context() 被取消，Provider 的实现据此**立刻返回**而不是
	// 继续跑完一次上游往返 —— 这是契约要求（见 gateway 的
	// verifyChatRespectsCancel），对 codearts 尤其重要：它每个账号只有 3 个
	// 并发会话槽，前端断开后上游调用仍在跑会白占槽位。
	//
	// ⚠ 会不会把"客户端断开"变成一次账号失败的惩罚？不会 ——
	// 取消会走 Provider.Chat 返回的 error → chatVia 走 terr → 只换号
	// 不喂熔断（与网络抖动同一条路径）。
	//
	// # 客户端 IP 也走 ctx（而不是 Client 上的字段）
	//
	// 见 gateway/clientip.go：字段形态在并发下会串扰（A 请求写、B 请求读），
	// 表现为上游看到来源错乱的 IP，且只在并发下出现。
	// 这里**无条件**放入（即使 passthrough_ip 关闭）：放不放的成本是零，
	// 而"要不要用"由出站客户端读它自己的配置决定 ——
	// 判断留在唯一知道该配置的那一层。
	ctx := gateway.WithClientIP(r.Context(), gateway.ExtractClientIP(r))

	tried := map[string]bool{}
	var lastErr error

	// degradedTried 本请求是否已经用过降级提示词重试。
	//
	// 初值取"进入本请求时降级是否已激活"：若上一批请求已经把 Gate 触发过，
	// 那么客户端**本次发的就是 Degraded 提示词** —— 再拦就说明问题在用户内容，
	// 没有第三次可试。这样"每个请求最多因内容拦截重试一次"是精确成立的，
	// 而不是靠一个恒为 false 的局部变量（那会让每次请求都白白多试一轮）。
	degradedTried := h.cfg.PromptGate.Active()

	// 会话粘性：从请求体提取会话键并解析绑定号（找不到/无效则 stickyUID 为空，走普通轮换）。
	sessKey := ""
	stickyUID := ""
	if h.cfg.Session != nil {
		sessKey = session.ExtractKey(body)
		if sessKey != "" {
			if uid, ok := h.cfg.Session.Resolve(sessKey); ok {
				stickyUID = uid
			}
		}
	}

	// 在途租约：成功选中即占名额；函数出口（含成功 return 与 panic）统一释放。
	var heldUID string
	defer func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
	}()
	releaseHeld := func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
			heldUID = ""
		}
	}
	// fail 在轮转失败分支统一：释放租约 + 若失败号正是粘性号则解绑（下次请求重新分配）。
	fail := func(uid string) {
		releaseHeld()
		if stickyUID != "" && uid == stickyUID {
			h.cfg.Session.Unbind(sessKey)
			stickyUID = ""
		}
	}

	for i := 0; i < h.cfg.MaxRotate; i++ {
		// 选号：粘性号优先（已校验 **provider + health + 在途未满**），否则普通轮换。
		//
		// ⚠ 必须用 PickByUIDFor(reqProvider, ...) 而不是 PickByUID(...)：
		// 会话粘性跨上游时，粘性命中会把**别的上游的账号**塞进出站循环。
		// 改造后出站按"账号自己的上游"分派（见 chatVia），所以它**不会报错** ——
		// 只会让「请求 workbuddy 的模型却用了 codearts 的账号」这种路由错误
		// 静默发生，比一次失败难查得多。
		//
		// 不匹配时返回 nil → 走下面的解绑 + 回落普通轮换分支，
		// 按本次请求的 reqProvider 重新选号。粘性是优化不是契约，
		// 跨上游时放弃绑定是正确降级。
		var acct *auth.Auth
		if stickyUID != "" {
			acct = h.cfg.Pool.PickByUIDFor(reqProvider, stickyUID)
			if acct == nil {
				// 粘性号当前不可用（别的上游/冷却/占满）→ 解绑，本次回落普通轮换。
				h.cfg.Session.Unbind(sessKey)
				stickyUID = ""
			}
		}
		if acct == nil {
			// 按 (上游, 模型) 选号：只在该上游的账号里挑，
			// 绝不让 codearts 的请求落到 workbuddy 的账号上。
			// reqProvider 为空（单上游模式 / 裸模型名）时池子按默认上游解释。
			acct = h.cfg.Pool.PickFor(reqProvider, reqModel, tried)
		}
		if acct == nil {
			st.status = http.StatusServiceUnavailable
			break
		}
		st.uid = acct.UID
		tried[acct.UID] = true

		// 占用在途名额：Pick 已跳过满额账号，此处 CAS 兜底并发抢名额的竞态。
		if !h.cfg.Pool.Acquire(acct.UID) {
			// 若被抢的正是粘性号，立即解绑并回落普通轮换，避免下一轮仍撞同一个
			// 满载粘性号再浪费一次 PickByUIDFor 往返（语义与 fail()/PickByUIDFor-nil 的解绑一致）。
			if stickyUID != "" && acct.UID == stickyUID {
				h.cfg.Session.Unbind(sessKey)
				stickyUID = ""
			}
			continue // 最后一个名额被并发抢走 → 换号
		}
		heldUID = acct.UID

		// 该不该刷由**上游自己**回答（失败冷却换号）。
		//
		// ⚠ 这里刻意**不是** `acct.NeedsRefresh(h.cfg.RefreshSkew)`：
		// 核心的 10m 窗口对 codearts（STS 仅约 2h、自己的窗口 3m）是错的，
		// 且错的方向是"太早"—— 剩 8m 就被核心判为该刷，而 codearts 的
		// CredentialRefresher 内部没有 skew 检查，于是真的去消费那个
		// **一次性**的 refresh_token。判据必须与"怎么刷"同处一地，见
		// needsRefreshVia 的注释。
		if h.needsRefreshVia(reqProvider, acct) {
			if err := h.refreshCredential(ctx, reqProvider, acct); err != nil {
				lastErr = err
				var re *RefreshError
				if errors.As(err, &re) && re.SessionDead {
					// 与出站路径同口径：刷新时的 session dead 也走计数门控。
					// 刷新失败常常正是**竞态**（另一个并发请求刚消费了 refresh token），
					// 一次就禁用是这个路径上最典型的误杀来源。
					h.cfg.Pool.NoteSessionDead(acct.UID)
				} else {
					h.cfg.Pool.NoteError(acct.UID)
				}
				fail(acct.UID)
				continue
			}
		}

		// ── 工具循环（上游声明的工具，客户端零改动）──
		//
		// 客户端**不需要**懂工具调用：它照常发一句"帮我画只猫"，
		// 网关替它注入工具、执行工具、把结果回喂给模型收尾。
		//
		// ⚠ 作用域天然是"本上游的"：工具定义与执行都问**装配层**要
		// （`ChatTools(id)` / `ExecuteChatTool(id,…)`），而 id 是
		// 本次被路由到的那个上游。所以 loomy 的请求拿到生图+搜索，
		// workbuddy 的请求（没实现该扩展点）一个都拿不到 ——
		// 不需要在核心写任何 `if provider == "loomy"`。
		//
		// 客户端自己带了 tools 时**不接管**：那说明它懂协议、要自己驱动
		// 工具循环，网关再插一脚会两边打架（见 shouldRunToolLoop）。
		if ext, yes := h.shouldRunToolLoop(reqProvider, outBody); yes {
			// 流式与**非流式**走两条实现（见 chattool_stream.go 的文件头）：
			//
			//	流式    逐帧转发，只在真调我们的工具时才拦 —— 保住打字机效果
			//	非流式  内部跑完整轮再一次性返回（原本就无流式可言）
			//
			// ⚠ 第一版只有非流式那条，于是**每个** loomy 请求都被缓冲成
			// 一整块，用户看到"文字不是流式的了"——范围远大于生图的回归。
			var done bool
			// loopStats 接收工具循环读到的统计（TTFB/tokens/usage）。
			// ⚠ 这条路径绕过了 chatStatsReader（普通 chat 用它读数），
			// 所以必须自己把统计接回来 —— 否则日志的 TTFB / tok / tok/s
			// 三列全是空的（用户实测报过）。见 chatstat.go 的文件头。
			var loopStats *toolLoopStats
			var werr error
			if peek.Stream {
				done, loopStats, werr = h.runToolLoopStream(w, toolLoopParams{
					ext:        ext,
					providerID: reqProvider,
					uid:        acct.UID,
					body:       outBody,
					stream:     true,
					start:      st.start,
					ctx:        ctx,
				}, h.chatToolsOf(reqProvider))
			} else {
				done, loopStats, werr = h.runToolLoop(w, toolLoopParams{
					ext:        ext,
					providerID: reqProvider,
					uid:        acct.UID,
					body:       outBody,
					stream:     false,
					ctx:        ctx,
				})
			}
			if werr != nil {
				// ── 已写出响应 → **绝不能**换号重试 ──
				//
				// 流式路径是边读边写的。一旦写过字节：
				//
				//	状态码收不回来（已发 200）
				//	已发内容收不回来
				//
				// 此时换号重试会往**同一个** ResponseWriter 再写一套 SSE，
				// 客户端收到两段交错的事件流（协议损坏、内容重复）。
				//
				// 所以这是"只能收尾"的收场：记日志 + 保持已发内容不变。
				// 对比之下"未写过"才是可重试的，那种情况照旧换号。
				var committed *ErrResponseCommitted
				if errors.As(werr, &committed) {
					log.Printf("chat tools: 响应已提交，停止重试 uid=%s provider=%s: %v",
						acct.UID, reqProvider, committed.Cause)
					// 账号不算失败：这轮错误是"上游在后续轮次回了 400"，
					// 而非凭证/额度问题 —— 计失败会误伤一个健康的账号。
					h.cfg.Pool.NoteSuccess(acct.UID)
					if sessKey != "" && h.cfg.Session != nil {
						h.cfg.Session.Bind(sessKey, acct.UID)
					}
					loopStats.applyTo(st)
					st.status = http.StatusOK
					return
				}
				// 循环内的硬失败（注入失败/上游连续报错）：
				// 分类后按换号处理，与普通 chat 路径同一条。
				lastErr = werr
				fail(acct.UID)
				continue
			}
			if done {
				// 已经写过响应了（含状态码），直接收工。
				h.cfg.Pool.NoteSuccess(acct.UID)
				if sessKey != "" && h.cfg.Session != nil {
					h.cfg.Session.Bind(sessKey, acct.UID)
				}
				// 把循环读到的统计写回 —— 日志的 TTFB/tok/tok/s 靠它。
				loopStats.applyTo(st)
				st.status = http.StatusOK
				return
			}
			// done=false：本轮没跑成（如上游报错），换号重试。
			fail(acct.UID)
			continue
		}

		// 出站：按**选中的账号所属上游**分派到它自己的实现。
		//
		// 这是本次修复的核心。改造前这里恒用 h.cfg.Upstream（= 默认上游的
		// upstream.Client），于是 codearts 的请求被 workbuddy 的客户端发出去 ——
		// 凭证格式不匹配，必然失败，而且失败得很晚（一次完整往返）。
		//
		// cfg.Provider == nil（单上游部署 / 既有测试）时逐字节回退到原行为，
		// 见 chatVia 的注释。
		cr, status, terr := h.chatVia(ctx, reqProvider, acct, outBody)
		if terr != nil {
			// 网络层抖动：只换号，不喂熔断计数（传输层错误对连续失败连坐熔断过于严苛）。
			// 上游 client 已打 transport error 日志。
			st.status = http.StatusServiceUnavailable
			lastErr = terr
			fail(acct.UID)
			continue
		}
		if status >= 400 {
			st.status = status
			// 非 2xx：上游的错误体在流里，读出来喂 Classify。
			//
			// ⚠ 上限 1MB：上游的错误体正常是几百字节，但一个坏的/恶意的响应
			// 可能是一整个 SSE 流。无上限 ReadAll 会把它整个灌进内存，
			// 而这里只是要一段能分类的文本。
			respBody, _ := io.ReadAll(io.LimitReader(cr, 1<<20))
			// 错误体读完即弃（response body 已到 EOF），Close 防连接泄漏 ——
			// 这与成功路径上的 rc.Close() 对称。
			_ = cr.Close()
			//
			// 分类按**选中账号所属上游**分派（见 classifyErr 的长注释）：
			// 改造前这里恒调 `upstream.Classify` —— 那是 workbuddy 的分类器，
			// 却作用在两条上游的响应体上，造成"codearts 额度漏判"与
			// "codearts 号被裸数字 12153 永久禁用"两条真危害。
			kind := h.classifyErr(reqProvider, status, respBody)
			lastErr = &chatError{
				Kind:   kind,
				Status: status,
				Msg:    string(respBody),
			}
			// 内容策略拦截：**不罚账号**，改用降级提示词重试一次。
			//
			// 见 gateway.ErrKindContentBlocked 的长注释：内容问题换号没有意义
			// （每个账号背后是同一套策略），正确的动作是换提示词。
			// 因此这里刻意**不调** applyErrorPolicy —— 它会把账号冷却/熔断，
			// 而账号在这件事上没有任何过错。
			if kind == gateway.ErrKindContentBlocked {
				if h.retryWithDegradedPrompt(w, st, reqProvider, acct.UID, status, respBody, &degradedTried) {
					// 内容问题**不是账号问题**，所以这一轮的动作刻意与其它错误不同：
					//
					//	不调 applyErrorPolicy → 不冷却 / 不计错 / 不喂熔断
					//	不调 fail(uid)        → 不解绑会话粘性（会话本身没坏）
					//
					// 只释放在途租约，并把该账号从 tried 里**撤掉** ——
					// 让下一轮能重新选回同一个账号。
					//
					// # 为什么必须撤 tried（这是单账号部署的生死线）
					//
					// 不撤的话，PickFor 会因为 "已试过" 而跳过它：
					// 单账号部署下一轮**选不出任何号** → 直接 break →
					// 返回 503 "all accounts unavailable"。
					// 于是"降级重试"在最常见的部署形态下根本没发生过，
					// 用户看到的是"内容拦截 = 服务不可用"。
					//
					// 保留粘性绑定则让下一轮优先回到同一个账号（PickByUIDFor 先行），
					// 多账号部署下若粘性未命中，普通轮换选到别的号也无妨 ——
					// 内容策略是账号无关的，用哪个号都一样。
					releaseHeld()
					delete(tried, acct.UID)
					continue
				}
				releaseHeld()
				return
			}
			// 模型级限流收窄（借鉴 omni2api-panel）：
			// 429 code=6004 明说"将在 … 重置"时，把冷却精确到那个墙钟，
			// 并**记录触发模型** —— 换模型请求时该账号视为可用。
			//
			// 必须在 applyErrorPolicy **之前**判：后者会给一个
			// "基数 × 指数退避"的账号级冷却，那正是本收窄要避免的形态
			// （它会让账号在只被单模型限流时被整体冷掉）。
			if kind == gateway.ErrKindSoftRate {
				if resetAt, ok := h.softRateReset(reqProvider, status, respBody); ok {
					h.cfg.Pool.CooldownSoftForModel(acct.UID, h.cfg.SoftCooldown,
						resetAt, reqModel, "429 模型级限流")
					fail(acct.UID)
					continue
				}
			}
			h.applyErrorPolicy(acct.UID, kind, reqProvider, status, respBody)
			fail(acct.UID)
			continue
		}
		rc := cr
		if peek.Stream {
			// ── 流式出站：装上两道时间闸门后逐帧透传 ──
			//
			// 闸门（总时长 / 首内容帧）装在这里而不是 internal/upstream 里，
			// 因为 chat 有**两条**出站路径（默认上游 cfg.Upstream 与其余上游
			// cfg.Provider），它们唯一的共同形状就是"返回一个 io.ReadCloser"。
			// 包在 body 外层 → 一次覆盖全部上游，且不要求任何上游配合改动。
			// 完整论证见 chatwatchdog.go 的文件头。
			//
			// ⚠ 总时长锚在 st.start（请求进入 handler 的时刻），跨换号共享：
			// 否则 MaxRotate 次重试会把上限乘 3 —— 那正是实测 380s 的形态
			// （多个上游各挂一段，每次都重新开始计时）。
			// ⚠ 不用 defer wd.Close()：本行在**换号循环**里，defer 会一直
			// 攒到函数返回才执行 —— 那正是下面 stream 分支反复强调的
			// "defer 在轮转场景下堆积 fd"。紧跟 wire.Stream 的显式 Close
			// 覆盖了全部出口（含闸门命中后的提前 return）。
			wd := newStreamWatchdog(rc, st.start, h.cfg.StreamMax, h.cfg.StreamFirstByte)

			// 统计与闸门共用同一个逐帧解析器：它既填 TTFB/tokens/usage，
			// 也把"见到产出"这个事实通过 outputFlag 告诉看门狗。
			// 判据只有一份 —— 见 chatStatsReader.parseSSELine 与 outputFlag。
			stats := newChatStatsReaderSince(wd, st.start)
			stats.SetOutputFlag(wd.out)
			// commitTracker 记录"响应是否已提交"：闸门命中后据此决定
			// 是回 504 还是写流内错误帧（两者都必须让客户端立刻知道结果，
			// 而不是挂到它自己的超时）。
			tw := &commitTracker{ResponseWriter: w}
			serr := wire.Stream(tw, stats)
			st.ttfb = stats.TTFB()
			// ⚠ 必须看 ok 标志：`st.toks, _ = stats.Tokens()` 会在「末帧没有
			// usage」时把 0 写进去，于是 -1 哨兵被悄悄抹掉 —— 日志显示
			// tok=0（"这次真的用了 0 个 token"）而不是 tok=-（"上游没给"）。
			// 这两个语义对聚合完全不同：0 会进平均，-1 被排除。实测踩到过：
			// 一次 200 的流式请求记录了 tokens=0，而同一账号的下一条正常记录是 648。
			if toks, has := stats.Tokens(); has {
				st.toks = toks
			}
			st.usage = stats.Usage()
			_ = wd.Close()

			// 闸门命中 → 走下面专门的中断收场分支（在流内错误分支之前判，
			// 因为中断时拿到的是一句"read on closed body"，不能被当成
			// "上游流内错误"或"空流"来解读）。
			if hit := streamTimeoutHit(wd, ctx, serr); hit != nil {
				h.finishStreamTimeout(w, tw, st, reqProvider, acct.UID, hit)
				return
			}

			var inband *wire.InBandError
			if errors.As(serr, &inband) {
				if inband.Committed {
					// 内容已流出、状态码已提交，错误帧也已原样下发：
					// 既不能改响应也不再换号（客户端已经"消费"了这个回复）。
					// 只记录，保留可排查的上游原文。
					log.Printf("chat stream uid=%s provider=%s: 上游流内错误（状态码已提交）: %s",
						acct.UID, reqProvider, inband.Message)
					h.cfg.Pool.NoteSuccess(acct.UID)
					if sessKey != "" && h.cfg.Session != nil {
						h.cfg.Session.Bind(sessKey, acct.UID)
					}
					st.status = http.StatusOK
					return
				}
				// 状态码未提交 → 与 status>=400 完全同路。
				//
				// classifyErr 用的是**该上游自己的**分类器（status 传 200：
				// 这正是"错误藏在 2xx 里"的事实，分类器必须看得见它）。
				// 模型名/通道类错误会落到 ErrKindNone（只换号不罚），
				// 额度类错误会落到 ErrKindHardCredit（进硬冷却）——
				// 判据全部来自上游，这里不新增任何假设。
				//
				// ⚠ status 传 200、body 传流内错误原文：解读也走同一份数据，
				// 否则会出现"因为余额不足被冷却、界面却说未知错误"。
				kind := h.classifyErr(reqProvider, http.StatusOK, []byte(inband.Body))
				lastErr = &chatError{
					Kind:   kind,
					Status: http.StatusOK,
					Msg:    inband.Message,
				}
				h.applyErrorPolicy(acct.UID, kind, reqProvider, http.StatusOK, []byte(inband.Body))
				fail(acct.UID)
				continue
			}

			// 到这里才是真正的成功流（serr 非 nil 的情形只有"上游空流"，
			// 那一帧 error + [DONE] 已由 Stream 写出，状态码必须保持 200）。
			h.cfg.Pool.NoteSuccess(acct.UID)
			// 粘性跟随最终成功号：本轮成功的账号成为该会话的粘性绑定（覆盖旧绑定）。
			if sessKey != "" && h.cfg.Session != nil {
				h.cfg.Session.Bind(sessKey, acct.UID)
			}
			st.status = http.StatusOK
			return
		}
		// ── 非流式：先装总时长闸门，再用**同一个** chatStatsReader 读数 ──
		//
		// 非流式的上游响应**本身就是 SSE**（Aggregate 读的就是它，只是最后
		// 合成一个 chat.completion）。所以这里不必新造一个"首字节计时 reader"：
		// 把既有的统计器套上，TTFB 就与流式路径**逐字节同口径**
		// （本仓库反复强调"统计判据只能有一份"：两个 reader 各写一遍，
		// 迟早有一边先漂移，而且漂移的方向是"非流式的 TTFB 比流式小"，
		// 正好会掩盖"上游慢"这件事）。
		//
		// 闸门在非流式路径上同样必要：timeout_seconds 只约束**默认上游**
		// （http.Client.Timeout 在 ChatHTTP 上是 0），其余上游既无总时长也无
		// 空闲监控 —— 实测那两条 inkstone 120.0s 挂死的记录正是这个形态。
		//
		// ⚠ 首帧看门狗只对流式有意义（非流式客户端一次收全，没有"打字机"可言），
		// 所以这里只给总时长。
		wd := newStreamWatchdog(rc, st.start, h.cfg.StreamMax, 0)
		stats := newChatStatsReaderSince(wd, st.start)
		stats.SetOutputFlag(wd.out)
		resp, err := wire.Aggregate(stats)
		// TTFB 与 usage 读数无条件搬进 st：它们描述的是"上游给了什么"，
		// 与该次请求最后成功还是失败无关 —— 失败路径（下面几支）也要有 TTFB，
		// 否则"120s 挂死"这类记录仍然看不出是上游慢还是我们慢。
		st.ttfb = stats.TTFB()
		st.usage = stats.Usage()
		_ = wd.Close()

		// 闸门命中：与流式同一个收场函数。非流式从没写过字节，
		// 因此走"回 504"那一支。必须在流内错误/解析失败分支**之前**判 ——
		// 否则超时会被误报成"上游流没有有效数据事件"（上游背锅，真相是我们掐的）。
		if hit := streamTimeoutHit(wd, ctx, err); hit != nil {
			log.Printf("chat sync uid=%s provider=%s: %s（回 504）", acct.UID, reqProvider, hit.Error())
			writeOpenAIError(w, http.StatusGatewayTimeout, string(hit.Reason), hit.Error())
			st.status = http.StatusGatewayTimeout
			return
		}

		// 非流式同样要拦流内错误：Aggregate 命中错误信封时返回 *InBandError，
		// 而不是合成一个 content:"" 的成功响应（改造前它就是这么被吞掉的）。
		var inband *wire.InBandError
		if errors.As(err, &inband) {
			kind := h.classifyErr(reqProvider, http.StatusOK, []byte(inband.Body))
			lastErr = &chatError{
				Kind:   kind,
				Status: http.StatusOK,
				Msg:    inband.Message,
			}
			h.applyErrorPolicy(acct.UID, kind, reqProvider, http.StatusOK, []byte(inband.Body))
			fail(acct.UID)
			continue
		}
		if err != nil {
			// 上游流解析失败：客户端还没看到任何输出，回 502 并告知原因。
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			st.status = http.StatusBadGateway
			return
		}
		h.cfg.Pool.NoteSuccess(acct.UID)
		if sessKey != "" && h.cfg.Session != nil {
			h.cfg.Session.Bind(sessKey, acct.UID)
		}
		writeJSON(w, http.StatusOK, resp)
		st.status = http.StatusOK
		st.toks = completionTokens(resp)
		st.usage = usageOf(resp)
		return
	}
	msg := "all accounts unavailable (cooling/disabled)"
	// 把"为什么一个都不可用"写进错误里（真实困惑：上游 402 触发冷却后，旧文案
	// 只说 all accounts unavailable —— 用户据此怀疑"登录失败了"，而真相是
	// "key 有效但账户没钱在冷却"。一句话的差别，省一轮客服）。
	if hint := h.poolUnavailableHint(reqProvider); hint != "" {
		msg += " —— " + hint
	}
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
	st.status = http.StatusServiceUnavailable
}

// finishStreamTimeout 收场一次**被闸门中断**的流式请求（见 chatwatchdog.go）。
//
// # 为什么有一个专门的分支
//
// 闸门中断底层流之后，wire.Stream 的返回值是底层那句"读失败了"
// （默认上游是 `read on closed body`，尊重 ctx 的上游是 context 取消）。
// 若不单独判它，它会落进下面两条既有分支之一，而两条都给不出正确答案：
//
//	空流兜底分支   报 "upstream stream contained no valid data events"
//	               —— 把一个**有名字的超时**说成"上游没发数据"，
//	                  排障时会去查上游，而真相是我们自己掐的
//	流内错误分支   把它当上游业务错误分类 → 可能给账号安排冷却
//	               —— 超时不涉及这个账号的任何过错（与网络抖动同一条红线）
//
// # 两种收场方式（取决于响应有没有提交）
//
//	未提交  状态码还能改 → 504 + OpenAI 错误 JSON。
//	        为什么不是 200：真相比"看起来成功"重要，客户端该拿到一个
//	        能重试/能上报的信号。
//	已提交  状态码与已流出的内容都收不回来 → 只能按仓库既有机制写
//	        流内错误信封 + [DONE]（与 wire.Stream 的空流兜底帧同一形状）。
//	        **不静默截断**：客户端收到那个 [DONE] 就会正常收尾，
//	        而不是挂到它自己的超时（改造前的 380s 就是这么来的）。
//
// # 为什么两种都不罚账号
//
// 超时是"这条流没跑完"，不是"这个账号有问题"。与 transport error 同一条：
// 只释放在途租约（由 chatCompletions 的 defer 完成），不冷却、不计错、不喂熔断。
// 若这里给账号记一笔，一个上游的慢就会把健康账号池拖进冷却 —— 池子耗尽后
// 所有请求一起 503，那是一类更严重的自伤。
func (h *Handler) finishStreamTimeout(
	w http.ResponseWriter, tw *commitTracker, st *chatStat,
	providerID, uid string, hit *streamTimeoutError,
) {
	st.status = http.StatusOK
	if !tw.committed() {
		// 一个字节都没写过：状态码仍可改。
		log.Printf("chat stream uid=%s provider=%s: %s（未提交，回 504）",
			uid, providerID, hit.Error())
		writeOpenAIError(w, http.StatusGatewayTimeout, string(hit.Reason), hit.Error())
		st.status = http.StatusGatewayTimeout
		return
	}
	// 已经流出了内容：只能如实收尾。
	log.Printf("chat stream uid=%s provider=%s: %s（已提交 200，写流内错误帧 + [DONE] 收尾）",
		uid, providerID, hit.Error())
	writeStreamTimeoutFrame(w, hit)
}

// retryWithDegradedPrompt 处理一次**内容策略拦截**（借鉴 omni2api-panel）。//
// # 语义
//
//	返回 true  → 已触发降级，调用方应 continue 让下一轮用中性提示词重发
//	返回 false → 已向客户端写出终态错误，调用方应立即 return（不再重试）
//
// # 为什么"重试一次"就够了，不做更多
//
// 上游只给一句"blocked"，无法区分「system 指纹误报」与「用户内容真的违规」。
// 换中性提示词重试一次是**唯一**能区分两者的实验：
//
//	重试后通过 → 是①，降级有效，本请求正常返回
//	重试后仍被拦 → 是②，问题在用户内容里，再怎么换提示词/换账号都没用
//
// 继续重试只会把一次内容问题拖成 MaxRotate 次白跑往返，
// 最后返回"所有账号不可用" —— 一个把内容问题误报成账号池故障的错误结论。
// 因此第二次被拦时**立刻如实告诉调用方**，带上上游原文。
//
// # 为什么账号不罚
//
// 内容策略是**账号无关**的：同一套策略在所有账号后面。因此这条路径
// 绝不调用 applyErrorPolicy（不冷却、不计错、不喂熔断）。
// 调用方只做 fail(uid) —— 那是**释放租约 + 解绑粘性**的资源动作，不是惩罚。
//
// # status 兜底
//
// 传入的是上游的原始状态码（正常是 400）。若上游给的是 2xx（错误藏在流内），
// 直接把它转发给客户端会让客户端以为成功 —— 因此夹到 400。
// 这一点很重要：本函数是在"读流内错误"的分支上被复用的。
func (h *Handler) retryWithDegradedPrompt(
	w http.ResponseWriter, st *chatStat, providerID, uid string,
	status int, respBody []byte, degradedTried *bool,
) bool {
	msg := contentBlockMsg(respBody)
	if h.cfg.PromptGate != nil && !*degradedTried {
		h.cfg.PromptGate.Trigger()
		*degradedTried = true
		log.Printf("chat uid=%s provider=%s: 内容策略拦截，已切降级提示词重试一次（降级至 %s）: %s",
			uid, providerID, h.cfg.PromptGate.Until().Format(time.RFC3339), msg)
		return true
	}
	// 中性提示词仍被拦（或本部署没接提示词体系）→ 判定为用户内容触发审核。
	//
	// 注意 nil gate 时也走这里：没有降级能力还继续换号是纯浪费，
	// 如实返回上游错误比"所有账号不可用"更有信息量。
	log.Printf("chat uid=%s provider=%s: 内容策略拦截（%s），判定为请求内容本身触发：%s",
		uid, providerID, contentBlockExhaustedReason(h.cfg.PromptGate, *degradedTried), msg)
	if status < 400 || status > 599 {
		status = http.StatusBadRequest
	}
	writeOpenAIError(w, status, "content_blocked", "上游内容策略拦截："+msg)
	st.status = status
	return false
}

// contentBlockExhaustedReason 给日志一个能区分两种终态的措辞。
func contentBlockExhaustedReason(gate *prompt.Gate, degradedTried bool) string {
	switch {
	case gate == nil:
		return "未接提示词体系，无法降级"
	case degradedTried:
		return "降级提示词后仍被拦"
	default:
		return "降级重试次数已用尽"
	}
}

// contentBlockMsg 摘要上游错误体，供日志与返回给客户端的文案使用。
//
// 上限 300 字节并按**字符边界**截断：上游错误体是 UTF-8 中文，
// 按字节切会把一个汉字切成两半，产生 \ufffd 乱码
// （本仓库已踩过这个坑，见 CHANGELOG 的「UTF-8 截断」条目）。
func contentBlockMsg(body []byte) string {
	s := strings.TrimSpace(string(body))
	if s == "" {
		return "(上游未返回错误详情)"
	}
	// 压掉换行，让日志保持"一行一条"
	s = strings.Join(strings.Fields(s), " ")
	const maxRunes = 300
	r := []rune(s)
	if len(r) > maxRunes {
		return string(r[:maxRunes]) + "…"
	}
	return s
}

// applyErrorPolicy 按错误分类对账号施加冷却/禁用/熔断策略（最终版状态机）。
// ⚠ 参数只有 uid：调用方（出站循环）只拿得到**出错的号**，拿不到"当前请求的
// 上游" —— 多上游下两者可能不同（粘性命中、轮换换号）。所以需要上游事实的
// 分支（ErrKindHardCredit）自己去池子反查归属：
//
//	Pool.ProviderOf(uid) → nextResetAt(uid) → cfg.Provider.ResetAt(id, cred)
//
// 这正是 P2 修复的形状：把"哪个上游"这个事实从 uid 反查出来，
// 而不是让一个无参回调替所有上游回答同一个时刻。
//
// kind 是唯一权威分类（来自**选中账号所属上游**的错误分类器，
// 经 gateway 层中立化后传进来），此处不再按原始 status 二次判断。
//
// 仅在 chatCompletions 轮转循环内调用：调用方已准备好 lastErr 并打算 continue 换号。
//
// # 为什么参数类型从 upstream.ErrKind 换成 gateway.ErrorKind
//
// 改造前它吃 `upstream.ErrKind` —— 那是 **workbuddy 的类型**，
// 于是 codearts 的分类结果**在类型上就传不进来**，core 只能拿
// workbuddy 的判据去判所有上游的响应（P2 缺口的技术根因）。
//
// 现在它吃 gateway.ErrorKind（跨上游中立，core 与上游都合法依赖 gateway）。
// 各上游在自己的包里把自己的 ErrKind 翻译过来，core 只做策略决策 ——
// **策略本身一行都没改**，只换了接缝上的词汇。
//
// 七条路径，各司其职：
//   - ErrKindHardCredit → CooldownUntilNextReset：即时硬冷却到**上游给出的**
//     下次重置时刻（workbuddy 是次日 04:00 等签到恢复；codearts 是配额窗口重置）。
//   - ErrKindSoftRate / ErrKindNotFound → Cooldown(CoolSoft)：即时软冷却（429/404）。
//   - ErrKindSessionDead → Disable：session 死亡，永久禁用（需人工重登）。
//     ⚠ **只有 workbuddy 会产生这个分类** —— codearts 的凭证失效是
//     ErrKindAuth（可自动续期恢复），刻意不映射到这里。
//   - ErrKindServer → NoteError：喂单一连续失败计数器 fails + 累计错误 errTotal，
//     达到 breakerThreshold 触发熔断（指数退避）。
//   - ErrKindAuth / 其他（default：ErrKindClient/ErrKindNone）→ 只换号不罚
//     （防雪崩），不喂熔断。
//
// # ErrKindAuth 为什么走 default（只换号不罚）
//
// 它是 codearts 的凭证失效（401/403）。这类失效**可以由
// RefreshCredential 恢复**（STS 凭证仅约 2 小时，401 是常见路径而非异常，
// 见 codearts/client.go 的 MaxAuthRetry 与 credentialrefresher.go）。
// 把它升级成 Disable 会把"一次 401"变成"永久禁用" —— 那正是本次要修的
// 危害 ② 的另一半。保守方向是明确的：宁可多换一次号，也不能把可恢复的
// 账号永久打掉。续期由后台任务 + 请求路径的 needsRefreshVia 分支负责。
//
// 恢复出口：CoolSoft/CoolHard 各自到期自动恢复；熔断按其指数退避截止到期；
// 成功（NoteSuccess）清 fails/熔断；签到解冻（ReenableIfCredits→reviveCoolingLocked）只清冷却，不动熔断。
// nextResetAt 问**该账号所属上游**"额度耗尽的号什么时候能再用"。
//
// # 四条路径（与 chatVia / refreshCredential / needsRefreshVia / classifyErr 同形状）
//
//	Provider == nil                    → 单上游：注入的回调就代表那个上游
//	取不到该 uid 的上游归属            → 接线问题（号不在池里），回落通用值
//	该上游没实现 ResetPolicyExt        → ok=false → 回落通用值
//	该上游上报了恢复排程               → 用它的时刻
//
// 通用值是 now+1h。选 1 小时而不是"次日某点"：那是个**通用**的保守值，
// 不假装知道任何上游的具体重置策略。上游上报了就以上游为准。
//
// # ⚠ 这里为什么先反查 provider（P2 修复的核心两行）
//
// 旧实现直接 `return h.cfg.NextResetAt()`，而旧回调**没有参数** ——
// 于是不管出错的号属于哪个上游，拿到的都是同一个（workbuddy 的次日 04:00）。
// codearts 的号因此被冷到一个对它毫无意义的时刻：它**没有签到恢复机制**，
// 04:00 既不是它的配额窗口边界，也不是它的任何事实。
//
// 归属由账号池回答（Pool.ProviderOf，只读 —— 池子按 provider 分域，
// 标签与默认值都在它手里）。出口层只按 ID 问，与其它四条多上游路径同构：
//
//	Chat / Credential / RefreshCredential / RefreshSkew → Provider 路由
//	ResetAt                                            → Provider 路由（本条）
//
// # 为什么 ProviderOf 失败时**不猜**成默认上游
//
// 号不在池里是接线问题，不是"它属于默认上游"。拿默认上游的排程去回答
// 一个没有归属的号，正是本 bug 的形态（用 A 的事实回答 B 的问题）。
// 回落通用值的方向是保守的：now+1h 比"次日 04:00"短得多，
// 最坏情况只是多撞一次硬错误，而不是白闲置一天。
//
// # 为什么单上游模式（Provider == nil）不走 ProviderOf
//
// 单上游没有"别的上游"概念，注入的回调（或它缺省时的 nil）就是那个上游的
// 全部事实 —— 与改造前逐字节一致。这也是既有部署与既有测试的路径。
func (h *Handler) nextResetAt(uid string) time.Time {
	if h.cfg.Provider != nil {
		providerID, _ := h.cfg.Pool.ProviderOf(uid)
		if cred, ok := h.cfg.Provider.Credential(providerID, uid); ok {
			if until, has := h.cfg.Provider.ResetAt(providerID, cred); has {
				return until
			}
		}
		// 该上游没上报排程 → 通用保守值（不回落到注入的回调：
		// 那个回调是**单上游/默认上游**的事实，见上面的"不猜"）。
		return time.Now().Add(time.Hour)
	}
	if h.cfg.NextResetAt != nil {
		if until, ok := h.cfg.NextResetAt(""); ok {
			return until
		}
	}
	return time.Now().Add(time.Hour)
}

// classifyErr 按**选中账号所属上游**的错误分类器判定错误类别。
//
// # 三条路径（与 chatVia / refreshCredential / needsRefreshVia 同形状）
//
//	Provider == nil                   → cfg.DefaultClassifier（单上游，逐字节不变）
//	Provider 在 && 上游实现了分类器   → 上游自己的分类（翻译成中立类型）
//	Provider 在 && 上游没实现分类器   → cfg.DefaultClassifier（回落默认上游判据）
//
// # 后两条回落为什么是同一个分支，且为什么**必须**用默认上游的判据
//
// 单上游模式没有"别的上游"概念，行为要与改造前一致。
//
// 而"该上游没实现 ErrorClassifier"的正确解释**不是**"用通用猜测"，
// 而是"用默认上游的判据"：默认上游就是 workbuddy，它的分类判据
// （hardMarkers / sessionDeadMarkers）是 workbuddy 的**上游知识**。
//
// ⚠ 刻意**不**回落到 `gateway.DefaultErrorKind`（只按状态码的通用兜底）：
// 那会丢掉 workbuddy 的两个正文判据 ——
//
//	`{"code":12153,...}` 在 200/401 上不再被判成 session dead
//	（TestChatSessionDeadDisables 会立刻变红）
//	`{"msg":"余额不足"}` 在 400 上不再触发硬冷却
//	（TestChatRotatesOnHardCredit 会立刻变红）
//
// 也就是说：通用兜底会**静默削弱已有判据**。
//
// # 这次改动把回落目标从 upstream.Classify 换成注入的分类器
//
// 原先这里直接调 `upstream.Classify` —— 那是**核心包 import 某个上游的
// 私有 SDK**，正是 privateSDKKnownDebt 里 server 那条债的形态。
//
// 现在改由装配层注入（`cmd/server` 里包一层 `workbuddy.Classify`）：
// **判据一个字没变**（还是 workbuddy 的那份），但核心不再需要认识
// upstream 这个包。这是"把上游知识留在上游、核心只收接口"的常规手法。
//
// 注入为 nil 时（测试里的手工构造、或单上游但没接线的部署）回落到
// `gateway.DefaultErrorKind` —— 只按状态码判断。那是**明确的降级**，
// 不是静默削弱：nil 注入是调用方的显式选择。
func (h *Handler) classifyErr(providerID string, status int, body []byte) gateway.ErrorKind {
	if h.cfg.Provider != nil {
		if kind, ok := h.cfg.Provider.Classify(providerID, status, string(body)); ok {
			return kind
		}
	}
	if h.cfg.DefaultClassifier != nil {
		return h.cfg.DefaultClassifier(status, string(body))
	}
	return gateway.DefaultErrorKind(status, string(body))
}

// RefreshError 续期失败的中立错误（装配层的适配器产出）。
//
// # 为什么需要它（而不是直接看 upstream.Error）
//
// 出口层原先用 `errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead`
// 判断"该不该走计数门控"。`*upstream.Error` 是 workbuddy 私有 SDK 的类型 ——
// 核心为了一个布尔判断依赖了某个上游的 SDK。
//
// 换成这个中立类型后判据完全一样（SessionDead 这个字段就是那个布尔），
// 而核心不再认识 upstream 包。
//
// ⚠ `SessionDead` 用**具名字段**而不是直接复用 gateway.ErrorKind：
// 这里的语义是"刷新路径上要不要走计数门控"，与"这次响应属于哪一类错误"
// 是两件事（后者才是 ErrorKind）。合成一个会让读者以为它们是同一套判据。
type RefreshError struct {
	// SessionDead 凭证会话已死（需人工重登）。
	SessionDead bool
	// Msg 上游原文（保留可排查性）。
	Msg string
	// Err 原始错误（便于日志与 errors.Is 链）。
	Err error
}

func (e *RefreshError) Error() string {
	if e == nil {
		return ""
	}
	if e.Msg != "" {
		return e.Msg
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return "refresh failed"
}

func (e *RefreshError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// chatError 出站循环里累积的最后一个错误（**只用于拼 503 的文案**）。
//
// # 为什么不用 upstream.Error（这次重构把它换掉了）
//
// 原先这里用的是 `*upstream.Error` —— 那是 **workbuddy 私有 SDK 的类型**，
// 而本包是核心出口层。为了一个纯展示用的结构体让核心依赖某个上游的 SDK，
// 正是 privateSDKKnownDebt 里 server 那条债的形态。
//
// 换成 server 自己的类型之后，Kind 直接就是 `gateway.ErrorKind`
// （中立类型），不再需要 upstreamKindOf 那层"中立 → upstream"的翻译 ——
// 那层翻译**存在的唯一理由是 upstream.Error.Kind 是 upstream.ErrKind**。
// 换句话说：这条依赖链被整条剪掉了，而不是被绕开。
type chatError struct {
	// Kind 错误类别（中立类型）。
	Kind gateway.ErrorKind
	// Status 上游返回的 HTTP 状态码（流内错误时为 200 —— 那是事实）。
	Status int
	// Msg 上游原文（原样保留，便于排查）。
	Msg string
}

func (e *chatError) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// softRateReset 问**该上游自己**"这次软限流有没有精确的重置时刻"。
//
// # 两条路径（与 classifyErr 同形状，但回落策略刻意不同）
//
//	Provider == nil（单上游）        → wire.ParseSoftRateReset（逐字节旧行为 + 收窄）
//	Provider 在 && 上游实现了扩展点  → 该上游自己解析
//	Provider 在 && 上游没实现        → ok=false（保持账号级软冷却）
//
// # ⚠ 为什么第三条**刻意不**回落到默认上游的解析器
//
// 这与 `classifyErr` 的取舍**相反**，是刻意的：
//
//	分类器回落（classifyErr）→ 保守方向：把不认识的错误判成"只换号不罚"
//	本函数回落              → **激进**方向：判成"模型级 + 已知重置时刻"
//	                          → 账号被收窄冷却**并被豁免**
//
// 拿 workbuddy 的 6004 判据去解析 codearts 的错误体，最坏情况是把
// codearts 的一次普通限流误判成"模型级且已给出重置时刻"：
// 冷却被错误收窄 + 该模型被豁免 → 账号过早回到候选集继续撞限流。
//
// 那是 P2 那类"用 A 的事实回答 B 的问题"，且后果比漏判更重。
// 所以多上游场景下，**没实现就保持旧行为**（账号级软冷却，永不更差）。
func (h *Handler) softRateReset(providerID string, status int, body []byte) (time.Time, bool) {
	if h.cfg.Provider != nil {
		return h.cfg.Provider.SoftRateReset(providerID, status, string(body))
	}
	// 单上游：默认上游（workbuddy）的解析器就是"那个上游的事实"。
	// status 必须显式限定为 429 —— 与 workbuddy.SoftRateReset 的两道闸门同口径。
	if status != http.StatusTooManyRequests {
		return time.Time{}, false
	}
	return wire.ParseSoftRateReset(string(body))
}

// chatVia 按**账号所属上游**把一次对话发出去，返回 (响应流, 状态码, 传输错误)。
//
// # 为什么必须分派（这是 861dee5 引入、预先存在的那条真 bug）
//
// 「账号池支持多上游」那次改动只改了**选号**（PickFor 按 provider 过滤），
// 没有改**出站**——出站仍然恒用 `cfg.Upstream`。于是：
//
//	PickFor("codearts", ...) 选出 codearts 账号   ← 对
//	cfg.Upstream.ChatStream(codearts 账号, body)  ← 用 workbuddy 的客户端发
//
// codearts 的签名（SDK-HMAC-SHA256 + DPoP）、路径前缀（/api/v2）、
// 通道头（maas_type）全在它自己的 Client.ChatStream 里。用 workbuddy 的
// 客户端发等于把这些全跳过，必然失败。
//
// 出口层**无法**自己选对实现：它只认 ID（不认识任何具体上游，见包注释）。
// 「ID → 能发请求的那个实例」是装配层的事实，所以这条路走 cfg.Provider。
//
// # 三条返回路径的形状（与改造前的四元组对齐）
//
//	Provider == nil           → cfg.Upstream 的原始四元组（逐字节回退）
//	Provider 在 && 取到凭证   → gateway.ChatStream 的 {Status, Body}
//	Provider 在 && 取不到凭证 → 传输错误（换号，不喂熔断）
//
// 第三条刻意复用 terr 而不是新造一种失败：它的语义与"网络抖动"在调用方
// 看来完全一致 —— 这个号现在发不出请求，换下一个，**不要**惩罚它
// （取不到凭证是接线问题，不是这个账号的错；这一点与本次修的 bug 同源：
// 改造前正是"账号被无关的失败惩罚"）。
//
// 关于 status>=400 时 Body 的语义：gateway.ChatStream 把非 2xx 的
// 上游错误体**也放在 Body 里**（见 gateway.Provider.Chat 的注释），
// 所以调用方要在分支里 io.ReadAll 它才能喂 Classify。这与
// upstream.Client.ChatStream 返回的 []byte respBody 是同一份数据，
// 只是形态从 []byte 变成了流。
func (h *Handler) chatVia(ctx context.Context, providerID string, acct *auth.Auth, body []byte) (io.ReadCloser, int, error) {
	if h.cfg.Provider == nil {
		// 单上游模式：没有任何多上游概念，行为与改造前**逐字节一致**。
		//
		// upstream.Client.ChatStream 在非 2xx 时把错误体放在**第三个返回值**里、
		// rc 为 nil；而调用方那段 status>=400 的分支是按 gateway.ChatStream 的
		// 契约写的（错误体在 Body 流里）。这里必须把 respBody **原样**包成流，
		// 而不是丢掉它。
		//
		// ⚠ 这一条是**真踩过的**：第一版回退路径写成 `_` 丢掉 respBody、
		// 返回一个空流。后果是 `upstream.Classify(status, "")` 永远拿不到
		// 上游的错误正文 —— 而 session dead 的判据正是正文里的 **12153**
		// （见 upstream.Classify）。于是 TestChatSessionDeadDisables 变红：
		// 401 不再被分类成 ErrSessionDead，账号不再被禁用。
		//
		// 换句话说：回退路径的"逐字节一致"不是修辞，丢掉一个 []byte 就足以
		// 让一条既有契约静默失效。测试抓住了它（这正是那些测试存在的理由）。
		rc, status, respBody, err := h.cfg.Upstream.ChatStreamWithIP(acct, body, gateway.ClientIPFrom(ctx))
		if err == nil && status >= 400 && rc == nil {
			return io.NopCloser(bytes.NewReader(respBody)), status, nil
		}
		return rc, status, err
	}

	// 多上游：拿到这个用户 id 对应的、该上游能用的凭证。
	// 出口层**不读 Secret**（那是上游的事实），只把装配层组装好的整份传下去。
	cred, ok := h.cfg.Provider.Credential(providerID, acct.UID)
	if !ok {
		return nil, 0, errNoProviderCredential
	}
	cs, registered, err := h.cfg.Provider.Chat(ctx, providerID, cred, body)
	if !registered {
		// 该上游没接上（未注册 / 实例缺失）：与"取不到凭证"同类 —— 接线问题，
		// 走传输错误分支换号，**不惩罚这个账号**。
		return nil, 0, errNoProviderCredential
	}
	if err != nil {
		// Provider.Chat 的契约：只有**传输层**失败才返回 error；
		// 非 2xx 属于业务错误，走 (ChatStream, nil)。
		return nil, 0, err
	}
	if cs.Body == nil {
		// 既没有 error 又没有 body：Provider 违约。
		// 造一个**有状态码**的空流，让调用方按 status 走分类而不是 panic
		// （契约测试会挡住这种情况，但生产路径不能依赖测试跑过）。
		return io.NopCloser(strings.NewReader("")), cs.Status, nil
	}
	return cs.Body, cs.Status, nil
}

// refreshCredential 按**账号所属上游**续期凭证。
//
// # 修的是什么
//
// 改造前：`h.cfg.Upstream.RefreshToken(acct)` —— 恒用 workbuddy 的客户端。
// 选中 codearts 账号时它会读到一份它不认识的凭证 → "no refreshToken"
// → 上层把**这个无辜的 codearts 账号**标记失败并冷却 → 池子耗尽 → 503。
// 实测：一次请求让 codearts 账号的 err_total +3。
//
// # 分派链条
//
//	cfg.Provider.RefreshCredential(ctx, id, cred)
//	  → 装配层按 id 取 Provider 实例
//	  → gateway.ExtOf[CredentialRefresher](pv)  ← 上游自报"怎么刷新我的"
//	  → workbuddy / codearts 各自的实现
//
// 出口层不认识任何具体上游，也不认识 gateway 之外的续期协议。
//
// # 三种"不刷新"为什么要区分
//
//	Provider == nil        → 回落 cfg.Upstream（单上游，行为不变）
//	取不到凭证             → 报错换号（接线问题，不能假装成功）
//	ok=false 且 err==nil   → **跳过刷新**，继续用现有凭证发请求
//
// 第三种是「该上游没实现 CredentialRefresher」，语义是**这个上游的凭证
// 不需要刷新**（例如纯 API Key 的上游）。把它当成失败会让这类上游
// 每次请求都白换一次号 —— 那正是"合法实现被通用假设误伤"。
func (h *Handler) refreshCredential(ctx context.Context, providerID string, acct *auth.Auth) error {
	if h.cfg.Provider == nil {
		// 单上游模式：逐字节回退（含落盘与失败日志，与改造前一致）。
		if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
			h.noteRefreshFailure(acct.UID)
			return err
		}
		if err := acct.SaveAtomic(); err != nil {
			// 刷新成功但落盘失败：下次启动会用旧 token，必须暴露
			log.Printf("chat refresh uid=%s: save auth failed: %v", acct.UID, err)
		}
		return nil
	}

	cred, ok := h.cfg.Provider.Credential(providerID, acct.UID)
	if !ok {
		return errNoProviderCredential
	}
	refreshed, err := h.cfg.Provider.RefreshCredential(ctx, providerID, cred)
	if err != nil {
		h.noteRefreshFailure(acct.UID)
		return err
	}
	if !refreshed {
		// 该上游没有续期实现 = 它的凭证不需要刷新。落盘也归它自己管
		// （我们连它要不要落盘都不知道，不该替它决定）。
		return nil
	}
	return nil
}

// noteRefreshFailure 记录一次凭证续期失败；连续失败达上限则禁用（A2 移植）。
//
// # 为什么在刷新路径上计数
//
// refresh token 连续失效基本是"已死"（被踢/过期），反复重试只会每次请求
// 都多付一次失败往返。达到上限直接禁用，让对账/重新登录接管。
func (h *Handler) noteRefreshFailure(uid string) {
	if h.cfg.Pool == nil {
		return
	}
	if h.cfg.Pool.NoteRefreshFailure(uid) {
		log.Printf("chat refresh uid=%s: 凭证续期连续失败达上限，已禁用（需重新登录）", uid)
	}
}

// needsRefreshVia 问**上游自己**"这个号现在要不要刷"。
//
// # 为什么核心不能自己拿 h.cfg.RefreshSkew 判（这是出站修复的漏网）
//
// 改造后 `refreshCredential` 已经把"**怎么**刷"交给了上游，但"**要不要**刷"
// 还留在核心：出站循环先做 `acct.NeedsRefresh(h.cfg.RefreshSkew)`（默认 10m），
// 只有它为真才会走到上游的续期实现。而"多早算该刷"同样是**上游的事实**：
//
//	workbuddy → access token 寿命以小时计，10m 窗口合理
//	codearts  → STS 凭证只有约 2h 寿命，它自己的 refreshSkew 是 3m
//
// 一个上游的 skew 被当成所有上游的 skew，后果是**两个方向的错**：
//
//	token 剩 8m（codearts 视角）→ 核心的 10m 判 true → 进续期分支 →
//	  codearts 的 CredentialRefresher 里**没有**自己的 skew 检查（它只
//	  断言+转发到 Client.RefreshToken），于是真的去消费 refresh_token ——
//	  而它是**一次性**的（用一次即作废，见 codearts/jobs.go 的包注释）。
//	  为省一次 401 往返烧掉一个凭证，这正是"核心越俎代庖"最贵的形态。
//
//	token 剩 4m 且某上游的窗口比 10m 更宽 → 核心判 false → 永不续期 → 必然 401。
//
// 所以判据必须和"怎么刷"待在同一个地方：上游。核心只问，不做判断。
//
// # 三条回落路径
//
//	Provider == nil        → 单上游：核心的 skew 就是那个上游的事实
//	取不到凭证             → 接线问题（与选号无关），不刷，出站会立刻报错换号
//	上游未上报窗口         → 回落到核心的通用兜底（明确的保守值）
//
// # 与"取不到凭证"为什么返回 false 而不是报错
//
// 本函数只回答"要不要刷"，不负责报告接线问题。取不到凭证时不刷，
// 紧接着的 `chatVia` 会在同一次迭代里拿到同一个 errNoProviderCredential
// 并按传输错误换号（**不惩罚账号**），语义与改造前一致。
// 在这里提前报错反而会让两条路径的错误处理分叉。
func (h *Handler) needsRefreshVia(providerID string, acct *auth.Auth) bool {
	if h.cfg.Provider == nil {
		// 单上游：没有别的上游可问，核心的窗口就是这个上游的事实。
		return acct.NeedsRefresh(h.cfg.RefreshSkew)
	}
	cred, ok := h.cfg.Provider.Credential(providerID, acct.UID)
	if !ok {
		// 取不到凭证 = 接线问题，不是"该刷了"。出站会报错换号。
		return false
	}
	skew, has := h.cfg.Provider.RefreshSkew(providerID, cred)
	if !has {
		// 上游只说了"怎么刷"，没说"多早刷" → 用核心的通用兜底。
		// 这是**明确的**保守值，不是"核心假装知道上游的寿命"。
		return refreshDueAt(acct, cred, h.cfg.RefreshSkew)
	}
	if skew <= 0 {
		// 上游明确声明"不需要提前刷"（只在 401 后被动续期）。
		// ⚠ 必须尊重它，不能用通用窗口覆盖 —— 那正是本函数要修的那类错误。
		return false
	}
	return refreshDueAt(acct, cred, skew)
}

// refreshDueAt 判"距过期不足 within" —— **优先用活凭证的过期时刻**。
//
// # 为什么不能直接 `acct.NeedsRefresh(within)`（实测的真 bug）
//
// `acct` 是**池投影**，而各上游的投影都刻意不抄 ExpiresAt
// （见 raccooncreds.go / clinecreds.go 的「投影纪律」：投影是启动快照，
// 活 secret 被续期后投影不会跟新）。实测 `/admin/accounts` 里
// `expires_at` 键**一个上游都没有** ⇒ `acct.ExpiresAt` 恒为 0。
//
// 而 `auth.NeedsRefresh` 对 `ExpiresAt <= 0` **恒返回 true**：
//
//	func (a *Auth) NeedsRefresh(within time.Duration) bool {
//		if a.ExpiresAt <= 0 { return true }   // ← 恒真
//		return time.Now().Add(within).Unix() >= a.ExpiresAt
//	}
//
// 于是"该不该刷"这个判断**对每个上游都恒为真** ⇒ 每个 chat 请求先做一次
// 续期往返。危害按 refresh_token 的形态分两档：
//
//	可重复使用（cline 等）→ 白费一次往返
//	**一次性**（raccoon / codearts）→ **每次请求烧掉一个 token**
//
// 实测 cline：发一次对话请求后 access_token **被换了**。
// raccoon 更严重：一次性 token 被刷爆 → 续期连续失败 → 账号禁用
// （正是用户报障的那个形态）。
//
// 所以这里优先读 `cred.ExpiresAt` —— 装配层已用
// `gateway.CredentialExpiryExt` 把它从活 secret 上补齐（8 个上游都实现了
// 该扩展点）。只有拿不到时才回落池投影。
//
// # 仍拿不到过期信息时保持**原有**行为（要刷）
//
// 那是"完全不知道什么时候过期"的上游。既有取舍是保守地刷
// （宁可多一次往返，也不要拿一个可能过期的凭证去发请求，让服务端回 401）。
// 本次**不改**这个取舍 —— 修的是"明明知道却不读"。
func refreshDueAt(acct *auth.Auth, cred gateway.Credential, within time.Duration) bool {
	at := cred.ExpiresAt.Unix()
	if at <= 0 && acct != nil {
		at = acct.ExpiresAt
	}
	if at <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= at
}

// applyErrorPolicy 按分类施加惩罚（冷却/禁用/熔断），并**顺带记录上游的解读**。
//
// # 为什么解读在这里记，而不是在分类器里
//
// 分类器是**纯函数**（契约要求：不发网络、不写状态），而"记到账号上"是状态变更。
// 出站循环是唯一同时掌握 (uid, status, body, provider) 的地方，所以记录点在这里。
//
// # provider / status / body 从哪来
//
// 三者都是出站循环手里已有的东西（它刚拿它们做过分类）。多传三个参数是为了
// 让**解读与分类用同一份输入** —— 若让解读自己去别处取数据，就会出现
// "因为余额不足被冷却、界面却说未知错误"这种自相矛盾。
//
// provider 为空时（单上游回落路径）跳过解读查询：那条路的分类器是
// upstream.Classify，它没有配套的 ErrorExplainExt。
func (h *Handler) applyErrorPolicy(uid string, kind gateway.ErrorKind, provider string, status int, body []byte) {
	// 先记解读（与分类同源，见上）。拿不到就不记 —— 界面回落通用文案。
	h.noteErrorExplain(uid, provider, status, body)

	switch kind {
	case gateway.ErrKindHardCredit:
		// 402 + 额度耗尽关键词：冷却到**上游给出的下次重置时刻**，立即换号。
		//
		// 早先这里直接调 pool.CooldownUntilTomorrow4AM —— 把 workbuddy 的
		// 「次日 04:00 等签到恢复」写进了出口层。现在改为向 Provider 要时刻：
		// 出口层只问"这个号什么时候能再用"，具体策略由上游定义
		// （见 Config.NextResetAt）。
		//
		// ⚠ 传 uid 而不是"当前请求的上游"：这一行**只拿得到出错的号**，
		// 而"这个号属于哪个上游"是账号池的事实 —— 由 nextResetAt 反查
		// （Pool.ProviderOf）。早先它无从反查，于是所有上游共用一个时刻。
		h.cfg.Pool.CooldownUntilNextReset(uid, h.nextResetAt(uid), "额度不足")
	case gateway.ErrKindSoftRate:
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.cfg.SoftCooldown, "429 rate limit")
	case gateway.ErrKindSessionDead:
		// ⚠ 只有 workbuddy 会产生这个分类（它的 401 + 12153）。
		// codearts 的凭证失效是 ErrKindAuth，走 default 的"只换号不罚"——
		// 见本函数上方的长注释（危害 ② 的另一半）。
		//
		// # 为什么不是一次就 Disable（借鉴 omni2api-panel）
		//
		// 12153 的成因里有一大类是**瞬时抖动**（网络闪断、上游瞬时故障、
		// token 刷新竞态）。改造前一次就永久禁用，于是一次抖动
		// 就能让一个健康的号掉出池子，而界面上只显示"已禁用"，
		// 没有任何线索指向真因。
		//
		// 现在交给 Pool.NoteSessionDead 计数门控：连续 3 次才禁用，
		// 任何一次成功都清零。真正失效的 session（每次都会 12153）
		// 仍然会被停掉，行为与改造前一致。
		h.cfg.Pool.NoteSessionDead(uid)
	case gateway.ErrKindNotFound:
		// 404 短冷却（软冷却），防雪崩。
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.cfg.SoftCooldown, "upstream 404")
	case gateway.ErrKindServer:
		// 5xx 上游故障：分类器已把 ≥500 判为 server，在此喂熔断计数（不再手写 status>=500）。
		h.cfg.Pool.NoteError(uid)
	default:
		// 其余（ErrKindAuth/ErrKindClient/ErrKindNone）：只换号不罚（防雪崩），不喂熔断。
	}
}

// noteErrorExplain 向上游索取错误解读并记到账号上（拿不到就什么都不做）。
//
// # 为什么失败必须静默
//
// 解读是**尽力而为的展示层增强**，不是正确性的一部分：
// 上游没实现 ErrorExplainExt、或它不认识这个错误，都不该影响
// 冷却/换号这些真正影响可用性的动作。所以这里只记日志、不返回错误。
//
// 但反过来，**拿到了就一定记**：用户要靠它分辨"这个号是没钱了还是被踢了"，
// 而那正是他看账号池时最需要回答的问题。
func (h *Handler) noteErrorExplain(uid, provider string, status int, body []byte) {
	if h.cfg.Pool == nil || uid == "" || provider == "" {
		return
	}
	ex, ok := h.cfg.Provider.Explain(provider, status, string(body))
	if !ok {
		return
	}
	h.cfg.Pool.NoteErrorExplain(uid, pool.ErrorExplainView{
		Code:       ex.Code,
		HTTPStatus: ex.HTTPStatus,
		Kind:       ex.Kind.String(),
		Summary:    ex.Summary,
		Detail:     ex.Detail,
		At:         time.Now(),
	})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// poolUnavailableHint 概括"这个上游域为什么选不出号"（≤3 个号时逐个给
// 状态；多了只给计数，消息不被刷屏）。空串 = 没信息可加。
//
// 只暴露冷却原因/时刻与禁用原因 —— 都是管理台可见信息，不含凭证内容。
func (h *Handler) poolUnavailableHint(provider string) string {
	if h.cfg.Pool == nil {
		return ""
	}
	return poolUnavailableHintOf(h.cfg.Pool.ListFor(provider))
}

// poolUnavailableHintOf 纯函数版（测试直接钉）。
func poolUnavailableHintOf(list []pool.Status) string {
	if len(list) == 0 {
		return "该上游池内暂无账号（先在控制台导入凭证或添加账号）"
	}
	if len(list) > 3 {
		nDis, nCool := 0, 0
		for _, st := range list {
			switch {
			case st.Disabled:
				nDis++
			case st.Cooling:
				nCool++
			}
		}
		return fmt.Sprintf("%d 个账号：%d 禁用、%d 冷却", len(list), nDis, nCool)
	}
	parts := make([]string, 0, len(list))
	for _, st := range list {
		uid := st.UID
		if len([]rune(uid)) > 8 {
			uid = string([]rune(uid)[:8])
		}
		switch {
		case st.Disabled:
			r := st.Reason
			if r == "" {
				r = "已禁用"
			}
			parts = append(parts, fmt.Sprintf("%s 禁用(%s，需启用或重新登录)", uid, r))
		case st.Cooling:
			r := st.Reason
			if r == "" {
				r = "冷却中"
			}
			tail := ""
			if !st.Until.IsZero() {
				tail = "，约 " + st.Until.Local().Format("15:04") + " 自动恢复"
			}
			parts = append(parts, fmt.Sprintf("%s %s%s", uid, r, tail))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "；")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}
