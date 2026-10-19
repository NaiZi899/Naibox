// health_explain_test.go 账号健康解读的**端到端**链路：
// 上游解读 → 池内记录 → /status 下发 → 前端读得到。
//
// 为什么要单独钉住这条链路：解读涉及三层（gateway 的类型、pool 的字段、
// server 的搬运），任何一层漏掉都**不会编译失败** —— 表现是"状态列一直
// 只显示通用文案"，而那与"上游没实现解读"长得一模一样，查不出来。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/NaiZi899/Naibox/internal/auth"
	"github.com/NaiZi899/Naibox/internal/gateway"
	"github.com/NaiZi899/Naibox/internal/pool"
	"github.com/NaiZi899/Naibox/internal/upstream"
)

// explainProvider 一个会报错误解读的上游替身（仅用于文档说明其角色；
// 真正的分派验证走 registryRouter，见 TestRegistryRouterDispatchesExplain）。

// TestNoteErrorExplainFlowsToStatus 上游解读必须出现在 /status 的账号上。
func TestNoteErrorExplainFlowsToStatus(t *testing.T) {
	p := pool.New("")
	p.SyncToDirFor("explainable", []*auth.Auth{{UID: "u-explain", Nickname: "基元测试号"}})

	// 未记录解读时，Status 里没有该字段。
	st := findStatus(t, p, "u-explain")
	if st.LastError != nil {
		t.Fatal("还没记录解读就已下发 last_error —— 前端会显示不存在的错误")
	}

	// 记录一份解读。
	p.NoteErrorExplain("u-explain", pool.ErrorExplainView{
		Code:       "INSUFFICIENT_BALANCE",
		HTTPStatus: 402,
		Kind:       gateway.ErrKindHardCredit.String(),
		Summary:    "余额不足",
		Detail:     "预付费余额已耗尽，需充值后恢复。",
		At:         time.Now(),
	})

	st = findStatus(t, p, "u-explain")
	if st.LastError == nil {
		t.Fatal("记录了解读但 /status 里没有 last_error —— 服务器侧丢了它")
	}
	if st.LastError.Code != "INSUFFICIENT_BALANCE" {
		t.Errorf("状态码未透出: %q（用户要的就是它）", st.LastError.Code)
	}
	if st.LastError.Summary != "余额不足" {
		t.Errorf("摘要未透出: %q", st.LastError.Summary)
	}
	if st.LastError.HTTPStatus != 402 {
		t.Errorf("HTTP 状态未透出: %d", st.LastError.HTTPStatus)
	}
	if st.LastError.Kind != "hard_credit" {
		t.Errorf("分类未透出: %q", st.LastError.Kind)
	}

	// 必须是**深拷贝**：改返回值不该改到池内状态。
	st.LastError.Summary = "被调用方改掉了"
	if again := findStatus(t, p, "u-explain"); again.LastError.Summary == "被调用方改掉了" {
		t.Error("Status 返回的 last_error 与池内是同一份（调用方能改到池内状态）")
	}
}

// TestExplainSurvivesSuccess 成功一次**不擦掉**解读。
//
// 这是刻意的行为：用户看账号池往往是因为"这个号好像有问题"。
// 若成功一次就擦掉，偶发失败的号在列表里永远显示"正常"——
// 而那正是最难排查的形态。
func TestExplainSurvivesSuccess(t *testing.T) {
	p := pool.New("")
	p.SyncToDirFor("explainable", []*auth.Auth{{UID: "u2", Nickname: "x"}})
	p.NoteErrorExplain("u2", pool.ErrorExplainView{Summary: "余额不足", At: time.Now()})
	p.NoteSuccess("u2")

	st := findStatus(t, p, "u2")
	if st.LastError == nil || st.LastError.Summary != "余额不足" {
		t.Fatal("一次成功就擦掉了错误解读 —— 偶发故障将在界面上完全隐形")
	}
	if st.Cooling {
		t.Error("前提被破坏：本用例要的是「曾报错但当前可用」")
	}
}

// TestExplainJSONShape 钉住下发的 JSON 字段名（前端按这些名字读）。
func TestExplainJSONShape(t *testing.T) {
	p := pool.New("")
	p.SyncToDirFor("explainable", []*auth.Auth{{UID: "u3"}})
	p.NoteErrorExplain("u3", pool.ErrorExplainView{
		Code: "UNAUTHORIZED", HTTPStatus: 401, Kind: "session_dead",
		Summary: "key 已失效", Detail: "需换一把新 key", At: time.Now(),
	})

	raw, err := json.Marshal(findStatus(t, p, "u3"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	le, ok := m["last_error"].(map[string]any)
	if !ok {
		t.Fatalf("下发 JSON 里没有 last_error 对象，字段=%v", keysOf(m))
	}
	for _, k := range []string{"code", "http_status", "kind", "summary", "detail", "at"} {
		if _, present := le[k]; !present {
			t.Errorf("last_error 缺字段 %q（前端读不到）", k)
		}
	}
}

// TestStatusEndpointCarriesExplain /status 端点的响应体里确实带上了它。
func TestStatusEndpointCarriesExplain(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u-status", Nickname: "基元状态号"})
	p.SyncToDirFor("explainable", []*auth.Auth{{UID: "u-status", Nickname: "基元状态号"}})
	p.NoteErrorExplain("u-status", pool.ErrorExplainView{
		Code: "INSUFFICIENT_BALANCE", HTTPStatus: 402, Kind: "hard_credit",
		Summary: "余额不足", Detail: "需充值", At: time.Now(),
	})

	h := NewHandler(Config{Pool: p, Upstream: wrapUpstream(upstream.New())})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/status", nil)

	h.status(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/status 状态码 = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "INSUFFICIENT_BALANCE") {
		t.Errorf("/status 响应里没有上游状态码 —— 解读没走到 HTTP 层:\n%s", truncateForTest(body, 600))
	}
	if !strings.Contains(body, "余额不足") {
		t.Errorf("/status 响应里没有解读摘要")
	}
}

// ---- helpers ----

func findStatus(t *testing.T, p *pool.Pool, uid string) pool.Status {
	t.Helper()
	for _, s := range p.List() {
		if s.UID == uid {
			return s
		}
	}
	t.Fatalf("账号 %s 不在池里", uid)
	return pool.Status{}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func truncateForTest(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
