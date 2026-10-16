// models_endpoint_cache.go /v1/models 的**按上游目录缓存 + 并发取 + 单上游超时**。
//
// # 为什么单独一个文件
//
// 这一段是"控制台卡顿"事故的第一现场：modelList() 对"其余上游"是
// **串行、无缓存、无超时**地调 ProviderRouter.Models()。实测 loomy 的目录
// 拉取是重尾分布（p50 1960ms / p90 10094ms / max 27497ms），而 /v1/models
// 每次请求都把每个上游重新拉一遍 —— 于是这个公共端点 p50 4107ms /
// max 22630ms，而控制台其它端点全是 1-14ms。
//
// 三件事写在一起，是因为它们各自的失败长相完全不同、必须各有一条回归用例：
//
//	缓存没了 → 调用次数随请求次数增长（每次都打上游）
//	并发没了 → 各上游耗时**相加**（最慢那个决定所有人的体验）
//	超时没了 → 一个不认 ctx 的上游能把这个端点挂到它自己的时间里
//
// 三条判据都在 models_endpoint_cache_test.go 里，且都做过变异验证。
//
// # 缓存范式与 dynamicModelsCache / modelCatalogCache 刻意一致
//
// 1h 正缓存 + 5min 失败负缓存、惰性（不加 ticker / 不加后台 goroutine）、
// 锁内读写。TTL 与冷却期**直接复用那两个包级常量**，不另立新值：
// 三份缓存的生命周期一旦分叉，"为什么这个上游看起来更慢"就会变成一个
// 没人查得出的问题（它们共用同一句 `time.Since(...) < dynamicModelsTTL`）。
package server

import (
	"context"
	"sync"
	"time"

	"github.com/NaiZi899/omni2api/internal/gateway"
)

// providerModelsTimeout 单个上游目录拉取的超时。
//
// # 为什么是 5s
//
// 各上游直连实测的 p50 都在 0.2-2s 以内（jiyuan 178ms / trae 354ms /
// loomy 1960ms），5s 已经盖住了 loomy 的 p90。这个值的意义不是性能目标，
// 而是**给"不认 ctx 的上游"划一条硬边界**：它是这个端点最坏情况的可接受上限。
//
// 做成包级常量而不是 Config 字段：它不是部署参数，而是"这个端点能等多久"
// 的工程判据。做成常量也让回归用例能引用它来断言上界（否则用例只能写死
// 一个魔数，实现改小了用例也不会发现）。
const providerModelsTimeout = 5 * time.Second

// providerModelsCollectGrace 收集并发结果的调度余量。
//
// 各分支是并发起的、各自带 providerModelsTimeout，所以"全部有结果"的时刻
// 本应≈超时本身。留这一段是为了容下 goroutine 调度与写缓存的开销 ——
// 不留余量会把"按时完成"的分支误判成卡住，表现为**静默少列一个上游**，
// 那种失败比慢更糟（用户看到的是"模型不见了"）。
const providerModelsCollectGrace = 250 * time.Millisecond

// providerModelsEntry 一个上游的目录缓存。
//
// # 为什么 models 与 ok 必须一起读
//
// 缓存必须能区分"缓存里根本没有这个上游"（没问过）与"缓存里有这个上游、
// 它给不出目录"（问过了，空）。前者要回源，后者 1h 内不该再问。
//
// ⚠ 别把它简化成"models == nil 就是没缓存"：那会让"上游明确回了空目录"
// 的部署每次请求都重新拉一遍（一次注定为空的往返），也就是把缓存这件事
// 退化成一次性的。
type providerModelsEntry struct {
	// models 上游给的目录。ok=false 时无意义（已被判为失败）。
	models []gateway.ModelInfo
	// ok 报告最近一次取数**是否成功**（而不是"列表非空"）。
	ok bool

	fetched  time.Time // 最近一次成功拉取时间
	lastFail time.Time // 最近一次拉取失败时间（负缓存）

	// inflight 报告"已经有一次回源在跑"（单飞）。
	//
	// 没有它，一个 30s 才回的上游在并发请求下会堆出 N 个 goroutine，
	// 每个都持着一次上游往返 —— 那是**真正的** goroutine 泄漏（不是
	// "迟早会退出的迟到分支"）。有它之后，同一上游在任一时刻只有一个
	// 回源 goroutine，于是"迟到分支"的总数有上界（≤ 上游数）。
	inflight bool
}

// providerModelsCache 一个 Handler 的**按上游**目录缓存，key = provider id。
type providerModelsCache struct {
	sync.RWMutex
	entries map[string]*providerModelsEntry // provider id → 目录缓存（惰性创建）
}

// reset 清空全部按上游缓存（供「刷新模型」与回归用例使用）。
//
// ⚠ 现在**没有生产调用点**：handler.go 的 ResetModelsCache 在别的写入面上，
// 本次没动它。收口时那一行应当改成同时调 resetProviderModelsCaches()，
// 否则「刷新模型」只清默认上游与倍率，其余上游的目录要等 TTL 自然过期。
func (c *providerModelsCache) reset() {
	c.Lock()
	defer c.Unlock()
	c.entries = map[string]*providerModelsEntry{}
}

// providerModelsCaches 把「按上游目录缓存」挂到各自的 Handler 上。
//
// # 为什么按 Handler 分实例，而不是一个全局的 map[provider]（与 dynamicModelsCache 不同）
//
// dynamicModelsCache 只服务**默认上游**一条路径，数据源是 h.cfg.Upstream ——
// 全进程一份是事实。而"其余上游"的目录来自装配层注入的 ProviderRouter，
// 同一个进程里可以有多个 Handler（测试里每个用例一个），它们各自握着
// **不同的** router。
//
// 全局按 provider id 缓存会让 A 用例的 inkstone 目录被 B 用例读到。这不是
// 假想的：models_supports_images_test.go 的三个用例都给 "inkstone" 配了
// **不同的**模型集，全局缓存会让后两个用例读到第一个的目录而变红。
// 也就是说，那种设计在测试里制造的是"替身之间的串味"，与被测行为无关。
//
// 按 Handler 分实例之后：生产只有一个 Handler（cmd/server 单实例），语义
// 与全局缓存等价；多个 Handler 各自一份，互不影响。
//
// # 为什么键是 *Handler 本身（强引用）——这一点是必需的，不是顺手
//
// 强引用保证被缓存的 Handler 不会被回收，于是它的地址不会被后面的 Handler
// 复用。若用弱引用/uintptr 之类的键，一次地址复用就会表现为"新实例读到旧
// 实例的目录"——那种错没有任何线索可查（目录内容看着完全正常，只是属于
// 别人）。代价是这张表会持有 Handler：生产里只有 1 个，测试里有几十个，
// 都是有界的。
//
// ⚠ 这与 modelCatalogCache 的 catalogHost 是同一个形状的取舍，区别在于
// 那边只记"当前宿主"，多实例并存时会静默错位（其注释自己写着这一点）；
// 这里用注册表避开那个错位。
var providerModelsCaches = struct {
	sync.Mutex
	m map[*Handler]*providerModelsCache
}{}

// dirCache 返回本 Handler 的按上游目录缓存（惰性创建）。
//
// Handler 里加不了字段（handler.go 是别的写入面），所以用上面那张注册表
// 把两者对上。见 providerModelsCaches 的注释。
func (h *Handler) dirCache() *providerModelsCache {
	providerModelsCaches.Lock()
	defer providerModelsCaches.Unlock()
	if c, ok := providerModelsCaches.m[h]; ok {
		return c
	}
	if providerModelsCaches.m == nil {
		providerModelsCaches.m = map[*Handler]*providerModelsCache{}
	}
	c := &providerModelsCache{entries: map[string]*providerModelsEntry{}}
	providerModelsCaches.m[h] = c
	return c
}

// resetProviderModelsCaches 清空**所有** Handler 的按上游目录缓存。
//
// 「刷新模型」的语义是"立刻看到最新目录"，而这个端点只认 ID 不认实例
// （ResetModelsCache 是包级函数），所以只能全清。见 (*providerModelsCache).reset
// 上方的说明。
func resetProviderModelsCaches() {
	providerModelsCaches.Lock()
	defer providerModelsCaches.Unlock()
	for _, c := range providerModelsCaches.m {
		c.reset()
	}
}

// providerModelsCached 取某上游的目录：缓存优先，未命中时回源一次（带超时）。
//
// 返回值与 modelList 里原来的 (infos, ok) 语义一致：ok=false 表示这个上游
// 现在给不出目录，调用方一律跳过它，不把整个 /v1/models 打成失败。
//
// # 为什么这条路径不惩罚账号
//
// 这里**没有** Pool.NoteError（与 fetchDynamicModels 不同），理由有两条：
//
//  1. 它根本不选号：凭证由装配层（registryRouter.credentialFor）从池里挑，
//     出口层连"这次用的是谁的号"都不知道。拿一个自己没选的 uid 去记错，
//     正是本仓库反复修过的那类 bug（惩罚无辜账号）。
//  2. 失败的最常见形态是**超时**，而超时说明的是"这个上游慢"，不是
//     "这个号坏了"—— 记错会让重尾上游的号被自己的延迟熔断掉。
//
// 所以失败只进本缓存的负缓存：既不误伤账号，也不反复打上游。
//
// # 失败为什么不回吐旧目录（与"缓存过期"不同）
//
// 失败即跳过，与 fetchDynamicModels / modelCatalogFor 同一取舍：宁可少列
// 一个上游，也不要列一份**可能已失效**的目录（模型目录是"上游此刻有什么"
// 的声明，列一个上游已下架的模型会让调用方发出必然失败的请求）。
// 唯一会回吐旧值的情形见下面 inflight 那一支：那是"正在刷新"而不是"失败"。
func (h *Handler) providerModelsCached(id string) ([]gateway.ModelInfo, bool) {
	c := h.dirCache()

	c.Lock()
	e := c.entries[id]
	if e == nil {
		// 缓存里没有这个上游 —— 这与"有缓存但列表为空"是两件事，所以
		// 这里建的是**带 ok=false 的条目**（还没问过），而不是空切片。
		e = &providerModelsEntry{}
		c.entries[id] = e
	}
	switch {
	case e.ok && time.Since(e.fetched) < dynamicModelsTTL:
		// 正缓存命中。⚠ 含"上游明确回了空目录"（ok=true 而 models 为空）：
		// 那是一次成功取数，1h 内不必再问。
		infos := e.models
		c.Unlock()
		return infos, true

	case !e.ok && !e.lastFail.IsZero() && time.Since(e.lastFail) < modelsFetchFailCooldown:
		// 失败负缓存：冷却期内不再打上游（与 dynamicModelsCache 同一套语义）。
		c.Unlock()
		return nil, false

	case e.inflight:
		// 已经有一次回源在跑：**不排队、也不再发一次**，直接用上一次拿到的东西。
		//
		// 排队没有意义（它们的目录是同一份，等到了也没有增量），而"再发一次"
		// 在重尾上游上会变成一场自制的风暴。这一支正是"缓存过期 + 并发请求"
		// 时的行为：还拿着（略过期的）旧目录的上游照常列出，不会因为
		// 刷新还没回来就从目录里消失。
		infos, ok := e.models, e.ok
		c.Unlock()
		return infos, ok
	}
	e.inflight = true
	c.Unlock()

	// 回源结束必须清 inflight，否则这个上游会被自己永久单飞掉（负缓存到期
	// 后也再没人去问它）。
	defer func() {
		c.Lock()
		e.inflight = false
		c.Unlock()
	}()

	// ⚠ 基座是 Background 而不是请求的 ctx：回源结果要进缓存（供 1h 内所有
	// 请求复用），所以它不该被**某一个**调用方的断连打断。硬上界由下面这个
	// WithTimeout 保证，不靠调用方的 ctx。
	ctx, cancel := context.WithTimeout(context.Background(), providerModelsTimeout)
	defer cancel()
	infos, ok := h.cfg.Provider.Models(ctx, id)

	c.Lock()
	if ok {
		e.models = infos
		e.ok = true
		e.fetched = time.Now()
		e.lastFail = time.Time{} // 成功则清空负缓存
	} else {
		e.models = nil
		e.ok = false
		e.lastFail = time.Now()
	}
	c.Unlock()

	if !ok {
		return nil, false
	}
	return infos, true
}

// providerDirectories 并发取多个上游的目录，返回 id → 目录（**只含拿到了的**）。
//
// ⚠ 返回的 map **不含**"给不出目录"的上游（与调用方原来的
// `if !ok || len(infos) == 0 { continue }` 同一判据）。调用方按 dirs[id]
// 取不到就是零值 nil，len==0 自然跳过。
//
// # 为什么必须并发（不是"顺手优化"）
//
// 串行时端点耗时 = 各上游耗时**之和**，于是最慢的那个上游决定所有人的体验。
// 实测串行相加约 0.6s 是基线（jiyuan 178ms + trae 354ms + …），而 loomy 的
// p90 10094ms / max 27497ms 会把它拖成一个 5-27s 的端点。并发之后耗时 =
// 各上游耗时的**最大值**，且被 providerModelsTimeout 封顶。
//
// # 顺序：这个函数**刻意不排序**，顺序由调用方决定
//
// 调用方（modelList 第 2 步）给的是 otherProviders(def)，那已经是"默认上游
// 在前、其余按字典序"的既有顺序。它按那个顺序遍历、从本函数返回的 map 里取
// 各自的目录，因此输出与串行版**逐条一致**。
//
// 刻意**不**在这里对 id 再排序，也不让 map 的键序参与输出：那会改变
// "路由器没按字典序枚举"的部署下的目录顺序，而顺序是既有客户端 data[0]
// 习惯所依赖的（TestV1ModelsConcurrentFetchKeepsOutputOrder 钉住这一点）。
//
// # 硬约束：不引入 goroutine 泄漏
//
// 分支自己带 providerModelsTimeout，正常情况下全部按时退出。但**上游可以不
// 认 ctx**（桩如此，将来的实现也可能），所以收集端另有一个 超时+余量 的
// 预算：到点就带着已有结果返回。此时迟到的分支有两个保护：
//
//   - 结果通道是**缓冲的**（容量 = 分支数）：迟到的分支写完就退出，
//     不会永久阻塞在发送上 —— "发不出去"才是会把 goroutine 永久钉住的东西；
//   - 单飞（providerModelsEntry.inflight）保证同一上游任一时刻只有一个
//     回源在跑，所以迟到分支的总数有上界（≤ 上游数），不随请求数增长。
//
// # 为什么用"数收到的份数"而不是 wg.Wait()
//
// 每个分支**无论成败都发一份**（fetched.ok 区分），于是"收到 len(ids) 份"
// 就等价于"所有分支都已退出"。这样收集端自己就能等，不需要额外一个
// `go func(){ wg.Wait(); close(done) }()` —— 而那个 goroutine 恰恰是这里
// 唯一的泄漏点：只要有一个上游不认 ctx，它就永久阻塞在 Wait 上，
// 泄漏量随请求数增长。数份数把这件事变成"多余的分支写完就走"。
//
// ✋ 这里**刻意不排序** map 的键：输出的顺序由 modelList 按调用方的 id 顺序
// 回填决定（见上）。map 只承担 id → 目录的查找。
func (h *Handler) providerDirectories(ids []string) map[string][]gateway.ModelInfo {
	if len(ids) == 0 {
		return nil
	}
	// ok 是必需的：它让"这个上游给不出目录"也占一份回执，否则收集端
	// 分不清"它失败了"与"它还没回来"。
	type fetched struct {
		id    string
		infos []gateway.ModelInfo
		ok    bool
	}
	ch := make(chan fetched, len(ids))
	for _, id := range ids {
		go func(id string) {
			infos, ok := h.providerModelsCached(id)
			ch <- fetched{id: id, infos: infos, ok: ok}
		}(id)
	}

	out := make(map[string][]gateway.ModelInfo, len(ids))
	timer := time.NewTimer(providerModelsTimeout + providerModelsCollectGrace)
	defer timer.Stop()
	for remaining := len(ids); remaining > 0; remaining-- {
		select {
		case f := <-ch:
			if f.ok {
				out[f.id] = f.infos
			}
		case <-timer.C:
			// 有分支卡住（上游不认 ctx）：带已有结果返回。迟到的分支照常把
			// 结果写进缓存，只是本次请求不等它 —— 下一次请求就能命中它写下的
			// 缓存，所以"卡住"最多让这一个上游缺席一次。
			return out
		}
	}
	return out
}
