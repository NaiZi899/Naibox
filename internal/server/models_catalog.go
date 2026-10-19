// models_catalog.go 模型目录（成本系数）缓存与回源。
//
// # 为什么单独一个文件
//
// 这一段是"控制台性能"事故的第二个现场：`/admin/models/preview` 与
// `/admin/stats` 里的倍率取数会**同步阻塞**在上游 /v3/config 上
// （原 handler.go 里的注释自己记着这个已知代价，代号 HIGH-2，
// 并写明"正确做法是把回源挪到后台 goroutine、本接口只读缓存"）。
//
// 摘出来之后，目录缓存的读/写/回源三个关注点有独立可读面，
// 也便于单独给它加"绝不阻塞请求"的约束与测试。
//
// # 现在的取数策略：stale-while-revalidate（SWR）
//
// 拆分时这里是**同步**回源（纯搬移、行为零变化），也就把上游的重尾
// 原样留在了请求路径上：实测 /admin/models/preview 出现过 27.06s
// 的单次响应，而控制台在 refresh() 里 `await loadModelMultipliers()`，
// 首屏被它直接卡住。现在改成：
//
//	缓存有效（TTL 内）        → 直接返回，零上游、零等待
//	缓存过期但**有旧值**      → 立刻回吐旧值 + 后台回源一次（单飞）
//	无旧值（首次）            → 同步等一次，但只等 modelCatalogFirstFetchBudget
//	回源失败                  → 保留旧值返回（绝不因为刷新失败而丢掉旧系数）
//
// 四条路径的取舍与理由分别写在 modelCatalogFirstFetchBudget、
// startCatalogRefresh、modelCatalogFor 的注释里。

package server

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NaiZi899/Naibox/internal/auth"
	"github.com/NaiZi899/Naibox/internal/gateway"
)

// modelCatalogCache 模型目录（GET /v3/config，成本系数）缓存，**按上游各一份**。
//
// 与 dynamicModelsCache 是**两份**缓存而不是合并成一份，理由与两个端点不能互相替代同源：
//   - 数据源不同：/console/enterprises/personal/models（模型可用性）vs /v3/config（成本系数）；
//   - 失败模式不同：系数拿不到只是少一个观测维度，不该把 /v1/models 的模型列表也拖下水。
//
// # 为什么按上游分域（海外版渠道支持）
//
// 多上游部署下每个上游的 /v3/config 是**各自的事实**（workbuddy 的 glm-5.2 倍率
// 与 workbuddy-intl 的 default-model 倍率互不相干）。单份缓存只能服务默认上游，
// 其余上游的模型在界面上永远显示不出倍率。key = provider 标识（"" = 默认上游，
// 与 Pool 的未打标签语义一致）。
//
// 生命周期策略刻意与 dynamicModelsCache 完全一致（1h 正缓存 + 5min 失败负缓存、
// 惰性拉取、不加 ticker/goroutine），这样「什么时候会打上游」在代码里只有一套心智模型。
type modelCatalogEntry struct {
	cat      *gateway.ModelCatalog
	fetched  time.Time // 最近一次成功拉取时间
	lastFail time.Time // 最近一次拉取失败时间（负缓存）
}

var modelCatalogCache struct {
	sync.RWMutex
	entries map[string]*modelCatalogEntry // provider → 目录缓存（惰性创建）

	// flights provider → 「这一次后台回源结束」的信号通道（单飞标记）。
	//
	// # 为什么不加 ticker、只留这张表
	//
	// 与 dynamicModelsCache 同一套心智模型：**没人查询就一次上游请求都不发**。
	// 这里的通道只在「缓存过期且有人来读」时才会被创建，回源结束即注销 ——
	// 所以 goroutine 数天然有界：同一 provider 同时至多一个。
	//
	// 两个必须存在的理由：
	//  1. 防并发重复回源：控制台是 5s 轮询 + 多个标签页，过期那一刻会有 N 个
	//     请求同时发现"该刷新了"。没有单飞就会打出 N 组上游请求（每组至多
	//     MaxRotate 次），对上游是风暴，对自己是白烧额度。
	//  2. 让"首次同步拉一次"能被短预算限时：等待者 select 这个通道，
	//     超时就自己走人（见 modelCatalogFirstFetchBudget），而回源本身
	//     继续在后台跑完 —— 它的成功会自己填缓存，下一次轮询就拿到了。
	//
	// 与 entries 共用同一把锁：两者总是同时被读改（决定要不要新建 flight
	// 的依据就是 entries 的状态），拆成两把锁只会多出一类"锁顺序"错误。
	flights map[string]chan struct{}
}

// toGatewayCatalog 搬到了装配层（cmd/server/defaultupstream.go）。
//
// # 为什么它不该留在核心
//
// 它的入参是 `*upstream.ModelCatalog` —— **workbuddy 私有 SDK 的类型**。
// 只要核心签名里出现它，核心就依赖了那个上游，无论函数体多"中立"。
//
// 现在 DefaultUpstream 接口直接声明返回 `*gateway.ModelCatalog`，
// 投影由装配层的适配器做（那里可以同时认识两边）。
//
// 函数体**逐字段原样搬走**，包括 MultiplierKnown 那条容易漏的填法：
//
//	MultiplierKnown: m.CreditsRaw != ""
//
// 语义是"上游给了非空 CreditsRaw 就说明它表态过"（哪怕系数解析成 0，
// 那是"免费"而不是"未知"）。这与 admin 的 modelsPreview「保留 0 倍率」
// 一致 —— x0.00 是"免费"这个有意义的事实。

// ModelCatalog 返回缓存的模型目录；缓存失效时惰性回源一次，失败返回 nil。
//
// 这是 FetchModelCatalog 的生产调用点。三条硬约束体现在实现里：
//
//  1. **失败永不向上传播**：签名没有 error，拿不到就返回 nil，调用方（/admin/stats）
//     降级成「unavailable」而不是把统计接口打成 500。
//  2. **不加 ticker/goroutine**：与 fetchDynamicModels 一样纯惰性 —— 只有被问到时才可能拉，
//     没人查询就一次上游请求都不发。
//  3. **失败惩罚账号**：与动态模型列表一致走 Pool.NoteError + 全局 5min 负缓存，
//     否则一个坏号会被反复 Pick 到并反复打上游（这里不主动刷新 token，
//     与 fetchDynamicModels 的取舍保持一致：系数是锦上添花，不值得为它触发一次 refresh）。
//
// 取号策略：**失败换号**。fetchDynamicModels 每次只 Pick 一个号，多账号下
// 「pick 到坏号 → 整个实例 5min 没有目录」的概率不低；系数归因只读多花至多 MaxRotate 次
// 请求即可显著提高成功率，且总次数有硬上限，不会形成风暴。
//
// 为什么是包级函数而不是 (*Handler) 方法：admin 需要这两个能力，而
// `server.NewHandler(Config{Admin: admin.New(...)})` 是一层自引用 ——
// 在构造 h 时 h 还不存在，没法把 h.modelCatalog 作为方法值传进 Config。
// 缓存本身也是包级变量（与 dynamicModelsCache 同款），所以不持有 Handler 也不丢东西。
// ModelCatalog 与 ModelCatalogState 通过 Config 的两个同名字段注入，
// 测试可覆盖成桩函数，生产由 cmd/server 直接传这两个包级函数。
// ModelCatalog 返回**默认上游**的模型目录（兼容旧签名；等价 ModelCatalogFor("")）。
func ModelCatalog() *gateway.ModelCatalog {
	if h := catalogHost.Load(); h != nil {
		return h.modelCatalogFor("")
	}
	return nil
}

// ModelCatalogFor 返回指定上游的模型目录；缓存失效时惰性回源一次，失败返回 nil。
//
// provider 为空串 = 默认上游（与 Pool 未打标签语义一致，单上游部署逐字节不变）。
// 多上游部署下每个上游的倍率各拉各的 /v3/config（见 modelCatalogCache 的注释）。
func ModelCatalogFor(provider string) *gateway.ModelCatalog {
	if h := catalogHost.Load(); h != nil {
		return h.modelCatalogFor(provider)
	}
	return nil
}

// ModelCatalogState 报告模型目录缓存状态并返回快照。**只读，绝不触发拉取**：
// 它会被 /admin/stats 调用，而统计是轮询接口，让它触发上游请求会把「看报表」
// 变成「打上游」。真正的回源只发生在 modelCatalog() 里。
//
// 返回 CatalogState（本包类型）而不是上游类型：状态描述是「缓存这件事」的属性，
// 与上游响应结构无关，放在本包才不需要让 admin 认识 upstream 的字段。
func ModelCatalogState() CatalogState {
	if h := catalogHost.Load(); h != nil {
		return h.modelCatalogState()
	}
	return CatalogState{State: "unavailable"}
}

// catalogHost 保存「目录缓存归属的 Handler」。
//
// 存在的唯一理由：admin 需要在 handler 构造前就拿到这两个取值函数（见 ModelCatalog 的注释）。
// 用 atomic.Pointer 而不是普通变量，是因为它会被 HTTP 处理路径读取，
// 而写入发生在 NewHandler（可能与其他初始化并发）。
//
// ⚠ 这是一个**隐式全局**：后构造的 Handler 会覆盖先前的，因此"谁拥有目录缓存"
// 在进程内只能有一个答案。当前唯一的生产构造点是 cmd/server（单实例），
// 测试也各自串行，所以实际成立；但引入第二个并存的 Handler 时这里会静默错位。
// 之所以接受它：缓存本身（modelCatalogCache）已经是包级变量，语义上就只有一个，
// 这里只是补一个"用谁的 Upstream/Pool 去回源"的答案。若将来真的需要多实例并存，
// 正确做法是把 modelCatalogCache 一并收进 Handler，而不是在这里加锁。
var catalogHost atomic.Pointer[Handler]

// registerCatalogHost 由 NewHandler 调用，登记目录缓存归属。
//
// 抽成函数而不是在 NewHandler 里裸写一行，是为了让"写入这个全局"这件事
// 在 grep 时可见（避免将来有人以为它是只读的）。
func registerCatalogHost(h *Handler) { catalogHost.Store(h) }

// CatalogState 模型目录缓存状态（供管理台区分「有数据 / 数据已过期 / 拿不到」）。
//
// 三态而不是「有没有目录」两态：stale 时目录内容仍然可用（只是可能已过期），
// 前端必须能把它和 ok 区分开，否则用户会拿一份过期系数当实时值读。
type CatalogState struct {
	State     string    `json:"state"` // ok | stale | unavailable
	FetchedAt time.Time `json:"fetched_at,omitempty"`
	Models    int       `json:"models"`
	Stale     bool      `json:"stale"`
	Cooldown  bool      `json:"cooldown,omitempty"` // 是否处于失败负缓存冷却期
}

func (h *Handler) modelCatalogState() CatalogState {
	// 状态按默认上游（"" 键）报告：与改造前单份缓存同一语义 ——
	// 状态是"缓存这件事"的观测维度，多上游各自成功/失败不合并成一个数。
	modelCatalogCache.RLock()
	defer modelCatalogCache.RUnlock()
	st := CatalogState{State: "unavailable"}
	e := modelCatalogCache.entries[""]
	if e == nil {
		return st
	}
	if cool := !e.lastFail.IsZero() && time.Since(e.lastFail) < modelsFetchFailCooldown; cool {
		st.Cooldown = true
	}
	if e.cat == nil {
		return st
	}
	st.Models = len(e.cat.Models)
	st.FetchedAt = e.fetched
	if !e.fetched.IsZero() && time.Since(e.fetched) >= dynamicModelsTTL {
		st.State, st.Stale = "stale", true
		return st
	}
	st.State = "ok"
	return st
}

// modelCatalogFor 是 (*Handler) 上的实现体，语义见包级 ModelCatalogFor。
// provider 为空串 = 默认上游。
func (h *Handler) modelCatalogFor(provider string) *gateway.ModelCatalog {
	if h.cfg.Upstream == nil || h.cfg.Pool == nil {
		return nil
	}

	// ⚠ 数据源分两路（多上游倍率改造）：
	//
	//	扩展点（gateway.ModelMultiplierExt）优先 —— codearts/loomy/trae 的
	//	倍率来自**它们自己的官方接口**（ratio_display / 展示名后缀）。
	//	workbuddy 系不实现该扩展点（它的倍率在 /v3/config），自然落入下方回退。
	//
	// 这样每个上游的倍率都由自己的官方数据源回答，且不走 workbuddy 的
	// /v3/config（拿别家凭证打 copilot.tencent.com 只会失败并惩罚无辜账号）。
	//
	// # ⚠⚠ 扩展点这条路**也必须走缓存**（本次修复的关键缺口）
	//
	// 原实现把这两条路做成"先问扩展点，命中就 return"，于是：
	//
	//	扩展点的 ModelMultipliers 内部调上游 /models **没有任何缓存**
	//	（见 internal/loomy/multipliers.go：每次都 ModelList）
	//	→ 缓存（本文件下半部分）根本轮不到
	//	→ SWR 形同虚设，27s 重尾原样留在请求路径上
	//
	// 实测症状：/admin/models/preview 冷启动 60s（打满 curl 超时），
	// 此后 0.2-4.5s 抖动 —— 抖的那几次就是这里每次重打 loomy。
	//
	// 所以现在两条路**共用同一份缓存**：缓存先查，未命中才问扩展点；
	// 扩展点的结果也写进同一格（它就是"这个上游的目录"的另一个来源）。
	// 缓存键与失效语义不变，CatalogState 的三态语义也不变。
	// ⚠⚠ 扩展点这条路**必须能回落到 /v3/config**（这是上一个版本的回归）
	//
	// 正确语义（改造前就是这样，见 git 历史的原实现）：
	//
	//	先问扩展点 → 有结果就用它；**没有结果就继续往下走** /v3/config 那条路
	//
	// 而不是"进了这个分支就在分支内解决"。上一版把它写成了后者，于是
	// **不实现 ModelMultiplierExt 的上游（workbuddy / inkstone / jiyuan）
	// 永远拿不到倍率** —— 实测症状：/admin/models/preview 从"各上游都有倍率"
	// 掉到只剩 loomy 的 8 条，/admin/stats 的 model_catalog 变成
	// `unavailable / 0`（那是默认上游 workbuddy 的状态）。
	//
	// 之所以当时没被发现：注册表里的 5 个上游 id 全部非空
	//（workbuddy/inkstone/jiyuan/loomy/trae），所以**每一个**都进了这个分支，
	// 而下面那段 /v3/config 的代码再也没有执行路径。
	//
	// 现在：缓存仍然优先（性能收益保留），但**只有真的拿到非空目录才 return**；
	// 拿不到就落到下面的 /v3/config 路径（含它自己的缓存与 SWR）。
	if h.cfg.Provider != nil && provider != "" {
		modelCatalogCache.RLock()
		e := modelCatalogCache.entries[provider]
		// ① 缓存有效：直接回（这一格里存的可能是扩展点的结果，也可能是
		//    /v3/config 的结果 —— 两者都是"这个上游的目录"，共用一格是对的）。
		if e != nil && e.cat != nil && time.Since(e.fetched) < dynamicModelsTTL {
			cat := e.cat
			modelCatalogCache.RUnlock()
			return cat
		}
		var stale *gateway.ModelCatalog
		if e != nil {
			stale = e.cat
		}
		modelCatalogCache.RUnlock()

		// ② 有旧值：SWR —— 立刻回吐，回源丢给后台。
		//    回源同时问扩展点与 /v3/config（refreshCatalog 内部会按
		//    "扩展点优先、失败回落"的顺序来，见 refreshCatalog）。
		if stale != nil {
			h.startCatalogRefresh(provider)
			return stale
		}

		// ③ 无旧值：同步问一次，但**只等预算**。
		//    ⚠ 这里不能只调 refreshFromMultiplierExt —— 那会让
		//    "没有扩展点的上游"在这次首屏里拿不到任何东西。用
		//    refreshCatalogAnySource：它先试扩展点，拿不到再走 /v3/config。
		//
		// ⚠ 预算过期后**必须就此返回**，不能"再试试 /v3/config"：那是
		// 第二段等待，总耗时变成"预算是假的"。实测踩到过 ——
		// 扩展点 sleep 10s 时调用方等了 5s（= 2×预算），
		// 由 models_catalog_ext_cache_test.go 的计时断言抓住。
		// 正确的分工：首屏只花一份预算；后台那次（可能要走 /v3/config）
		// 由 SWR 在后续请求里补上。
		done := make(chan struct{})
		go func() {
			defer close(done)
			h.refreshCatalogAnySource(provider)
		}()
		timer := time.NewTimer(modelCatalogFirstFetchBudget)
		defer timer.Stop()
		select {
		case <-done:
			// 预算内跑完：可能拿到了（扩展点或 /v3/config），也可能是空
			//（两条路都没数据）—— 以缓存现状为准。
			return h.cachedCatalog(provider)
		case <-timer.C:
			// 超预算：不把上游重尾转嫁给首屏，也不在这里补第二次等待。
			// 后台那次继续跑，成功后会自己填缓存，下一次请求就读到了。
			return nil
		}
	}

	modelCatalogCache.RLock()
	e := modelCatalogCache.entries[provider]
	if e != nil && e.cat != nil && time.Since(e.fetched) < dynamicModelsTTL {
		cat := e.cat
		modelCatalogCache.RUnlock()
		return cat
	}
	// 内有未过期目录但已超 TTL：先取出来 —— 它既是 SWR 要立刻回吐的值，
	// 也是回源全失败时要保留的旧目录。
	var stale *gateway.ModelCatalog
	if e != nil {
		stale = e.cat
	}
	// 失败负缓存：冷却期内不再请求上游（与 dynamicModelsCache 同一套语义）。
	//
	// ⚠ 与改造前的一处**有意的差别**：有旧值时回吐旧值而不是 nil。
	// 改造前这里恒返回 nil，于是"刷新失败"会把**上一次成功留下的系数**
	// 也一起从界面上抹掉 —— 与紧邻的成功路径注释（"过期的系数比没有系数
	// 有用得多"）自相矛盾。现在两种状态的读口径统一为：
	// **只要还有旧值就回吐旧值**，失败只体现在 state 的 cooldown/stale 上。
	// 负缓存本身（不再打上游）不受影响。
	if e != nil && !e.lastFail.IsZero() && time.Since(e.lastFail) < modelsFetchFailCooldown {
		modelCatalogCache.RUnlock()
		return stale
	}
	modelCatalogCache.RUnlock()

	// —— SWR 的分岔点 ——
	//
	// 有旧值：**立刻**回吐，回源丢给后台，请求路径不再碰上游。
	// 这是本次改动的全部要点：27s 的重尾从此只发生在后台 goroutine 上，
	// 前端拿到的是"上一次的系数 + 一个 stale 标记"。
	//
	// ⚠ 这里**绝不能**等 startCatalogRefresh 的返回通道：那正是改造前的
	// 同步阻塞形态（把它改回 `<-ch` 后，本函数的测试会在单飞表上与后台
	// 回源互等而死锁 —— 实测过一次，见 models_catalog_swr_test.go 里
	// 那条"立刻返回"的计时断言）。
	if stale != nil {
		h.startCatalogRefresh(provider) // 只触发，不等待：结果由下一次请求读缓存拿到
		return stale
	}

	// 无旧值（首次）：同步等一次，但**只等预算**（见 modelCatalogFirstFetchBudget）。
	// 等待的是同一张单飞表，所以"首次"也不会被并发请求放大成 N 组上游请求。
	done := h.startCatalogRefresh(provider)
	timer := time.NewTimer(modelCatalogFirstFetchBudget)
	defer timer.Stop()
	select {
	case <-done:
		// 预算内跑完：以缓存现状为准（成功则新目录，失败则 nil）。
		return h.cachedCatalog(provider)
	case <-timer.C:
		// 超预算：立刻返回 nil 并进负缓存 —— 绝不把上游的重尾转嫁给首屏。
		//
		// ⚠ 写负缓存前必须确认"这一格还是我读到的那个 entry"：
		// 后台那一次可能**刚刚**成功并换了新 entry，此时写 lastFail 会
		// 把新目录覆盖成 nil（丢数据，还要白等 5min 冷却）。指针变了就让位。
		modelCatalogCache.Lock()
		if modelCatalogCache.entries == nil {
			modelCatalogCache.entries = map[string]*modelCatalogEntry{}
		}
		if cur := modelCatalogCache.entries[provider]; cur != e {
			// 已被后台回源填上（或换过）：用它，不覆盖。
			modelCatalogCache.Unlock()
			return cur.cat
		}
		modelCatalogCache.entries[provider] = &modelCatalogEntry{lastFail: time.Now()}
		modelCatalogCache.Unlock()
		return nil
	}
}

// modelCatalogFirstFetchBudget 首次（缓存完全空）拉取的同步等待预算。
//
// # 为什么首次还留一次同步等待
//
// 完全异步（冷启动也立刻返回 nil）会让界面第一次打开时**一直是空的**：
// 要等某个后台刷新恰好跑完、且用户再轮询一次，倍率才出现。用户看到的是
// "这功能坏了"，而不是"数据在加载"。保留同步语义，前端零改动。
//
// # 为什么必须有预算
//
// 上游有重尾（实测 /admin/models/preview 出现过 27.06s），而首次这一支
// 最多打 MaxRotate 次上游。没有预算就等于把重尾原样转嫁给首屏，
// 而这正是本次要消灭的现象。超预算立即返回 nil + 进负缓存，
// **后台那一次继续跑**：它成功后自己填缓存，下一次轮询就拿到了。
//
// 产品取舍（明确记录在案）：这把"首次点击等满一次拉取"的上限从"等上游"
// 改成"最多 2.5s"。上游比预算慢时，首屏**没有**系数（而不是 27s 后才有）——
// 拿"少数情况下首次不显示系数"换"首屏永不卡住"。
// 这个等待只发生在缓存完全空的冷启动，之后永远走 SWR 的不等待路径。
const modelCatalogFirstFetchBudget = 2500 * time.Millisecond

// startCatalogRefresh 触发一次**后台**回源（单飞），返回"这次回源结束"的通道。
//
// 同一 provider 若已有回源在跑，直接复用它的通道 —— 调用方因此既能
// "不阻塞"，也能在需要时（首次路径）等到这一次结束。
//
// 生命周期约束（"不能无界泄漏"）：这里的 goroutine 数上界是
// **同时活跃的 provider 数**（生产上是 1 ～ 几个），且每个都在一次回源
// 结束后自行注销 flight 并退出；没有 ticker、没有常驻循环、没人读就不发请求。
// 刻意不做"进程退出时优雅等待"：回源的唯一副作用是写这份内存缓存，
// 进程退出时丢掉它没有任何损失（下次启动重新拉）。
func (h *Handler) startCatalogRefresh(provider string) <-chan struct{} {
	modelCatalogCache.Lock()
	if ch := modelCatalogCache.flights[provider]; ch != nil {
		// 已有回源在跑：复用它的完成信号（这就是单飞）。
		modelCatalogCache.Unlock()
		return ch
	}
	if modelCatalogCache.flights == nil {
		modelCatalogCache.flights = map[string]chan struct{}{}
	}
	ch := make(chan struct{})
	modelCatalogCache.flights[provider] = ch
	modelCatalogCache.Unlock()

	go func() {
		// 后台 goroutine 不在请求路径的 recover 保护范围内（harden.go 只兜
		// ServeHTTP 的那条栈），一旦 panic 就是整个进程崩掉。回源失败本来是
		// 可接受的降级，不该升级成进程级事故，所以这里自己兜一层。
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("model catalog refresh panic provider=%q: %v", provider, rec)
				}
			}()
			// ⚠ 用 AnySource：后台回源同样要"扩展点优先、失败回落 /v3/config"。
			// 只调 refreshCatalog 会让不实现扩展点的上游（workbuddy 系）
			// 在 SWR 路径上永远拉不到倍率 —— 与上一版那个回归同形。
			h.refreshCatalogAnySource(provider)
		}()

		modelCatalogCache.Lock()
		// 只注销**自己**那一次：否则会把 reset/后续回源新建的 flight 误删，
		// 单飞就漏了。
		if modelCatalogCache.flights[provider] == ch {
			delete(modelCatalogCache.flights, provider)
		}
		modelCatalogCache.Unlock()
		close(ch) // 等待者只关心"结束了"，结果一律从缓存读
	}()
	return ch
}

// cachedCatalog 读一份当前缓存目录（不触发任何回源/负缓存判断）。
// 首次路径用它把"后台那一次的结果"取回来。
func (h *Handler) cachedCatalog(provider string) *gateway.ModelCatalog {
	modelCatalogCache.RLock()
	defer modelCatalogCache.RUnlock()
	if e := modelCatalogCache.entries[provider]; e != nil {
		return e.cat
	}
	return nil
}

// refreshCatalogAnySource 按"扩展点优先、失败回落 /v3/config"回源一次。
//
// # 这个顺序是**语义**，不是优化
//
// 每个上游的倍率必须由它自己的官方数据源回答：
//
//	loomy / trae / codearts  实现 ModelMultiplierExt（官方接口自报倍率）
//	workbuddy / inkstone / jiyuan  不实现，倍率在 workbuddy 的 /v3/config
//
// 而 workbuddy 的 /v3/config **只能拿 workbuddy 的凭证打** —— 拿别家凭证
// 打 copilot.tencent.com 只会失败并惩罚一个无辜账号。所以顺序不能颠倒：
// 先问扩展点（那是"这个上游自己说的"），拿不到才认为"它是 workbuddy 系"。
//
// ⚠ 上一版的回归正是丢掉了这个回落：进了扩展点分支就不再往下走，
// 于是不实现扩展点的上游倍率全丢（preview 只剩 loomy 的 8 条）。
func (h *Handler) refreshCatalogAnySource(provider string) {
	if h.refreshFromMultiplierExt(provider) {
		return
	}
	h.refreshCatalog(provider)
}

// refreshFromMultiplierExt 经**扩展点**回源一次，成功则写进缓存并返回 true。
//
// # 为什么与 refreshCatalog 写同一格
//
// 两者回答的是同一个问题（"这个上游现在的模型倍率表是什么"），只是
// 数据源不同（扩展点的官方接口 vs workbuddy 的 /v3/config）。
// 缓存按 provider 分格，所以两条路必须共用那一格 —— 否则会出现
// "扩展点成功了、缓存里却是空的"，下一次又去打扩展点，SWR 白做。
//
// # 返回值的用途
//
// **只表示"这次拿到东西了吗"**，供调用方决定要不要回落 /v3/config。
// 所以"上游没给"与"扩展点不存在"都返回 false，而**失败时不写空值进缓存**
// （保持旧目录原样，与 refreshCatalog 一致：成本系数是观测维度，
// 过期的系数比没有有用）。
//
// # 失败为什么不惩罚账号
//
// 与 providerModelsCached 同一判据：扩展点的入参凭证由装配层给，
// 出口层不知道用的是谁的号；拿一个自己没选的 uid 记错会惩罚无辜账号。
// 且失败最常见形态是**超时**（loomy 的 /models 重尾），
// 那说明"这个上游慢"，不是"这个号坏了"。
func (h *Handler) refreshFromMultiplierExt(provider string) bool {
	if h.cfg.Provider == nil {
		return false
	}
	m, ok := h.cfg.Provider.ModelMultipliers(context.Background(), provider)
	if !ok || len(m) == 0 {
		return false // 上游没给（或没实现扩展点）：交给调用方回落
	}
	cat := gateway.CatalogOf(m)
	modelCatalogCache.Lock()
	if modelCatalogCache.entries == nil {
		modelCatalogCache.entries = map[string]*modelCatalogEntry{}
	}
	// ⚠ 就地更新而不是新建 entry：SWR 下并发请求会撞在一起，
	// 换新 entry 会让"另一路正在读的那个指针"变成孤儿。
	modelCatalogCache.entries[provider] = &modelCatalogEntry{
		cat:      cat,
		fetched:  time.Now(),
		lastFail: time.Time{},
	}
	modelCatalogCache.Unlock()
	return true
}

// refreshCatalog 真实回源一次并把结果写回缓存（由后台 goroutine 执行）。
//
// 取号/换号/惩罚/失败保留旧目录这些语义与改造前的同步实现**逐条相同**，
// 唯一差别是执行位置（请求路径 → 后台），以及失败时保留的是
// "刷新开始那一刻缓存里的旧目录"。
func (h *Handler) refreshCatalog(provider string) {
	tried := map[string]bool{}
	for i := 0; i < h.cfg.MaxRotate; i++ {
		// 按 provider 取号：多上游部署下不能拿默认上游的账号去拉别的上游的 /v3/config。
		// provider 为空串时退回 PickForExcluding("")（默认上游，单上游部署逐字节不变）。
		var acct *auth.Auth
		if provider == "" {
			acct = h.cfg.Pool.PickForExcluding("", tried)
		} else {
			for _, uid := range h.cfg.Pool.AvailableUIDsFor(provider) {
				if tried[uid] {
					continue
				}
				acct = h.cfg.Pool.AuthByUID(uid)
				break
			}
		}
		if acct == nil {
			break
		}
		tried[acct.UID] = true
		// ⚠ 这里原先是一处**装配边界**：`cfg.Upstream` 是 workbuddy 的客户端，
		// 它返回 workbuddy 自己的 `*upstream.ModelCatalog`，而缓存与出口层用
		// 中立类型 `gateway.ModelCatalog` —— 所以调一个 toGatewayCatalog 做投影。
		//
		// 现在那个投影**搬到了装配层**（见 cmd/server 的 defaultUpstream 适配器）：
		// 接口直接声明返回 `*gateway.ModelCatalog`，核心拿到的就是中立类型。
		// 投影本身一个字段都没变（逐字段搬运 + MultiplierKnown 的填法），
		// 只是发生的位置从"核心里"挪到了"接缝上"。
		cat, err := h.cfg.Upstream.FetchModelCatalog(acct)
		if err != nil || cat == nil || len(cat.Models) == 0 {
			// 与 fetchDynamicModels 同口径：惩罚该账号，避免下次 Pick 又选中同一个反复失败的号。
			h.cfg.Pool.NoteError(acct.UID)
			continue
		}
		modelCatalogCache.Lock()
		if modelCatalogCache.entries == nil {
			modelCatalogCache.entries = map[string]*modelCatalogEntry{}
		}
		modelCatalogCache.entries[provider] = &modelCatalogEntry{
			cat:      cat,
			fetched:  time.Now(),
			lastFail: time.Time{},
		}
		modelCatalogCache.Unlock()
		return
	}

	// 全部尝试失败：只**打时间戳**进负缓存，目录一律保留原样 ——
	// 成本系数是观测维度，过期的系数比没有系数有用得多（前端会标 stale 提示其不可信）。
	//
	// ⚠ 这里**不重建 entry**（改造前是 `&modelCatalogEntry{cat: stale, ...}`）：
	// 重建会把 cat 换成"调用方读到的那个"旧指针，而回源期间**可能已经有一次
	// 成功的刷新填了新目录**（SWR 下多个请求会撞在一起）。就地更新
	// lastFail/cat 既能保留最新旧值，也让 entry 指针稳定 ——
	// 首次路径的"超时写负缓存"正是靠指针相等判断自己有没有被抢先。
	modelCatalogCache.Lock()
	if modelCatalogCache.entries == nil {
		modelCatalogCache.entries = map[string]*modelCatalogEntry{}
	}
	if e := modelCatalogCache.entries[provider]; e != nil {
		e.lastFail = time.Now()
	} else {
		modelCatalogCache.entries[provider] = &modelCatalogEntry{lastFail: time.Now()}
	}
	modelCatalogCache.Unlock()
}

// defaultMaxBodyMB 请求体上限的兜底值（MiB）。
//
// 与改造前那句 `io.LimitReader(r.Body, 8<<20)` 的 8 MiB **数值相同** ——
// 本改动的意图不是改变"能收多大"，而是改变"收不下时说什么"：
// 从"无声截断 → 上游 11101 → 客户端以为自己的 JSON 写错了"，
// 变成"读的时候就报 413 → 客户端知道是大小问题"。
