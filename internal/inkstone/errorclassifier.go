// errorclassifier.go 书生·端砚的错误分类（gateway.ErrorClassifier）。
//
// 书生端砚推理端点回标准 OpenAI 错误信封：
//
//	{"error":{"message":"...","type":"...","code":"invalid_api_key"}}
//
// 已知码（推理实测）：
//
//	invalid_api_key           API Key 无效/被禁用 → 会话失效，永久禁用（需重建 key）
//	model_not_available       该模型在 TokenPlan 上不可用 → 只换号不罚
//	"user token expired"      管理 API 的 uaa-token 失效（不影响推理，额度查询才碰）
//
// 拿不准的返回 ErrKindNone（"我不认识"比"猜错"安全，见 gateway 包注释）。
package inkstone

import (
	"strings"

	"github.com/NaiZi899/omni2api/internal/gateway"
)

// Classify 实现 gateway.ErrorClassifier（状态码 + 响应体）。
func (p *Provider) Classify(status int, body string) gateway.ErrorKind {
	body = strings.ToLower(body)

	// 会话/凭证失效：key 无效或被平台停用。永久禁用（需重建 key）。
	if strings.Contains(body, "invalid_api_key") ||
		strings.Contains(body, "invalid api key") ||
		strings.Contains(body, "auth_error") ||
		strings.Contains(body, "unauthorized") && status >= 400 {
		return gateway.ErrKindSessionDead
	}

	// 模型在 TokenPlan 不可用 → 参数/选择问题，只换号不罚。
	if strings.Contains(body, "model_not_available") ||
		strings.Contains(body, "not supported by tokenplan") {
		return gateway.ErrKindClient
	}

	// 通用状态码兜底（与 gateway.DefaultErrorKind 一致）。
	switch {
	case status == 402:
		return gateway.ErrKindHardCredit
	case status == 429:
		return gateway.ErrKindSoftRate
	case status == 404:
		return gateway.ErrKindNotFound
	case status >= 500:
		return gateway.ErrKindServer
	case status >= 400:
		return gateway.ErrKindClient
	}
	return gateway.ErrKindNone
}

// compile-time: Provider 实现错误分类。
var _ gateway.ErrorClassifier = (*Provider)(nil)
