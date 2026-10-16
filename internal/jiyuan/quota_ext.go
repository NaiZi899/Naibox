// quota_ext.go 基元律动实现 gateway.QuotaExt —— "我的额度怎么取"。
//
// # 为什么本上游的额度必须自己一条路（用户明确要求）
//
// 用户原话：「jiyuan 这个模块不要跟其他上游共用余额接口即可」。
//
// 这条要求不只是偏好 —— 它对应一个**硬约束**：基元律动的余额只有
// 登录凭证（`tr_session`）读得到，而其它上游各有自己的余额来源
// （workbuddy 积分接口、codearts 信用包、loomy 积分网关）。
// 强行共用只会造出一个"什么字段都有、但每个上游只填一部分"的中间层，
// 那正是 gateway/credential.go 里反复拒绝的那种设计。
//
// 所以本文件**独立**走基元律动的 `/api/usage-summary`，不复用任何
// 其它上游的余额代码，核心侧也只走通用的 QuotaExt 接缝
// （quota_refresh.go：按账号自己的 provider 去 registry 找实现）。
//
// # 为什么余额只能靠登录凭证（实测，穷举过）
//
//	Authorization: Bearer sk_…  → /api/me、/api/usage-summary、
//	                              /api/api-keys 全部 **401 UNAUTHORIZED**
//	Cookie: tr_session=sk_…     → 同样 401
//	/v1/credits /v1/balance /dashboard/billing/credit_grants → **404**
//	/dashboard/billing/usage    → 401「请重新登录看板后查询」
//	推理响应里的 cost_cny       → 只是**本次调用**费用，不是余额
//
// 结论：**没有第二条路**。要让额度列有值，账号就必须带 Session。
// 这正是 Auth.Session 字段存在的唯一理由（见 credential.go 的长注释）。
//
// # 单位：分（int64）而不是元
//
// `pool.QuotaView.Remaining` 是 **int64**，而基元律动的余额是小数金额
// （实测 `"availableBalanceCny":"99.74987600"`）。
//
// 直接 `int64(余额)` 是**错的**：¥0.50 会被截成 `0` —— 一个"查到了余额，
// 界面却显示 0"的镜像 bug，而本项目已经为这类"把有值显示成无值"的
// 缺陷付过几次代价（见 import.go 里 fetchBalance 从未被执行那条）。
//
// 所以本上游以**分**为单位上报：`99.75 元 → 9975`。
// 选分而不是元，是因为它是"金额的最小常见单位"，精度足够且仍是整数，
// 不需要动 pool 的类型（那是核心接口，改它的影响面远大于本上游内部）。
//
// ⚠ 代价（如实记录）：这让"额度"这一列在不同上游之间**量纲不同**
// （workbuddy/codearts 是积分，jiyuan 是分）。界面上两者都只显示一个数字，
// 用户看到 jiyuan 的 `9975` 需要知道那是分。这是本方案已知的瑕疵 ——
// 换来的是不用改核心接口就能显示真实余额。若将来要统一展示，
// 正确的做法是给 QuotaView 加一个显式的 unit 字段，而不是在这里
// 私自换单位。
package jiyuan

import (
	"context"
	"log"
	"math"

	"github.com/NaiZi899/omni2api/internal/gateway"
)

// quotaTimeoutSec 额度刷新的 ctx 超时（与 sessionTimeout 同量级：
// 这是给人点的短操作，不该挂住控制台）。
const quotaTimeoutSec = 25

// RefreshQuota 实现 gateway.QuotaExt：读某账号的实时余额。
//
// 三态返回（与 workbuddy / codearts 完全同形，判据必须一致）：
//
//	ok=false          这条 uid 不是本上游的（池未接线 / 号不存在）
//	ok=true,  HasData 读到余额 → 调用方写回池
//	ok=true, !HasData 读不到（无 Session / sess_ 过期 / 上游 5xx）
//	                  → **界面显示 `—`，不填 0**
func (p *Provider) RefreshQuota(uid string) (gateway.QuotaView, bool) {
	a, ok := p.credential(uid)
	if !ok || a == nil {
		// 契约里的 ok=false = "这条 uid 我服务不了"。调用方据此跳过或回 404，
		// 而不是把一个"问不到"当成"额度是 0"。
		return gateway.UnknownQuota(), false
	}

	// 没有 Session 的账号**读不到余额**（实测：key 打管理面全 401）。
	//
	// ⚠ 这里必须返回**未知**而不是 0 或快照：快照是一个历史值，
	// 拿它当"当前额度"会让用户以为余额没变（见 Auth.Balance 的警告）。
	// 显示 `—` 是诚实的 —— 它准确表达"我们不知道现在的余额"。
	if a.Session == "" {
		// 不写日志：这是**正常情况**（用户选择只用 key 导入），
		// 不是故障。给它刷日志会把真正的失败淹掉。
		return gateway.UnknownQuota(), true
	}

	ctx, cancel := context.WithTimeout(context.Background(), quotaTimeoutSec*1e9)
	defer cancel()

	bal, err := fetchBalance(ctx, a.Session)
	if err != nil {
		// 取不到 → 未知。**不返回 0**，也**不沿用旧值**。
		log.Printf("jiyuan: 额度刷新失败 uid=%s: %v（显示为未知，不填 0）", uid, err)
		return gateway.UnknownQuota(), true
	}

	// 币种非 CNY 时**拒绝换算**：本上游实测恒为 CNY，出现别的币种说明
	// 上游改了行为，此时把它当"分"上报会是一个**错了一个数量级**的数字。
	// 宁可显示未知，也不要显示一个看起来合理但错误的金额。
	if bal.Currency != "" && bal.Currency != "CNY" {
		log.Printf("jiyuan: 额度币种不是 CNY（得到 %q）uid=%s —— "+
			"本适配层按分上报，不认识的币种不换算（显示为未知）", bal.Currency, uid)
		return gateway.UnknownQuota(), true
	}

	return gateway.CreditsQuotaUnit(yuanToFen(bal.Available), gateway.QuotaUnitFen), true
}

// yuanToFen 把"元"金额转成**分**（int64）。
//
// 取 Available（可用余额）而不是 Total：能花的是可用余额，
// 冻结部分花不出去 —— 拿 Total 展示会让用户以为还能用那么多。
// 这与 import.go 的取法一致（先 Available，为 0 才回落 Total）。
//
// 负数（上游可能返回欠款）截到 0：负额度在展示上无意义，
// 且会污染 pool 的选号权重（负数权重比无记录更糟）。
//
// 用 math.Round 而不是截断：金额的小数位是**真实精度**（分），
// 不是"不值得展示的尾数"。99.749876 元 = 9974.9876 分 → 9975 分。
// 这与 codearts 的 `int64(CreditRemain)`（积分，截断）刻意不同 ——
// 那里的 CreditRemain 是积分计数，小数位确实是噪声。
func yuanToFen(yuan float64) int64 {
	if math.IsNaN(yuan) || math.IsInf(yuan, 0) {
		return 0
	}
	fen := math.Round(yuan * 100)
	if fen <= 0 {
		return 0
	}
	// 防溢出：int64 装得下的分 ≈ 9.2e16 元，现实金额不可能触及。
	// 但仍设一道闸，免得异常上游把 maxInt64 塞进权重计算。
	const maxFen = int64(1) << 62
	if fen > float64(maxFen) {
		return maxFen
	}
	return int64(fen)
}

// compile-time 断言：Provider 实现额度扩展点。
//
// 不实现会在这里编译失败，而不是等到运行时"界面上额度一直空着"。
var _ gateway.QuotaExt = (*Provider)(nil)
