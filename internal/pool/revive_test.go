package pool

import (
	"testing"
	"time"

	"github.com/NaiZi899/Naibox/internal/auth"
)

// ---------------------------------------------------------------------------
// ReviveHardCreditIfUsable 的边界。
//
// 它是"充值后自动解冻"落地的最后一环，判据错在任何一个方向都有实际后果：
//
//	放太宽  把熔断/软限流的运行态一起清了 → 稳定违规重试
//	放太窄  余额恢复了却解不开 → 用户充了钱还得人工点
//
// 核心的 pool 层断言：只清 until / coolKind / reason 三个字段。
// ---------------------------------------------------------------------------

// setUsableQuota 给账号写一份"有余额"的额度。
func setUsableQuota(p *Pool, uid string, remaining int64) {
	p.SetQuota(uid, QuotaView{
		Kind: QuotaKindCredits, Remaining: remaining, HasData: true, Unit: "分",
	})
}

// TestReviveHardCreditClearsOnlyCreditCooldown 解冻只碰余额相关三字段。
//
// # 这条为什么必须在 pool 层测
//
// admin 层的用例只能观察"账号还冷不冷"，看不到熔断计数与退避进度有没有被
// 顺手清掉。而那正是 ClearCooldown 与 ReviveHardCreditIfUsable 的唯一区别。
func TestReviveHardCreditClearsOnlyCreditCooldown(t *testing.T) {
	p := New("")
	p.AddFor("jiyuan", &auth.Auth{UID: "u1"}, nil)
	setUsableQuota(p, "u1", 6674)

	// 制造"熔断运行态 + 余额耗尽冷却"并存。
	// 走 NoteError 而不是直接摆字段：这条路径才是生产里真实的叠加顺序
	//（先连续失败，再因余额被判死）。
	p.NoteError("u1")
	p.NoteError("u1")
	p.NoteError("u1")
	p.Cooldown("u1", CoolHard, 30*time.Minute, "余额不足")

	before, ok := p.Status("u1")
	if !ok || !before.Cooling {
		t.Fatalf("夹具前提不成立：应处于冷却中，得到 %+v", before)
	}
	// ⚠ 断言 BreakerUntil 而不是 BreakerFails：cooldownUntil 内部会调
	// recordBreakerFailureLocked，那一步会把 fails 计数按自己的规则重排。
	// BreakerUntil 才是"熔断退避进度"的可观察载体，也正是 ClearCooldown
	// 会清零的字段 —— 用它比对才有判别力。
	if before.BreakerUntil.IsZero() {
		t.Fatalf("夹具前提不成立：应带着熔断退避时刻，得到 %+v", before)
	}

	if !p.ReviveHardCreditIfUsable("u1") {
		t.Fatal("余额 > 0 且处于 CoolHard，应当解冻")
	}

	after, _ := p.Status("u1")
	// ⚠ 这里**不能**断言 `!after.Cooling`：Cooling 是
	// `until` 与 `breakerUntil` 的并集，而本方法刻意不动熔断退避。
	// 一个"既在余额耗尽冷却、又带着熔断退避"的号被解冻后，仍然是
	// 不可用状态 —— 那是正确的，余额证明不了熔断已经过去。
	// 本方法只该负责把 CoolHard 那一路撤掉。
	//
	// ⚠ 也不能拿 CoolKind 判断：`CoolHard` 的 iota 值是 **0**，与"无冷却"同值，
	// 所以 `coolKind==0` 无法区分"刚被解冻"和"仍在余额冷却"。唯一的权威载体
	// 是 until（Cooling 的判据也是它）。
	if !after.Until.IsZero() {
		t.Errorf("CoolHard 的截止时刻应被清空（这是该路冷却的唯一权威载体），得到 %v", after.Until)
	}
	if after.Reason == "余额不足" {
		t.Error("冷却原因应一并清掉，否则界面上会继续显示「余额不足」而余额明明有钱")
	}
	if !after.BreakerUntil.Equal(before.BreakerUntil) {
		t.Errorf("熔断退避时刻被改动了（%v → %v）。解冻只该清余额相关字段；"+
			"清掉熔断等于替上游宣布「这个号没问题」，而余额证明不了这件事",
			before.BreakerUntil, after.BreakerUntil)
	}
}

// TestReviveHardCreditLeavesSoftRateAlone 软限流状态下不解冻。
func TestReviveHardCreditLeavesSoftRateAlone(t *testing.T) {
	p := New("")
	p.AddFor("jiyuan", &auth.Auth{UID: "u2"}, nil)
	setUsableQuota(p, "u2", 6674)
	p.Cooldown("u2", CoolSoft, 10*time.Minute, "429 限流")

	if p.ReviveHardCreditIfUsable("u2") {
		t.Error("CoolSoft（429）不是余额问题，不该被解冻")
	}
	if st, _ := p.Status("u2"); !st.Cooling {
		t.Error("429 冷却必须保持")
	}
}

// TestReviveHardCreditRequiresPositiveBalance 余额 0 或没有数据都不解冻。
func TestReviveHardCreditRequiresPositiveBalance(t *testing.T) {
	cases := []struct {
		name string
		prep func(p *Pool)
	}{
		{"余额为 0", func(p *Pool) { setUsableQuota(p, "u3", 0) }},
		{"没有额度数据", func(p *Pool) { p.SetQuota("u3", QuotaView{}) }},
	}
	for _, c := range cases {
		p := New("")
		p.AddFor("jiyuan", &auth.Auth{UID: "u3"}, nil)
		c.prep(p)
		p.Cooldown("u3", CoolHard, 30*time.Minute, "余额不足")

		if p.ReviveHardCreditIfUsable("u3") {
			t.Errorf("%s：不该解冻 —— 解冻会让它立刻再撞一次 402", c.name)
		}
		if st, _ := p.Status("u3"); !st.Cooling {
			t.Errorf("%s：必须保持冷却", c.name)
		}
	}
}

// TestReviveHardCreditIgnoresExpiredCooldown 已到期的冷却不算"本次解冻"。
//
// 返回 false 是为了让调用方不要把"本来就该好了"记成"刷新救活了它" ——
// 那会让计数器说谎。
func TestReviveHardCreditIgnoresExpiredCooldown(t *testing.T) {
	p := New("")
	p.AddFor("jiyuan", &auth.Auth{UID: "u4"}, nil)
	setUsableQuota(p, "u4", 6674)
	p.Cooldown("u4", CoolHard, 0, "余额不足") // 0 时长 = 立刻到期

	if p.ReviveHardCreditIfUsable("u4") {
		t.Error("已到期的冷却应走自然恢复，本方法不该报解冻")
	}
}

// TestReviveHardCreditUnknownUID 不存在的 uid 安全返回 false。
func TestReviveHardCreditUnknownUID(t *testing.T) {
	p := New("")
	if p.ReviveHardCreditIfUsable("nobody") {
		t.Error("不存在的 uid 应返回 false，而不是 panic 或误判")
	}
}
