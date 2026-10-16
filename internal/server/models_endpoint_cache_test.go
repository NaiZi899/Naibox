// models_endpoint_cache_test.go —— /v1/models 多上游目录构建的**性能回归闸门**。
//
// # 为什么这四条断言必须存在（它们各自守的是不同的失效形态）
//
// 本次改动把"其余上游"的取数从 串行+无缓存+无超时 换成 并发+按上游缓存+5s 超时。
// 每一件事单独回退都会让端点退回一个**不同的**坏状态，而它们的线上表现
// （"控制台卡住了"）看起来一模一样 —— 所以必须一条一件地钉住：
//
//	缓存回退 → 调用次数随请求次数线性增长（每次都重打上游）
//	并发回退 → 端点耗时 = 各上游耗时**之和**（最慢的那个决定所有人）
//	超时回退 → 一个不认 ctx 的上游能把端点挂到它自己的时间里
//
// # 为什么断言"调用次数"而不是"耗时上限"
//
// 耗时断言在 CI/共享机器上会抖（本文件只在**有对比量**时才断耗时：例如
// "10 次缓存命中的总耗时小于 1 次未命中会花的 200ms×10"），而调用次数是
// 确定性的整数事实。凡是能写成次数的判据都写成次数，耗时只作为**辅助**
// （它证明"慢桩真的慢过"，否则次数断言可能因为桩根本没睡而假绿）。
//
// # 为什么每个用例自建 Handler
//
// 按上游缓存在 models_endpoint_cache.go 里是**按 Handler 分实例**的，所以
// 用例之间天然不串味（这正是那样设计的目的之一 —— 见 providerModelsCaches
// 的注释：models_supports_images_test.go 的三个用例给同一个 "inkstone"
// 配了不同的模型集，全局缓存会让后两个用例读到第一个的目录）。
package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/NaiZi899/omni2api/internal/auth"
	"github.com/NaiZi899/omni2api/internal/gateway"
)

// probeRouter 目录桩：在 testRouter 之上加**观测能力**（次数/延迟/是否忽略 ctx）。
//
// # 为什么嵌 testRouter 而不是重写一遍
//
// ProviderRouter 有十几个方法（其中大半是出站能力，本文件完全不涉及）。
// 嵌 testRouter 让那些方法原样传递，本文件只覆盖要观测的 Models ——
// 这也让"桩与接口逐字一致"这件事继续由 models_multiprovider_test.go 的
// `var _ ProviderRouter = testRouter{}` 守着，不必在这里重复一遍。
//
// # 为什么延迟/卡住要按上游分（而不是一个全局开关）
//
// 本文件最核心的判据是"**某个**上游卡住时，**其它**上游的模型照常列出"。
// 一个全局延迟开关表达不了"只有其中一个卡住"，那样测出来的只是"全都慢"。
type probeRouter struct {
	testRouter

	mu sync.Mutex
	// calls 每个上游被调了几次（缓存/负缓存的判据就是它）。
	calls map[string]int
	// delays 模拟"慢上游"：睡满这段时间**再**返回（尊重 ctx 的实现会先被超时打断）。
	delays map[string]time.Duration
	// hangs 模拟"不认 ctx 的上游"：无条件睡满，完全不看 ctx。
	//
	// 这不是虚构的边界：上游实现在自己的调用里插了多少层 HTTP 客户端、
	// 有没有真的把 ctx 传下去，出口层无从验证 —— 所以出口层**不能**假设
	// "ctx 一旦超时上游就会回来"。providerDirectories 的收集预算正是为它准备的。
	hangs map[string]time.Duration
	// emptyOK 里的上游返回 (空列表, ok=true)：复刻"上游明确说自己没有可用模型"。
	emptyOK map[string]bool
	// lastDeadline 记录最近一次拿到的 ctx 剩余预算（证明每次调用都带了自己的超时）。
	lastDeadline map[string]time.Duration
	// sawDeadline 记录最近一次调用是否带 deadline。
	sawDeadline map[string]bool
}

var _ ProviderRouter = (*probeRouter)(nil)

// Models 覆盖内嵌桩的 Models：先记次数与 ctx 预算，再按配置施加延迟/卡住。
func (r *probeRouter) Models(ctx context.Context, id string) ([]gateway.ModelInfo, bool) {
	r.mu.Lock()
	if r.calls == nil {
		r.calls = map[string]int{}
	}
	r.calls[id]++
	delay, hang := r.delays[id], r.hangs[id]
	emptyOK := r.emptyOK[id]
	dl, hasDL := ctx.Deadline()
	if r.lastDeadline == nil {
		r.lastDeadline = map[string]time.Duration{}
		r.sawDeadline = map[string]bool{}
	}
	if hasDL {
		r.lastDeadline[id] = time.Until(dl)
	}
	r.sawDeadline[id] = hasDL
	r.mu.Unlock()

	if delay > 0 {
		// 尊重 ctx 的慢上游：超时先到就按时返回（真实上游递归传给 HTTP 客户端
		// 之后就是这个行为）。
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, false
		}
	}
	if hang > 0 {
		// ⚠ 故意**不**看 ctx：这是本文件唯一需要"卡住"语义的地方。
		time.Sleep(hang)
	}
	if emptyOK {
		return nil, true
	}
	if r.testRouter.fail[id] {
		return nil, false
	}
	ms, ok := r.testRouter.models[id]
	if !ok || len(ms) == 0 {
		return nil, false
	}
	return ms, true
}

// callsOf 返回某上游被调了几次。
func (r *probeRouter) callsOf(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[id]
}

// deadlineSeen 返回最近一次调用拿到的 ctx 剩余预算与是否带 deadline。
func (r *probeRouter) deadlineSeen(id string) (time.Duration, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastDeadline[id], r.sawDeadline[id]
}

// probeHandler 造一个"默认上游 + 若干探测上游"的 handler。
//
// 默认上游用桩上游复刻动态目录（与 models_multiprovider_test.go 同一手法），
// 这样目录里默认那一支是确定的 16 条 —— 用例就能把注意力放在"其余上游"上，
// 而默认上游那条路径（fetchDynamicModels）的行为不由本文件负责。
func probeHandler(t *testing.T, r *probeRouter) *Handler {
	t.Helper()
	resetDynamicCache()
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, baselineDynamicBody(), false
	})
	return NewHandler(Config{
		Pool:            p,
		Upstream:        up,
		Provider:        r,
		DefaultProvider: "workbuddy",
		OwnedBy:         "workbuddy",
	})
}

// modelsOrdered 拉一次 /v1/models，返回**保留响应顺序**的 id 列表与本次耗时。
//
// 现有的 modelIDs（models_multiprovider_test.go）会把结果排序 —— 那对
// "目录里有哪些模型"是对的，但本文件还要钉住"并发取没有打乱输出顺序"，
// 所以需要一份不排序的取数。
func modelsOrdered(t *testing.T, h *Handler) ([]string, time.Duration) {
	t.Helper()
	rec := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	elapsed := time.Since(start)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v body=%s", err, rec.Body)
	}
	out := make([]string, 0, len(resp.Data))
	for _, m := range resp.Data {
		out = append(out, asString(m["id"]))
	}
	return out, elapsed
}

// ---------------------------------------------------------------------------
// 判据 1：第二次调用不再打上游
// ---------------------------------------------------------------------------

// TestV1ModelsOtherProviderDirectoryIsCachedOnce 是缓存的主断言：
// 连续两次请求，每个上游的 Models() **只被调一次**。
//
// 破了会怎样（这是改造前的实际状态）：每次请求都重打每个上游，于是
// 端点的耗时与失败率是"各上游直连体验"的叠加 —— 上游的重尾直接变成
// 控制台的卡顿（实测 /v1/models p50 4107ms / max 22630ms）。
func TestV1ModelsOtherProviderDirectoryIsCachedOnce(t *testing.T) {
	r := newProbeRouter([]string{"workbuddy", "alpha", "beta"}, map[string][]gateway.ModelInfo{
		"alpha": {{ID: "a-1", ContextWindow: 1000}},
		"beta":  {{ID: "b-1", ContextWindow: 2000}},
	})
	h := probeHandler(t, r)

	first, _ := modelsOrdered(t, h)
	if len(first) == 0 {
		t.Fatal("首次请求就没拿到目录 —— 用例失去意义")
	}
	// 先确认桩真的被问了（否则下面的"只调一次"可能因为桩压根没被走到而假绿）。
	for _, id := range []string{"alpha", "beta"} {
		if got := r.callsOf(id); got != 1 {
			t.Fatalf("首次请求后 %s 被调 %d 次，want 1 —— 桩没走到，后面的缓存断言会假绿", id, got)
		}
	}

	second, _ := modelsOrdered(t, h)
	if len(second) != len(first) {
		t.Fatalf("两次请求的目录条数不同：%d vs %d", len(first), len(second))
	}
	for _, id := range []string{"alpha", "beta"} {
		if got := r.callsOf(id); got != 1 {
			t.Errorf("★ 缓存失效：第二次请求后 %s 被调 %d 次，want 1 —— 每个请求都在重打上游", id, got)
		}
	}
}

// TestV1ModelsResetProviderCachesForcesRefetch 钉住缓存**真的可清**：
// 清空后下一个请求必须重新回源。
//
// 为什么需要：一个只能命中的缓存等于一份永久快照。上游当天上架了新模型，
// 用户点「刷新模型」之后必须能看到 —— 这条判定清缓存的那条路没被写坏。
//
// ⚠ 这里调的是 resetProviderModelsCaches（本文件的落点）。handler.go 的
// ResetModelsCache 在**他人写入面上**，本次没动它 —— 收口时那里要补上这一句。
func TestV1ModelsResetProviderCachesForcesRefetch(t *testing.T) {
	r := newProbeRouter([]string{"workbuddy", "alpha"}, map[string][]gateway.ModelInfo{
		"alpha": {{ID: "a-1", ContextWindow: 1000}},
	})
	h := probeHandler(t, r)

	modelsOrdered(t, h)
	modelsOrdered(t, h)
	if got := r.callsOf("alpha"); got != 1 {
		t.Fatalf("预热阶段 alpha 被调 %d 次，want 1", got)
	}

	resetProviderModelsCaches()
	modelsOrdered(t, h)
	if got := r.callsOf("alpha"); got != 2 {
		t.Errorf("★ 清缓存后 alpha 被调 %d 次，want 2 —— 缓存清了却没回源，"+
			"「刷新模型」会变成一次空操作", got)
	}
}

// ---------------------------------------------------------------------------
// 判据 2：缓存命中时耗时与调用次数都不随请求次数增长
// ---------------------------------------------------------------------------

// TestV1ModelsCachedHitDoesNotGrowWithRequestCount 钉住"命中"是**真的**命中：
// 10 次请求的总耗时必须远小于"每次都回源"会花的代价。
//
// 桩给一个 200ms 的延迟（模拟 loomy 那条 1960ms 的路径的量级），
// 于是判据是可以算术的：
//
//	无缓存：10 次 × 200ms = 2s 以上
//	有缓存：首次 200ms + 9 次近似 0
//
// 断言 1s 的界，两侧留足一倍的余量（不会因为机器抖动而假红）。
//
// # 为什么还要同时断次数
//
// 耗时断言会被"桩被优化掉了"骗过（比如哪天延迟参数没生效）。次数是确定性
// 事实，两者一起才构成闭环。
func TestV1ModelsCachedHitDoesNotGrowWithRequestCount(t *testing.T) {
	const slowDelay = 200 * time.Millisecond
	const repeats = 10

	r := newProbeRouter([]string{"workbuddy", "slow"}, map[string][]gateway.ModelInfo{
		"slow": {{ID: "s-1", ContextWindow: 1000}},
	})
	r.delays = map[string]time.Duration{"slow": slowDelay}
	h := probeHandler(t, r)

	// 预热：这一次必然回源，耗时至少 slowDelay —— 它同时证明"慢桩真的慢"。
	ids, warm := modelsOrdered(t, h)
	if !contains(ids, "slow/s-1") {
		t.Fatalf("预热请求里没有慢上游的模型 %v —— 延迟把结果弄丢了？", ids)
	}
	if warm < slowDelay {
		t.Fatalf("预热耗时 %v < 桩的延迟 %v —— 延迟没生效，"+
			"下面的耗时断言会失去意义（变成一次恒真的检查）", warm, slowDelay)
	}

	start := time.Now()
	for i := 0; i < repeats; i++ {
		got, _ := modelsOrdered(t, h)
		if !contains(got, "slow/s-1") {
			t.Fatalf("第 %d 次请求里慢上游的模型消失了: %v", i, got)
		}
	}
	total := time.Since(start)

	if got := r.callsOf("slow"); got != 1 {
		t.Errorf("★ 缓存失效：%d 次请求后慢上游被调 %d 次，want 1 —— "+
			"调用次数随请求次数增长了", repeats, got)
	}
	// 无缓存时这 10 次至少要 2s（10 × 200ms）。1s 的界只在"真命中"时才成立。
	if total > time.Second {
		t.Errorf("★ 缓存没生效：%d 次命中请求共花 %v（单次延迟只要 %v）—— "+
			"每次请求都在等上游", repeats, total, slowDelay)
	}
}

// TestV1ModelsCachedEmptyDirectoryIsNotRefetched 钉住缓存能区分
// "没这个上游"（没问过，要回源）与"缓存里是空列表"（问过了，1h 内不再问）。
//
// # 为什么必须区分（不是洁癖）
//
// 最自然的偷懒写法是拿 `len(entry.models) > 0` 当"有没有缓存"。那样一来，
// "上游明确回了空目录"（账号全被限流 / 该上游暂时没模型）会被当成"没缓存"，
// 于是**每次请求**都为它打一次注定为空的往返 —— 一个从不返回内容的慢上游
// 就变成了每次请求都要等的那个上游，正好抵消本次修复。
func TestV1ModelsCachedEmptyDirectoryIsNotRefetched(t *testing.T) {
	r := newProbeRouter([]string{"workbuddy", "emptysrc"}, nil)
	// 上游明确回 (空, ok=true)：这是"成功取数，结果是空"，不是失败。
	r.emptyOK = map[string]bool{"emptysrc": true}
	h := probeHandler(t, r)

	for i := 0; i < 3; i++ {
		ids, _ := modelsOrdered(t, h)
		// 空目录不该在响应里留下任何条目（连前缀都不该有）。
		for _, id := range ids {
			if len(id) > 9 && id[:9] == "emptysrc/" {
				t.Fatalf("第 %d 次请求里空目录上游竟然有条目: %s", i, id)
			}
		}
	}
	if got := r.callsOf("emptysrc"); got != 1 {
		t.Errorf("★ 空目录被当成没缓存了：上游被调 %d 次，want 1 —— "+
			"每次请求都会为它打一次注定为空的往返（实测这正好抵消本次修复）", got)
	}
}

// ---------------------------------------------------------------------------
// 判据 3：某个上游卡住时，端点在超时上限内返回，且其它上游照常列出
// ---------------------------------------------------------------------------

// TestV1ModelsStuckUpstreamDoesNotStallEndpoint 是最重要的一条：
// 一个**完全不看 ctx** 的上游（睡到 2× 超时）不得拖死整个端点。
//
// # 三个断言的各自意义
//
//	① 端点在 超时+余量 的上限内返回  → 超时/收集预算生效
//	② 别的上游的模型仍在            → 一个坏上游没有把整个目录打成空
//	③ 默认上游的模型仍在            → 公共端点对既有客户端没有退化
//
// ① 破了：控制台卡在读目录上（这就是事故的现场）。
// ② 破了：一个上游的坏运气变成所有上游都不可用。
func TestV1ModelsStuckUpstreamDoesNotStallEndpoint(t *testing.T) {
	r := newProbeRouter([]string{"workbuddy", "stuck", "healthy"}, map[string][]gateway.ModelInfo{
		"healthy": {{ID: "ok-1", ContextWindow: 1000}},
	})
	// 卡住的上游睡 2× 超时：远超端点的上限，只在"没有超时机制"时才会影响结果。
	r.hangs = map[string]time.Duration{"stuck": 2 * providerModelsTimeout}
	h := probeHandler(t, r)

	ids, elapsed := modelsOrdered(t, h)

	// 上限 = 单上游超时 + 收集余量。再留 1s 的空间给调度与写缓存，避免抖动假红。
	limit := providerModelsTimeout + providerModelsCollectGrace + time.Second
	if elapsed > limit {
		t.Fatalf("★ 端点被一个卡住的上游拖死：耗时 %v，上限 %v（单上游超时 %v）—— "+
			"控制台读模型目录时会一直转圈", elapsed, limit, providerModelsTimeout)
	}
	// ② 健康上游必须照常列出。
	if !contains(ids, "healthy/ok-1") {
		t.Errorf("★ 一个上游卡住导致别的上游模型丢失: %v", ids)
	}
	// ③ 默认上游照常。
	if miss := missing(ids, []string{"workbuddy/auto", "workbuddy/glm-5.2"}); len(miss) > 0 {
		t.Errorf("★ 卡住的上游影响了默认上游: %v", miss)
	}
	// 卡住的上游自己不出现在目录里（拿不到目录即跳过，不是列一条空的）。
	for _, id := range ids {
		if len(id) > 6 && id[:6] == "stuck/" {
			t.Errorf("拿不到目录的上游不该有条目: %s", id)
		}
	}
}

// TestV1ModelsProviderCallCarriesOwnTimeout 直接钉住"每次 Models() 调用
// 都带自己的 deadline"这件事。
//
// # 为什么需要一条专门的用例（而不是靠上面那条的耗时）
//
// 上面那条断的是"端点有上界"，而**那个上界由两处独立机制共同提供**：
//
//	(a) 单次调用自己的 context.WithTimeout(providerModelsTimeout)
//	(b) providerDirectories 的收集预算（超时 + 余量）
//
// 只有 (b) 时端点同样能在上限内返回 —— 于是"(a) 被删掉了"这件事在耗时
// 断言下是**看不见的**。而 (a) 是有真实作用的：它让上游实现有机会自己
// 收手（关掉连接、停止重试），并且让失败尽快落进负缓存；(b) 只是兜底。
//
// 所以这里不断言耗时，而是断言**调用方递给上游的 ctx 本身**带预算，
// 且预算不超过 providerModelsTimeout。去掉 WithTimeout 会让 hasDeadline
// 变成 false，这条立刻红。
func TestV1ModelsProviderCallCarriesOwnTimeout(t *testing.T) {
	r := newProbeRouter([]string{"workbuddy", "alpha"}, map[string][]gateway.ModelInfo{
		"alpha": {{ID: "a-1", ContextWindow: 1000}},
	})
	h := probeHandler(t, r)

	modelsOrdered(t, h)

	budget, hasDL := r.deadlineSeen("alpha")
	if !hasDL {
		t.Fatalf("★ 调上游时给的 ctx 没有 deadline —— "+
			"providerModelsCached 里的 context.WithTimeout(%v) 没了，上游可以自己决定等多久", providerModelsTimeout)
	}
	if budget <= 0 {
		t.Errorf("递给上游的 ctx 预算已耗尽（剩余 %v）—— 它一上手就注定失败，等于永远拿不到目录", budget)
	}
	// 留一点宽限：预算是在调用前算的，到桩读到它已经过去了一点点。
	if budget > providerModelsTimeout {
		t.Errorf("递给上游的 ctx 预算 %v 超过 providerModelsTimeout=%v —— "+
			"超时被谁放大了", budget, providerModelsTimeout)
	}
}

// ---------------------------------------------------------------------------
// 判据 4：失败负缓存 —— 上游连续失败时不反复重试
// ---------------------------------------------------------------------------

// TestV1ModelsFailedProviderIsNegativelyCached 钉住失败上游的 5min 负缓存：
//
//	连续 3 次请求 → 只打 1 次上游（冷却期内直接跳过）
//	冷却期过后    → 重新回源（负缓存是**有界**的，不是永久放逐）
//
// # 为什么"重新回源"这半条必须有
//
// 只断前一半的话，一个"失败后永久不再尝试"的实现也能过 —— 而那是**更坏**
// 的行为：上游只是抖了一下，目录就会整个小时里都少一个上游，且没有任何
// 自愈路径。所以冷却期到期后必须能恢复，这条与负缓存本身同等重要。
func TestV1ModelsFailedProviderIsNegativelyCached(t *testing.T) {
	r := newProbeRouter([]string{"workbuddy", "flaky"}, nil)
	r.fail = map[string]bool{"flaky": true}
	h := probeHandler(t, r)

	for i := 0; i < 3; i++ {
		ids, _ := modelsOrdered(t, h)
		if contains(ids, "flaky/") {
			t.Fatalf("失败上游不该出现在目录里: %v", ids)
		}
	}
	if got := r.callsOf("flaky"); got != 1 {
		t.Errorf("★ 负缓存失效：连续 3 次请求后失败上游被调 %d 次，want 1 —— "+
			"一个持续失败的上游会被反复重试（每次都要等它超时）", got)
	}

	// 把失败时间戳拨到冷却期之外：语义与 handler_test.go 里
	// TestModelsNegativeCacheOnFetchFailure 的同一手法（那里拨的是
	// dynamicModelsCache.lastFail）。
	c := h.dirCache()
	c.Lock()
	c.entries["flaky"].lastFail = time.Now().Add(-10 * time.Minute)
	c.Unlock()

	modelsOrdered(t, h)
	if got := r.callsOf("flaky"); got != 2 {
		t.Errorf("★ 负缓存没有到期重试：冷却期过后失败上游仍只被调 %d 次，want 2 —— "+
			"上游抖一下就会整个 TTL 里都从目录消失，没有自愈路径", got)
	}
}

// ---------------------------------------------------------------------------
// 顺序：并发取不得打乱输出（默认上游在前，其余按调用方给的 id 顺序）
// ---------------------------------------------------------------------------

// TestV1ModelsConcurrentFetchKeepsOutputOrder 钉住并发改造**没有引入**自己的排序。
//
// # 为什么这条值得单独测
//
// 并发实现里最省事的写法是"收集完 map 再对 key 排序"，那会悄悄改变输出顺序；
// 而顺序是**有消费者的**：既有客户端取 data[0] 拿默认上游。
//
// 这里刻意让桩的 IDs() 返回一个**非字典序**的顺序（workbuddy, zeta, alpha）：
// 改造前串行实现就是按这个顺序输出的，并发实现必须逐条一致。
// 顺序要是被排序了，zeta/alpha 两段就会互换 —— 立刻可见。
func TestV1ModelsConcurrentFetchKeepsOutputOrder(t *testing.T) {
	r := newProbeRouter([]string{"workbuddy", "zeta", "alpha"}, map[string][]gateway.ModelInfo{
		"zeta":  {{ID: "z-1", ContextWindow: 1000}},
		"alpha": {{ID: "a-1", ContextWindow: 1000}},
	})
	// 让 zeta 明显更慢：若实现退化成"谁先回来谁先写"，顺序会被延迟打乱。
	r.delays = map[string]time.Duration{"zeta": 80 * time.Millisecond}
	h := probeHandler(t, r)

	var got []string
	for i := 0; i < 2; i++ { // 第二遍走缓存，顺序也必须一样
		ids, _ := modelsOrdered(t, h)
		if !contains(ids, "zeta/z-1") || !contains(ids, "alpha/a-1") {
			t.Fatalf("第 %d 次请求缺少探测上游的模型: %v", i, ids)
		}
		if i == 0 {
			got = ids
			continue
		}
		if len(ids) != len(got) {
			t.Fatalf("两次请求条数不同: %d vs %d", len(got), len(ids))
		}
		for j := range got {
			if got[j] != ids[j] {
				t.Fatalf("第 %d 条的 id 变了: 缓存前 %q / 缓存后 %q —— "+
					"输出顺序依赖了缓存命中与否", j, got[j], ids[j])
			}
		}
	}

	// 关键判据：zeta（调用方给的第二个）必须排在 alpha（第三个）之前，
	// 且都排在默认上游之后。sorted-by-key 的实现会让 alpha 跑到前面。
	iz, ia := indexOf(got, "zeta/z-1"), indexOf(got, "alpha/a-1")
	if iz < 0 || ia < 0 {
		t.Fatalf("顺序断言的前提不成立: %v", got)
	}
	if iz > ia {
		t.Errorf("★ 输出顺序被打乱：zeta(第 %d 条) 应当排在 alpha(第 %d 条) 之前 "+
			"—— 顺序必须照抄调用方给的 id 顺序（默认上游在前、其余按路由枚举序），"+
			"不能由并发收集或 map 排序决定", iz, ia)
	}
	// 默认上游的条目仍必须整体排在其余上游之前（既有客户端取 data[0]）。
	// zeta 是第一个"其余上游"，它之前的每一条都必须是默认上游的。
	for _, id := range got[:iz] {
		if hasProviderPrefix(id, "workbuddy") {
			continue
		}
		t.Errorf("其余上游 %q 排到了默认上游前面 —— data[0] 不再是客户端熟悉的那个", id)
		break
	}
}

// TestV1ModelsConcurrentFetchIsMaxNotSum 直接钉住并发改造的**收益本身**：
// 端点的首个请求耗时≈**最慢上游**，而不是各上游耗时**之和**。
//
// # 为什么不能只靠 TestV1ModelsStuckUpstreamDoesNotStallEndpoint
//
// 那条用的是"完全不认 ctx"的桩，断的是"有硬上界"。它过了并不代表并发生效：
// 两个都尊重 ctx、各睡 300ms 的**稳定**上游在串行实现下总耗时 600ms，
// 而那条用例的界是 6.25s —— 串行也能过。所以"从和变最大值"这件事必须
// 有一条自己的断言，否则本次设计里最核心的那半（并发）没有守卫。
//
// # 判据的算法（两侧都有余量）
//
//	4 个上游各睡 300ms
//	串行：4 × 300 = 1200ms 以上
//	并发：≈300ms
//	断在 700ms：比并发形态（300ms）宽一倍多，又比串行形态（1200ms）低四成
//
// 破了会怎样：这条端点的耗时重新变成"各上游直连体验的叠加"——
// 实测就是 5-27s 那个区间（loomy 的重尾）。
func TestV1ModelsConcurrentFetchIsMaxNotSum(t *testing.T) {
	const (
		perUpstream = 300 * time.Millisecond
		// 断在 和(1200ms) 与 最大值(300ms) 之间，两侧各留足余量。
		limit = 700 * time.Millisecond
	)

	slowProviders := []string{"s1", "s2", "s3", "s4"}
	ids := append([]string{"workbuddy"}, slowProviders...)
	delays := make(map[string]time.Duration, len(slowProviders))
	models := make(map[string][]gateway.ModelInfo, len(slowProviders))
	for _, name := range slowProviders {
		delays[name] = perUpstream
		models[name] = []gateway.ModelInfo{{ID: name + "-m", ContextWindow: 1000}}
	}

	r := newProbeRouter(ids, models)
	r.delays = delays
	h := probeHandler(t, r)

	got, elapsed := modelsOrdered(t, h)

	// 前提自检：每个慢上游都被真的问了（否则"快"可能是因为压根没取数）。
	for _, name := range slowProviders {
		if !contains(got, name+"/"+name+"-m") {
			t.Fatalf("慢上游 %s 的模型不在目录里: %v", name, got)
		}
	}
	if elapsed > limit {
		t.Errorf("★ 并发没生效：%d 个各睡 %v 的上游，端点花了 %v（上限 %v）—— "+
			"耗时是各上游**相加**（串行）而不是取最大值（并发）",
			len(slowProviders), perUpstream, elapsed, limit)
	}
	t.Logf("首个请求耗时 %v（%d 个上游各睡 %v：串行应 ≥%v、并发应 ≈%v）",
		elapsed, len(slowProviders), perUpstream,
		time.Duration(len(slowProviders))*perUpstream, perUpstream)
}

// ---------------------------------------------------------------------------
// 并发安全：无法跑 -race，所以用**可断言的不变量**代替
// ---------------------------------------------------------------------------

// TestV1ModelsConcurrentRequestsShareSingleFlight 并发请求下的两条不变量。
//
// # 为什么这条不能用 -race 替代（以及反过来）
//
// -race 能发现数据竞争，但**不能**发现"竞争本身不该发生"这件事：一个
// 每次都重复回源的实现照样是 race-free 的。而"同一上游任一时刻只有一个
// 回源在跑"（单飞）是本次设计的**正确性不变量**，它有两个具体后果：
//
//	① 迟到分支的总数有上界（≤ 上游数）—— 那正是"不泄漏 goroutine"的证明；
//	② 一个 30s 才回的上游不会被 N 个并发请求各打一次（自制风暴）。
//
// 所以这条断言的是**上游被调了几次**，而不是"有没有竞争"。两者互补，
// 不能互相替代（-race 在这台机器上还不可用：无 gcc 且本模块要求 CGO_ENABLED=0）。
//
// # 冷启动洪峰下的取舍（这条用例同时把它钉住）
//
// 缓存**从未**成功过时，并发请求里只有第一个会去回源，其余（撞上 inflight
// 的那批）本次请求拿不到该上游 —— 它们**不排队等**。这是刻意的：排队的
// 结果是把"冷启动的一次上游往返"变成"所有并发请求都等那次往返"，
// 正好是本次要消灭的形态。
//
// ⚠ 与之相对，**缓存过期（有旧值）**时撞上 inflight 的请求拿到的是旧值
// （见 providerModelsCached 的 inflight 分支），所以那条路径下上游不会
// 从目录里消失 —— 只有"从未成功过"的冷启动会出现一次性的缺席。
func TestV1ModelsConcurrentRequestsShareSingleFlight(t *testing.T) {
	const (
		parallel = 8
		slow     = 300 * time.Millisecond
	)
	r := newProbeRouter([]string{"workbuddy", "slow"}, map[string][]gateway.ModelInfo{
		"slow": {{ID: "s-1", ContextWindow: 1000}},
	})
	r.delays = map[string]time.Duration{"slow": slow}
	h := probeHandler(t, r)

	// 并发 8 次请求，每个请求都必须成功（不得因为并发而 500/panic）。
	//
	// ⚠ 这里**不用** modelsOrdered：它内部会 t.Fatalf，而 t.Fatalf 只能从
	// 测试自身的 goroutine 调用（在子 goroutine 里调会 runtime.Goexit，
	// 让那条断言静默消失）。所以子 goroutine 只把结果送回来，断言留在外面。
	type outcome struct {
		ids  []string
		err  string
		code int
	}
	ch := make(chan outcome, parallel)
	var wg sync.WaitGroup
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
			if rec.Code != 200 {
				ch <- outcome{code: rec.Code}
				return
			}
			var resp struct {
				Data []map[string]any `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				ch <- outcome{err: "解析响应失败: " + err.Error()}
				return
			}
			ids := make([]string, 0, len(resp.Data))
			for _, m := range resp.Data {
				ids = append(ids, asString(m["id"]))
			}
			ch <- outcome{ids: ids}
		}()
	}
	wg.Wait()
	close(ch)

	n := 0
	for o := range ch {
		if o.err != "" {
			t.Fatalf("并发请求失败: %s", o.err)
		}
		if o.code != 0 && o.code != 200 {
			t.Fatalf("并发请求返回 code=%d（公共端点不该因为并发而失败）", o.code)
		}
		n++
		// 默认上游在任何并发形态下都必须照常（公共端点不能对既有客户端退化）。
		if miss := missing(o.ids, []string{"workbuddy/auto"}); len(miss) > 0 {
			t.Errorf("并发请求丢了默认上游 %v（实际共 %d 条）", miss, len(o.ids))
		}
	}
	if n != parallel {
		t.Fatalf("只收到 %d/%d 个并发请求的结果 —— 有请求挂住了", n, parallel)
	}
	// 关键不变量：慢上游最多被调 1 次（单飞）。
	if got := r.callsOf("slow"); got > 1 {
		t.Errorf("★ 单飞失效：%d 个并发请求把同一个慢上游打了 %d 次（want ≤1）—— "+
			"迟到的回源 goroutine 数会随请求数增长（这正是 goroutine 泄漏的形态）", parallel, got)
	}
	// 慢桩真的慢过：否则上面的"≤1"可能因为回源快到没人撞上 inflight 而假绿。
	if r.callsOf("slow") == 0 {
		t.Fatal("慢上游一次都没被调 —— 用例没走到回源路径，断言失去意义")
	}

	// 洪峰过后缓存已建立：再来一次必须命中，且慢上游仍只被调 1 次。
	ids, elapsed := modelsOrdered(t, h)
	if !contains(ids, "slow/s-1") {
		t.Errorf("洪峰过后缓存没建立（慢上游的模型仍不在目录里）: %v", ids)
	}
	if got := r.callsOf("slow"); got != 1 {
		t.Errorf("洪峰后慢上游被调 %d 次，want 1（应当命中缓存）", got)
	}
	if elapsed >= slow {
		t.Errorf("洪峰后的请求仍花了 %v（≥ 慢桩延迟 %v）—— 没走缓存", elapsed, slow)
	}
}

// ---------------------------------------------------------------------------
// 桩与辅助
// ---------------------------------------------------------------------------

// newProbeRouter 造一个探测桩。
//
// def 固定为 workbuddy（默认上游），ids 由调用方给（用于钉住顺序）。
// models 为 nil 时该上游返回失败（ok=false），与 testRouter 同一语义。
func newProbeRouter(ids []string, models map[string][]gateway.ModelInfo) *probeRouter {
	return &probeRouter{
		testRouter: testRouter{
			def:    "workbuddy",
			ids:    ids,
			models: models,
		},
	}
}

// contains 报告 id 是否精确出现在列表里。
func contains(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// indexOf 返回 id 在列表里的位置（-1 = 不存在）。
func indexOf(ids []string, want string) int {
	for i, id := range ids {
		if id == want {
			return i
		}
	}
	return -1
}

// hasProviderPrefix 报告 id 是否带 <provider>/ 前缀。
//
// 用 gateway.SplitModel 而不是字符串前缀比较：前缀的判定规则（"a/b" 形式、
// 空前缀不算）在仓库里只有那一处说了算，别的地方自己拼字符串迟早会分叉。
func hasProviderPrefix(id, provider string) bool {
	got, _, ok := gateway.SplitModel(id)
	return ok && got == provider
}
