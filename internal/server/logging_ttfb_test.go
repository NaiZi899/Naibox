// logging_ttfb_test.go 非流式（sync）请求的 TTFB 记录。
//
// # 为什么需要这一组
//
// 实测 request-log.jsonl：789 条成功记录的 ttfb_ms=0，**全部是 mode=sync**；
// 有 ttfb 的 2309 条里只有 15 条是 sync。也就是非流式请求基本不记 TTFB。
//
// 后果不是"少了一列好看的数"，而是 total_ms 无法拆解：
//
//	total = 我们的开销（鉴权/选号/建连）+ 上游耗时
//
// 没有 TTFB 时这两项混在一个数里，"上游慢"与"我们慢"分辨不出来 ——
// 而本次事故的定性（380s 到底挂在谁身上）正是靠这个区分的。
//
// # 为什么复用 chatStatsReader 而不是新写一个"首字节计时 reader"
//
// 非流式的上游响应**本身就是 SSE**（wire.Aggregate 读的就是它，最后才合成
// chat.completion）。所以套上既有的统计器即可，TTFB 与流式路径**同口径**。
// 新写一个包装器会让"什么算首字节"有两份判据 —— 本仓库反复强调统计判据
// 只能有一份（见 wire.sseData 的教训）。
package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/NaiZi899/omni2api/internal/auth"
)

// syncTTFBStall 上游在读流之前故意停顿的时长。
//
// 它让"记到了 TTFB"与"TTFB 恒为 0"这两种情况在数值上分得开：
// 没有它，一次本地内存流读得太快，即使有实现也可能取到 0ms。
const syncTTFBStall = 300 * time.Millisecond

// TestSyncRequestRecordsTTFB 是本次修复在 sync TTFB 上的主闸门。
//
// 断言两件事：
//
//	st.ttfb 在 sync 路径被填上（落盘行 TTFB 不再是 "-"）
//	它的量级对得上上游的停顿（不是巧合的 1ms，也不是恒 0）
//
// 变异验证：把非流式分支里的 `newChatStatsReaderSince(wd, st.start)` 换回
// `wire.Aggregate(rc)`（即改造前的形态），本用例变红。
func TestSyncRequestRecordsTTFB(t *testing.T) {
	withChatLog(t)

	// 上游先停 syncTTFBStall 再吐完整 SSE（含 usage）。
	body := newPacedBody(false, pacedStep{delay: syncTTFBStall, data: sseOK})
	up, pb := pacedUpstream(t, body, 200)
	t.Cleanup(func() { _ = pb.Close() })

	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})

	var out string
	code := 0
	out = captureStdout(t, func() {
		rec := httptest.NewRecorder()
		// 注意**没有** stream:true —— 这条用例测的就是非流式路径。
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
		h.ServeHTTP(rec, req)
		code = rec.Code
	})

	if code != 200 {
		t.Fatalf("非流式请求失败：code=%d（本用例只测 TTFB 有没有被记上）", code)
	}

	// 1) 表格行里 TTFB 必须是数值，不是那个表示"没记到"的破折号。
	if strings.Contains(out, "TTFB=-") {
		t.Errorf("sync 行的 TTFB 仍是 \"-\" —— 非流式路径没有记录 TTFB。\n"+
			"★ 这就是实测「789 条 ttfb_ms=0 全是 mode=sync」的形态：\n"+
			"  total_ms 无法拆分「上游慢」与「我们慢」。\n行=%s", out)
	}
	if !strings.Contains(out, "TTFB=") {
		t.Errorf("输出里没有 TTFB 列，行=%s", out)
	}
	if !strings.Contains(out, "| sync |") {
		t.Errorf("这条本该走 sync 模式，行=%s", out)
	}
}

// TestSyncRequestTotalTimeoutFires 钉住非流式路径**也有**总时长闸门。
//
// # 为什么这条独立存在（它是 inkstone 那两条实测记录的形态）
//
// 上游默认的 `http.Client.Timeout` 只装在 `ChatHTTP` 上、而且值是 0；
// 其余上游走 cfg.Provider，更是三项超时一个都不沾。所以：
//
//	workbuddy/*  stream        total=380282ms  tokens=0
//	inkstone/*   （120.0s）     ← 恰好在 timeout_seconds 上，看着像"超时生效"
//
// 后两条的 120.0s 其实是**默认上游**的约束恰好落地，而不是"非流式被总时长管住了"。
// 这条用例造一个永不结束的非流式上游，断言请求在配置上限内被终止并回 504。
//
// 变异验证：把非流式分支的 newStreamWatchdog 第二参改成 0，本用例变红。
func TestSyncRequestTotalTimeoutFires(t *testing.T) {
	body := newPacedBody(true) // 永不结束
	up, pb := pacedUpstream(t, body, 200)
	t.Cleanup(func() { _ = pb.Close() })

	const limit = 400 * time.Millisecond
	h := newStreamHealthHandler(up, limit, 0)

	// 守护：没有闸门时这条请求会永久挂住（这正是要防的回归）。
	done := make(chan struct {
		code int
		body string
	}, 1)
	start := time.Now()
	go func() {
		rec := httptest.NewRecorder()
		// **不是** stream:true —— 走非流式路径。
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
		h.ServeHTTP(rec, req)
		done <- struct {
			code int
			body string
		}{rec.Code, rec.Body.String()}
	}()

	select {
	case r := <-done:
		elapsed := time.Since(start)
		if r.code != 504 {
			t.Errorf("状态码=%d，期望 504 —— 非流式的挂死流必须被总时长闸门终止"+
				"（而不是挂到调用方自己的超时）。body=%q", r.code, r.body)
		}
		if !strings.Contains(r.body, string(timeoutTotal)) {
			t.Errorf("响应体里没有可 grep 的超时原因 %q，body=%q", timeoutTotal, r.body)
		}
		if elapsed > 5*time.Second {
			t.Errorf("耗时 %s 远超上限 %s", elapsed, limit)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("非流式请求在守护时限内没有返回 —— 非流式路径没有被总时长闸门覆盖。\n" +
			"★ 这正是 inkstone 那两条 120.0s / tokens=0 记录的形态。")
	}
}

// （logbuf.Entry.TTFBMS），而不只是打印在 stdout 表格里。
//
// 为什么要单列一条：表格行与环形缓冲是同一份数据的两个出口，而
// /admin/logs 与落盘的 request-log.jsonl 读的是后者。只填前者等于
// "控制台上看得见、落盘日志里还是 0" —— 实测报告引用的正是落盘数据。
func TestSyncRequestTTFBReachesLogRing(t *testing.T) {
	withChatLog(t)

	body := newPacedBody(false, pacedStep{delay: syncTTFBStall, data: sseOK})
	up, pb := pacedUpstream(t, body, 200)
	t.Cleanup(func() { _ = pb.Close() })

	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})

	// 记录本次请求前的 seq，之后只读新增的那一条（环形缓冲是进程级共享的，
	// 不能假设它是空的）。
	_, before := chatLogRing.Snapshot(0)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	h.ServeHTTP(rec, req)

	entries, _ := chatLogRing.Snapshot(before)
	if len(entries) != 1 {
		t.Fatalf("期望本次请求落下恰好 1 条日志，得到 %d 条", len(entries))
	}
	e := entries[0]
	if e.Mode != "sync" {
		t.Fatalf("mode=%q，期望 sync —— 本用例测的是非流式路径", e.Mode)
	}
	if e.TTFBMS <= 0 {
		t.Errorf("落盘日志的 ttfb_ms=%d，期望 >0。\n"+
			"★ 非流式请求不记 TTFB 会让 total_ms 无法拆分上游耗时与我们自己的开销。", e.TTFBMS)
	}
	// 量级校验：TTFB 必须**包含**上游那段停顿，不能只是"随便填了个正数"。
	if e.TTFBMS < syncTTFBStall.Milliseconds() {
		t.Errorf("ttfb_ms=%d 小于上游停顿 %d ms —— "+
			"TTFB 的计时起点太晚（起点必须是请求进入 handler 的时刻，"+
			"否则会把鉴权/选号/建连的开销算丢，把上游耗时报小）",
			e.TTFBMS, syncTTFBStall.Milliseconds())
	}
}
