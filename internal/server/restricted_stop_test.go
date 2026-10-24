// restricted_stop_test.go 上游风控标记（403 + 11140"账号级限制"）的停手语义。
//
// # 这个文件守的两条红线
//
//	① 命中即停手：一次客户端请求最多只打 1 次上游，不换号。
//	   （改造前走 applyErrorPolicy + continue，MaxRotate=3 就意味着
//	   最多拿 3 个号去撞同一条出口/同一台设备 —— 社区实测 7 个号全封。）
//
//	② 停手要有交代：该号被禁用、整池进入暂停、客户端拿到带 Retry-After 的 503，
//	   而不是被伪装成"所有账号不可用"。
package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/NaiZi899/Naibox/internal/auth"
)

// restrictedBody 上游风控标记的正文（与 upstream.restrictedMarkers 对齐，
// 取自真实抓包；request illegal + displayMsg 两种措辞都在里面）。
const restrictedBody = `{"code":11140,"msg":"request illegal","requestId":"x",` +
	`"displayMsg":{"zh":"账号存在异常行为，已被限制使用",` +
	`"en":"Your account has been restricted due to unusual activity"}}`

// newRestrictedHandler 造一个三号池 + 恒返回 403 风控标记的假上游。
func newRestrictedHandler(t *testing.T, rotateMax int) (*Handler, *chatRecorder) {
	t.Helper()
	rec := &chatRecorder{}
	rec.responses = append(rec.responses, struct {
		status int
		body   string
		stream bool
	}{http.StatusForbidden, restrictedBody, false})

	h := NewHandler(Config{
		Pool: testPoolWith(
			&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
			&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
			&auth.Auth{UID: "u3", AccessToken: "at3", ExpiresAt: 9999999999},
		),
		Upstream:            newRecorderUpstream(t, rec),
		DefaultClassifier:   defaultClassifierOf(),
		MaxRotate:           5, // 给足预算：若实现仍在换号，这里会跑满 5 次
		RestrictedRotateMax: rotateMax,
	})
	return h, rec
}

// disabledCount 数出池里被禁用的账号数（不关心是哪一个 —— 选号是加权随机的）。
func disabledCount(t *testing.T, h *Handler) int {
	t.Helper()
	n := 0
	for _, st := range h.cfg.Pool.List() {
		if st.Disabled {
			n++
		}
	}
	return n
}

// TestAccountRestrictedStopsAfterFirstHit 命中一次就停手：上游只被打 1 次。
func TestAccountRestrictedStopsAfterFirstHit(t *testing.T) {
	h, rec := newRestrictedHandler(t, 0)

	w := newBuf()
	h.ServeHTTP(w, httptestNewChat(chatReqBody))

	// 关键断言 ①：只打了一次上游。
	if got := rec.calls(); got != 1 {
		t.Fatalf("风控命中后必须立即停手，上游被打了 %d 次 —— "+
			"跑满说明仍在换号重试，这会把同一出口上的其它号一起送进去", got)
	}
	// 关键断言 ②：被命中的号被禁用（阈值 1，需人工确认后启用）。
	if n := disabledCount(t, h); n != 1 {
		t.Errorf("应有且仅有 1 个账号被禁用，实际 %d", n)
	}
	// 关键断言 ③：整池暂停已生效。
	until, reason := h.cfg.Pool.SuspendedUntil()
	if until.IsZero() || reason == "" {
		t.Fatalf("命中风控后整池必须暂停，实际 until=%v reason=%q", until, reason)
	}
	// 关键断言 ④：503 + Retry-After（让客户端自己退避，而不是秒级重试）。
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码应为 503，实际 %d：%.300s", w.Code, w.Body.String())
	}
	if ra := w.Header().Get("Retry-After"); ra == "" {
		t.Error("暂停期必须带 Retry-After —— 没有它，客户端会按自己的节奏继续打")
	}
	// 关键断言 ⑤：不把上游原文（403 / request illegal / 11140）透给客户端。
	body := w.Body.String()
	for _, leak := range []string{"request illegal", "11140", "403"} {
		if strings.Contains(body, leak) {
			t.Errorf("响应体泄漏了 %q（客户端会据此把整条链判成鉴权失败）：%.300s", leak, body)
		}
	}
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是 JSON: %v; %.300s", err, body)
	}
	if !strings.Contains(resp.Error.Message, "风控") {
		t.Errorf("message 应说明是上游风控停手，实际 %q", resp.Error.Message)
	}
}

// TestAccountRestrictedSecondRequestNeverTouchesUpstream 暂停期内不再打上游。
//
// 这条比"只打一次"更重要：客户端的重试是**秒级**的，若暂停没有闸住选号，
// 第二个请求会立刻再打一次上游 —— 等于我们自己在替客户端 hammer。
func TestAccountRestrictedSecondRequestNeverTouchesUpstream(t *testing.T) {
	h, rec := newRestrictedHandler(t, 0)

	h.ServeHTTP(newBuf(), httptestNewChat(chatReqBody))
	first := rec.calls()

	w2 := newBuf()
	h.ServeHTTP(w2, httptestNewChat(chatReqBody))

	if got := rec.calls(); got != first {
		t.Errorf("暂停期内第二个请求不得打到上游：第一次后 %d 次，第二次后 %d 次", first, got)
	}
	if w2.Code != http.StatusServiceUnavailable {
		t.Errorf("暂停期应直接 503，实际 %d", w2.Code)
	}
	if ra := w2.Header().Get("Retry-After"); ra == "" {
		t.Error("暂停期的 503 必须带 Retry-After")
	}
}

// TestAccountRestrictedRotateRollback 回滚通道：RestrictedRotateMax>0 时回到旧行为。
//
// 它不是"想要的功能"，而是**现场排查用的开关**（对应环境变量
// NAIBOX_RESTRICTED_ROTATE_MAX）：万一上游把一次软限流的措辞改成判据认得的
// 样子，需要一条不改配置文件的路把轮转临时打开。
func TestAccountRestrictedRotateRollback(t *testing.T) {
	h, rec := newRestrictedHandler(t, 2)

	h.ServeHTTP(newBuf(), httptestNewChat(chatReqBody))

	// 首次命中 + 2 次额外换号 = 3 次；MaxRotate=5 是上界，不得跑满。
	if got := rec.calls(); got != 3 {
		t.Errorf("RestrictedRotateMax=2 时应恰好 3 次上游调用，实际 %d", got)
	}
}
