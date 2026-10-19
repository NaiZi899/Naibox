// errorclassifier.go 基元律动的错误分类与解读（gateway.ErrorClassifier +
// gateway.ErrorExplainExt）。
//
// # 上游的错误信封（实测）
//
// 与标准 OpenAI 的 `{"error":{...}}` **不同**，基元律动用自己的信封：
//
//	{"code":"UNAUTHORIZED","message":"未认证或登录已过期","traceId":"trace_…"}
//	{"code":"INSUFFICIENT_BALANCE","message":"余额不足",
//	 "data":{"retryAfterSeconds":60},"traceId":"trace_…"}
//	{"code":"NOT_FOUND","message":"接口不存在","traceId":"trace_…"}
//
// 注意 `code` 是**字符串枚举**（不是数字）—— 分类直接按它匹配即可，
// 不需要像 workbuddy 那样把数字码写在注释里靠人背。
//
// # 为什么分类与解读在同一个文件里
//
// 两者**必须来自同一份判据**（见 gateway/errorexplain_ext.go 的说明）：
// 分类决定"怎么办"，解读决定"界面上怎么说"。分开写会漂移成
// "因为余额不足被冷却，界面却说是参数错误"。
// 所以这里的关键词匹配只写一遍（matchKind），两边共用。
//
// ⚠ 这也是实测踩过的坑的教训：验证脚本曾用 `'error' in d` 判断成功，
// 于是把 402 误报成 OK（响应里没有 choices，content 取到空串）。
// 判据必须落在**顶层 code** 上。
package jiyuan

import (
	"encoding/json"
	"strings"

	"github.com/NaiZi899/Naibox/internal/gateway"
)

// upstreamError 基元律动的错误信封（顶层 code，不是 OpenAI 的 error 对象）。
type upstreamError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    struct {
		RetryAfterSeconds int `json:"retryAfterSeconds"`
	} `json:"data"`
}

// parseUpstreamError 尝试把响应体解成上游错误信封。
//
// 返回 ok=false 表示"这不是上游错误信封"（可能是成功响应、或别的形状），
// 此时调用方只按状态码判断。
func parseUpstreamError(body string) (upstreamError, bool) {
	trim := strings.TrimSpace(body)
	if trim == "" || trim[0] != '{' {
		return upstreamError{}, false
	}
	var e upstreamError
	if err := json.Unmarshal([]byte(trim), &e); err != nil {
		return upstreamError{}, false
	}
	// 必须有 code 或 message 之一才算识别成功。否则是一个碰巧能解成
	// 空结构体的 JSON（比如成功响应 `{"object":"list",...}`）。
	if strings.TrimSpace(e.Code) == "" && strings.TrimSpace(e.Message) == "" {
		return upstreamError{}, false
	}
	return e, true
}

// matchKind 把 (状态码, 错误码, 原文) 归到一类。
//
// **这是本上游唯一的判据来源** —— Classify 与 Explain 都走它，
// 所以"怎么处置"与"怎么说"永远一致。
//
// 判据优先级：上游错误码（最精确）→ 中文关键词（兜底，因为 message 是
// 中文而 code 偶尔缺失）→ HTTP 状态码（最后兜底）。
func matchKind(status int, e upstreamError, rawLower string) gateway.ErrorKind {
	code := strings.ToUpper(strings.TrimSpace(e.Code))

	// ── 1. 按上游错误码（精确）──
	switch code {
	case "INSUFFICIENT_BALANCE", "INSUFFICIENT_QUOTA", "QUOTA_EXCEEDED", "BALANCE_NOT_ENOUGH":
		return gateway.ErrKindHardCredit
	case "UNAUTHORIZED", "TOKEN_EXPIRED", "INVALID_API_KEY", "API_KEY_DISABLED":
		return gateway.ErrKindSessionDead
	case "RATE_LIMIT", "RATE_LIMITED", "TOO_MANY_REQUESTS":
		return gateway.ErrKindSoftRate
	case "NOT_FOUND":
		return gateway.ErrKindNotFound
	case "BAD_REQUEST", "INVALID_REQUEST", "MODEL_NOT_FOUND", "UNSUPPORTED_MODEL":
		return gateway.ErrKindClient
	case "INTERNAL_ERROR", "SERVER_ERROR", "UPSTREAM_ERROR":
		return gateway.ErrKindServer
	case "CONTENT_FILTER", "CONTENT_BLOCKED":
		return gateway.ErrKindContentBlocked
	}

	// ── 2. 中文关键词兜底（code 缺失时 message 仍是中文）──
	switch {
	case strings.Contains(rawLower, "余额不足") || strings.Contains(rawLower, "欠费") ||
		strings.Contains(rawLower, "insufficient_balance"):
		return gateway.ErrKindHardCredit
	case strings.Contains(rawLower, "未认证") || strings.Contains(rawLower, "登录已过期") ||
		strings.Contains(rawLower, "unauthorized"):
		return gateway.ErrKindSessionDead
	case strings.Contains(rawLower, "请求过于频繁") || strings.Contains(rawLower, "rate limit"):
		return gateway.ErrKindSoftRate
	case strings.Contains(rawLower, "内容") && strings.Contains(rawLower, "违规"):
		return gateway.ErrKindContentBlocked
	}

	// ── 3. HTTP 状态码兜底（与 gateway.DefaultErrorKind 同一判据）──
	switch {
	case status == 402:
		// 上游没给可识别 code 的 402：仍然按"没钱了"处理。
		// 402 在 HTTP 语义里就只有这一种含义。
		return gateway.ErrKindHardCredit
	case status == 429:
		return gateway.ErrKindSoftRate
	case status == 401 || status == 403:
		// 本上游只有 key 一种凭证，401/403 没有第二种解释。
		return gateway.ErrKindSessionDead
	case status == 404:
		return gateway.ErrKindNotFound
	case status >= 500:
		return gateway.ErrKindServer
	case status >= 400:
		return gateway.ErrKindClient
	}
	return gateway.ErrKindNone
}

// Classify 实现 gateway.ErrorClassifier（状态码 + 响应体）。
//
// ⚠ 必须是纯函数：不发网络请求、不读写共享状态。它在出站循环的每次
// 非 2xx 上被调用一次。
func (p *Provider) Classify(status int, body string) gateway.ErrorKind {
	e, _ := parseUpstreamError(body)
	return matchKind(status, e, strings.ToLower(body))
}

// Explain 实现 gateway.ErrorExplainExt —— 把错误读成人话。
//
// # 为什么让上游报而核心不写表
//
// 状态码是各上游自定义的字符串（这里是 `INSUFFICIENT_BALANCE`，
// workbuddy 是 12153，codearts 是 `InferHub.4291`），核心写不出来。
// 而用户要的"状态码解读"恰恰就是这一层 —— 见 gateway/errorexplain_ext.go。
func (p *Provider) Explain(status int, body string) (gateway.ErrorExplain, bool) {
	e, hasEnv := parseUpstreamError(body)
	kind := matchKind(status, e, strings.ToLower(body))

	// 既没有上游码、又分不出有意义类别 → 如实说"我不认识"。
	// 编造解读比沉默更糟：用户会据此做一个错误的动作。
	if kind == gateway.ErrKindNone && !hasEnv && status < 400 {
		return gateway.ErrorExplain{}, false
	}

	ex := gateway.ErrorExplain{
		Code:       strings.TrimSpace(e.Code),
		HTTPStatus: status,
		Kind:       kind,
	}
	if ex.Code == "" && status > 0 {
		// 上游没给码：把 HTTP 状态码当"码"展示，至少能对上日志。
		ex.Code = "HTTP " + itoaStatus(status)
	}

	switch kind {
	case gateway.ErrKindHardCredit:
		ex.Summary = "余额不足"
		ex.Detail = "基元律动是**预付费**钱包：这把 key 的余额已耗尽，" +
			"调用会被 402 拒绝。需要充值（或换一把有余额的 key）后才会恢复。" +
			retryHint(e)
	case gateway.ErrKindSessionDead:
		ex.Summary = "key 已失效"
		ex.Detail = "上游返回未认证：这把 key 被删除、停用，或格式不对。" +
			"基元律动的 key 没有续期机制，**必须换一把新 key** —— " +
			"在用户中心的 API Key 页重新生成，然后到本页「批量导入」。" +
			"（连续 3 次失败后该账号会被自动禁用。）"
	case gateway.ErrKindSoftRate:
		ex.Summary = "触发限流"
		ex.Detail = "上游要求降速。网关已按 429 做软冷却（指数退避），" +
			"到期自动恢复；期间请求会换到其它 key。" + retryHint(e)
	case gateway.ErrKindServer:
		ex.Summary = "上游故障"
		ex.Detail = "上游返回 5xx，是**基元律动侧**的问题，不是这把 key 的问题。" +
			"网关会累计熔断计数（连续 3 次后暂时摘除该账号）；稍后会自动重试。"
	case gateway.ErrKindNotFound:
		ex.Summary = "上游 404"
		ex.Detail = "请求的路径或模型在上游不存在。对账号本身通常是一次短暂异常，" +
			"网关只做短冷却，不累计熔断计数。"
	case gateway.ErrKindContentBlocked:
		ex.Summary = "内容被拦截"
		ex.Detail = "上游内容策略拦截了这次请求。**换 key 没有意义** —— " +
			"所有 key 背后是同一套策略；网关会改用降级提示词重试一次。"
	case gateway.ErrKindClient:
		ex.Summary = "请求被拒"
		ex.Detail = describeClientError(e, status)
	default:
		ex.Summary = "未知错误"
		ex.Detail = "网关不认识这个错误，按「只换号不惩罚」处理（保守方向）。" +
			"如果这类错误反复出现，请把它反馈给维护者补充判据。"
	}

	return ex, true
}

// describeClientError 给 4xx 类错误一句更具体的解读（能说清就说清）。
func describeClientError(e upstreamError, status int) string {
	switch strings.ToUpper(strings.TrimSpace(e.Code)) {
	case "MODEL_NOT_FOUND", "UNSUPPORTED_MODEL":
		return "上游不认识请求里的模型名。检查模型是否已下线 —— " +
			"网关的模型目录是实时拉取的，可点「刷新目录」重新同步。"
	case "INVALID_REQUEST", "BAD_REQUEST":
		return "请求体不合法（缺字段 / 参数越界）。客户端侧的问题，" +
			"网关会换号重试，但同样的请求在任何 key 上都会失败。"
	}
	if status == 400 {
		return "上游拒绝了这次请求（400）。通常是请求参数问题；" +
			"网关会换号重试一次，若所有 key 都回 400 则应检查请求本身。"
	}
	return "上游返回 " + itoaStatus(status) + "。网关按「只换号不惩罚」处理 ——" +
		"这类错误不清空该 key 的健康度。"
}

// retryHint 附上上游给的 retryAfterSeconds（有就写，没有不提）。
//
// 这个字段实测出现在流式 402 的回执里 —— 是上游自己对"多久后再试"的
// 判断，比我们猜的更有依据，所以值得透出来。
func retryHint(e upstreamError) string {
	if e.Data.RetryAfterSeconds <= 0 {
		return ""
	}
	return "（上游提示 " + itoaStatus(e.Data.RetryAfterSeconds) + " 秒后可重试。）"
}

// itoaStatus 小整数转字符串（避免为一句文案引入 strconv 之外的复杂度）。
func itoaStatus(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// compile-time: Provider 实现错误分类与解读。
var (
	_ gateway.ErrorClassifier = (*Provider)(nil)
	_ gateway.ErrorExplainExt = (*Provider)(nil)
)
