// quota_refresh.go 核心把**上游自报的额度**写回账号池。
//
// # 核心在这里只做一件事
//
// **把结果写回池。** 它不解释额度怎么算 —— 打哪个端点、怎么解析、
// 取不到时算不算"有数据"，全部由上游的 gateway.QuotaExt 回答。
// 这样加第 N 个上游时本文件零改动（判据 1）。
//
// # 为什么需要这条路径（T2 的根因）
//
// 改造前 `/admin/accounts` 的 `quota`/`credits` **只有 workbuddy 有值**：
//
//	workbuddy  POST /admin/credits/refresh
//	           → workbuddy.RefreshCredits
//	           → client.UserResource（真上游）
//	           → pool.SetCredits            ← 它自己会把结果写回池
//
// 而 `/admin/accounts` **自己不查任何东西**，它只是把 pool.Status 内嵌进
// AccountView 就返回（见 admin.go 的 accountViews）。所以字段有没有值，
// **完全取决于有没有人调过 SetQuota / SetCredits**。
//
// codearts 一条写回路径都没有 → 它的额度落到 pool.restoreQuota 的兜底分支
// → 被造成 `{kind:credits, has_data:true, remaining:0}` 的**假数据**
// （"从没查过"被伪装成"查过了，确实是 0"）。
//
// 这条路径把那个洞补上：**核心遍历所有上游，问它们各自的额度，写回池。**
package admin

import (
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"sync"

	"github.com/NaiZi899/Naibox/internal/gateway"
	"github.com/NaiZi899/Naibox/internal/pool"
)

// quotaRefresher 一次额度刷新的统计（供端点回执与测试断言）。
type quotaRefresher struct {
	// Updated 成功拿到数据并写回池的账号数。
	Updated int
	// Unknown 上游明确回答"我不知道额度"的账号数（界面显示 `—`）。
	//
	// ⚠ 与 Failed 分开计数：这是**正常情况**，不是错误。
	// 混在一起会让"上游不报额度"看起来像故障，从而被误修。
	Unknown int
	// Failed 上游返回 nil 或刷新过程出错（账号解析不了等）的账号数。
	Failed int
	// Skipped 因上游未实现 QuotaExt 而**未被问过额度**的账号数。
	//
	// ⚠ 与 Unknown 区分：
	//   Unknown = "问了，上游说不知道"（正常情况，不该刷错误日志）
	//   Skipped = "压根没问" —— 它意味着某个上游**完全没有额度接入**，
	//            是本文件唯一真正需要人介入的状态。
	//
	// 这个字段存在的理由：本文件曾经对"没实现 QuotaExt 的上游"静默 continue，
	// 于是"某个上游整片账号没被刷到"在日志/回执/界面上**全部不可见**。
	Skipped int
	// Providers 本次真正被问过额度的上游（排序后，供回执/日志）。
	//
	// ⚠ 注意它的语义边界：它只收录**真正问过**的上游。被跳过（未实现
	// QuotaExt）的上游**不会**出现在这里 —— 所以单独看 Providers
	// 无法区分"这个上游只有 0 个账号"和"这个上游整片被跳过"。
	// 后者只能靠 Skipped + 下面那条按 provider 去重的跳过日志来暴露。
	Providers []string
	// Revived 因余额恢复而被解除硬冷却的账号数。
	//
	// ⚠ 只有实现了 gateway.QuotaUsabilityExt 且返回 true 的上游才可能非零
	//（目前只有 jiyuan）。其它上游恒为 0，不是"没解冻成功"。
	Revived int
}

// quotaRefreshConcurrency 一轮额度刷新的并发上限。
//
// # 为什么是 4
//
// 这条链路是"每个账号打一次上游管理面"，而单次往返常在 200ms~2s。
// 串行时整轮耗时 = 账号数 × 往返：25 个账号要十几秒，而 HTTP 端点
// `/admin/accounts/quota/refresh` 是**同步**返回的，用户点一下要等十几秒。
//
// 并发 4 把 25 个账号压到 1~2 个往返的量级。不再往上加的理由是上游侧：
// 同一个上游的管理面通常有速率限制，且这些请求带的是**同一个账号池**的
// 凭证 —— 并发太高容易被判成扫描。
const quotaRefreshConcurrency = 4

// quotaTask 第一段解析出来的一个待办（纯内存，不含网络）。
//
// 之所以拆出这个结构：并发段只该做 IO，不该再碰池与注册表
// （那些是有锁的共享状态，在 N 个 goroutine 里读会让锁竞争变成瓶颈）。
type quotaTask struct {
	uid      string
	provider string
	ext      gateway.QuotaExt
	// usable 该上游是否"余额即门禁"（gateway.QuotaUsabilityExt）。
	// 在第一段就查好，避免并发段重复做类型断言。
	usable bool
}

// quotaTaskResult 单个任务的探测结果。
type quotaTaskResult struct {
	qv gateway.QuotaView
	ok bool
}

// refreshQuotas 遍历池中所有账号，按各自的上游问额度并写回。
//
// # 为什么要按 provider 分派而不是直接问"当前上游"
//
// 账号池是**多上游共用**的（每个 entry 带 provider 标签）。
// 拿一个"当前上游"去刷全部账号，会把 A 的额度写到 B 的账号上 ——
// 那是比不刷新更糟的错（显示了看起来合理的错数）。
//
// # 没有 QuotaExt 的上游怎么办
//
// **跳过，并且不写池。** 账号保持 `HasData=false` → 界面显示 `—`。
// 这正是用户要的"有什么显示什么，没有的话就不显示" ——
// 不填 0（0 会被误读成"没有额度了"）。
//
// # 三段式（并发化的关键）
//
//	① 串行解析：uid → provider → 扩展点（纯内存）
//	② 并发打上游（4 路）：只做 IO，结果按下标写回
//	③ 串行收尾：写池、解冻、计数
//
// 这样锁只在 ①③ 里出现，②里没有任何共享状态。若把 ①②混在一个循环里，
// `Pool.Status` 与 `Registry.All` 会被 4 个 goroutine 反复加锁，
// 并发收益被自己吃回去。
func (h *Handler) refreshQuotas(uids []string) quotaRefresher {
	var out quotaRefresher
	if h.cfg.Pool == nil || h.cfg.Registry == nil {
		return out
	}

	// provider → 该上游的额度扩展点。**只对实现了 QuotaExt 的上游建映射**，
	// 没实现的天然不在表里 → 走"跳过"分支。
	exts := map[string]gateway.QuotaExt{}
	// provider → 余额是否直接决定可用性（QuotaUsabilityExt）。
	usable := map[string]bool{}
	for _, p := range h.cfg.Registry.All() {
		if ext, ok := gateway.ExtOf[gateway.QuotaExt](p); ok {
			exts[p.ID()] = ext
		}
		if u, ok := gateway.ExtOf[gateway.QuotaUsabilityExt](p); ok && u.QuotaImpliesUsable() {
			usable[p.ID()] = true
		}
	}

	// ---- ① 串行解析（纯内存）----
	tasks := make([]quotaTask, 0, len(uids))
	seen := map[string]bool{}
	// loggedSkip 保证"某个上游没实现 QuotaExt"只打**一行**日志。
	//
	// # 为什么必须去重
	//
	// 这个循环是按账号跑的：一个来自没接额度的上游如果有 50 个账号，
	// 不去重就会打 50 行同样的日志 —— 真正的故障（failed 的堆栈）
	// 会被自己的噪音淹没。日志的价值在"有无"，不在"条数"。
	loggedSkip := map[string]bool{}
	for _, uid := range uids {
		st, ok := h.cfg.Pool.Status(uid)
		if !ok {
			continue
		}
		// ⚠ 用**账号自己**的 provider 去找上游，不是全局默认上游。
		// 空串按默认上游解释 —— 与 pool 的 providerOf 语义一致
		//（旧状态文件没有 provider 字段，那批账号属于默认上游）。
		provider := st.Provider
		if provider == "" {
			provider = h.cfg.DefaultProvider
		}
		ext, ok := exts[provider]
		if !ok {
			// 该上游不报额度 → 保持未知，不写池（行为不变）。
			//
			// ⚠ 但**必须记下来**：这里曾经是一句光秃秃的 continue。
			// 于是"某个上游整片账号压根没被问过"在日志、回执、界面上
			// 全部不可见 —— 这正是本文件那个 bug 能藏住的直接原因。
			// 计数 + 按 provider 去重的日志，让它一眼可见。
			out.Skipped++
			if !loggedSkip[provider] {
				loggedSkip[provider] = true
				log.Printf("admin: 额度刷新跳过上游 %q（未实现 gateway.QuotaExt）—— 它的额度在界面上将恒为未知", provider)
			}
			continue
		}
		if !seen[provider] {
			seen[provider] = true
			out.Providers = append(out.Providers, provider)
		}
		tasks = append(tasks, quotaTask{
			uid:      uid,
			provider: provider,
			ext:      ext,
			usable:   usable[provider],
		})
	}

	// ---- ② 并发打上游 ----
	results := make([]quotaTaskResult, len(tasks))
	if len(tasks) > 0 {
		sem := make(chan struct{}, quotaRefreshConcurrency)
		var wg sync.WaitGroup
		for i := range tasks {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				qv, ok := h.safeRefreshQuota(tasks[i].ext, tasks[i].uid)
				results[i] = quotaTaskResult{qv: qv, ok: ok}
			}(i)
		}
		wg.Wait()
	}

	// ---- ③ 串行收尾：写池 + 解冻（顺序与并发无关，结果可复现）----
	for i, t := range tasks {
		r := results[i]
		if !r.ok {
			out.Failed++
			continue
		}
		if !r.qv.HasData {
			// 上游如实回答"我不知道" —— **不写池**（写 0 就是造假）。
			out.Unknown++
			continue
		}
		h.cfg.Pool.SetQuota(t.uid, toPoolQuota(r.qv))
		out.Updated++
		// 余额恢复了，且该上游的可用性由余额决定 → 解除"余额耗尽"型硬冷却。
		//
		// ⚠ 只调 ReviveHardCreditIfUsable，**不是** ClearCooldown：
		// 后者会连熔断退避与软限流一起清掉，而周期刷新每 15 分钟跑一次，
		// 那等于把正在吃 429 的号定期放回池子。余额只能证明"钱够"。
		if t.usable && h.cfg.Pool.ReviveHardCreditIfUsable(t.uid) {
			out.Revived++
			log.Printf("admin: 额度已恢复，解除余额耗尽冷却 uid=%s provider=%s", t.uid, t.provider)
		}
	}

	sort.Strings(out.Providers)
	// 收尾汇总：一行看全五个计数。
	//
	// ⚠ skipped 必须出现在这里。它和 unknown 是**完全不同**的东西：
	// unknown 是上游如实回答"不知道"（正常），skipped 是"我们压根没问"
	// （某个上游没接额度）。只报 unknown 会让后者伪装成前者。
	log.Printf("admin: 额度刷新完成 updated=%d unknown=%d failed=%d skipped=%d revived=%d providers=%v",
		out.Updated, out.Unknown, out.Failed, out.Skipped, out.Revived, out.Providers)
	return out
}

// RefreshAllQuotas 刷新全池额度，供装配层的周期任务调用。
//
// # 与 HTTP 端点共用同一套逻辑
//
// 差别只在**入参怎么来**：端点按作用域（全池/单上游/单账号）筛，
// 这里固定全池。跳过/并发/解冻的判据一字不差 —— 两条路径各写一遍
// 必然漂移，而漂移的表现是"点按钮能刷出额度、等 15 分钟却不行"。
//
// # 为什么这里要预过滤（端点不做）
//
// 周期任务每 15 分钟跑一次，而端点只在用户点击时跑。两者对"噪音"的
// 耐受度完全不同：
//
//	禁用账号        它们本来就不参与选号，每轮问一次纯属浪费上游配额
//	无 QuotaExt 上游 每轮都会刷一行 skipped 日志 —— 15 分钟一条，一天 96 条，
//	                真正的故障会被这个周期性噪音埋掉
//
// 端点上保留 skipped 计数是有价值的（用户点一下就想知道"为什么这个上游
// 没额度"），周期任务上则是纯噪音。
func (h *Handler) RefreshAllQuotas() quotaRefresher {
	var out quotaRefresher
	if h.cfg.Pool == nil || h.cfg.Registry == nil {
		return out
	}
	// 先算出哪些上游实现了 QuotaExt，避免对整片没接额度的上游白跑一遍。
	hasQuota := map[string]bool{}
	for _, p := range h.cfg.Registry.All() {
		if _, ok := gateway.ExtOf[gateway.QuotaExt](p); ok {
			hasQuota[p.ID()] = true
		}
	}
	all := h.cfg.Pool.List()
	uids := make([]string, 0, len(all))
	for _, st := range all {
		if st.Disabled {
			continue
		}
		if !hasQuota[h.providerOf(st)] {
			continue
		}
		uids = append(uids, st.UID)
	}
	if len(uids) == 0 {
		// 空池/全禁用/全没接额度：静默返回。
		// 这里**必须**不打日志 —— 否则一个纯 workbuddy 部署会每 15 分钟
		// 收到一条"额度刷新完成 updated=0 ..."，而它什么也没做错。
		return out
	}
	return h.refreshQuotas(uids)
}

// safeRefreshQuota 调上游的 RefreshQuota，把 panic 兜住。
//
// # 为什么要 recover
//
// gateway 契约要求实现方不得 panic，但这条路径是**遍历所有上游**跑的：
// 一个上游的 bug 会让整轮刷新中断，**连累其它正常上游的额度也刷不出来**。
// 额度是展示性信息，任何单点异常都只该降级成"这个号未知"。
//
// 同样的手法在 workbuddy.dosageHint 里已有一份（理由相同）。
func (h *Handler) safeRefreshQuota(ext gateway.QuotaExt, uid string) (qv gateway.QuotaView, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("admin: 额度刷新 panic（上游实现违约）uid=%s: %v", uid, r)
			qv, ok = gateway.QuotaView{}, false
		}
	}()
	return ext.RefreshQuota(uid)
}

// toPoolQuota 把上游的额度视图投影成 pool 的类型。
//
// # 为什么需要这一层转换（而不是共用一个类型）
//
// gateway 不得依赖 internal/pool（它是"上游与核心之间的唯一接缝"，
// 必须保持零核心依赖，否则加一个上游就要连带改 pool）。
// 反过来 pool 也不该依赖 gateway。于是两边各声明一份，
// 由**装配层**（这里）逐字段直译 —— 与 SettingsExts 同一手法。
//
// 转换是**无信息损失**的：字段一一对应。
func toPoolQuota(q gateway.QuotaView) pool.QuotaView {
	out := pool.QuotaView{
		Kind:      q.Kind,
		Remaining: q.Remaining,
		HasData:   q.HasData,
		// Unit 必须透传：漏掉它的后果是"界面上量纲标注整片消失"，
		// 而那**不报错** —— 用户看到的是 jiyuan 的 ¥99.75 显示成裸 `9975`，
		// 与 workbuddy 的积分 9975 长得一模一样，无法区分。
		Unit: q.Unit,
	}
	if len(q.ByModel) > 0 {
		// 深拷贝：pool 会把这份 map 存进 entry，之后可能被其它 goroutine 读。
		// 直接递上游的 map 会让上游后续改动**绕过池的锁**改到池内状态
		//（pool.QuotaView.Clone 的注释记着这个真实缺陷）。
		m := make(map[string]int64, len(q.ByModel))
		for k, v := range q.ByModel {
			m[k] = v
		}
		out.ByModel = m
	}
	return out
}

// refreshAccountQuotas GET/POST /admin/accounts/quota/refresh —— 刷新额度并写回池。
//
// 语义与 /admin/credits/refresh（workbuddy 专有）的区别：
//
//	/admin/credits/refresh        单上游（workbuddy），且会顺手做"解冻"判断
//	/admin/accounts/quota/refresh **全上游通用**，只做"取额度 + 写回池"
//
// 两者并存是刻意的：后者不碰解冻（那需要上游回答"能不能用"，是另一件事），
// 所以它对已有路径**零行为改变**，纯粹是新增的只读+写额度入口。
//
// # 三种作用域（用户报的问题：「行内按钮却刷了全部」）
//
// 用户实测：在账号池里点某一行的「额度」按钮，**所有账号的额度都被刷新了**。
// 原因是本端点以前**不收任何参数**，恒为全池刷新 ——
// 而前端行内按钮与顶部「刷新全部额度」打的是同一个 URL，
// 于是"行内"这个作用域只存在于按钮的视觉位置里，后端根本不知道。
//
// 现在按可选参数分三档：
//
//	（无参数）            全部账号        顶部「刷新全部额度」
//	{"uid":"..."}         只刷这一个账号   行内「额度」按钮
//	{"provider":"..."}    只刷该上游的账号 上游分组标题上的「刷新本上游额度」
//
// # 向后兼容
//
// 不带参数时行为与改造前**逐字段相同**（全池刷新）。既有调用方
// （含前端顶部按钮、脚本、curl）一行都不用改。
func (h *Handler) accountsQuotaRefresh(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Pool == nil {
		writeError(w, http.StatusNotImplemented, "账号池未接线")
		return
	}
	// 请求体可缺（GET 或空 body 都合法 = 全池刷新）。
	var body struct {
		UID      string `json:"uid"`
		Provider string `json:"provider"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	// query 参数也认（便于 curl 与脚本按作用域调用，不必构造 JSON）。
	if body.UID == "" {
		body.UID = r.URL.Query().Get("uid")
	}
	if body.Provider == "" {
		body.Provider = r.URL.Query().Get("provider")
	}

	all := h.cfg.Pool.List()
	var uids []string
	scope := "all"

	switch {
	case body.UID != "":
		// 单账号：先确认它在池里，否则 404 而不是"刷了 0 个"。
		scope = "account"
		want := body.UID
		for _, st := range all {
			if st.UID == want {
				uids = append(uids, want)
				break
			}
		}
		if len(uids) == 0 {
			writeError(w, http.StatusNotFound, "账号不存在: "+want)
			return
		}
	case body.Provider != "":
		// 按上游：该上游自己在池里的全部账号。
		scope = "provider"
		pid := body.Provider
		for _, st := range all {
			if h.providerOf(st) == pid {
				uids = append(uids, st.UID)
			}
		}
		if len(uids) == 0 {
			writeError(w, http.StatusNotFound, "该上游在池里没有账号: "+pid)
			return
		}
	default:
		uids = make([]string, 0, len(all))
		for _, st := range all {
			uids = append(uids, st.UID)
		}
	}

	res := h.refreshQuotas(uids)
	writeJSON(w, http.StatusOK, map[string]any{
		"updated":   res.Updated,
		"unknown":   res.Unknown,
		"failed":    res.Failed,
		"skipped":   res.Skipped,
		"providers": res.Providers,
		// Revived 因余额恢复而解除硬冷却的账号数。
		//
		// 单独一个字段而不是并进 updated：两者含义完全不同 ——
		// updated 是"读到了额度"，revived 是"这个号因此重新可用了"。
		// 混在一起，用户无法判断充值到底有没有把号救回来。
		"revived": res.Revived,
		// Scope 告诉前端"这次刷的是哪一档"，回执文案据此措辞
		//（"已刷新 3 个账号" vs "已刷新全部 12 个账号"）。
		"scope": scope,
		"count": len(uids),
		// 回执里带上刷新后的账号视图，前端一次调用就能重绘表格。
		"accounts": h.accountViews(),
	})
}

// providerOf 取账号所属上游（空则落默认上游，与 accountViews 同一判据）。
func (h *Handler) providerOf(st pool.Status) string {
	if st.Provider != "" {
		return st.Provider
	}
	return h.cfg.DefaultProvider
}
