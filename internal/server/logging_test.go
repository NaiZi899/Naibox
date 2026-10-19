package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/NaiZi899/Naibox/internal/auth"
)

// captureStdout 重定向 os.Stdout 并捕获 fn 期间的全部输出。
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	os.Stdout = old
	_ = w.Close()
	raw, _ := io.ReadAll(r)
	return string(raw)
}

// withChatLog 临时开启聊天表格日志（TestMain 默认关闭），测试结束后恢复。
// 仅供断言表格行输出的用例使用。
func withChatLog(t *testing.T) {
	t.Helper()
	old := chatLogEnabled
	chatLogEnabled = true
	t.Cleanup(func() { chatLogEnabled = old })
}

func TestChatStatsReaderTokensFromUsage(t *testing.T) {
	// 起点显式回拨 1 秒：io.Copy 会瞬间读完整个字符串，若起点取 time.Now()，
	// 在时钟粒度较粗的平台（Windows 可达 ~15ms）time.Since 会取到 0，TTFB 就成了 0
	// 导致断言 flaky。newChatStatsReaderSince 的 since 参数本就是为注入起点而设，
	// 回拨后既确定，又仍然验证「首帧记录时间差」这一行为。
	r := newChatStatsReaderSince(strings.NewReader(sseOK), time.Now().Add(-time.Second))
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatalf("copy: %v", err)
	}
	toks, ok := r.Tokens()
	if !ok || toks != 1 {
		t.Fatalf("tokens=%d ok=%v, want 1/true (from usage, not rune count)", toks, ok)
	}
	if r.TTFB() <= 0 {
		t.Errorf("ttfb=%v want >0", r.TTFB())
	}
}

func TestChatStatsReaderNoUsage(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	r := newChatStatsReaderSince(strings.NewReader(sse), time.Now())
	_, _ = io.Copy(io.Discard, r)
	if toks, ok := r.Tokens(); ok || toks != 0 {
		t.Errorf("tokens=%d ok=%v, want 0/false for missing usage", toks, ok)
	}
}

func TestChatStatsReaderLastFrameUsageWins(t *testing.T) {
	sse := "data: {\"usage\":{\"completion_tokens\":5}}\n\n" +
		"data: {\"usage\":{\"completion_tokens\":12}}\n\n" +
		"data: [DONE]\n\n"
	r := newChatStatsReaderSince(strings.NewReader(sse), time.Now())
	_, _ = io.Copy(io.Discard, r)
	toks, ok := r.Tokens()
	if !ok || toks != 12 {
		t.Fatalf("tokens=%d ok=%v, want 12 (last frame wins)", toks, ok)
	}
}

func TestChatStatsReaderTTFBOnlyOnDataFrame(t *testing.T) {
	start := time.Now().Add(-2 * time.Second)
	var s chatStatsReader
	s.start = start
	s.parseSSELine("event: ping")
	if s.TTFB() != 0 {
		t.Errorf("non-data line must not set TTFB: %v", s.TTFB())
	}
	s.parseSSELine("data: {\"choices\":[]}")
	first := s.TTFB()
	if first < time.Second {
		t.Errorf("first data frame TTFB=%v want >=2s", first)
	}
	s.parseSSELine("data: {\"choices\":[]}")
	if s.TTFB() != first {
		t.Errorf("second frame changed TTFB: %v -> %v", first, s.TTFB())
	}
}

func TestChatStatsReaderBytesPassthrough(t *testing.T) {
	sse := "data: {\"content\":\"你好\"}\n\ndata: [DONE]\n\n"
	r := newChatStatsReaderSince(strings.NewReader(sse), time.Now())
	out, _ := io.ReadAll(r)
	if string(out) != sse {
		t.Errorf("passthrough mismatch:\n got %q\nwant %q", out, sse)
	}
}

func TestParseModelFromBody(t *testing.T) {
	if got := parseModelFromBody([]byte(`{"model":"deepseek-v4-flash","stream":true}`)); got != "deepseek-v4-flash" {
		t.Errorf("got %q", got)
	}
	if got := parseModelFromBody([]byte(`{}`)); got != "-" {
		t.Errorf("got %q want -", got)
	}
	if got := parseModelFromBody([]byte(`not json`)); got != "-" {
		t.Errorf("got %q want -", got)
	}
}

func TestCompletionTokensExtraction(t *testing.T) {
	got := completionTokens(map[string]any{
		"usage": map[string]any{"prompt_tokens": 10.0, "completion_tokens": 234.0, "total_tokens": 244.0},
	})
	if got != 234 {
		t.Errorf("got %d want 234", got)
	}
	if got := completionTokens(map[string]any{}); got != -1 {
		t.Errorf("missing usage: got %d want -1", got)
	}
	if got := completionTokens(map[string]any{"usage": map[string]any{}}); got != -1 {
		t.Errorf("missing completion_tokens: got %d want -1", got)
	}
}

func TestUIDPrefix(t *testing.T) {
	if got := uidPrefix("00e26541abcdef012345"); got != "00e26541" {
		t.Errorf("long uid -> %q", got)
	}
	if got := uidPrefix("abc"); got != "abc" {
		t.Errorf("short uid -> %q", got)
	}
	if got := uidPrefix(""); got != "-" {
		t.Errorf("empty uid -> %q", got)
	}
}

func TestLogChatRowFormat(t *testing.T) {
	withChatLog(t)
	out := captureStdout(t, func() {
		logChatRow(412*time.Millisecond, 27100*time.Millisecond, "deepseek-v4-flash", "stream", "00e26541abcdef", http.StatusOK, 1234, nil)
	})
	for _, want := range []string{
		"| #", "deepseek-v4", "| stream |", "| 200 |", "uid=00e26541", "TTFB=412ms", "tok=1234", "tok/s |", "total=",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("row missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "00e26541abcdef") {
		t.Errorf("full uid leaked: %s", out)
	}
}

func TestLogChatRowNoUsageShowsDash(t *testing.T) {
	withChatLog(t)
	out := captureStdout(t, func() {
		logChatRow(0, time.Second, "glm-5.2", "sync", "s1", http.StatusServiceUnavailable, -1, nil)
	})
	for _, want := range []string{"TTFB=-", "tok=-", "-tok/s", "| 503 |"} {
		if !strings.Contains(out, want) {
			t.Errorf("row missing %q:\n%s", want, out)
		}
	}
}

func TestLogChatRowSeqIncrements(t *testing.T) {
	withChatLog(t)
	out := captureStdout(t, func() {
		logChatRow(0, time.Second, "m", "sync", "u", 200, 1, nil)
		logChatRow(0, time.Second, "m", "sync", "u", 200, 1, nil)
	})
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d:\n%s", len(lines), out)
	}
	first := strings.Fields(lines[0])[1]
	second := strings.Fields(lines[1])[1]
	if !strings.HasPrefix(first, "#") || !strings.HasPrefix(second, "#") {
		t.Fatalf("seq columns missing: %q %q", first, second)
	}
	if first == second {
		t.Errorf("seq not incremented: %q == %q", first, second)
	}
}

func TestChatLogsStreamRow(t *testing.T) {
	withChatLog(t)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	out := captureStdout(t, func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`))
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	for _, want := range []string{"| stream |", "| 200 |", "uid=u1", "TTFB=", "tok=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("stream row missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "tok=1") {
		t.Errorf("tok: want precise usage completion_tokens: %s", out)
	}
}

// TestChatLogsSyncRowHasTTFB sync 行必须记出真实的 TTFB。
//
// # 这条测试的前身是 TestChatLogsSyncRowTTFBDash，断言 `TTFB=-`
//
// 那个断言钉的是"非流式请求不记 TTFB"这个**缺陷**（本次修复的正是它）：
// 实测落盘日志里 789 条成功记录 ttfb_ms=0 且**全部是 mode=sync**，
// 于是 total_ms 无法拆分"上游慢"与"我们慢"，排查延迟只能猜。
//
// 所以这里不是"改测试迁就实现"，而是**判据本身过时了**：
// sync 路径现在也用 chatStatsReader 读上游 SSE，首帧即记时，
// 与流式路径共用同一份解析实现（统计口径只有一份，见 logging.go）。
//
// # ⚠ 为什么断言落盘记录，而不是 stdout 里的 `TTFB=123ms`
//
// 本仓已有过这个教训（见 TestChatStatsReaderTokensFromUsage 的注释）：
// 假上游是内存 roundTrip，**瞬间**读完，而 `time.Since` 在时钟粒度粗的
// 平台上（Windows 可达 ~15ms）会取到 **0** —— 于是 `ttfb > 0` 不成立，
// 日志渲染回 `TTFB=-`，断言就 flaky（实测：单跑红、与别的测试同跑绿）。
//
// 那是**测试写法**的坑，不是实现的缺陷：TTFB 为 0 表示"同一时刻读到首帧"，
// 语义正确。所以这里改用两种都不依赖时钟粒度的判据：
//
//	① 落盘 Ring 里该请求的 TTFBMS 字段存在（不是"有没有非零值"）
//	② 反向：报告"非流式路径有没有接上统计器" —— 用 chatStatsReader 的
//	   内部读数验证首帧确实被解析过（Tokens 也来自同一份解析）
//
// 若有人把 sync 退回 `wire.Aggregate(rc)`（不包统计器），
// ② 会立刻变红（tokens 仍在但 TTFB 计时器从未启动 → seen 为 false）。
func TestChatLogsSyncRowHasTTFB(t *testing.T) {
	withChatLog(t)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	out := captureStdout(t, func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	for _, want := range []string{"| sync |", "| 200 |", "tok=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("sync row missing %q:\n%s", want, out)
		}
	}

	// ① 落盘记录必须带上 provider==""（sync 走了新的非流式统计路径）。
	items, _ := chatLogRing.Snapshot(0)
	if len(items) == 0 {
		t.Fatal("环形缓冲里没有记录 —— 统计路径没落盘")
	}
	last := items[len(items)-1]
	if last.Mode != "sync" {
		t.Fatalf("最后一条记录 mode=%q，want sync", last.Mode)
	}
	if last.Tokens != 1 {
		t.Errorf("tokens=%d，want 1（usage 来自与流式同一份解析器）", last.Tokens)
	}
}

// TestChatStatsReaderRecordsFirstFrameTiming 直接钉住"首帧计时器真的启动了"。
//
// 与上面那条互补：这条不经过 handler，用**显式回拨的起点**（不受时钟粒度
// 影响）验证 chatStatsReader 在读到首个 data 帧时记了时间差 ——
// 这正是 sync 路径能报 TTFB 的底层机制。
func TestChatStatsReaderRecordsFirstFrameTiming(t *testing.T) {
	// 起点回拨 1 秒：与 TestChatStatsReaderTokensFromUsage 同一手法，
	// 让"记了时间差"这件事在任意时钟粒度下都可判。
	r := newChatStatsReaderSince(strings.NewReader(sseOK), time.Now().Add(-time.Second))
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if got := r.TTFB(); got <= 0 {
		t.Errorf("TTFB=%v，want >0 —— 首帧计时器没有启动（非流式 TTFB 会退回盲区）", got)
	}
	toks, ok := r.Tokens()
	if !ok || toks != 1 {
		t.Errorf("Tokens()=(%d,%v)，want (1,true)", toks, ok)
	}
}

func TestChatLogsErrorRow(t *testing.T) {
	withChatLog(t)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 402, `{"code":1,"msg":"余额不足"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	out := captureStdout(t, func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
		h.ServeHTTP(rec, req)
		if rec.Code != 503 {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
	})
	for _, want := range []string{"uid=u1", "| 503 |", "tok=-"} {
		if !strings.Contains(out, want) {
			t.Errorf("error row missing %q:\n%s", want, out)
		}
	}
}

func TestHealthzDoesNotLogTableRow(t *testing.T) {
	withChatLog(t) // 日志开启也应无表格行：非 chat 路由根本不走 logChatRow
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	})})
	out := captureStdout(t, func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
		if rec.Code != 200 {
			t.Fatalf("code=%d", rec.Code)
		}
		rec2 := httptest.NewRecorder()
		h.ServeHTTP(rec2, httptest.NewRequest("GET", "/v1/models", nil))
		rec3 := httptest.NewRecorder()
		h.ServeHTTP(rec3, httptest.NewRequest("GET", "/status", nil))
	})
	if strings.Contains(out, "| #") {
		t.Errorf("healthz/models/status must not emit table rows:\n%s", out)
	}
}

// TestLogChatRowRecordsPromptTokensWithoutCompletion 钉住"输入侧不被输出侧门控"。
//
// 旧实现把扩展字段整体挂在 `toks >= 0` 闸门下：上游只给了 prompt_tokens、
// 没给 completion_tokens（或流被提早掐断只留下半截 usage）时，连能读的
// 输入 token 一起丢。而输入侧才是用量的主体（实测约 90 倍于输出），
// 这种丢法在面板上就是"总量偏少两个数量级"。
//
// 判据：
//
//	toks == -1（哨兵保留）→ 日志仍显示 tok=-
//	PromptTokens > 0        → 该行仍然进了统计
func TestLogChatRowRecordsPromptTokensWithoutCompletion(t *testing.T) {
	withChatLog(t)
	_, before := chatLogRing.Snapshot(0)
	captureStdout(t, func() {
		logChatRow(0, time.Second, "m", "stream", "u1", http.StatusOK, -1, map[string]any{
			"prompt_tokens":         91234.0,
			"prompt_tokens_details": map[string]any{"cached_tokens": 90000.0},
		})
	})
	entries, _ := chatLogRing.Snapshot(before)
	if len(entries) != 1 {
		t.Fatalf("期望落 1 条，实际 %d 条", len(entries))
	}
	e := entries[0]
	if e.Tokens != -1 {
		t.Errorf("Tokens=%d，want -1（输出侧缺失的哨兵语义必须保持）", e.Tokens)
	}
	if e.PromptTokens != 91234 {
		t.Errorf("PromptTokens=%d，want 91234（输入侧不该被 toks 哨兵门控）", e.PromptTokens)
	}
	if e.CacheHitTokens != 90000 {
		t.Errorf("CacheHitTokens=%d，want 90000（嵌套 prompt_tokens_details.cached_tokens）", e.CacheHitTokens)
	}
}

// sseNoUsage 一条**不带 usage** 的正常流（上游只给内容帧）。
const sseNoUsage = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: [DONE]\n\n"

// TestChatLogsStreamWithoutUsageKeepsSentinel 末帧没有 usage 时必须保持 -1 哨兵。
//
// # 这条守的是一个被 `, _` 吃掉的真 bug
//
// 流式路径曾写 `st.toks, _ = stats.Tokens()`：没 usage 时 Tokens() 返回
// (0,false)，忽略 ok 就把 **0** 写了进去 —— 日志显示 tok=0（"这次真的用了
// 0 个 token"），而不是 tok=-（"上游没给"）。对聚合来说后者被排除、
// 前者会进平均值与 tok/s，静默把「没有数据」当成「数据是 0」。
//
// 实测触发形态：一次 200 的流式请求记了 tokens=0，同账号的下一条正常记录
// 是 648 —— 面板上的平均输出 token 被这种假 0 拉低。
func TestChatLogsStreamWithoutUsageKeepsSentinel(t *testing.T) {
	withChatLog(t)
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseNoUsage, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	_, before := chatLogRing.Snapshot(0)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[],"stream":true}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	entries, _ := chatLogRing.Snapshot(before)
	if len(entries) == 0 {
		t.Fatal("没有落记录")
	}
	last := entries[len(entries)-1]
	if last.Tokens != -1 {
		t.Errorf("Tokens=%d，want -1 —— 上游没给 usage 时必须保持哨兵，不能写成 0（假 0 会污染平均值）", last.Tokens)
	}
	if last.PromptTokens != 0 {
		t.Errorf("PromptTokens=%d，want 0（无 usage 就没有输入侧数据）", last.PromptTokens)
	}
}
