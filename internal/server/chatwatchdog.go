// chatwatchdog.go chat 出站流的**时间闸门**：请求级总时长上限 + 首内容帧看门狗。
//
// # 它修的是什么（用户实测的现象）
//
// request-log.jsonl 里最长的几条 stream 记录：
//
//	workbuddy/deepseek-v4.1-flash  stream  total=380282ms  tokens=0
//	workbuddy/deepseek-v4-flash    stream  total=325001ms  tokens=0
//
// 一个 token 都没产出，却把客户端挂满了 380 秒。根因不是某个客户端配错，
// 而是流式出站链路上**没有任何"总时长"概念**：
//
//	ChatHTTP.Timeout = 0                    聊天流刻意不设总时长
//	Transport.ResponseHeaderTimeout = 120s  只管"首字节之前"
//	IdleTimeout = 300s                      只管"两次读之间"，且要求上游
//	                                        真的被读得动（380s > 300s
//	                                        就是它没兜住的证据）
//
// 所以要补两道**与流量形态无关**的闸门：到点必断，无论上游当时在发什么。
//
// # ⚠ 为什么加在 handler 层（这是本文件存在的全部理由）
//
// chat 有**两条**出站路径：
//
//	默认上游   cfg.Upstream.ChatStreamWithIP(...)   ← internal/upstream
//	其余上游   cfg.Provider.Chat(ctx, ...)          ← jiyuan / loomy / trae / inkstone…
//
// 只在 internal/upstream 里加超时**只能管住第一条**（改造前正是只有它有
// ResponseHeaderTimeout + IdleTimeout），第二条既无总时长也无空闲监控。
//
// 两条路径唯一的共同形状是"返回一个 io.ReadCloser"，所以闸门就包在那个
// body 外层 —— 一次覆盖全部上游（含将来新增的），且不依赖任何上游配合改动。
//
// # 闸门是"一次性绝对时刻"，不是间隙检测
//
//	总时长     到点必断（无论中间多活跃）—— 预算跨换号共享，不重开
//	首内容帧   到点时若已见过产出帧 → 永久放行，不再复查
//
// 与 IdleTimeout 的"静默超过阈值"是互补的两件事：缓慢但持续的正常长回答
// 不会被它们掐掉（第二道见产出即放行）。反向断言见
// TestStreamLongHealthyAnswerNotKilled。
//
// # 中断手段与 internal/upstream/idle.go 是同一个
//
// 到点就 Close 底层 body —— 两条路径的 body 都遵守"Close 即中断"：
//
//	默认上游   monitorBody.Close() → cancel() 请求 ctx → transport 中止 body
//	其余上游   Provider 自己的 http body → Close 关连接（ctx 到点同理）
//
// 所以本文件不是新机制，而是把那套"到点 cancel/Close 中断阻塞中的 Read"
// 的手法，从"只覆盖默认上游的空闲检测"扩成"覆盖全部上游的总时长 + 首帧闸门"。
//
// # 没有 goroutine
//
// 两道闸门都是 time.AfterFunc（一次性定时器），Close 之后不留任何定时器或
// 协程；Read 始终在调用方 goroutine 里同步直通，不做转发、不加缓冲。
//
// # ⚠ 已知边界：工具循环那条路径**不在**本闸门覆盖内
//
// handler 的轮转循环里，工具循环（shouldRunToolLoop 命中时）在 chatVia **之前**
// 就分流走了，它自己调 `Provider.Chat`（见 chattool_send.go 的 chatStreamOnce），
// body 不经过这里。所以"上游实现了工具注入（如 loomy）且请求走了工具循环"时，
// 总时长闸门不生效。
//
// 为什么这次**不**顺手把它也包上：闸门要包在 body 外层，而那个 body 是
// chattool_send.go 内部创建的 —— 在那个文件里加包装点会同时改动工具循环
// 的读取路径（它自己用 wire.NewFrameScanner 逐帧读，且统计要靠
// toolLoopStats 接回来），风险远大于收益，且超出本任务的写入面。
//
// 判定它是否是"真缺口"的依据是数据：实测挂死最久的两条是
// workbuddy/deepseek-v4.1-flash 与 workbuddy/deepseek-v4-flash，
// 而 workbuddy 未实现 ChatTools 扩展点 → 那两个请求根本不走工具循环。
//
// 要补的话，正确做法是在 chattool_send.go 的 chatStreamOnce 返回处套同一个
// newStreamWatchdog（参数用 toolLoopParams 里已有的 start），
// 而不是把它挪到 handler 层 —— 那时闸门仍然只包 body，语义不变。
// 这一步留给后续任务，由它自己的测试守着。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// streamTimeoutReason 闸门命中的原因。同时用作流内错误帧的 code 字段，
// 让排障脚本可以按一个稳定的字符串 grep（而不是去猜文案）。
type streamTimeoutReason string

const (
	// timeoutTotal 请求级总时长上限命中（绝对时刻，跨换号共享同一份预算）。
	timeoutTotal streamTimeoutReason = "stream_total_timeout"
	// timeoutFirstByte 首内容帧看门狗命中（每次建连独立计时）。
	timeoutFirstByte streamTimeoutReason = "stream_no_first_frame"
)

// streamTimeoutError 闸门命中。
//
// 为什么要一个**有名字的**错误类型：闸门中断阻塞中的 Read 时，上层拿到的
// 本来只会是底层那句 "read on closed body"（或者 proto 层的一句 context
// 取消）—— 那是"读失败了"，不是"谁超时了"。上层要靠原因决定收场方式
// （能不能换号、回 504 还是写流内错误帧），所以判据必须带得出来。
type streamTimeoutError struct {
	Reason streamTimeoutReason
	// Limit 命中的那道闸门的时限（日志与文案用）。
	Limit time.Duration
}

func (e *streamTimeoutError) Error() string {
	switch e.Reason {
	case timeoutTotal:
		return fmt.Sprintf("上游流总时长超过上限 %s，已断流", e.Limit)
	case timeoutFirstByte:
		return fmt.Sprintf("上游建连后 %s 内未产出任何内容，已断流", e.Limit)
	}
	return "上游流超时，已断流"
}

// outputFlag 记录"上游这条流是否已经产出过内容"。
//
// 它是统计器（chatStatsReader）与看门狗（streamWatchdog）之间**唯一**的
// 共享状态：逐帧解析的那个组件知道"什么算产出"，看门狗只读这个布尔值。
// 这样"产出"的判据只有一份（在 chatStatsReader.parseSSELine 里），
// 而不是在两处各写一遍 —— 两处各写一遍的漂移形态本仓库已经付出过代价
// （见 wire.sseData 的注释）。
//
// 方法对 nil 接收者安全：既有调用点（工具循环的统计器）不装闸门，
// 它们的 output 是 nil，写日志的路径不该因此 panic。
type outputFlag struct{ v atomic.Bool }

func (f *outputFlag) mark() {
	if f != nil {
		f.v.Store(true)
	}
}

func (f *outputFlag) ok() bool { return f != nil && f.v.Load() }

// streamWatchdog 包在上游 body 外层，施加两道时间闸门。
//
// 字段 out 是"是否已产出"的共享标志：由外层的 chatStatsReader 逐帧置位
// （见 outputFlag 与 chatStatsReader.parseSSELine）。
type streamWatchdog struct {
	rc  io.ReadCloser
	out *outputFlag

	// totalLimit/firstByteLimit 只用于日志与错误文案（闸门本身记绝对时刻）。
	totalLimit     time.Duration
	firstByteLimit time.Duration

	total *time.Timer // 总时长闸门（绝对时刻，可能为 nil = 不限）
	first *time.Timer // 首内容帧闸门（可能为 nil = 不限）

	mu      sync.Mutex
	hit     *streamTimeoutError
	stopped bool

	closeOnce sync.Once
	closeErr  error
}

// newStreamWatchdog 装闸门。
//
//	since      请求进入 handler 的时刻（与 chatStat.start 同一个值）
//	total      请求级总时长上限；<=0 = 不设这道闸门
//	firstByte  上游建连成功后首个产出帧的等待上限；<=0 = 不设这道闸门
//
// ⚠ 总时长的锚点是 since（绝对时刻）而不是"从现在起算 total"：
// 换号重试**必须**共享同一份预算，否则 MaxRotate 次重试等于把上限乘 3 ——
// 那正是"挂到天荒地老"的形态（380s ≈ 多个上游各挂一段）。
//
// ⚠ 首帧闸门相反，是**从现在起算**：它的语义是"连接已建立但迟迟没有内容"，
// 所以每次建连独立计时；这也让首帧命中后换号重试是有意义的（新号有新预算），
// 而整体仍被总时长闸门兜住。
func newStreamWatchdog(rc io.ReadCloser, since time.Time, total, firstByte time.Duration) *streamWatchdog {
	b := &streamWatchdog{
		rc:             rc,
		out:            &outputFlag{},
		totalLimit:     total,
		firstByteLimit: firstByte,
	}
	if total > 0 {
		b.total = time.AfterFunc(remainUntil(since.Add(total)), func() {
			b.trip(&streamTimeoutError{Reason: timeoutTotal, Limit: total})
		})
	}
	if firstByte > 0 {
		b.first = time.AfterFunc(firstByte, func() {
			// 到点只复查**一次**：已经有产出 → 这道闸门永久放行。
			// 所以"慢慢吐字但一直在吐"的正常长回答绝不会被它掐掉。
			if b.out.ok() {
				return
			}
			b.trip(&streamTimeoutError{Reason: timeoutFirstByte, Limit: firstByte})
		})
	}
	return b
}

// remainUntil 返回到 deadline 的剩余时长；已过则夹到 0（AfterFunc 立刻触发）。
func remainUntil(deadline time.Time) time.Duration {
	if d := time.Until(deadline); d > 0 {
		return d
	}
	return 0
}

// Read 直通底层流；闸门命中后把底层的中断错误换成**有名字的**超时错误。
//
// 注意返回 (n>0, 超时错误) 是合法的 io.Reader 行为（数据在前、错误在后），
// 而两处消费者都能正确处理：bufio.ReadString 会把这一段数据连同错误一起
// 交出来，wire.Stream 先写完这一行再返回错误。
func (b *streamWatchdog) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if hit := b.tripped(); hit != nil {
		return n, hit
	}
	return n, err
}

// Close 停掉闸门并关闭底层流。
//
// # 为什么必须幂等
//
// 生产路径上 Close 会被调两次：一次是 handler 在 wire.Stream 之后显式收尾，
// 一次是闸门自己 trip 时中断阻塞中的 Read。下面 rc.Close 本身就是幂等的
// （net/http body 的契约），但**上游实现不一定**：有的 body 的 Close 会
// 把某个 channel 关掉或写某个状态，调两次就 panic / 破坏连接复用。
// 所以这一层把幂等责任接过来，而不是指望每一路上游都做对。
func (b *streamWatchdog) Close() error {
	b.closeOnce.Do(func() {
		b.stop()
		b.closeErr = b.rc.Close()
	})
	return b.closeErr
}

// stop 撤销两道闸门。撤销之后到点的定时器不会再中断 body —— 流已经结束了。
func (b *streamWatchdog) stop() {
	b.mu.Lock()
	b.stopped = true
	b.mu.Unlock()
	for _, t := range []*time.Timer{b.total, b.first} {
		if t != nil {
			t.Stop()
		}
	}
}

// tripped 返回已命中的闸门；未命中返回 nil。
func (b *streamWatchdog) tripped() *streamTimeoutError {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.hit
}

// totalTimeoutError 造一个"总时长命中"的错误（供 ctx 先到点时复用同一文案）。
func (b *streamWatchdog) totalTimeoutError() *streamTimeoutError {
	return &streamTimeoutError{Reason: timeoutTotal, Limit: b.totalLimit}
}

// trip 记录命中原因并中断底层流。幂等：只有第一次命中生效。
func (b *streamWatchdog) trip(e *streamTimeoutError) {
	b.mu.Lock()
	if b.stopped || b.hit != nil {
		b.mu.Unlock()
		return
	}
	b.hit = e
	b.mu.Unlock()
	// Close 之后阻塞中的 Read 会返回错误（机制见文件头），
	// 于是"到点"这件事变成一次可被上层识别的读取失败。
	_ = b.rc.Close()
}

// streamTimeoutHit 把"读取失败"翻译成闸门命中；不是超时就返回 nil。
//
// # 为什么认两种来源
//
// 闸门与 ctx deadline 是**互补的两条兜底**（见文件头）：
//
//	默认上游   不收 ctx（ChatStreamWithIP 内部自建 context）→ 只有闸门能兜
//	其余上游   尊重 ctx → 两条都能兜，命中哪条取决于谁先到点
//
// 上层只关心两件事：这是不是超时、能不能换号重试。所以这里把两种形态
// 归一成一个原因。
//
// ⚠ 只认 DeadlineExceeded，**不认** Canceled：后者是"客户端断开"，
// 是既有的另一条路径（取消 → terr → 只换号不喂熔断），本文件不改它的行为。
func streamTimeoutHit(wd *streamWatchdog, ctx context.Context, err error) *streamTimeoutError {
	if err == nil {
		return nil
	}
	if hit := wd.tripped(); hit != nil {
		return hit
	}
	if ctx != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return wd.totalTimeoutError()
	}
	return nil
}

// commitTracker 记录"响应是否已经写过任何字节"。
//
// 闸门命中后的收场方式取决于这一事实：
//
//	未写过   状态码还能改 → 回一个明确的 504（客户端拿到标准错误 JSON）
//	已写过   状态码收不回来 → 只能写流内错误帧 + [DONE]
//
// 前者回 504 而不是"假装成功"，后者补 [DONE] 而不是静默截断 —— 两条都是
// "让客户端立刻知道发生了什么"，改造前那种"直接 return"会让它挂到自己的超时。
//
// ⚠ 它**必须**自己实现 Flush：包一层 ResponseWriter 会丢掉原类型的可选接口，
// wire.Stream 的 `w.(http.Flusher)` 一旦失败，SSE 就不再逐帧 flush
// （本仓库在 harden.go 里有同样一条教训）。
type commitTracker struct {
	http.ResponseWriter
	wrote atomic.Bool
}

func (t *commitTracker) Write(p []byte) (int, error) {
	t.wrote.Store(true)
	return t.ResponseWriter.Write(p)
}

func (t *commitTracker) WriteHeader(code int) {
	t.wrote.Store(true)
	t.ResponseWriter.WriteHeader(code)
}

// Flush 转发给底层（不承诺底层一定支持 —— 与 wire.Stream 的用法一致）。
func (t *commitTracker) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 让 http.ResponseController 之类按惯例能找到底层 writer。
func (t *commitTracker) Unwrap() http.ResponseWriter { return t.ResponseWriter }

// committed 报告响应是否已提交（状态码还能不能改）。
func (t *commitTracker) committed() bool { return t.wrote.Load() }

// writeStreamTimeoutFrame 在**已经开始输出**之后，用流内错误信封告诉客户端
// "上游卡住了，本次回答到此为止"，并补一个 [DONE] 让它能正常收尾。
//
// # 为什么是 error 帧而不是别的东西
//
// 帧形态与 wire.Stream 的空流兜底帧同一形状（{"error":{...}}），
// 客户端按 OpenAI 规范会把它当上游错误；多带的 code 字段给排障脚本 grep 用
// （未知字段客户端会忽略）。刻意不复用 normalizeFrame：它会剥掉 error 字段。
func writeStreamTimeoutFrame(w http.ResponseWriter, e *streamTimeoutError) {
	payload, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": e.Error(),
			"type":    "upstream_error",
			"code":    string(e.Reason),
		},
	})
	if err != nil {
		// 不可能：全是字符串。留着是为了不静默吞掉"将来有人改坏了 payload"。
		return
	}
	_, _ = io.WriteString(w, "data: "+string(payload)+"\n\n")
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
