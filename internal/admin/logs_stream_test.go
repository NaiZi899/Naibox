package admin

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/NaiZi899/omni2api/internal/logbuf"
)

// 这组测试起**真实** HTTP 服务（httptest.NewServer）并用 http.Client 读真实
// SSE 流，刻意不 mock ResponseWriter / Flusher —— Flush 的时机、连接断开、
// chunked 传输这几件事正是 SSE 最容易写错的地方，mock 会把它们全部掩盖掉。
//
// 关于 -race：本机无 cgo，Windows 下 race 检测要 gcc，所以并发正确性不靠
// race 检测器，而是靠可观察的行为断言（取消后 handler 必须返回）。

const (
	// frameTimeout 单帧等待上限。轮询间隔 300ms，3s 足以覆盖 CI 抖动。
	frameTimeout = 3 * time.Second
	// silentWindow 断言「再没有下一帧」的观察窗口。必须明显大于轮询间隔，
	// 否则「没收到」可能只是还没轮到下一次轮询。
	silentWindow = 700 * time.Millisecond
)

// sseFrame 一帧原始 SSE（空行分隔）。保留全部原始行，含注释行 ——
// 否则没法断言「心跳不污染 data」。
type sseFrame struct {
	lines []string
}

// event 返回 `event:` 字段值（无则空串）。
func (f sseFrame) event() string {
	for _, l := range f.lines {
		if v, ok := strings.CutPrefix(l, "event: "); ok {
			return v
		}
	}
	return ""
}

// id 返回 `id:` 字段值（无则空串）。
func (f sseFrame) id() string {
	for _, l := range f.lines {
		if v, ok := strings.CutPrefix(l, "id: "); ok {
			return v
		}
	}
	return ""
}

// data 返回所有 `data:` 行拼接（SSE 规范：多行 data 用 \n 连接）。
func (f sseFrame) data() string {
	var parts []string
	for _, l := range f.lines {
		if v, ok := strings.CutPrefix(l, "data: "); ok {
			parts = append(parts, v)
		}
	}
	return strings.Join(parts, "\n")
}

// isComment 是否注释帧（首行以 ':' 开头）—— 心跳就是这种帧。
func (f sseFrame) isComment() bool {
	return len(f.lines) > 0 && strings.HasPrefix(f.lines[0], ":")
}

// openStream 建立 SSE 连接。
//
// 依赖一个关键事实：handler 在进入循环前就 WriteHeader + Flush，因此
// http.Get 返回时服务端**已经**完成游标初始化。这让「先连上、再推日志」
// 的时序在新条目上是确定的，测试不需要 sleep 去凑。
func openStream(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("建立 SSE 连接失败: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("HTTP %d: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		resp.Body.Close()
		t.Fatalf("Content-Type=%q，want text/event-stream", ct)
	}
	// 关流必须早于 srv.Close：httptest.Server.Close 会等所有在途请求结束，
	// 而 SSE 请求在客户端断开前永不结束 —— 注册顺序反了就是死锁。
	// （t.Cleanup 是 LIFO，这里注册得比 srv.Close 晚，所以先执行。）
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// readFrames 后台读流，把帧送进 channel；body 读完/关闭即结束。
func readFrames(body io.ReadCloser) <-chan sseFrame {
	ch := make(chan sseFrame, 32)
	go func() {
		defer close(ch)
		defer body.Close()
		sc := bufio.NewScanner(body)
		var cur []string
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				if len(cur) > 0 {
					ch <- sseFrame{lines: cur}
					cur = nil
				}
				continue
			}
			cur = append(cur, line)
		}
	}()
	return ch
}

// nextFrame 等一帧；超时即失败（说明该推的没推）。
func nextFrame(t *testing.T, ch <-chan sseFrame) sseFrame {
	t.Helper()
	select {
	case f, ok := <-ch:
		if !ok {
			t.Fatalf("流已结束，没有等到期望的帧")
		}
		return f
	case <-time.After(frameTimeout):
		t.Fatalf("%s 内没有收到任何帧", frameTimeout)
	}
	return sseFrame{}
}

// expectNoFrame 断言观察窗口内没有帧（用于「不该推」的那一侧）。
func expectNoFrame(t *testing.T, ch <-chan sseFrame, why string) {
	t.Helper()
	select {
	case f, ok := <-ch:
		if !ok {
			return
		}
		t.Fatalf("%s，却收到了帧 %q", why, f.lines)
	case <-time.After(silentWindow):
	}
}

// decodeLog 把一帧 event:log 解回 logbuf.Entry，并校验 id 与 seq 一致。
func decodeLog(t *testing.T, f sseFrame) logbuf.Entry {
	t.Helper()
	if f.event() != "log" {
		t.Fatalf("event=%q，want \"log\"（帧内容 %q）", f.event(), f.lines)
	}
	var e logbuf.Entry
	if err := json.Unmarshal([]byte(f.data()), &e); err != nil {
		t.Fatalf("data 不是合法 JSON: %v（%q）", err, f.data())
	}
	if want := strconv.FormatInt(e.Seq, 10); f.id() != want {
		t.Fatalf("id=%q 与 data.seq=%d 不一致", f.id(), e.Seq)
	}
	return e
}

// pushEntry 造一条最小可辨识的日志。
func pushEntry(ring *logbuf.Ring, model string) int64 {
	return ring.Push(logbuf.Entry{
		At:     time.Now(),
		Model:  model,
		Mode:   "sync",
		Status: http.StatusOK,
		UID:    "uid-" + model,
		Tokens: 1,
	})
}

// TestLogsStreamOnlyPushesRowsAfterConnect 契约核心：since 缺省时**不重放历史**，
// 只推连上之后的增量。推入 3 条 → 连上 → 再推 2 条 → 只收到后 2 条。
func TestLogsStreamOnlyPushesRowsAfterConnect(t *testing.T) {
	ring := logbuf.New(16)
	for i := 0; i < 3; i++ {
		pushEntry(ring, "history")
	}

	h := New(Config{Ring: ring})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp := openStream(t, srv.URL+"/admin/logs/stream")
	frames := readFrames(resp.Body)

	// 连上之后的第一段静默：3 条历史一条都不许出来。
	expectNoFrame(t, frames, "since 缺省时不得重放历史日志")

	seqA := pushEntry(ring, "fresh-a")
	seqB := pushEntry(ring, "fresh-b")

	got := []logbuf.Entry{decodeLog(t, nextFrame(t, frames)), decodeLog(t, nextFrame(t, frames))}
	if got[0].Seq != seqA || got[0].Model != "fresh-a" {
		t.Fatalf("第 1 条 = %+v，want seq=%d model=fresh-a", got[0], seqA)
	}
	if got[1].Seq != seqB || got[1].Model != "fresh-b" {
		t.Fatalf("第 2 条 = %+v，want seq=%d model=fresh-b", got[1], seqB)
	}
	// 条目与 /admin/logs 同构：字段必须完整穿过 JSON，不是被裁剪过的摘要。
	if got[0].UID != "uid-fresh-a" || got[0].Status != http.StatusOK || got[0].Mode != "sync" {
		t.Fatalf("data 字段不完整: %+v", got[0])
	}

	expectNoFrame(t, frames, "只有两条新增，不该有第三条")
}

// TestLogsStreamSinceCursor 显式 since 的续传语义：since=第 1 条的 seq
// → 收到第 2、3 条（不含第 1 条，也不含更早的）。
func TestLogsStreamSinceCursor(t *testing.T) {
	ring := logbuf.New(16)
	seq1 := pushEntry(ring, "m1")
	seq2 := pushEntry(ring, "m2")
	seq3 := pushEntry(ring, "m3")

	h := New(Config{Ring: ring})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp := openStream(t, srv.URL+"/admin/logs/stream?since="+strconv.FormatInt(seq1, 10))
	frames := readFrames(resp.Body)

	got := []logbuf.Entry{decodeLog(t, nextFrame(t, frames)), decodeLog(t, nextFrame(t, frames))}
	if got[0].Seq != seq2 || got[0].Model != "m2" {
		t.Fatalf("第 1 条 = %+v，want seq=%d model=m2", got[0], seq2)
	}
	if got[1].Seq != seq3 || got[1].Model != "m3" {
		t.Fatalf("第 2 条 = %+v，want seq=%d model=m3", got[1], seq3)
	}
	expectNoFrame(t, frames, "since 之前的条目不得重复推送")

	// 续传后游标继续生效：再推一条，仍然立刻到达。
	seq4 := pushEntry(ring, "m4")
	if e := decodeLog(t, nextFrame(t, frames)); e.Seq != seq4 {
		t.Fatalf("续传后的新条目 seq=%d，want %d", e.Seq, seq4)
	}
}

// TestLogsStreamKeepAliveDoesNotPolluteData 心跳必须是注释帧：
// 既不产生 event，也不混进任何 data 行 —— 否则前端会把心跳当日志渲染，
// 或者 JSON.parse 直接炸掉。
func TestLogsStreamKeepAliveDoesNotPolluteData(t *testing.T) {
	// 15s 的心跳在测试里等不起；临时调快，Cleanup 还原。
	old := logsStreamKeepAlive
	logsStreamKeepAlive = 60 * time.Millisecond
	t.Cleanup(func() { logsStreamKeepAlive = old })

	ring := logbuf.New(16)
	h := New(Config{Ring: ring})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp := openStream(t, srv.URL+"/admin/logs/stream")
	frames := readFrames(resp.Body)

	hb := nextFrame(t, frames)
	if !hb.isComment() {
		t.Fatalf("期望先收到心跳注释帧，实际 %q", hb.lines)
	}
	if hb.lines[0] != ": keep-alive" {
		t.Fatalf("心跳帧内容 = %q，want \": keep-alive\"", hb.lines[0])
	}
	if hb.event() != "" || hb.data() != "" {
		t.Fatalf("心跳帧污染了 event/data: %q", hb.lines)
	}

	// 心跳与日志交错时，日志帧依然干净（跳过继续到来的心跳帧）。
	seq := pushEntry(ring, "after-heartbeat")
	for {
		f := nextFrame(t, frames)
		if f.isComment() {
			continue
		}
		if e := decodeLog(t, f); e.Seq != seq {
			t.Fatalf("日志帧 seq=%d，want %d", e.Seq, seq)
		}
		if strings.Contains(f.data(), "keep-alive") {
			t.Fatalf("日志帧的 data 里混进了心跳: %q", f.data())
		}
		break
	}
}

// TestLogsStreamStopsOnContextCancel 客户端断开后 handler 必须返回 ——
// 这就是"goroutine 随 ctx 退出"的可观察形式（无 cgo 跑不了 -race）。
func TestLogsStreamStopsOnContextCancel(t *testing.T) {
	ring := logbuf.New(16)
	h := New(Config{Ring: ring})

	returned := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r)
		close(returned)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/admin/logs/stream", nil)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("建立 SSE 连接: %v", err)
	}
	frames := readFrames(resp.Body)

	// 先证明流是真的活着，避免"本来就退出了"被误判成通过。
	seq := pushEntry(ring, "before-cancel")
	if e := decodeLog(t, nextFrame(t, frames)); e.Seq != seq {
		t.Fatalf("取消前推送未到达: seq=%d want %d", e.Seq, seq)
	}

	cancel()
	select {
	case <-returned:
	case <-time.After(frameTimeout):
		t.Fatal("ctx 取消后 handler 仍未返回 —— goroutine 泄漏（每个面板开关都会留一份日志缓冲引用）")
	}
}

// TestLogsStreamWithoutFlusherReturns501：包装过的 ResponseWriter 丢掉 Flush 时
// 必须明确报 501，而不是假装连上、让帧憋在 net/http 的缓冲里。
//
// noFlush 只暴露 ResponseWriter 的方法集（不实现 http.Flusher）——
// 这不是"mock 掉 Flusher"，而是复现真实故障形态：中间件包一层很常见。
type noFlush struct{ http.ResponseWriter }

func TestLogsStreamWithoutFlusherReturns501(t *testing.T) {
	h := New(Config{Ring: logbuf.New(4)})
	req := httptest.NewRequest(http.MethodGet, "/admin/logs/stream", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()

	h.ServeHTTP(noFlush{rec}, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("HTTP %d，want 501（body=%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Flush") {
		t.Fatalf("501 响应未说明原因: %s", rec.Body.String())
	}
}

// TestLogsStreamWithoutRingReturns503：未接线环形缓冲时明确报不可用，
// 不要挂一条只发心跳的空连接（前端会一直显示"已连接但无数据"）。
//
// 这里用 httptest.ResponseRecorder 直接调 ServeHTTP —— 它**实现**了
// http.Flusher，所以能穿过 Flusher 检查走到 Ring 判空分支。
func TestLogsStreamWithoutRingReturns503(t *testing.T) {
	h := New(Config{})
	req := httptest.NewRequest(http.MethodGet, "/admin/logs/stream", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("HTTP %d，want 503（body=%s）", rec.Code, rec.Body.String())
	}
}

// TestLogsStreamIsBehindAdminAuth 鉴权必须与其它 /admin/* 完全一致，不在
// 这条端点上另开一条路径：非 loopback 且没带管理钥匙 → 403。
//
// 这条端点会把完整请求日志（uid/model/消耗）持续推给客户端，鉴权开洞等于
// 把账号池活动直播出去，所以它必须被一条测试钉住。
//
// 两段覆盖的是同一道门的两侧：
//   - 非 loopback 无钥匙 → 403（用 httptest.NewRequest 伪造 RemoteAddr）；
//   - 真实连接（必然来自 127.0.0.1）→ 放行且流真的能推数据。
//
// 远程 + 管理钥匙的放行分支没法用真实连接造（httptest 的来源地址固定），
// 它由 ServeHTTP 的统一入口负责，不在这条路由里另写一遍。
func TestLogsStreamIsBehindAdminAuth(t *testing.T) {
	ring := logbuf.New(8)
	h := New(Config{Ring: ring, APIKey: "k-test"})

	// 非 loopback、无钥匙 → 403。
	req := httptest.NewRequest(http.MethodGet, "/admin/logs/stream", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("无凭据远程访问 HTTP %d，want 403（body=%s）", rec.Code, rec.Body.String())
	}

	// loopback 直连 → 放行。用真实服务读流，确认不是"403 变 200 但没流"。
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	resp := openStream(t, srv.URL+"/admin/logs/stream")
	frames := readFrames(resp.Body)
	seq := pushEntry(ring, "authorized")
	if e := decodeLog(t, nextFrame(t, frames)); e.Seq != seq {
		t.Fatalf("放行后的流没收到推送: seq=%d want %d", e.Seq, seq)
	}
}
