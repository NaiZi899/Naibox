// quota_ext.go 书生·端砚的额度查询（gateway.QuotaExt）。
//
// # 额度数据源
//
// 墨点余额在管理 API `GET /api/tokenplan/v1/credits/balance`，鉴权用 **SSO uaa-token**
// （不是 API Key）。逆向实测：
//
//	用 API Key 调 balance → {"msgCode":"A0211","msg":"user token expired"}
//	用 uaa-token 调      → {"code":0,"data":{"available_credits":"10.000000",...}}
//
// 因此只有当该账号凭证里**配置了 user_token** 时本扩展点才能给出真值；
// 否则（用户只给 API Key、没给 uaa-token）返回 UnknownQuota —— 界面额度列显示 `—`，
// 并打一条信息日志说明原因，绝不拿一个 0 当"查到了"。
package inkstone

import (
	"context"
	"strings"
	"time"

	"github.com/NaiZi899/Naibox/internal/gateway"
)

// RefreshQuota 取某账号的墨点余额（gateway.QuotaExt）。
//
//	ok=false            uid 为空
//	ok=true + 有数据     凭证里配了 user_token 且查到 balance → 写回池
//	ok=true + 取不到     没配 user_token 或查询失败 → HasData=false，界面 `—`
func (p *Provider) RefreshQuota(uid string) (gateway.QuotaView, bool) {
	if strings.TrimSpace(uid) == "" {
		return gateway.UnknownQuota(), false
	}
	a, ok := p.credential(uid)
	if !ok || a == nil {
		return gateway.UnknownQuota(), true
	}
	if strings.TrimSpace(a.UserToken) == "" {
		// 没配 uaa-token：额度未知是唯一诚实的答案，别拿 0 充数。
		return gateway.UnknownQuota(), true
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, err := balance(ctx, a.UserToken)
	if err != nil {
		// 查询失败（token 过期/网络/上游 5xx）→ 未知，不写池。
		return gateway.UnknownQuota(), true
	}
	credits, perr := ParseAvailableCredits(b.AvailableCredits)
	if perr != nil {
		return gateway.UnknownQuota(), true
	}
	return gateway.CreditsQuota(clampNonNegative(credits)), true
}

func clampNonNegative(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}

// compile-time: Provider 实现额度扩展点。
var _ gateway.QuotaExt = (*Provider)(nil)
