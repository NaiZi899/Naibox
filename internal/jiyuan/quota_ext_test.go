// quota_ext_test.go 基元律动的额度（QuotaExt）与 session-key 配对。
//
// # 这个文件回答两个问题
//
//  1. 额度列的 `—` 怎么才能变成真数字？（→ RefreshQuota）
//  2. session 怎么挂到已有的 key 上，而不是让用户重新导入一遍？（→ 前缀配对）
//
// # 为什么单位是"分"
//
// `pool.QuotaView.Remaining` 是 **int64**，而余额是小数金额。
// 若直接 `int64(余额)`，¥0.50 会变成 0 —— 一个"查到了余额却显示 0"的
// 镜像 bug（本项目已为"把有值显示成无值"付过代价）。
// 所以以分为单位上报，本文件用边界值把它钉住。
package jiyuan

import (
	"math"
	"testing"

	"github.com/NaiZi899/omni2api/internal/gateway"
)

// ---- 单位换算 ----

// TestYuanToFen 元→分：小数一位都不能丢。
func TestYuanToFen(t *testing.T) {
	cases := []struct {
		name string
		yuan float64
		want int64
	}{
		{"整数元", 100, 10000},
		{"实测余额", 99.749876, 9975}, // 上游真实值（math.Round）
		{"两位小数", 12.34, 1234},
		{"半个元", 0.5, 50}, // ← 直接截断会变成 0，这是本设计的核心理由
		{"一分", 0.01, 1},  // ← 同上
		{"九分", 0.09, 9},  // ← 同上
		{"零", 0, 0},
		{"负数（欠款）", -3.5, 0}, // 截到 0：负额度会污染选号权重
		{"极小正数", 0.001, 0},  // 1 厘 → 四舍五入到 0，可以接受
		{"大额", 1e6, 1e8},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := yuanToFen(c.yuan); got != c.want {
				t.Errorf("yuanToFen(%v) = %d，want %d", c.yuan, got, c.want)
			}
		})
	}
}

// TestYuanToFenNeverLosesSmallAmounts 这条把"分"这个单位的存在理由单独钉住。
//
// 若哪天有人把本函数改成 `int64(yuan)`，下面这些会全部变成 0 ——
// 于是"账号还有 ¥0.50"在界面上显示为 `0`，用户会去删号或充值。
func TestYuanToFenNeverLosesSmallAmounts(t *testing.T) {
	for _, yuan := range []float64{0.5, 0.9, 0.01, 0.99, 1.5} {
		if got := yuanToFen(yuan); got <= 0 {
			t.Errorf("¥%v 换算成了 %d —— 金额被丢掉了（用户会以为没余额）", yuan, got)
		}
	}
}

// TestYuanToFenHandlesNonFinite 非有限值不该造出一个巨数。
func TestYuanToFenHandlesNonFinite(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if got := yuanToFen(v); got != 0 {
			t.Errorf("yuanToFen(%v) = %d，want 0", v, got)
		}
	}
}

// TestYuanToFenClampsOverflow 超大金额被夹住，不会溢出成负数。
//
// 溢出成负数的后果很隐蔽：pool 的选号权重会把它当成"额度为负"，
// 该账号被永久降权 —— 而不是报错。
func TestYuanToFenClampsOverflow(t *testing.T) {
	got := yuanToFen(1e30)
	if got <= 0 {
		t.Fatalf("超大金额被换算成 %d —— 溢出成了负数会污染选号权重", got)
	}
}

// ---- RefreshQuota 的三态 ----

// TestRefreshQuotaUnknownUID 非本上游的 uid → ok=false（调用方据此跳过）。
func TestRefreshQuotaUnknownUID(t *testing.T) {
	p := New(Config{})
	qv, ok := p.RefreshQuota("no-such-uid")
	if ok {
		t.Error("不存在的 uid 应返回 ok=false（契约：我不服务这条 uid）")
	}
	if qv.HasData {
		t.Error("ok=false 时不该声称有数据")
	}
}

// TestRefreshQuotaWithoutSessionIsUnknownNotZero 没有 session → **未知**，不是 0。
//
// # 这是本模块最容易写错的一处
//
// "没有登录凭证"与"余额是 0"是两件完全不同的事：
//
//	返回 UnknownQuota() → 界面 `—` → 用户知道"不知道"
//	返回 0              → 界面 `0`  → 用户以为"这个号没钱了"，去删号
//
// 而按上游事实，**只用 key 导入的账号永远没有 session**（余额只有
// 登录凭证能读，实测穷举过）—— 也就是说这条分支是**常态**，不是边缘情况。
func TestRefreshQuotaWithoutSessionIsUnknownNotZero(t *testing.T) {
	dir := t.TempDir()
	// 写一份只有 key、没有 session 的凭证（模拟"只用 API Key 栏导入"）。
	a := NewAuth("sk_tr_1a2b3c4d5e6f7a8b9c0d", "only-key")
	if err := writeAuth(t, dir, a); err != nil {
		t.Fatalf("写凭证: %v", err)
	}

	// 不接线 credentialSrc → 走目录扫描路径。
	p := New(Config{AuthDir: dir})
	qv, ok := p.RefreshQuota(a.UID)

	if !ok {
		t.Fatal("这是本上游的账号，ok 应为 true（ok=false 会被记成 Failed 计数）")
	}
	if qv.HasData {
		t.Error("没有 session 时不该声称有额度数据")
	}
	if qv.Remaining != 0 {
		t.Errorf("未知应当没有数值，得到 %d", qv.Remaining)
	}
	if qv.Kind == "credits" {
		t.Error("未知不该带 kind —— 那会让界面把 `—` 渲染成一个形态")
	}
}

// TestRefreshQuotaWithSessionReadsBalance 有 session → 走真实上游读余额。
//
// 用假上游（session_flow_test.go 的 fakeUpstream），验证：
// 分单位正确、Kind 是 credits、HasData 为 true。
func TestRefreshQuotaWithSessionReadsBalance(t *testing.T) {
	f := newFakeUpstream()
	f.serve(t)

	dir := t.TempDir()
	a := NewAuth("sk_tr_FAKE_omni2api_SECRETVALUE", "with-session")
	a.Session = "sess_fake_session_value"
	if err := writeAuth(t, dir, a); err != nil {
		t.Fatalf("写凭证: %v", err)
	}

	p := New(Config{AuthDir: dir})
	qv, ok := p.RefreshQuota(a.UID)
	if !ok {
		t.Fatal("应有本上游的账号")
	}
	if !qv.HasData {
		t.Fatal("有 session 且有余额时应声称有数据（否则界面显示 `—`）")
	}
	// 假上游：available=99.75、frozen=20.25、total=120。
	// 必须取 **available**（冻结的钱花不出去）——取 Total 会得到 12000 分，
	// 一个"看起来更富"的错值，会让选号误以为这个号更宽裕。
	if qv.Remaining != 9975 {
		t.Errorf("余额应为 9975 分（¥99.75 可用，不是 ¥120 总额），得到 %d", qv.Remaining)
	}
	if qv.Kind != "credits" {
		t.Errorf("Kind 应为 credits，得到 %q", qv.Kind)
	}
	// ⚠ 单位必须上报：没有它，界面上的 `9975` 与 workbuddy 的 9975 **积分**
	// 长得一模一样，用户无法区分"¥99.75"与"9975 积分"。
	//
	// 本包的额度以**分**为单位（int64 装不下小数金额），所以必须带"分"。
	if qv.Unit != gateway.QuotaUnitFen {
		t.Errorf("额度单位应为 %q（本上游以分为单位），得到 %q —— "+
			"界面上会显示裸 9975，与积分无法区分", gateway.QuotaUnitFen, qv.Unit)
	}
}

// TestRefreshQuotaRejectsNonCNY 非 CNY 不换算（宁可未知，也不给错数量级的数字）。
func TestRefreshQuotaRejectsNonCNY(t *testing.T) {
	f := newFakeUpstream()
	f.serve(t)
	f.overrideUsage = `{"code":0,"message":"ok","data":{"balanceCny":"99.75","availableBalanceCny":"99.75","currency":"USD"}}`

	dir := t.TempDir()
	a := NewAuth("sk_tr_FAKE_omni2api_SECRETVALUE", "usd")
	a.Session = "sess_fake_session_value"
	if err := writeAuth(t, dir, a); err != nil {
		t.Fatalf("写凭证: %v", err)
	}

	p := New(Config{AuthDir: dir})
	qv, ok := p.RefreshQuota(a.UID)
	if !ok {
		t.Fatal("应为本上游账号")
	}
	if qv.HasData {
		t.Errorf("USD 余额不该按分上报（会错一个数量级）—— 应显示未知，得到 %+v", qv)
	}
}

// ---- session-key 前缀配对 ----

// TestMatchingPrefix 前缀配对的方向必须正确。
//
// 上游给的是**前缀**，本地持有完整 key ——
// `HasPrefix(本地完整key, 上游前缀)`。反过来写永远匹配不上。
func TestMatchingPrefix(t *testing.T) {
	full := "sk_tr_EXAMP1_FAKE_FULL_KEY_FOR_TESTS"
	entries := []apiKey{
		{Name: "omni2api", KeyPrefix: "sk_tr_5Jm6Ji"},
		{Name: "demo-a", KeyPrefix: "sk_tr_EXAMP1"}, // ← 与 full 匹配
		{Name: "demo-b", KeyPrefix: "sk_tr_SAMPLE"},
	}

	if got := matchingPrefix(full, entries); got != "sk_tr_EXAMP1" {
		t.Errorf("应匹配到 nai 的前缀，得到 %q", got)
	}
	if got := matchingPrefix("sk_tr_NOMATCH0000", entries); got != "" {
		t.Errorf("无匹配应返回空，得到 %q", got)
	}
	if got := matchingPrefix("", entries); got != "" {
		t.Errorf("空 key 应返回空，得到 %q", got)
	}
	// 上游前缀为空时跳过（不能拿空串当"匹配一切"）。
	if got := matchingPrefix(full, []apiKey{{KeyPrefix: ""}}); got != "" {
		t.Errorf("空前缀不该匹配任何东西，得到 %q", got)
	}
	// 前缀比本地 key 长 → 不匹配（防"拿短的去 HasPrefix 长的"这类反向写法）。
	if got := matchingPrefix("sk_tr_3", []apiKey{{KeyPrefix: "sk_tr_EXAMP1XXX"}}); got != "" {
		t.Errorf("更长的前缀不该匹配，得到 %q", got)
	}
}
