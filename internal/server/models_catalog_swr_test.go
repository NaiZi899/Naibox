// models_catalog_swr_test.go 目录回源**不阻塞请求路径**的回归测试（SWR）。
//
// # 这一组测试守的是什么
//
// 实测 /admin/models/preview 出现过 27.06s 的单次响应，控制台在 refresh() 里
// `await loadModelMultipliers()`，首屏被直接卡住。修法是把回源移出请求路径
// （stale-while-revalidate，见 models_catalog.go 顶部的说明）。
//
// 这组测试的写法刻意选成"**用慢桩 + 计时**"而不是"断言代码里没有同步调用"：
// 前者钉住的是**可观测行为**（调用方等了多久、上游被打了几次），
// 后者只能钉住实现形状 —— 而实现形状正是这次要改的东西。
//
// 因而每一条都能抓住"回退成同步回源"这个变异：桩一慢，计时断言立刻红。
package server

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NaiZi899/omni2api/internal/auth"
	"github.com/NaiZi899/omni2api/internal/gateway"
)

// slowCatalogUpstream 目录回源的测试桩：**可控地慢**。
//
// 用桩而不是真 HTTP：这里要测的是"等了多久"，不是"怎么解析 /v3/config"，
// 而 sleep 必须发生在**回源内部**（请求路径之外）才有意义。
type slowCatalogUpstream struct {
	hits  atomic.Int32
	delay time.Duration
	gate  chan struct{} // 非 nil 时阻塞到关闭（比 sleep 更确定的"慢"）
	fail  bool
	entry gateway.ModelCatalogEntry
}

func (s *slowCatalogUpstream) FetchModelCatalog(*auth.Auth) (*gateway.ModelCatalog, error) {
	s.hits.Add(1)
	if s.gate != nil {
		<-s.gate
	} else if s.delay > 0 {
		time.Sleep(s.delay)
	}
	if s.fail {
		return nil, errors.New("stub: upstream down")
	}
	return &gateway.ModelCatalog{Models: []gateway.ModelCatalogEntry{s.entry}}, nil
}

func (s *slowCatalogUpstream) FetchModels(*auth.Auth) ([]DefaultModel, error) { return nil, nil }

func (s *slowCatalogUpstream) RefreshToken(*auth.Auth) error { return nil }

func (s *slowCatalogUpstream) ChatStreamWithIP(*auth.Auth, []byte, string) (io.ReadCloser, int, []byte, error) {
	return nil, 0, nil, errors.New("stub: 本用例不打对话")
}

// waitCatalogRefreshes 等到没有后台回源在跑。
//
// 两个用途，缺一不可：
//   - 正面断言"后台回源确实发生了"：等它结束后缓存应当已被更新；
//   - 防止用例之间串味：上一个用例的回源 goroutine 若还在跑，
//     它的写回会落在下一个用例刚清空的缓存里（表现为随机 flake）。
func waitCatalogRefreshes(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		modelCatalogCache.RLock()
		n := len(modelCatalogCache.flights)
		modelCatalogCache.RUnlock()
		if n == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("后台回源未在时限内结束 —— goroutine 生命周期约束被破坏（泄漏）")
}

// waitCatalogHits 等到上游被打了 n 次。
//
// 必须等：后台 goroutine 是**异步**起的，返回后它可能还没跑到桩里。
// 直接断言 hits 会变成"赌调度器"（本地常绿、CI 偶红）。
func waitCatalogHits(t *testing.T, up *slowCatalogUpstream, n int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if up.hits.Load() >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等到超时：上游命中 %d 次（want %d）—— 后台回源从未发生", up.hits.Load(), n)
}

// resetCatalogState 清空目录缓存**与单飞表**，让每条用例从干净状态开始。
//
// 先等在跑的回源结束再清：否则它结束后会把刚清掉的缓存重新填上。
func resetCatalogState(t *testing.T) {
	t.Helper()
	waitCatalogRefreshes(t)
	modelCatalogCache.Lock()
	modelCatalogCache.entries = nil
	// flights 一并清空：单飞表是缓存的一部分状态，只清一半会让
	// "上一条用例留下的通道"被复用（等一个已经结束的信号，等效于没等）。
	modelCatalogCache.flights = nil
	modelCatalogCache.Unlock()
	t.Cleanup(func() { waitCatalogRefreshes(t) })
}

// seedStaleCatalog 直接写一份**已过期但仍有内容**的缓存。
//
// 直接写 entry 而不是先拉一次再把时间拨回去：本组测的是"过期以后怎么取数"，
// 播种过程不该自己引入一次上游往返（那会污染 hits 计数）。
func seedStaleCatalog(provider string, e gateway.ModelCatalogEntry) *gateway.ModelCatalog {
	cat := &gateway.ModelCatalog{Models: []gateway.ModelCatalogEntry{e}}
	modelCatalogCache.Lock()
	if modelCatalogCache.entries == nil {
		modelCatalogCache.entries = map[string]*modelCatalogEntry{}
	}
	modelCatalogCache.entries[provider] = &modelCatalogEntry{
		cat:     cat,
		fetched: time.Now().Add(-2 * dynamicModelsTTL), // 远超 TTL
	}
	modelCatalogCache.Unlock()
	return cat
}

// 回归 1 + 2：缓存过期且**有旧值**时，调用立刻返回旧值；
// 同时在后台触发一次回源，最终缓存被更新。
//
// 变异判据：把 SWR 改回同步回源（去掉 stale 快速返回），
// 第一条断言立刻红 —— 桩要 sleep 2s，而这里要求 < 200ms。
func TestCatalogStaleReturnsImmediatelyAndRevalidatesInBackground(t *testing.T) {
	resetCatalogState(t)
	up := &slowCatalogUpstream{
		delay: 2 * time.Second, // 模拟上游重尾
		entry: gateway.ModelCatalogEntry{ID: "new-model", Multiplier: 2},
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	_ = NewHandler(Config{Pool: p, Upstream: up})

	seedStaleCatalog("", gateway.ModelCatalogEntry{ID: "old-model", Multiplier: 1})

	start := time.Now()
	cat := ModelCatalog()
	elapsed := time.Since(start)

	// 1) 立刻返回 —— 这是本次改动的全部要点（27s → 不等待）。
	if el := elapsed; el >= 200*time.Millisecond {
		t.Fatalf("★ 缓存过期时的调用阻塞了 %v（want < 200ms）—— SWR 退化成了同步回源", el)
	}
	// 返回的必须是**旧值**，而不是 nil 或空目录：
	// 过期系数是观测维度，比没有系数有用得多。
	if cat == nil || len(cat.Models) != 1 {
		t.Fatalf("过期时应回吐旧目录, got %+v", cat)
	}
	if v, ok := cat.Multiplier("old-model"); !ok || v != 1 {
		t.Fatalf("回吐的应是旧值 old-model=1, got (%v,%v) %+v", v, ok, cat.Models)
	}
	if _, ok := cat.Multiplier("new-model"); ok {
		t.Fatal("旧值里不该出现新模型（说明这次读的是回源结果，等于没走 SWR）")
	}
	// 2) 后台回源确实被触发了（stale 时**必须**刷新，否则缓存永远陈旧）。
	waitCatalogHits(t, up, 1)

	// 3) 后台跑完后缓存被更新 —— 下一个轮询就能拿到新目录。
	waitCatalogRefreshes(t)
	after := ModelCatalog()
	if after == nil {
		t.Fatal("后台回源后应能读到新目录")
	}
	if v, ok := after.Multiplier("new-model"); !ok || v != 2 {
		t.Fatalf("后台回源未写回缓存: %+v", after.Models)
	}
}

// 回归 3：并发调用**只触发一次**回源（单飞有效）。
//
// 场景真实性：控制台 5s 轮询 + 多个标签页，缓存过期那一刻会有 N 个请求同时
// 发现"该刷新了"。没有单飞就是 N 组上游请求（每组至多 MaxRotate 次），
// 对上游是风暴、对自己是白烧额度。
//
// 用 gate 而不是 sleep 来制造"慢"：gate 未关闭时回源**必然**还在跑，
// 于是"并发请求全部命中单飞"这件事是确定的，不依赖 sleep 与调度的赛跑。
func TestCatalogStaleConcurrentCallsSingleFlight(t *testing.T) {
	resetCatalogState(t)
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	defer release() // 断言中途 Fatal 也要放行，否则 goroutine 泄漏污染后续用例

	up := &slowCatalogUpstream{
		gate:  gate,
		entry: gateway.ModelCatalogEntry{ID: "new-model", Multiplier: 2},
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	_ = NewHandler(Config{Pool: p, Upstream: up})

	seedStaleCatalog("", gateway.ModelCatalogEntry{ID: "old-model", Multiplier: 1})

	const callers = 20
	var wg sync.WaitGroup
	nilResults := make([]bool, callers)
	elapsedBy := make([]time.Duration, callers) // 每 goroutine 独占一格：避免"最后写的赢"掩盖掉慢的那次
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(idx int) {
			defer wg.Done()
			start := time.Now()
			cat := ModelCatalog()
			elapsedBy[idx] = time.Since(start)
			nilResults[idx] = cat == nil
		}(i)
	}
	wg.Wait()

	// 全部立刻拿到旧值（没有人在等上游）。
	for i, el := range elapsedBy {
		if el >= 200*time.Millisecond {
			t.Fatalf("★ 第 %d 个并发调用阻塞了 %v（want < 200ms）", i, el)
		}
	}
	for i, isNil := range nilResults {
		if isNil {
			t.Errorf("第 %d 个并发调用拿到 nil（应回吐旧值）", i)
		}
	}
	// 单飞：20 个调用只允许 1 次上游请求。
	waitCatalogHits(t, up, 1)
	if n := up.hits.Load(); n != 1 {
		t.Fatalf("★ 并发 %d 次调用触发了 %d 次回源（want 1）—— 单飞失效", callers, n)
	}

	// 放行后缓存更新，且**不会再打第二次**上游。
	release()
	waitCatalogRefreshes(t)
	if n := up.hits.Load(); n != 1 {
		t.Fatalf("回源结束后上游命中变成 %d（want 1）", n)
	}
	if cat := ModelCatalog(); cat == nil {
		t.Fatal("回源结束后应读到新目录")
	}
}

// 回归 4：首次（缓存为空）且上游慢 —— 在短预算内返回 nil，不等满。
//
// 这是本次改动唯一的**产品取舍**：完全异步会让首屏永远空白（等某个后台刷新
// 恰好跑完 + 再轮询一次），所以保留"首次同步拉一次"；但没有预算就等于
// 把上游的重尾（实测 27s）原样转嫁给首屏。于是加硬上限：
// 超预算立刻返回 nil 并进负缓存，**后台那一次继续跑**，成功了自己填缓存。
func TestCatalogFirstFetchBoundedByBudget(t *testing.T) {
	resetCatalogState(t)
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	defer release()

	up := &slowCatalogUpstream{
		gate:  gate, // 回源永远"比预算慢"
		entry: gateway.ModelCatalogEntry{ID: "new-model", Multiplier: 2},
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	_ = NewHandler(Config{Pool: p, Upstream: up})

	start := time.Now()
	cat := ModelCatalog()
	elapsed := time.Since(start)

	if cat != nil {
		t.Fatalf("上游比预算慢时应返回 nil（而不是空目录/半成品）, got %+v", cat)
	}
	// 上限生效：必须**真的**用了预算（否则"立刻返回"就退化成"永远空白首屏"），
	// 但也必须在预算附近收手（而不是一路等到上游回来）。
	if elapsed < modelCatalogFirstFetchBudget {
		t.Errorf("首次同步等待只用了 %v，短于预算 %v —— 预算未被真正使用",
			elapsed, modelCatalogFirstFetchBudget)
	}
	if elapsed > modelCatalogFirstFetchBudget+2*time.Second {
		t.Fatalf("★ 首次调用等了 %v（预算 %v + 宽限 2s）—— 预算没兜住上游重尾",
			elapsed, modelCatalogFirstFetchBudget)
	}
	// 首次只允许一次回源（超时返回 nil 也不该再补一发）。
	waitCatalogHits(t, up, 1)
	if n := up.hits.Load(); n != 1 {
		t.Fatalf("首次回源命中 %d 次（want 1）", n)
	}

	// 超时必须进负缓存，否则后续轮询会反复发起新的回源（每次又等 2.5s）。
	if st := ModelCatalogState(); !st.Cooldown {
		t.Errorf("超时后应处于失败冷却期, got %+v", st)
	}

	// ⚠ 关键：超时**只是本接口不等了**，后台那次回源必须继续跑完并填上缓存 ——
	// 否则"第一次没等到"会变成"永远拿不到"。
	release()
	waitCatalogRefreshes(t)
	cat = ModelCatalog()
	if cat == nil {
		t.Fatal("★ 超时后后台回源的结果应被写回缓存（否则首屏永远没有倍率）")
	}
	if v, ok := cat.Multiplier("new-model"); !ok || v != 2 {
		t.Fatalf("后台回源未写回缓存: %+v", cat.Models)
	}
}

// 回源失败必须**保留旧值**：过期的系数比没有系数有用得多。
//
// 这条也是"失败不丢数据"的守卫：旧实现里失败会把 cat 换成 nil，
// 于是一次网络抖动就让界面上已有的系数整片消失。
func TestCatalogRefreshFailureKeepsStaleValue(t *testing.T) {
	resetCatalogState(t)
	up := &slowCatalogUpstream{
		fail:  true,
		delay: 20 * time.Millisecond,
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	_ = NewHandler(Config{Pool: p, Upstream: up})

	seedStaleCatalog("", gateway.ModelCatalogEntry{ID: "old-model", Multiplier: 1})

	cat := ModelCatalog()
	if cat == nil {
		t.Fatal("回源尚未失败前，应先回吐旧值")
	}
	waitCatalogRefreshes(t)

	// 回源已失败并进了负缓存：仍然必须给旧值。
	if n := up.hits.Load(); n != 1 {
		t.Fatalf("回源命中 %d 次 want 1（池里只有 1 个号）", n)
	}
	again := ModelCatalog()
	if again == nil {
		t.Fatal("★ 回源失败后旧系数被丢掉（应保留旧值并让 state 报 stale/cooldown）")
	}
	if v, ok := again.Multiplier("old-model"); !ok || v != 1 {
		t.Fatalf("失败后应回吐旧值 old-model=1, got %+v", again.Models)
	}
	// 负缓存仍在：冷却期内不该再打上游（否则失败会变成风暴）。
	if n := up.hits.Load(); n != 1 {
		t.Fatalf("负缓存未生效：上游命中 %d 次 want 1", n)
	}
	// 状态要如实说明"这份数据已经不可信"。
	st := ModelCatalogState()
	if !st.Cooldown {
		t.Errorf("失败后 state 应报 cooldown, got %+v", st)
	}
	if st.Models != 1 {
		t.Errorf("state 应仍能报告旧目录里有 1 个模型, got %d", st.Models)
	}
}

// 缓存有效（TTL 内）时**零上游**：SWR 不能把命中路径也变成后台刷新。
func TestCatalogFreshCacheDoesNotRefresh(t *testing.T) {
	resetCatalogState(t)
	up := &slowCatalogUpstream{entry: gateway.ModelCatalogEntry{ID: "m", Multiplier: 1}}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	_ = NewHandler(Config{Pool: p, Upstream: up})

	seedStaleCatalog("", gateway.ModelCatalogEntry{ID: "m", Multiplier: 1})
	// 把时间拨到"刚刚成功拉过" → 命中 1h 正缓存。
	modelCatalogCache.Lock()
	modelCatalogCache.entries[""].fetched = time.Now()
	modelCatalogCache.Unlock()

	for i := 0; i < 5; i++ {
		if cat := ModelCatalog(); cat == nil {
			t.Fatal("正缓存内应返回目录")
		}
	}
	waitCatalogRefreshes(t)
	if n := up.hits.Load(); n != 0 {
		t.Fatalf("正缓存内打了 %d 次上游（want 0）—— 命中也触发了刷新", n)
	}
}
