// taskslot.go 后台任务槽：同一时刻只允许一个全量任务。
//
// # 搬运说明（Task 3c）
//
// 本文件原先在 internal/admin/admin.go（类型名 taskSlot）。
// 它随签到/保活/旅行/成长这几条全量端点一起搬过来，判定逻辑逐字未改：
// 已有任务在跑时返回 false，调用方回 409 而**不是排队**
// （排队会让界面误以为立刻执行了）。
//
// 任务结果是 []map[string]any 而不是强类型：这个槽要同时装下
// 签到结果（调度器类型）、旅行/成长结果（本包类型），
// 而它们对外的 JSON 形状一致（uid/status/detail/credits）。
// 用后端 shape 中转，避免为一个纯展示结构引入第三份类型定义。
package workbuddy

import (
	"fmt"
	"sync"
	"time"
)

// slotHardTimeout 单个全量任务的硬上限。
//
// # 为什么必须有它（实测事故）
//
// 改造前这里只有 recover()，没有任何 deadline。而 fn() 内部的
// `act.run(p, a)` / `p.growthTasks(a)` 打的是真实上游 HTTP，既没有
// ctx 也没有 http.Client.Timeout —— 一旦某个账号的请求挂住
// （TCP 已 ESTAB 但对端不返回），fn() 永远不返回：
//
//	running 恒为 true → 之后所有任务（签到/成长/旅行）全部 409
//	任务槽没有自愈路径，只能重启进程
//
// 实测：growth-auto-all 跑 10 分钟只处理了 5/8 个账号，
// 最后一条 autotask 日志之后 5 分半零输出，而 ss 显示仍有连接挂着。
//
// 取 20 分钟：正常全量（十几个号 × 十几个任务，含节流 sleep）是分钟级，
// 20 分钟足以覆盖慢上游；超过就说明有请求真挂了。
const slotHardTimeout = 20 * time.Minute

// TaskSlot 的默认实现。
type taskSlot struct {
	mu       sync.Mutex
	running  bool
	kind     string
	started  time.Time
	finished time.Time
	results  []map[string]any
	errMsg   string

	// gen 每次 Start 自增。看门狗与任务 goroutine 都带上自己那一代，
	// 只有代次一致才允许写状态 —— 否则"上一代的看门狗"会把
	// "新一代正在跑"的槽位误标成空闲。
	gen uint64
}

// NewTaskSlot 建一个空的任务槽（可注入给 AdminEnv.TaskSlot）。
//
// 导出的唯一理由：cmd/server 需要把它交给调度器，
// 让 /admin/schedule+/admin/task 两条端点与签到/保活共用同一个槽。
func NewTaskSlot() TaskSlot { return newTaskSlot() }

func newTaskSlot() *taskSlot { return &taskSlot{} }

// Start 尝试占用任务槽并异步执行 fn（在调用方 goroutine 外另起一个）。
//
// panic 被捕获并记录成 error 字段而不是让进程崩：这些任务会打上游，
// 单次上游的意外不该带崩整个网关。
//
// # 看门狗（本次修复）
//
// 除 panic 外再加一道**硬超时**：超过 slotHardTimeout 就强行释放槽位，
// 并把已经产出的部分结果落进 errMsg 让用户看得见"这趟是被掐断的"。
//
// ⚠ 掐断**不中断** fn() 本身 —— Go 没有安全的 goroutine 强杀。卡住的
// 那个请求会继续挂在后台直到上游自己断开（或 TCP 超时）。这是有意的取舍：
//
//	留着槽位被占死 → 网关所有后台任务永久瘫痪（实测事故）
//	放掉槽位但有泄漏 goroutine → 只是这一趟白跑，功能可用
//
// 真正的治本要给它一个带 ctx 的 http.Client（见 autotask.go 的调用点）。
// 看门狗是让"已经发生的事故"能自愈，不是替代那条修复。
func (t *taskSlot) Start(kind string, fn func() []map[string]any) bool {
	t.mu.Lock()
	if t.running {
		t.mu.Unlock()
		return false
	}
	t.running = true
	t.kind = kind
	t.started = time.Now()
	t.finished = time.Time{}
	t.results = nil
	t.errMsg = ""
	t.gen++
	gen := t.gen
	t.mu.Unlock()

	go func() {
		var results []map[string]any
		var errMsg string
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					errMsg = fmt.Sprintf("panic: %v", rec)
				}
			}()
			results = fn()
		}()
		t.mu.Lock()
		// 代次不符 = 本代已被看门狗判定超时并释放（或新任务已接管）。
		// 此时不得回写，否则会把新任务的运行态覆盖成"已完成"。
		if t.gen == gen && t.running {
			t.running = false
			t.finished = time.Now()
			t.results = results
			t.errMsg = errMsg
		}
		t.mu.Unlock()
	}()

	// 看门狗：到点强行释放槽位，让后续任务可以再进来。
	go func() {
		timer := time.NewTimer(slotHardTimeout)
		defer timer.Stop()
		<-timer.C
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.gen != gen || !t.running {
			return // 本代早已正常结束（或已被接管），不动它
		}
		t.running = false
		t.finished = time.Now()
		t.errMsg = fmt.Sprintf("任务超过 %s 硬上限被看门狗终止（部分结果已丢弃）；"+
			"上游很可能有请求挂死 —— 本次释放槽位以免阻塞后续任务，建议查上游连通性",
			slotHardTimeout)
	}()
	return true
}

// ForceRelease 强行把槽位置为空闲（卡死时的逃生门）。
//
// # 与看门狗的关系
//
// 看门狗是**自动**兜底（20 分钟），本方法是**人工**立刻解。
// 两者共用同一条代次失效逻辑：都只置 running=false 并递增代次，
// 于是那个还挂着的 fn() goroutine 醒来时发现自己代次过期，不会回写。
//
// 未在跑时返回 false —— 让调用方能区分"解开了"与"本来就是空的"。
func (t *taskSlot) ForceRelease(reason string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.running {
		return false
	}
	t.running = false
	t.finished = time.Now()
	t.gen++ // 让在途的那趟失效（它醒来后不会覆盖状态）
	if reason == "" {
		reason = "被人工强制释放"
	}
	t.errMsg = reason + "（部分结果已丢弃；在途的上游请求可能仍在后台，直到上游断开）"
	return true
}

// Snapshot 当前任务槽状态。
//
// 字段名是**对外契约**（前端按它渲染），必须与改造前的 admin.taskSlot.snapshot 一致。
func (t *taskSlot) Snapshot() map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[string]any{
		"running": t.running,
		"kind":    t.kind,
		"results": t.results,
	}
	if !t.started.IsZero() {
		out["started_at"] = t.started
	}
	if t.running {
		out["elapsed_sec"] = int64(time.Since(t.started).Seconds())
	} else if !t.finished.IsZero() {
		out["finished_at"] = t.finished
		out["duration_sec"] = int64(t.finished.Sub(t.started).Seconds())
		// 看门狗终止时 results 恒为 nil（那趟的部分结果在卡死的 goroutine 里）。
		// 显式给个空数组而不是 null：前端对 null 与 [] 的处理不同，
		// 而这里的语义是"有结果可看，只是这一趟没产出"。
		if t.results == nil && t.errMsg != "" {
			out["results"] = []map[string]any{}
		}
		out["ok"] = sumStatus(t.results, statusOK)
		out["fail"] = sumStatus(t.results, "fail")
		out["already"] = sumStatus(t.results, "already")
		if t.errMsg != "" {
			out["error"] = t.errMsg
		}
	}
	return out
}

// sumStatus 统计结果里某个状态的条数（读的是中转后的 JSON 形状）。
func sumStatus(rs []map[string]any, want string) int {
	n := 0
	for _, r := range rs {
		if s, ok := r["status"].(string); ok && s == want {
			n++
		}
	}
	return n
}
