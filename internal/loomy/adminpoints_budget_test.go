// adminpoints_budget_test.go 新手任务 / 邀请码列表的**超时预算必须按账号给**。
//
// # 这个 bug 的形态（用户报"老是读取数据失败"）
//
// 两个 handler 原实现都在**循环外**取一次 `pointsCtx()`（20s），然后 N 个账号
// 串行共用那一份预算。loomy 的上游是重尾（实测单次 136ms–17.4s），
// 于是"谁排在列表后面"变成"谁失败"，而排序与账号健康毫无关系。
//
// 实测证据（直连上游、每账号 3 次，全部返回 code:000000 成功）：
//
//	180****7745   534 / 1918 / 148 ms
//	139****2899  3334 / 12511 / 7722 ms
//	159****7372  5782 / 174 / 10135 ms
//	177****7863  6703 / 17379 / 14790 ms
//
// 即 session 全是好的，纯粹是上游慢 —— 而失败被报成了"读取数据失败"。
//
// # 判别性判据（关键）
//
// 要区分"按账号给"与"整批共用"，必须让 **各账号耗时累加超过预算、而单个不超过**：
//
//	3 个账号 × 各 120ms，预算 250ms
//	  按账号给（正确）  → 每个都在自己的 250ms 内完成 → 3 个全 ok
//	  整批共用（缺陷）  → 120+120=240 还行，第 3 个累计 360ms > 250ms → 第 3 个失败
//
// 用真实的 20s 预算做不到这个区分（1.2s 的慢号在 20s 预算下怎么都够），
// 所以测试把包级 `pointsTimeout` 临时收紧。这也是它保持 var 的唯一理由。
package loomy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// budgetTestUpstream 假上游：对**所有**请求都延迟 perCall 后成功。
//
// 全部账号一样慢，是刻意的：这样"整批共用预算"必然在最后一个账号上崩，
// 而"按账号给预算"能让每一个都过 —— 两者结果不同，判据才有判别力。
func budgetTestUpstream(t *testing.T, perCall time.Duration) *httptest.Server {
	t.Helper()
	// ⚠ 路径必须带 /api/v1 前缀：handler 用 NewWithBase(srv.URL)，
	// 而 Client 直接把 BaseURL 与端点路径拼起来（上游真实 base 含 /api/v1）。
	// 注册成裸路径会让请求落到兜底 handler、回空对象，
	// 然后被解析层报成"回执里没有 X 字段" —— 看起来像被测代码的 bug。
	const pfx = "/api/v1"

	mux := http.NewServeMux()
	slow := func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(perCall):
		case <-r.Context().Done():
			return
		}
	}
	mux.HandleFunc(pfx+"/onboarding/tasks", func(w http.ResponseWriter, r *http.Request) {
		slow(w, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"000000","desc":"成功","data":{"tasks":{}}}`))
	})
	mux.HandleFunc(pfx+"/points/activation", func(w http.ResponseWriter, r *http.Request) {
		slow(w, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"000000","desc":"成功","data":{"activated":true,"appliedInvitationCode":""}}`))
	})
	mux.HandleFunc(pfx+"/invitation-codes", func(w http.ResponseWriter, r *http.Request) {
		slow(w, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"000000","desc":"成功","data":{"codes":[]}}`))
	})
	mux.HandleFunc(pfx+"/", func(w http.ResponseWriter, r *http.Request) {
		slow(w, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"000000","desc":"成功","data":{}}`))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// providerWithSessions 造一个 Provider，凭证目录里放 n 个账号。
//
// UID 刻意排序后可预测（a0 < a1 < a2），因为 accounts() 按 UID 升序 ——
// 本用例依赖"最后一个账号"来确定谁会因累计超时而失败。
func providerWithSessions(t *testing.T, srvURL string, n int) *Provider {
	t.Helper()
	dir := t.TempDir()
	for i := 0; i < n; i++ {
		uid := "a" + string(rune('0'+i)) + "000000000000000"
		body, _ := json.Marshal(map[string]any{
			"uid": uid, "nickname": "测试号" + string(rune('0'+i)), "session": "SESS-" + uid,
		})
		if err := os.WriteFile(filepath.Join(dir, "loomy-"+uid+".json"), body, 0o600); err != nil {
			t.Fatalf("写凭证失败: %v", err)
		}
	}
	return NewWithConfig(Config{Client: NewWithBase(srvURL), AuthDir: dir})
}

// tightenBudget 把包级超时临时收紧，用例结束还原。
func tightenBudget(t *testing.T, d time.Duration) {
	t.Helper()
	old := pointsTimeout
	pointsTimeout = d
	t.Cleanup(func() { pointsTimeout = old })
}

// TestTasksBudgetIsPerAccount 每个账号各拿一份预算，慢账号不拖垮后面的。
//
// 判据：3 个账号各 120ms、预算 250ms：
//
//	按账号给 → 三个都 ok（每个 120 < 250）
//	整批共用 → 第三个累计 360 > 250 → 第三个失败（本用例变红）
func TestTasksBudgetIsPerAccount(t *testing.T) {
	const perCall = 120 * time.Millisecond
	tightenBudget(t, 250*time.Millisecond)

	srv := budgetTestUpstream(t, perCall)
	p := providerWithSessions(t, srv.URL, 3)

	rec := httptest.NewRecorder()
	p.handleTasksList(rec, httptest.NewRequest(http.MethodGet, tasksPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("tasks 状态码 = %d", rec.Code)
	}
	var out struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("回执不是合法 JSON: %v", err)
	}
	if len(out.Accounts) != 3 {
		t.Fatalf("应当有 3 个账号，实际 %d", len(out.Accounts))
	}
	for i, a := range out.Accounts {
		if a["ok"] != true {
			t.Errorf("★ 第 %d 个账号失败（error=%v）—— 超时预算是整批共用而不是按账号，"+
				"于是前面账号的耗时会累加到后面账号头上。这正是「老是读取数据失败」的形态。",
				i+1, a["error"])
		}
	}
}

// TestInviteBudgetIsPerAccount invite 同样必须按账号给预算。
//
// invite 每账号要打 4 个接口，所以这里的"每账号耗时"是 4×perCall ——
// 有缺陷的实现会比 tasks 更早崩（实测里它的失败账号也确实更多）。
func TestInviteBudgetIsPerAccount(t *testing.T) {
	// invite 每账号 4 次调用，所以 perCall 要更小，
	// 让"单个账号的总耗时"仍在预算内、而"3 个账号累加"一定超出。
	const perCall = 40 * time.Millisecond
	tightenBudget(t, 400*time.Millisecond)

	srv := budgetTestUpstream(t, perCall)
	p := providerWithSessions(t, srv.URL, 3)

	rec := httptest.NewRecorder()
	p.handleInviteList(rec, httptest.NewRequest(http.MethodGet, invitePath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("invite 状态码 = %d", rec.Code)
	}
	var out struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("回执不是合法 JSON: %v", err)
	}
	if len(out.Accounts) != 3 {
		t.Fatalf("应当有 3 个账号，实际 %d", len(out.Accounts))
	}
	for i, a := range out.Accounts {
		if a["ok"] != true {
			t.Errorf("★ 邀请码列表第 %d 个账号失败（error=%v）—— 预算没按账号给。",
				i+1, a["error"])
		}
	}
}

// TestTasksSlowAccountStillReportsItsOwnError 局部失败仍要如实报出来。
//
// 反向断言：不能为了让"后面的成功"而把失败账号的错误吞掉 ——
// 用户需要知道"哪一个账号坏、为什么"。
func TestTasksSlowAccountStillReportsItsOwnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"100002","desc":"登录已失效，请重新登录"}`))
	}))
	defer srv.Close()

	p := providerWithSessions(t, srv.URL, 1)

	rec := httptest.NewRecorder()
	p.handleTasksList(rec, httptest.NewRequest(http.MethodGet, tasksPath, nil))
	var out struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("回执不是合法 JSON: %v", err)
	}
	if len(out.Accounts) != 1 {
		t.Fatalf("应当有 1 个账号，实际 %d", len(out.Accounts))
	}
	if out.Accounts[0]["ok"] == true {
		t.Error("上游明确报错，这个账号不该被报成成功")
	}
	if e := out.Accounts[0]["error"]; e == nil || e == "" {
		t.Error("失败必须带 error 文案（用户要知道是哪个账号、为什么）")
	}
}
