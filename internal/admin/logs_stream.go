package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"
)

// logsStreamPollInterval 增量查询间隔。
//
// 300ms 是「人感觉是实时」与「读侧开销可忽略」的折中：
//   - 日志面板里追加一行的延迟只要低于人的注意阈值就够了；300ms 的追加
//     读起来和「本地立刻发生」没有差别（打字回声才要求 <100ms）；
//   - 每个 tick 只做一次 mutex 保护下的 ≤2000 条定长切片拷贝 + 一次 seq 过滤，
//     没有 IO、没有序列化以外的分配。10 个面板同时开着也只是 33 次/秒的空转；
//   - 走 50ms 只会让空闲连接白烧 CPU，走 1s 则会让用户点完「发一次请求」
//     之后开始怀疑这条链路没生效。
//
// 用 time.Ticker 而不是在 logbuf 里挂订阅：见 logsStream 的注释。
const logsStreamPollInterval = 300 * time.Millisecond

// logsStreamKeepAlive 心跳帧间隔。
//
// 15s 同时满足三件事：比反代/网关常见的 60s 空闲断连阈值留足余量；
// 比浏览器 EventSource 判定掉线的窗口更早，客户端不会误报重连；
// 又足够稀疏，空闲连接不会每 tick 都有字节上行走。
//
// 声明成变量而不是常量，只为测试能在不真等 15s 的前提下观察到心跳帧
// （见 logs_stream_test.go 的 TestLogsStreamKeepAliveDoesNotPolluteData）。
var logsStreamKeepAlive = 15 * time.Second

// logsStream 实时推送请求日志（Server-Sent Events 长连接）。
//
// 契约（前端 EventSource 一一对应，不得单方面改形状）：
//
//	GET /admin/logs/stream?since=<seq>
//	Content-Type: text/event-stream
//	event: log
//	id: <seq>
//	data: <logbuf.Entry 的 JSON，与 /admin/logs 的 items 元素同构>
//	（空闲）每 logsStreamKeepAlive 写一行 ": keep-alive"
//
// # 与 /admin/logs、/admin/logs/history 的职责切分
//
// 本端点只推「连上之后」新增的行，**不重放历史**：
// since 缺省或 <=0 时把游标初始化到当前最大 seq，于是「打开面板」这个动作
// 不会灌进去两千条历史（那由 /admin/logs/history 分页承担，两条端点的窗口
// 不重叠）。要续传就显式带 since=上次收到的最大 id，从下一条开始推。
//
// # 为什么读侧轮询，而不是给 logbuf.Ring 加订阅/回调
//
// Ring 的写路径是**每个 chat 请求**都会走的热路径。给它加订阅者集合意味着
// 每个请求多一次读者列表遍历，还会引入「慢读者把写路径拖住」这种新的失效
// 模式 —— 而收益只是把延迟从 300ms 降到 0。复杂度全部关在本 handler 里，
// 且 Ring.Snapshot(sinceSeq) 本来就是为增量游标设计的（见其文档注释）。
//
// # 生命周期
//
// 一次请求 = 一个 goroutine，随 r.Context() 结束而退出：前端按面板可见性
// 关 EventSource，桌面版会反复开关，泄漏一个 goroutine 就是泄漏一份
// （最多 2000 条）日志的引用。ctx 取消、写失败、客户端断开三条路径都直接
// return，ticker 由 defer 停掉。
func (h *Handler) logsStream(w http.ResponseWriter, r *http.Request) {
	// Flusher 是 SSE 的前提：没有它，写进 ResponseWriter 的帧会攒在
	// net/http 的 4KB 缓冲里，客户端看到的是「连上了但一直没有数据」。
	// 这是部署形态问题（某些中间件包装 ResponseWriter 时会丢掉 Flush），
	// 不是客户端问题，所以明确回 501 + 原因，而不是假装连上。
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusNotImplemented,
			"当前 ResponseWriter 不支持 Flush，无法提供 SSE 长连接；请检查是否有中间件包装了 ResponseWriter")
		return
	}
	if h.cfg.Ring == nil {
		// 未接线请求日志缓冲时不装死：长连接挂在那儿只发心跳，前端会一直
		// 显示「已连接但无数据」，比直接报错更难查。
		writeError(w, http.StatusServiceUnavailable, "请求日志环形缓冲未启用（admin.ring 未接线）")
		return
	}
	ring := h.cfg.Ring

	// 游标：显式 since>0 表示「我上次看到这儿了」，从下一条开始推；
	// 缺省/非法/<=0 表示「只要新的」，初始化到当前最大 seq。
	cursor, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	if cursor <= 0 {
		// 用 maxInt64 探一次就能拿到当前最大 seq，且不返回任何条目
		//（Snapshot 在 sinceSeq >= total 时直接返回 nil, total）。
		_, cursor = ring.Snapshot(math.MaxInt64)
	}

	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache, no-transform")
	// 反代（nginx 默认会缓冲上游响应）需要它才会逐帧转发。
	hdr.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// 先刷一次头：客户端要看到 200 + Content-Type 才算连上，
	// 否则在没有任何新日志的静默期里它会一直以为自己没连上。
	flusher.Flush()

	poll := time.NewTicker(logsStreamPollInterval)
	defer poll.Stop()
	keep := time.NewTicker(logsStreamKeepAlive)
	defer keep.Stop()

	for {
		select {
		case <-r.Context().Done():
			// 客户端断开 / 服务端关闭：直接退出，不写任何收尾帧
			//（对端已经不在了，写只会拿到 EPIPE）。
			return

		case <-keep.C:
			if _, err := w.Write([]byte(": keep-alive\n\n")); err != nil {
				return // 写失败 = 连接已死，提前退出而不是继续空转
			}
			flusher.Flush()

		case <-poll.C:
			items, max := ring.Snapshot(cursor)
			if len(items) == 0 {
				// cursor 与 max 同步一下：环形缓冲滚过之后 Snapshot 会把
				// 过旧游标夹到窗口下界，这里以 max 为准，避免重复索取。
				cursor = max
				continue
			}
			// 一个 tick 内可能有多条（比如突发请求或客户端刚带 since 续传），
			// 攒进一个 buffer 一次性写 + 一次 Flush：帧数不变，系统调用变少，
			// 客户端也不会看到「半个批次」。
			var buf bytes.Buffer
			for _, e := range items {
				raw, err := json.Marshal(e)
				if err != nil {
					// Entry 全是基础类型 + time.Time，Marshal 理论上不会失败；
					// 真失败就跳过这一条，不因为一条脏数据掐掉整条流。
					continue
				}
				fmt.Fprintf(&buf, "event: log\nid: %d\ndata: %s\n\n", e.Seq, raw)
			}
			cursor = max
			if buf.Len() == 0 {
				continue
			}
			if _, err := w.Write(buf.Bytes()); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
