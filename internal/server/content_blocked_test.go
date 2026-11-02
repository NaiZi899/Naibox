// content_blocked_test.go 内容策略拦截的端到端判据（提示词网关删除后的形态）。
//
// # 这一版钉住的三条性质
//
//	A. 一次上游往返：内容拦截**不换号**，客户端拿到上游的 400 与原文
//	B. 不罚账号：不冷却、不计错、不喂熔断、不禁用
//	C. 流内错误（2xx + 错误信封）同样适用，不会因为"藏在 200 里"而被换号绕过
//
// # 历史
//
// 改造前这里还有两条路径：首次拦截 → 切中性提示词重试一次；单账号部署要把
// 该号从 tried 里撤掉才能重试。提示词网关整体删除后，那两条路径连同其判据
// 一起消失 —— 改写 system prompt 的能力不再存在，"换提示词再试一次"没有实现
// 基础，留着测试会成为对死代码的断言。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/NaiZi899/Naibox/internal/auth"
)

// newBuf 返回一个记录响应的 ResponseRecorder。
func newBuf() *httptest.ResponseRecorder { return httptest.NewRecorder() }

// httptestNewChat 造一个 POST /v1/chat/completions 请求。
func httptestNewChat(body string) *http.Request {
	return httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
}

// chatRecorder 记录每次出站请求的 body，并按调用序返回预设响应。
type chatRecorder struct {
	mu     sync.Mutex
	bodies []string

	// responses[i] 是第 i 次调用的 (status, body, isStream)；
	// 超出长度时复用最后一个。
	responses []struct {
		status int
		body   string
		stream bool
	}
}

func (r *chatRecorder) record(b []byte) {
	r.mu.Lock()
	r.bodies = append(r.bodies, string(b))
	r.mu.Unlock()
}

func (r *chatRecorder) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

func (r *chatRecorder) bodyAt(i int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i >= len(r.bodies) {
		return ""
	}
	return r.bodies[i]
}

func (r *chatRecorder) next(i int) (int, string, bool) {
	if len(r.responses) == 0 {
		return 500, `{"code":1,"msg":"recorder 未配置响应"}`, false
	}
	idx := i
	if idx >= len(r.responses) {
		idx = len(r.responses) - 1
	}
	x := r.responses[idx]
	return x.status, x.body, x.stream
}

// newRecorderUpstream 造一个会记录请求 body 的假上游。
func newRecorderUpstream(t *testing.T, r *chatRecorder) upstreamAdapter {
	t.Helper()
	c := newFakeUpstream(t, func(string) (int, string, bool) { return 500, "", false })
	c.Client().HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(req.Body)
		n := r.calls()
		r.record(raw)
		status, body, stream := r.next(n)
		ct := "application/json"
		if stream {
			ct = "text/event-stream"
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{ct}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})}
	return c
}

// blockedBody 是上游内容策略拦截的响应体（workbuddy 的标记之一）。
const blockedBody = `{"code":1,"msg":"blocked by security policy"}`

// chatReqBody 带 system 的请求体。
const chatReqBody = `{"model":"glm-5.2","messages":[` +
	`{"role":"system","content":"客户端原始人格"},` +
	`{"role":"user","content":"hi"}]}`

// assertNotPunished 断言账号既没进冷却也没被熔断/禁用。
//
// # 这是本文件的核心断言
//
// "内容问题不是账号问题"这句话必须落在池状态上才算数：
// 只要 applyErrorPolicy 被误调一次，账号就会进冷却，
// 而它在界面上表现为"这个号有问题" —— 一个完全错误、且没人会怀疑的归因。
func assertNotPunished(t *testing.T, h *Handler, uid string) {
	t.Helper()
	st, ok := h.cfg.Pool.Status(uid)
	if !ok {
		t.Fatalf("池里找不到账号 %s", uid)
	}
	if st.Cooling {
		t.Errorf("内容拦截不得让账号进冷却：uid=%s reason=%q until=%v", uid, st.Reason, st.Until)
	}
	if st.Disabled {
		t.Errorf("内容拦截不得禁用账号：uid=%s", uid)
	}
	if !st.BreakerUntil.IsZero() {
		t.Errorf("内容拦截不得触发熔断：uid=%s breakerUntil=%v", uid, st.BreakerUntil)
	}
	// 计错计数也必须保持 0：它是成功率权重的输入，
	// 被内容拦截污染会让这个号在选号时被莫名降权。
	if st.ErrTotal != 0 {
		t.Errorf("内容拦截不得计入错误数：uid=%s errTotal=%d", uid, st.ErrTotal)
	}
}

// newContentBlockedHandler 造一个三号池 + 恒返回 400 审核文案的假上游。
func newContentBlockedHandler(t *testing.T, maxRotate int) (*Handler, *chatRecorder) {
	t.Helper()
	rec := &chatRecorder{}
	rec.responses = append(rec.responses, struct {
		status int
		body   string
		stream bool
	}{http.StatusBadRequest, blockedBody, false})

	h := NewHandler(Config{
		Pool: testPoolWith(
			&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
			&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
			&auth.Auth{UID: "u3", AccessToken: "at3", ExpiresAt: 9999999999},
		),
		Upstream:          newRecorderUpstream(t, rec),
		DefaultClassifier: defaultClassifierOf(),
		MaxRotate:         maxRotate,
	})
	return h, rec
}

// TestContentBlockedReturns400WithoutRotating 一次上游往返、如实返回上游原文。
func TestContentBlockedReturns400WithoutRotating(t *testing.T) {
	h, rec := newContentBlockedHandler(t, 5)

	w := newBuf()
	h.ServeHTTP(w, httptestNewChat(chatReqBody))

	if got := rec.calls(); got != 1 {
		t.Fatalf("内容拦截不得换号重试，上游被打了 %d 次 —— "+
			"跑满说明仍在轮转（MaxRotate=%d），那会把一次内容问题报成账号池故障", got, 5)
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("应如实返回上游的 400，得到 %d：%.300s", w.Code, w.Body.String())
	}
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是 JSON: %v; %.300s", err, w.Body.String())
	}
	if resp.Error.Code != "content_blocked" {
		t.Errorf("error.code=%q，期望 content_blocked（客户端据此区分「内容问题」与「账号池不可用」）",
			resp.Error.Code)
	}
	if !strings.Contains(resp.Error.Message, "blocked by security policy") {
		t.Errorf("应带上游原文，便于用户定位：%q", resp.Error.Message)
	}
}

// TestContentBlockedDoesNotPunishAccount 三个号里任何一个都不得被罚。
func TestContentBlockedDoesNotPunishAccount(t *testing.T) {
	h, _ := newContentBlockedHandler(t, 5)

	h.ServeHTTP(newBuf(), httptestNewChat(chatReqBody))

	for _, uid := range []string{"u1", "u2", "u3"} {
		assertNotPunished(t, h, uid)
	}
}

// TestContentBlockedInBandStopsRotation 错误藏在 2xx 流内时同样不换号。
func TestContentBlockedInBandStopsRotation(t *testing.T) {
	rec := &chatRecorder{}
	// 上游用 200 + 流内错误信封表达内容拦截。
	rec.responses = append(rec.responses, struct {
		status int
		body   string
		stream bool
	}{http.StatusOK, `{"error":{"message":"blocked by security policy","type":"invalid_request_error"}}` + "\n", false})

	uid := "u1"
	h := NewHandler(Config{
		Pool:              testPoolWith(&auth.Auth{UID: uid, AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:          newRecorderUpstream(t, rec),
		DefaultClassifier: defaultClassifierOf(),
		MaxRotate:         5,
	})

	w := newBuf()
	h.ServeHTTP(w, httptestNewChat(chatReqBody))

	if got := rec.calls(); got != 1 {
		t.Errorf("流内内容拦截同样不得换号，上游被打了 %d 次", got)
	}
	assertNotPunished(t, h, uid)
}
