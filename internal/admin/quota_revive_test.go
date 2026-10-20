package admin

import (
	"testing"
	"time"

	"github.com/NaiZi899/Naibox/internal/auth"
	"github.com/NaiZi899/Naibox/internal/gateway"
	"github.com/NaiZi899/Naibox/internal/pool"
)

// ---------------------------------------------------------------------------
// 本文件守住"充值后自动解冻"的边界。
//
// 额度刷新成功后要不要把一个冷却中的账号放回池子，取决于**余额是否就是
// 那个上游的可用性门禁**。这个判据由上游自己声明（gateway.QuotaUsabilityExt），
// 核心不猜。
//
// 猜错的两个方向都很难受：
//
//	太保守  jiyuan 充值了，账号却卡在"余额耗尽"冷却里出不来，要人工清
//	太激进  workbuddy 凭证已死的号被每轮刷新解冻一次 → 稳定的 401 重试风暴
//
// 下面五条分别钉住这两侧，以及"别把软限流也一起放了"。
// ---------------------------------------------------------------------------

// quotaUsableStub 实现了 QuotaExt + QuotaUsabilityExt 的上游。
//
// 单独一个类型而不是给 quotaStub 加字段：一旦 quotaStub 实现了
// QuotaImpliesUsable，**所有**既有用例里的 stub 都会变成"余额即门禁"，
// 那会静默改变它们测的东西（它们本来是"没有这个扩展点"的对照组）。
type quotaUsableStub struct {
	*quotaStub
	usable bool
}

func (s *quotaUsableStub) QuotaImpliesUsable() bool { return s.usable }

// coolingHard 把某个账号打成"余额耗尽"型硬冷却。
func coolingHard(t *testing.T, p *pool.Pool, uid string) {
	t.Helper()
	p.Cooldown(uid, pool.CoolHard, 30*time.Minute, "余额不足")
	st, ok := p.Status(uid)
	if !ok || !st.Cooling {
		t.Fatalf("夹具前提不成立：%s 应处于冷却中，实际 %+v", uid, st)
	}
}

// TestQuotaRefreshRevivesWhenQuotaImpliesUsable 余额恢复 + 上游声明"余额即门禁" → 解冻。
//
// 这是"充值后不用手动清冷却"的正向路径：用户充了钱，下一轮刷新（最长 15 分钟）
// 账号自己回到可用状态。
func TestQuotaRefreshRevivesWhenQuotaImpliesUsable(t *testing.T) {
	stub := &quotaUsableStub{
		quotaStub: &quotaStub{id: "jiyuan", view: gateway.CreditsQuotaUnit(6674, "分"), ok: true},
		usable:    true,
	}
	h, p, _ := newQuotaTestHandler(t, stub)
	p.AddFor("jiyuan", &auth.Auth{UID: "jy-1"}, nil)
	coolingHard(t, p, "jy-1")

	res := h.refreshQuotas([]string{"jy-1"})

	if res.Updated != 1 {
		t.Fatalf("应写入 1 个账号的额度，得到 updated=%d unknown=%d failed=%d",
			res.Updated, res.Unknown, res.Failed)
	}
	if res.Revived != 1 {
		t.Fatalf("余额已恢复且上游声明余额即门禁，应解冻 1 个账号，得到 revived=%d", res.Revived)
	}
	if st, _ := p.Status("jy-1"); st.Cooling {
		t.Error("解冻后不该仍在冷却中 —— 用户已经充钱了，却要等到有人手动清冷却")
	}
}

// TestQuotaRefreshKeepsCoolingWhenBalanceZero 余额仍是 0 → **不**解冻。
//
// ⚠ 这条是防"每轮刷新都解冻一次"的关键：余额 0 的号被放回池子，
// 会立刻再吃 402/403，然后重新进冷却，每 15 分钟一遍。
func TestQuotaRefreshKeepsCoolingWhenBalanceZero(t *testing.T) {
	stub := &quotaUsableStub{
		quotaStub: &quotaStub{id: "jiyuan", view: gateway.CreditsQuota(0), ok: true},
		usable:    true,
	}
	h, p, _ := newQuotaTestHandler(t, stub)
	p.AddFor("jiyuan", &auth.Auth{UID: "jy-2"}, nil)
	coolingHard(t, p, "jy-2")

	res := h.refreshQuotas([]string{"jy-2"})

	if res.Updated != 1 {
		t.Fatalf("查到了余额（哪怕是 0）也算 updated，得到 updated=%d", res.Updated)
	}
	if res.Revived != 0 {
		t.Errorf("余额为 0 时不该解冻，得到 revived=%d —— 那会让它立刻再吃一次 402", res.Revived)
	}
	if st, _ := p.Status("jy-2"); !st.Cooling {
		t.Error("余额 0 的账号必须保持冷却，否则会形成稳定的失败重试")
	}
}

// TestQuotaRefreshDoesNotReviveWithoutUsabilityExt 上游没声明 → 行为与改造前一致。
//
// workbuddy 就是这一档：积分只是计费口径，凭证本身有独立生命周期
// （refresh token、设备绑定、会话）。余额够不等于能用。
func TestQuotaRefreshDoesNotReviveWithoutUsabilityExt(t *testing.T) {
	// 注意这里用 quotaStub（**不**实现 QuotaUsabilityExt）。
	stub := &quotaStub{id: "workbuddy", view: gateway.CreditsQuota(9999), ok: true}
	h, p, _ := newQuotaTestHandler(t, stub)
	coolingHard(t, p, "wb-1")

	res := h.refreshQuotas([]string{"wb-1"})

	if res.Updated != 1 {
		t.Fatalf("额度仍应写回池，得到 updated=%d", res.Updated)
	}
	if res.Revived != 0 {
		t.Errorf("上游没声明 QuotaUsabilityExt，不该解冻，得到 revived=%d", res.Revived)
	}
	if st, _ := p.Status("wb-1"); !st.Cooling {
		t.Error("未声明可用性模型的上游，其冷却不该被额度刷新改动")
	}
}

// TestQuotaRefreshDoesNotReviveSoftRateCooldown 软限流（429）绝不因余额解冻。
//
// # 这条是本功能的边界
//
// 余额只能证明"钱够"，不能证明"限流窗口过了"。若周期刷新把正在吃 429
// 指数退避的号每 15 分钟放回池子，退避永远长不起来，就变成稳定的违规重试 ——
// 那是对上游的持续骚扰，比账号不可用严重得多。
func TestQuotaRefreshDoesNotReviveSoftRateCooldown(t *testing.T) {
	stub := &quotaUsableStub{
		quotaStub: &quotaStub{id: "jiyuan", view: gateway.CreditsQuota(6674), ok: true},
		usable:    true,
	}
	h, p, _ := newQuotaTestHandler(t, stub)
	p.AddFor("jiyuan", &auth.Auth{UID: "jy-3"}, nil)
	p.Cooldown("jy-3", pool.CoolSoft, 10*time.Minute, "429 限流")

	res := h.refreshQuotas([]string{"jy-3"})

	if res.Revived != 0 {
		t.Errorf("软限流不该被余额解冻，得到 revived=%d —— 会形成稳定的违规重试", res.Revived)
	}
	if st, _ := p.Status("jy-3"); !st.Cooling {
		t.Error("429 冷却必须保持：余额证明不了限流窗口已经过去")
	}
}

// TestRefreshAllQuotasSkipsDisabledAndNonQuota 周期任务要预过滤，端点不预过滤。
//
// # 为什么周期任务必须预过滤
//
// 它每 15 分钟跑一次。禁用账号本来不参与选号，问它们纯属浪费上游配额；
// 没接 QuotaExt 的上游则会让每轮刷一行 skipped 日志 —— 一天 96 条，
// 真正的故障被周期性噪音埋掉。
//
// 端点上保留 skipped 计数仍有价值（用户点一下就想知道"这上游为什么没额度"），
// 所以预过滤只加在 RefreshAllQuotas 里。
func TestRefreshAllQuotasSkipsDisabledAndNonQuota(t *testing.T) {
	silent := &quotaSilentStub{id: "workbuddy"} // 没有 QuotaExt
	usable := &quotaUsableStub{
		quotaStub: &quotaStub{id: "jiyuan", view: gateway.CreditsQuota(100), ok: true},
		usable:    true,
	}
	h, p, _ := newQuotaTestHandler(t, silent, usable)
	p.AddFor("jiyuan", &auth.Auth{UID: "jy-ok"}, nil)
	p.AddFor("jiyuan", &auth.Auth{UID: "jy-off"}, nil)
	p.Disable("jy-off", "测试用禁用")

	res := h.RefreshAllQuotas()

	// workbuddy 的账号（wb-1）与 jiyuan 的禁用账号（jy-off）都不该被问。
	if res.Skipped != 0 {
		t.Errorf("周期任务应预过滤掉没有 QuotaExt 的上游，skipped 应恒为 0，得到 %d", res.Skipped)
	}
	if res.Updated != 1 {
		t.Fatalf("只有 jy-ok 参与刷新，应 updated=1，得到 updated=%d unknown=%d failed=%d",
			res.Updated, res.Unknown, res.Failed)
	}
	// 两个上游都在注册表里，但只有 jiyuan 真正被问过。
	if len(res.Providers) != 1 || res.Providers[0] != "jiyuan" {
		t.Errorf("providers 应只有 jiyuan，得到 %v", res.Providers)
	}
}

// TestRefreshAllQuotasSilentWhenNothingToDo 没有可刷账号时不打日志、不报错。
//
// 一个纯 workbuddy 部署（没接任何 QuotaExt 上游）会每 15 分钟触发一次周期任务。
// 若那里无条件打一行"额度刷新完成 updated=0 ..."，日志会被无意义地填满，
// 而它什么也没做错。
func TestRefreshAllQuotasSilentWhenNothingToDo(t *testing.T) {
	silent := &quotaSilentStub{id: "workbuddy"}
	h, _, _ := newQuotaTestHandler(t, silent)

	res := h.RefreshAllQuotas()
	if res.Updated != 0 || res.Unknown != 0 || res.Failed != 0 || res.Skipped != 0 || res.Revived != 0 {
		t.Errorf("没有可刷账号时应返回全零，得到 %+v", res)
	}
	if len(res.Providers) != 0 {
		t.Errorf("providers 应为空，得到 %v", res.Providers)
	}
}
