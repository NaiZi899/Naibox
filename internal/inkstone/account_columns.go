// account_columns.go 书生·端砚在账号池里显示的列（gateway.AccountColumnsExt）。
//
// 与 loomy 的取舍一致：不显示「Token（剩余期）」「今日签到」。
// 书生端砚的 API Key 默认 6 个月有效，但我们无法从 key 本身精确得知到期时刻，
// 故不占 token_expiry 列（显示 — 没意义）。无签到/福利概念，故无 checkin/welfare 列。
package inkstone

import "github.com/NaiZi899/Naibox/internal/gateway"

// AccountColumns 实现 gateway.AccountColumnsExt。
func (p *Provider) AccountColumns() []string {
	return []string{
		gateway.AccountColProvider,
		gateway.AccountColNickname,
		gateway.AccountColUID,
		gateway.AccountColQuota,
		gateway.AccountColStatus,
		gateway.AccountColSuccess,
		gateway.AccountColOps,
	}
}

// compile-time: Provider 实现账号列自报。
var _ gateway.AccountColumnsExt = (*Provider)(nil)
