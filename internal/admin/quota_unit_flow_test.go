// quota_unit_flow_test.go 额度量纲（unit）穿过**完整链路**的断言。
//
// # 为什么单测 gateway 那一份不够
//
// `Unit` 要穿过三个类型与三个转换点才到界面：
//
//	上游 gateway.QuotaView
//	  → admin.toPoolQuota        （漏写 Unit: 就断在这里）
//	  → pool.QuotaView           （json tag 漏了也会断）
//	  → /admin/accounts 的 JSON
//
// 每一跳漏掉都不会编译失败、不会让既有测试变红 —— 界面只是**静默地**
// 少了那两个字。这正是本项目的主线缺陷（中间层丢字段），
// 所以必须有一条端到端断言把它钉住。
package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NaiZi899/omni2api/internal/auth"
	"github.com/NaiZi899/omni2api/internal/gateway"
	"github.com/NaiZi899/omni2api/internal/pool"
)

// TestToPoolQuotaCarriesUnit 转换点必须透传 Unit。
//
// 这条直接盯住最容易漏的那一行。
func TestToPoolQuotaCarriesUnit(t *testing.T) {
	in := gateway.CreditsQuotaUnit(9975, gateway.QuotaUnitFen)
	out := toPoolQuota(in)
	if out.Unit != "分" {
		t.Fatalf("toPoolQuota 丢了 Unit（得到 %q）—— 界面上量纲标注会整片消失，"+
			"而且不报错", out.Unit)
	}
	if out.Remaining != 9975 || !out.HasData || out.Kind != gateway.QuotaKindCredits {
		t.Errorf("其余字段也要一并透传，得到 %+v", out)
	}
}

// TestPoolQuotaUnitSurvivesJSON Unit 必须出现在 /admin/accounts 的线格式里。
//
// 这条覆盖 pool.QuotaView 的 json tag —— 漏 tag 时字段名会变成 "Unit"，
// 前端读 `q.unit` 恒 undefined。
func TestPoolQuotaUnitSurvivesJSON(t *testing.T) {
	q := pool.FromCreditsUnit(9975, "分")
	raw, err := json.Marshal(q)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["unit"] != "分" {
		t.Errorf("线格式字段应为 unit=分，实际 %v（原始 %s）", m["unit"], raw)
	}
}

// TestPoolQuotaEmptyUnitStaysAbsent 空单位不落进线格式（老前端行为不变）。
func TestPoolQuotaEmptyUnitStaysAbsent(t *testing.T) {
	raw, _ := json.Marshal(pool.FromCredits(4200))
	if got := string(raw); strings.Contains(got, `"unit"`) {
		t.Errorf("空单位不该出现在线格式里（那会改变既有 workbuddy 的下线形状），实际 %s", got)
	}
}

// TestAccountsEndpointCarriesQuotaUnit **端到端**：额度单位要出现在
// `/admin/accounts` 的响应里，且能被前端按 `quota.unit` 读到。
//
// 这是用户真正看到的东西 —— 请求链路是：
//
//	/admin/accounts  → accountViews 内嵌 pool.Status → JSON
//
// 若 Unit 在 pool 里存住了但没进 Status 的线格式，这条会变红。
func TestAccountsEndpointCarriesQuotaUnit(t *testing.T) {
	h, p := newUnitFixture(t)
	// 模拟"上游刷新额度 → toPoolQuota → SetQuota"这条真实路径。
	p.SetQuota("u-jy", toPoolQuota(gateway.CreditsQuotaUnit(9975, gateway.QuotaUnitFen)))

	body := accountsBody(t, h)
	if !strings.Contains(body, `"unit":"分"`) {
		t.Errorf("响应里应有 quota.unit=\"分\"（否则界面显示裸 9975，与积分无法区分）:\n%s", body)
	}
	if !strings.Contains(body, `"remaining":9975`) {
		t.Errorf("数值也要一并下发:\n%s", body)
	}
}

// TestAccountsEndpointOmitsEmptyUnit 无量纲账号**不**带 unit 字段。
//
// 这条保证本次改动对既有上游零影响：workbuddy/codearts 的响应与改造前
// 逐字节相同（前端走的是"无单位 → 只显示数字"那条老路径）。
func TestAccountsEndpointOmitsEmptyUnit(t *testing.T) {
	h, p := newUnitFixture(t)
	p.SetQuota("u-wb", toPoolQuota(gateway.CreditsQuota(4200)))

	body := accountsBody(t, h)
	if strings.Contains(body, `"unit"`) {
		t.Errorf("无量纲账号的响应里不该出现 unit 字段（对既有上游应为逐字节不变）:\n%s", body)
	}
	if !strings.Contains(body, `"remaining":4200`) {
		t.Errorf("数值仍要下发:\n%s", body)
	}
}

// newUnitFixture 造一个含两个账号（jiyuan / workbuddy）的池 + handler。
//
// 用**真实的 pool + handler** 而不是桩：本文件要验证的正是
// "Unit 能不能穿过 pool 到线格式"，桩会把这一段绕过去。
func newUnitFixture(t *testing.T) (*Handler, *pool.Pool) {
	t.Helper()
	reg := gateway.NewRegistry()
	p := pool.New("")
	for _, c := range []struct{ uid, provider string }{
		{"u-jy", "jiyuan"}, {"u-wb", "workbuddy"},
	} {
		p.AddFor(c.provider, &auth.Auth{UID: c.uid, Nickname: c.uid}, nil)
	}
	h := New(Config{Registry: reg, Pool: p, DefaultProvider: "workbuddy"})
	return h, p
}

// accountsBody 打一次 /admin/accounts 并返回响应体。
//
// 必须设 RemoteAddr：admin 全部端点都限本机（与 uimanifest_test.go 同）。
func accountsBody(t *testing.T, h *Handler) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d，want 200（body=%s）", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}
