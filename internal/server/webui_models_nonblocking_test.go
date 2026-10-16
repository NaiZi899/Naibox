package server

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 源码级守卫：模型面板并行取数、且不阻塞 refresh()
// ---------------------------------------------------------------------------
//
// # 这条守卫守的是什么 bug（用户实测）
//
// 控制台 `refresh()` 把两个模型相关端点**串行 await** 在关键路径上：
//
//	await loadModelMultipliers();                          // GET /admin/models/preview
//	try { renderModels((await api('/v1/models')).data); }   // GET /v1/models
//
// 而这两个恰好是全页最慢的端点，同一次测量里：
//
//	/admin/models/preview  max 27.06s
//	/v1/models             p50 4107ms / max 22630ms
//	/healthz               1ms
//	/status                1.2ms
//	/admin/accounts        11ms
//	/admin/ui/manifest     1.2ms
//
// 于是**首屏与「立即刷新」的回执**都被模型目录拖住：两条串行 ≈ 两个端点之和
// （实测最坏 27.06 + 22.63 ≈ 49.7s），而同一时刻卡片/账号/导航早就该画好了。
//
// 后端同期在给这两个端点加缓存与 SWR —— 但那是**另一层**：即使后端变成 10ms，
// 前端也仍然不该把「装饰/选择」信息串在关键路径上（一次慢查询就够把首屏拖住）。
// ∴ 本守卫守的是**前端编排**，与后端多快无关。
//
// # 为什么写成源码级守卫而不是只靠浏览器实测
//
// 与 webui_models_unified_test.go 逐字同一条理由：tests/frontend/ 那套
// Node + 真 Chrome 能证明"今天是对的"，但它不会在**每次** go test 时执行 ——
// 于是把 `await` 加回去的人不会立刻看到红。下面的断言只看 webui.html 源码。
//
// # 判据为什么剥注释
//
// ⚠ 与 webui_models_unified_test.go 的教训逐字相同：本文件与 webui.html 里
// 解释"改造前是串行 await"的注释**必然**会写出旧写法（`await loadModelMultipliers();`
// / `renderModels((await api('/v1/models')).data)`）。那是说明文字，不是代码。
// 不剥注释的话，说明文字会让守卫自己红，而红得"有理由"，于是下一个人会去删注释
// 而不是改代码：守卫就废了。
// ∴ 所有判据都跑在 stripJSCommentsDeep 的输出上（剥法保留换行，行号不失真）。
//
// # 为什么**不**要求"注释里不许出现旧写法"
//
// 反了。旧写法必须留在注释里 —— 它是"为什么会有这个函数"的唯一书面依据
//（见 loadModelsPanel 的注释）。守卫要守的是**活代码**里没有串行 await。

// --------------------------------------------------------------------------
// 判据组 1：两路取数并发发起
// --------------------------------------------------------------------------

// modelsPanelSerialProblems 判定「模型面板两路取数是否串行」；全绿返回 nil。
//
// 入参是**剥注释后**的函数体（loadModelsPanel）。抽成纯函数是为了让
// TestWebUIModelsPanelGuardCatchesRevertedCode 把串行版本喂进来确认它报违规
// （抓不住回退的守卫是装饰品 —— 与 webui_models_unified_test.go 同一模式）。
func modelsPanelSerialProblems(body string) []string {
	var problems []string
	if strings.TrimSpace(body) == "" {
		return []string{"传入的 loadModelsPanel 函数体是空的 —— 判据失效（fail-open），守卫变成装饰品"}
	}

	// ---- fail-closed 自检：切出来的必须确实像 loadModelsPanel --------------
	// 判据读的不是这个函数，绿红都不可信（与 webui_models_unified_test.go 里
	// 对 renderModels / multTableKey 的 fail-closed 自检同一模式）。
	if !strings.Contains(body, "loadModelMultipliers()") || !strings.Contains(body, "api('/v1/models')") {
		return []string{"切出来的函数体不像 loadModelsPanel（缺 `loadModelMultipliers()` / `api('/v1/models')`，" +
			"共 " + itoa(len(body)) + " 字节）—— 切分逻辑已失效，本守卫此时无论绿红都不可信"}
	}

	// ---- ① 两路取数必须在**同一个** Promise.all 的参数列表里 ---------------
	// 判据是结构性的：只看 "文件里有 Promise.all" 会被假绿 ——
	// 完全可以保留一个 `Promise.all([Promise.resolve(), listP])`，
	// 而把倍率改回串行 await（实测最慢的那个就是倍率，27.06s）。
	iAll := strings.Index(body, "Promise.all([")
	if iAll < 0 {
		problems = append(problems, "loadModelsPanel 里找不到 `Promise.all([` —— 两个模型端点没有并行发起。\n"+
			"    破了会怎样：退回串行 await，模型面板的等待是**两个**端点之和"+
			"（实测最坏 27.06s + 22.63s ≈ 49.7s），而它们恰好是全页最慢的两个")
	} else {
		args, ok := slicePromiseAllArgs(body[iAll:])
		if !ok {
			return append(problems, "Promise.all 的参数列表括号不闭合 —— 并发判据无法判定（fail-open），"+
				"此时本守卫无论绿红都不可信")
		}
		hasMult := strings.Contains(args, "loadModelMultipliers()")
		hasList := strings.Contains(args, "api('/v1/models')")
		switch {
		case !hasMult && !hasList:
			problems = append(problems, "Promise.all([…]) 里既没有 loadModelMultipliers() 也没有 api('/v1/models') —— "+
				"并发的不是这两个模型端点（args = "+quoteForMsg(args)+"）。\n"+
				"    破了会怎样：看起来『已经并行化』，而模型端点仍在别处串行等待 —— "+
				"判据被一个无关的 Promise.all 糊过去（假绿）")
		case !hasMult:
			problems = append(problems, "Promise.all([…]) 里没有 `loadModelMultipliers()`（只有目录那一路）。\n"+
				"    破了会怎样：倍率仍在别处 await —— 它是最慢的那个（实测 max 27.06s），"+
				"并行只做了一半，首屏照样被它拖住")
		case !hasList:
			problems = append(problems, "Promise.all([…]) 里没有 `api('/v1/models')`（只有倍率那一路）。\n"+
				"    破了会怎样：目录仍在倍率之后才发起 —— 实测 p50 4107ms / max 22630ms，"+
				"串在两个原子操作之间等于把延迟相加")
		}
	}

	// ---- ② 不许有"先 await 倍率、再发起目录"的串行形态 ----------------------
	// 这两条是本次消灭的**原文形态**。分开判：回退时可能只命中其中一条，
	// 而失败信息要能指出到底是哪一条回来了。
	if strings.Contains(body, "await loadModelMultipliers()") {
		problems = append(problems, "loadModelsPanel 里又出现 `await loadModelMultipliers()` —— "+
			"倍率被串行等在目录取数之前（改造前的原文形态）。\n"+
			"    破了会怎样：目录请求要等倍率回来才发出去，两个端点延迟相加；"+
			"而倍率正是实测最慢的那个（max 27.06s）")
	}
	if strings.Contains(body, "renderModels((await api('/v1/models')).data)") {
		problems = append(problems, "loadModelsPanel 里又出现 `renderModels((await api('/v1/models')).data)` —— "+
			"取数与渲染被内联成一条串行 await（改造前的原文形态）。\n"+
			"    破了会怎样：这是『看起来只是一行』的回退，最容易漏过评审；"+
			"它同时把失败局部化也丢了（一个端点抛就把另一个的数据一起丢掉）")
	}

	return problems
}

// slicePromiseAllArgs 从 `Promise.all([` 处截出参数列表（含两端方括号）。
//
// 返回的第二值表示"方括号配对成功"。配对失败时必须返回 false —— 调用方据此
// 报 fail-open，而不是拿一段截短的内容去判（那会静默变绿或报错得没道理）。
//
// 简化：按 `[` `]` 计数，不跳过字符串/模板串里的方括号。本函数的目标是
// loadModelsPanel 里那个**字面量**参数列表，里面没有方括号字面量；
// 真有人塞进去，结果是"配对到更远处"，届时 args 里照样能找到两路引用
// （判据仍绿），不会误报。
func slicePromiseAllArgs(s string) (string, bool) {
	i := strings.Index(s, "([")
	if i < 0 {
		return "", false
	}
	depth := 0
	for j := i + 1; j < len(s); j++ {
		switch s[j] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return s[i+1 : j+1], true
			}
		}
	}
	return "", false
}

// --------------------------------------------------------------------------
// 判据组 2：refresh() 不等模型面板
// --------------------------------------------------------------------------

// refreshNonBlockingProblems 判定「refresh() 是否仍在关键路径上等模型面板」；
// 全绿返回 nil。入参是**剥注释后**的 refresh 函数体。
func refreshNonBlockingProblems(body string) []string {
	var problems []string
	if strings.TrimSpace(body) == "" {
		return []string{"传入的 refresh 函数体是空的 —— 判据失效（fail-open），守卫变成装饰品"}
	}

	// ---- fail-closed 自检：切出来的必须确实像 refresh ----------------------
	// 标记取 `renderAccounts` / `refresh._last`：一个在 refresh 中段、
	// 一个在末尾，缺任一个都说明切分串到了别处或被截短。
	if !strings.Contains(body, "renderAccounts(") || !strings.Contains(body, "refresh._last") {
		return []string{"切出来的函数体不像 refresh（缺 `renderAccounts(` / `refresh._last`，" +
			"共 " + itoa(len(body)) + " 字节）—— 切分逻辑已失效，本守卫此时无论绿红都不可信"}
	}

	// ---- ① 必须**调用**模型面板（只触发、不 await） -------------------------
	if !strings.Contains(body, "loadModelsPanel()") {
		problems = append(problems, "refresh() 里找不到 `loadModelsPanel()` —— 模型面板整个从刷新链上消失了。\n"+
			"    破了会怎样：模型面板只靠「强制刷新目录」按钮才会更新，"+
			"普通刷新后它停在旧目录上（静默陈旧，比慢更糟）")
	}
	// ---- ② 不得 await 它 ----------------------------------------------------
	if strings.Contains(body, "await loadModelsPanel()") {
		problems = append(problems, "refresh() 里又出现 `await loadModelsPanel()` —— 模型面板被重新放回关键路径。\n"+
			"    破了会怎样：refresh()（以及「立即刷新」按钮的 busyRun 回执）又要等"+
			"两个模型端点，首屏被拖住的原始症状原样复发 —— 这正是本次要消灭的形态")
	}
	// ---- ③ 不得退回成 await + try/catch 的旧串行形态 ------------------------
	if strings.Contains(body, "await loadModelMultipliers()") {
		problems = append(problems, "refresh() 里又出现 `await loadModelMultipliers()` —— 倍率被串在关键路径上。\n"+
			"    破了会怎样：实测该端点 max 27.06s，refresh() 的完成被它拖住，"+
			"而同一次测量里其它端点全在 1-14ms")
	}
	if strings.Contains(body, "try { renderModels((await api('/v1/models')).data); }") {
		problems = append(problems, "refresh() 里又出现 `try { renderModels((await api('/v1/models')).data); }` —— "+
			"目录取数回到关键路径（改造前的原文形态）。\n"+
			"    破了会怎样：实测 p50 4107ms / max 22630ms，串在 refresh 尾部"+
			"等于让整页等它")
	}
	// ---- ④ 不得绕过模型面板函数直接取 /v1/models ----------------------------
	if strings.Contains(body, "api('/v1/models')") {
		problems = append(problems, "refresh() 里直接出现了 `api('/v1/models')` —— 取数绕开了 loadModelsPanel。\n"+
			"    破了会怎样：失败局部化与 loading 态都跟着绕过去了"+
			"（这条路径上 refresh 历史上没有 try/catch，一个失败就是未捕获的 Promise rejection）")
	}

	return problems
}

// --------------------------------------------------------------------------
// 判据组 3：失败只落在模型面板
// --------------------------------------------------------------------------

// modelsPanelDegradeProblems 判定「模型端点失败时降级是否只落在模型面板」；
// 全绿返回 nil。入参是**剥注释后**的 loadModelsPanel 函数体。
//
// # 为什么单独一组
//
// 并行化最容易顺手弄坏的正是降级：把两路塞进一个 Promise.all 却**不给**
// 逐路失败分支 → 任一失败整体 reject，而 refresh() 故意不 await 它 →
// 页面出现未捕获的 Promise rejection，**且两个端点的数据一起丢掉**。
func modelsPanelDegradeProblems(body string) []string {
	var problems []string
	if strings.TrimSpace(body) == "" {
		return []string{"传入的 loadModelsPanel 函数体是空的 —— 判据失效（fail-open），守卫变成装饰品"}
	}
	// fail-closed 自检：降级判据要读的是"错误往哪写"，定位不准就什么都判不了。
	if !strings.Contains(body, "api('/v1/models')") || !strings.Contains(body, "$('models')") {
		return []string{"切出来的函数体不像 loadModelsPanel（缺 `api('/v1/models')` / `$('models')`，" +
			"共 " + itoa(len(body)) + " 字节）—— 切分逻辑已失效，本守卫此时无论绿红都不可信"}
	}

	// ---- ① 目录那一路必须有失败分支 ----------------------------------------
	// `api(...).then(ok, err)` 的第二参就是失败分支。`.catch(` 也算
	//（两种写法都能吃掉失败，不该只认一种）。
	iList := strings.Index(body, "api('/v1/models')")
	tail := body[iList:]
	if !strings.Contains(tail, "e => ({ ok: false") && !strings.Contains(tail, ".catch(") {
		problems = append(problems, "目录那一路找不到失败分支（既没有 `.then(ok, err)` 的第二参，也没有 `.catch(`）。\n"+
			"    破了会怎样：`/v1/models` 一失败就是**未捕获的 Promise rejection**"+
			"（refresh() 故意不 await 它，没人接得住）—— 页面报错，且模型面板停在 loading 态")
	}

	// ---- ② 失败提示落点：必须真有写到 #models 的地方，且经 esc 转义 --------
	if !strings.Contains(body, "$('models').innerHTML") {
		problems = append(problems, "loadModelsPanel 里没有 `$('models').innerHTML` —— 错误/空态没有落点。\n"+
			"    破了会怎样：模型端点失败时面板停在 loading 文案上，"+
			"用户读成『还在加载』而不是『取不到目录』")
	}
	if !strings.Contains(body, "esc(") {
		problems = append(problems, "loadModelsPanel 里错误文案没有经 esc() 转义 —— 错误消息来自服务端响应体（外部数据）。\n"+
			"    破了会怎样：一条含 HTML 的错误消息会在面板里被当标签解析")
	}

	// ---- ③ #mcount 的清理只能发生在**取数之前** -----------------------------
	//
	// #mcount 是**面板级结论**的落点（renderModelCatalogWarning 把
	//「倍率目录未加载」写进它的 innerHTML，renderModels 把模型计数写进它的
	// textContent）。每轮开头清一次是对的 —— 不清的话，上一轮挂上的红色警告会
	// 在本轮成功之后**原地留着**，看起来"重试也没用"（旧行为正是如此）。
	//
	// ⚠ 但它必须排在取数**之前**：`loadModelMultipliers()` 一失败就会**同步**
	// 走 catch 调 renderModelCatalogWarning 把警告挂上；清理若排在 Promise.all
	// 之后，就会把本轮刚挂上的警告当场擦掉 —— 降级提示整个消失（静默失败）。
	iClear := strings.Index(body, "$('mcount').innerHTML = ''")
	iAll := strings.Index(body, "Promise.all([")
	if iClear >= 0 && iAll >= 0 && iClear > iAll {
		problems = append(problems, "#mcount 的清理（下标 "+itoa(iClear)+"）排在取数 `Promise.all([`（下标 "+itoa(iAll)+"）**之后** —— "+
			"会把本轮刚挂上的『倍率目录未加载』警告当场擦掉。\n"+
			"    破了会怎样：倍率目录整体失败时面板顶部**没有任何提示**，"+
			"用户看到满屏模型却没有倍率，以为上游真的没配 —— 这正是本项目反复出现的静默失败形态")
	}
	// 写 #mcount 的**结果**（警告/计数）只允许在取数之后 —— 取数前就写，
	// 等于把上一轮的结论提前拿来当真（与上面同一条规则的另一面）。
	for _, w := range []string{"$('mcount').textContent ="} {
		if i := strings.Index(body, w); i >= 0 && iAll >= 0 && i < iAll && i != iClear {
			problems = append(problems, "写 #mcount 结论的动作 "+w+"（下标 "+itoa(i)+"）排在取数之前 —— "+
				"那是拿上一轮的结论当真。\n    破了会怎样：本轮还没取到数据就已经改了面板级文案")
		}
	}

	// ---- ④ renderModels 有独立的 try/catch ---------------------------------
	if !strings.Contains(body, "try { renderModels(") {
		problems = append(problems, "loadModelsPanel 里 `renderModels(` 没有包在自身的 try/catch 里。\n"+
			"    破了会怎样：脏目录数据（缺 id / 非法字段）在渲染期抛出的异常"+
			"会变成未捕获 rejection，而**已经取到的数据白拿了** —— 面板停在 loading 态")
	}

	// ---- ④ 失败不外溢：整段不得写其它面板容器 ------------------------------
	// 这条是"失败要局部化"的直接判据：模型面板只许碰 #models 与 #mcount。
	for _, other := range []string{"$('accts')", "$('cards')", "$('nav')", "$('content')", "$('toast')"} {
		if strings.Contains(body, other) {
			problems = append(problems, "loadModelsPanel 里写了 "+other+" —— 模型端点的失败会外溢到别的面板。\n"+
				"    破了会怎样：一个慢/挂的模型端点把账号表或卡片一起改写，"+
				"用户看到的是『整个控制台坏了』，而真相只是目录接口慢")
		}
	}

	// ---- ⑤ 倍率那一路不许被 await ------------------------------------------
	// loadModelMultipliers 自带 try（失败走空表 + multCatalogFailed + 面板提示），
	// await 它等于把倍率的延迟串到目录取数前面。
	if strings.Contains(body, "await loadModelMultipliers()") {
		problems = append(problems, "loadModelsPanel 里 `loadModelMultipliers()` 被 await 了 —— "+
			"并行化只做了一半（倍率实测 max 27.06s）")
	}

	return problems
}

// --------------------------------------------------------------------------
// 判据组 4：迟到的旧一轮不许覆盖新一轮（不阻塞之后才出现的新风险）
// --------------------------------------------------------------------------

// modelsPanelStaleWriteProblems 判定「陈旧回填守卫」是否成立；全绿返回 nil。
//
// 入参是**剥注释后**的**整份源码**（编号变量是模块级，函数体里看不到它的声明）。
func modelsPanelStaleWriteProblems(code string) []string {
	var problems []string
	if strings.TrimSpace(code) == "" {
		return []string{"传入的源码是空的 —— 判据失效（fail-open），守卫变成装饰品"}
	}
	body, ok := extractJSFunction(code, "async function loadModelsPanel(")
	if !ok {
		return []string{"找不到 loadModelsPanel —— 陈旧回填判据无法定位（fail-open）"}
	}
	if !strings.Contains(body, "Promise.all([") || !strings.Contains(body, "api('/v1/models')") {
		return []string{"切出来的函数体不像改造后的 loadModelsPanel（缺 `Promise.all([` / `api('/v1/models')`，" +
			"共 " + itoa(len(body)) + " 字节）—— 切分逻辑已失效"}
	}

	// ---- ① 编号变量必须是**模块级**的（跨轮次比较才可能） -------------------
	if !strings.Contains(code, "let modelsPanelSeq = 0;") {
		problems = append(problems, "找不到模块级 `let modelsPanelSeq = 0;` —— 跨轮次的序号没有落点。\n"+
			"    破了会怎样：迟到的旧一轮会覆盖新一轮的目录，界面『刷新完又变回旧的』且不报错")
	}
	if !strings.Contains(body, "++modelsPanelSeq") {
		problems = append(problems, "loadModelsPanel 里没有 `++modelsPanelSeq` —— 本轮没有编号，无法与旧一轮比较。\n"+
			"    破了会怎样：守卫恒真（`seq` 恒等于当前值），拦不住任何陈旧回填")
	}

	// ---- ② 回填 DOM 之前必须比一次编号 -------------------------------------
	const guard = "if (seq !== modelsPanelSeq) return;"
	iGuard := strings.Index(body, guard)
	if iGuard < 0 {
		return append(problems, "loadModelsPanel 里找不到陈旧回填守卫 `"+guard+"`（共 "+itoa(len(body))+" 字节）—— "+
			"不阻塞之后这条是必须的：两轮可以同时在飞（启动那轮还没回来用户就点了「立即刷新」）")
	}
	// 顺序判据 A：取数必须在守卫之前（取数之前就比，等于拿旧编号比自己）。
	iAll := strings.Index(body, "Promise.all([")
	if iAll < 0 || iAll > iGuard {
		problems = append(problems, "`Promise.all([` 的下标（"+itoa(iAll)+"）不在陈旧守卫（"+itoa(iGuard)+"）之前 —— "+
			"守卫必须在**取数之后、回填之前**")
	}
	// 顺序判据 B：**结果回填**的每一处都必须在守卫之后。
	//
	// ⚠ 只判"结果回填"三处（错误落点 + 渲染 + 计数），不判 #mcount 的**清理**
	// —— 清理按设计就写在**取数之前**（见 modelsPanelDegradeProblems 第③条的
	// 注释：排在取数之后会把本轮刚挂上的倍率目录警告擦掉）。
	// 把它也纳入""回填必须晚于守卫""会让守卫对正确写法报红 → 变成噪音。
	iRender := strings.Index(body, "try { renderModels(")
	if iRender < 0 {
		problems = append(problems, "loadModelsPanel 里找不到 `try { renderModels(` —— 渲染点定位失败（fail-open），"+
			"陈旧守卫拦住了什么就判不出来")
	}
	for _, w := range []string{"$('models').innerHTML = `<span class=\"err\">", "try { renderModels("} {
		if i := strings.Index(body, w); i >= 0 && i < iGuard {
			problems = append(problems, "结果回填动作 "+w+"（下标 "+itoa(i)+"）排在陈旧守卫（"+itoa(iGuard)+"）**之前** —— "+
				"守卫拦不住它。\n    破了会怎样：旧一轮已经写进界面了，后面的 return 只能省下剩下的写入"+
				"（界面已是半旧半新）")
		}
	}

	// ---- ③ 编号必须真的存下来（不是现取现比） -------------------------------
	if !strings.Contains(body, "const seq = ++modelsPanelSeq;") {
		problems = append(problems, "loadModelsPanel 里找不到 `const seq = ++modelsPanelSeq;` —— "+
			"本轮编号没有存下来，后面的 `seq !== modelsPanelSeq` 无从比较")
	}

	return problems
}

// --------------------------------------------------------------------------
// 正例
// --------------------------------------------------------------------------

// TestWebUIModelsPanelFetchIsConcurrent 守住两个模型端点**不是**串行 await。
//
// 判据（都在 loadModelsPanel 的函数体上）：
//
//	① 两路取数出现在**同一个** `Promise.all([…])` 的参数列表里
//	② 没有 `await loadModelMultipliers()`
//	③ 没有 `renderModels((await api('/v1/models')).data)`
func TestWebUIModelsPanelFetchIsConcurrent(t *testing.T) {
	src, err := readWebUIHTML()
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}
	if len(strings.TrimSpace(src)) == 0 {
		t.Fatal("webui.html 读到了空内容 —— 守卫失效（fail-open）")
	}
	code := stripJSCommentsDeep(src)

	body, ok := extractJSFunction(code, "async function loadModelsPanel(")
	if !ok {
		t.Fatal("找不到 loadModelsPanel 函数 —— 守卫无法定位（fail-open）；" +
			"如果它被改名/拆分了，本守卫的判据也要跟着改，而不是让它空转")
	}
	if len(body) > 8000 {
		t.Fatalf("切出来的函数体过大（共 %d 字节）—— 切分逻辑已失效，本守卫此时无论绿红都不可信", len(body))
	}

	for _, p := range modelsPanelSerialProblems(body) {
		t.Errorf("%s", p)
	}
}

// TestWebUIModelsPanelDoesNotBlockRefresh 守住模型面板**不阻塞** refresh()。
//
// 判据（都在 refresh 的函数体上）：
//
//	① 调用了 loadModelsPanel()（面板仍会被刷新）
//	② **没有** await 它 —— refresh() 先完成卡片/账号/manifest 等关键面板
//	③ 没有退回 `await loadModelMultipliers()` / `try { renderModels((await api('/v1/models')).data); }`
//	④ 没有绕开 loadModelsPanel 直接 `api('/v1/models')`
func TestWebUIModelsPanelDoesNotBlockRefresh(t *testing.T) {
	src, err := readWebUIHTML()
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}
	if len(strings.TrimSpace(src)) == 0 {
		t.Fatal("webui.html 读到了空内容 —— 守卫失效（fail-open）")
	}
	code := stripJSCommentsDeep(src)

	body, ok := extractJSFunction(code, "async function refresh()")
	if !ok {
		t.Fatal("找不到 refresh 函数 —— 守卫无法定位（fail-open）；" +
			"如果它被改名/拆分了，本守卫的判据也要跟着改，而不是让它空转")
	}
	// fail-closed 自检：不能串到相邻函数（loadModelsPanel 紧挨在 refresh 前面）。
	if strings.Contains(body, "async function loadModelsPanel(") {
		t.Fatalf("切出来的 refresh 串到了 loadModelsPanel（共 %d 字节）—— 切分逻辑已失效", len(body))
	}

	for _, p := range refreshNonBlockingProblems(body) {
		t.Errorf("%s", p)
	}
}

// TestWebUIModelsPanelDegradesLocally 守住失败降级**仍在**且只影响模型面板。
//
// 判据见 modelsPanelDegradeProblems（5 条，全部结构判据）。
func TestWebUIModelsPanelDegradesLocally(t *testing.T) {
	src, err := readWebUIHTML()
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}
	code := stripJSCommentsDeep(src)

	body, ok := extractJSFunction(code, "async function loadModelsPanel(")
	if !ok {
		t.Fatal("找不到 loadModelsPanel 函数 —— 降级判据无法定位（fail-open）")
	}

	for _, p := range modelsPanelDegradeProblems(body) {
		t.Errorf("%s", p)
	}
}

// TestWebUIModelsPanelSeqGuardsStaleWrite 守住"迟到的旧一轮不许覆盖新一轮"。
//
// # 为什么必须有它（不阻塞之后才出现的新风险）
//
// `loadModelsPanel()` 不阻塞 refresh() → 两轮可以同时在飞（启动那轮还没回来，
// 用户就点了「立即刷新」）。旧一轮若后到，把新一轮渲染好的目录覆盖回旧数据 ——
// 用户看到的是"刷新了，然后自己变回旧的"，而且**没有任何报错**。
//
// 判据见 modelsPanelStaleWriteProblems。
func TestWebUIModelsPanelSeqGuardsStaleWrite(t *testing.T) {
	src, err := readWebUIHTML()
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}
	code := stripJSCommentsDeep(src)

	for _, p := range modelsPanelStaleWriteProblems(code) {
		t.Errorf("%s", p)
	}
}

// --------------------------------------------------------------------------
// 反向验证：守卫必须抓得住"回退到串行阻塞"的真实写法
// --------------------------------------------------------------------------

// TestWebUIModelsPanelGuardCatchesRevertedCode 用**回退前的真实代码形态**
// 喂给上面四组判据，确认它们报违规；同时确认干净样本不报（否则会退化成噪音）。
//
// # 为什么守卫本身也需要被验证
//
// 一条抓不住回退的守卫 = 装饰品。与 webui_models_unified_test.go 的
// TestWebUIModelIDsUnifiedGuardCatchesRevertedCode、
// webui_endpoint_guard_test.go 的 TestWebUIEndpointGuardCatchesInjectedDefect
// 同一条理由：用**与真实缺陷逐字同形**的样本喂给判据。
//
// ⚠ 脏样本都不是编的：
//
//	脏样本 refresh   —— 本次改动**消灭前的原文**（webui.html 改动前 refresh 尾部，逐字）
//	脏样本 panel     —— 把两路改回串行 await（本次变异验证的第 1 刀）
//	脏样本 half      —— 并行只做了一半（最慢的倍率留在 Promise.all 之外）
//	脏样本 no-catch  —— 并行了但没有逐路失败分支
//	脏样本 no-seq    —— 去掉了陈旧回填守卫
func TestWebUIModelsPanelGuardCatchesRevertedCode(t *testing.T) {
	// ---- 脏样本 1：refresh() 里的串行 await（本次消灭的原文） ----------------
	const dirtyRefresh = "" +
		"  async function refresh() {\n" +
		"    renderCards(h, s);\n" +
		"    try { renderAccounts((await admin('/admin/accounts')).accounts); }\n" +
		"    catch { renderAccounts(s.accounts); }\n" +
		"    await loadModelMultipliers();\n" +
		"    try { renderModels((await api('/v1/models')).data); }\n" +
		"    catch (e) { $('models').innerHTML = `<span class=\"err\">${esc(e.message)}</span>`; }\n" +
		"    refresh._last = { healthy: h.healthy || 0, total: h.total || 0 };\n" +
		"    return true;\n" +
		"  }\n"
	got := refreshNonBlockingProblems(dirtyRefresh)
	if len(got) == 0 {
		t.Fatal("守卫抓不住『refresh 里串行 await 模型端点』——它是装饰品（本用例证明判据是假绿的）")
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{"await loadModelMultipliers()", "api('/v1/models')", "loadModelsPanel()"} {
		if !strings.Contains(joined, want) {
			t.Errorf("回退样本被报了，但没报出 %q 这一条 —— 报的是别的问题。守卫输出：\n%s", want, joined)
		}
	}

	// ---- 脏样本 1b：只把新增的不阻塞调用改回 await（变异验证那一刀） --------
	// 函数还在、并行还在，只把调用点加回 await —— 这是最小回退形态，
	// 也是最可能真发生的那种（下一个人觉得"不 await 不放心"）。
	const dirtyAwaitPanel = "" +
		"  async function refresh() {\n" +
		"    renderAccounts(s.accounts);\n" +
		"    await loadModelsPanel();\n" +
		"    refresh._last = { healthy: h.healthy || 0, total: h.total || 0 };\n" +
		"    return true;\n" +
		"  }\n"
	gotAwait := refreshNonBlockingProblems(dirtyAwaitPanel)
	if len(gotAwait) == 0 {
		t.Fatal("守卫抓不住『refresh 里 await loadModelsPanel()』——这恰恰是变异验证那一刀" +
			"（把不阻塞改回串行），抓不住它等于本守卫守不住交付物")
	}
	if !strings.Contains(strings.Join(gotAwait, "\n"), "await loadModelsPanel()") {
		t.Errorf("回退样本被报了，但没报出 `await loadModelsPanel()` 这一条。守卫输出：\n%s",
			strings.Join(gotAwait, "\n"))
	}

	// ---- 干净样本 1：本次改动后的真实写法 --------------------------------
	const cleanRefresh = "" +
		"  async function refresh() {\n" +
		"    renderCards(h, s);\n" +
		"    try { renderAccounts((await admin('/admin/accounts')).accounts); }\n" +
		"    catch { renderAccounts(s.accounts); }\n" +
		"    loadModelsPanel();\n" +
		"    refresh._last = { healthy: h.healthy || 0, total: h.total || 0 };\n" +
		"    return true;\n" +
		"  }\n"
	if got := refreshNonBlockingProblems(cleanRefresh); len(got) != 0 {
		t.Fatalf("守卫对修复后的 refresh 写法误报（会变成噪音，让人不再看它的输出）：\n%s",
			strings.Join(got, "\n"))
	}

	// ---- 脏样本 2：loadModelsPanel 内部退回串行（变异验证第 1 刀的真实形态） --
	const dirtyPanel = "" +
		"  async function loadModelsPanel() {\n" +
		"    const seq = ++modelsPanelSeq;\n" +
		"    $('mcount').innerHTML = '';\n" +
		"    $('models').innerHTML = '<span class=\"dim\">载入模型目录…</span>';\n" +
		"    await loadModelMultipliers();\n" +
		"    try { renderModels((await api('/v1/models')).data); }\n" +
		"    catch (e) { $('models').innerHTML = `<span class=\"err\">${esc(e.message)}</span>`; }\n" +
		"    if (seq !== modelsPanelSeq) return;\n" +
		"  }\n"
	got2 := modelsPanelSerialProblems(dirtyPanel)
	if len(got2) == 0 {
		t.Fatal("守卫抓不住『loadModelsPanel 里两路串行』——它是装饰品")
	}
	joined2 := strings.Join(got2, "\n")
	for _, want := range []string{"Promise.all", "await loadModelMultipliers()"} {
		if !strings.Contains(joined2, want) {
			t.Errorf("回退样本被报了，但没报出 %q 这一条 —— 报的是别的问题。守卫输出：\n%s", want, joined2)
		}
	}

	// ---- 脏样本 2b：并行只做了一半（倍率留在 Promise.all 之外） ------------
	// 形态：目录进了 Promise.all，倍率却仍被串行 await 在它前面。
	// 只查"有没有 Promise.all"会被它糊过去 —— 这正是本判据要能抓住的假绿。
	const dirtyHalfParallel = "" +
		"  async function loadModelsPanel() {\n" +
		"    await loadModelMultipliers();\n" +
		"    const listP = api('/v1/models').then(r => ({ ok: true, data: r && r.data }), e => ({ ok: false }));\n" +
		"    const [, list] = await Promise.all([Promise.resolve(), listP]);\n" +
		"    if (!list.ok) { $('models').innerHTML = `<span class=\"err\">${esc('x')}</span>`; return; }\n" +
		"    try { renderModels(list.data); } catch (e) { $('models').innerHTML = esc(e.message); }\n" +
		"  }\n"
	gotHalf := modelsPanelSerialProblems(dirtyHalfParallel)
	if len(gotHalf) == 0 {
		t.Fatal("守卫抓不住『并行只做了一半』（倍率仍串行 await）——它会假绿：" +
			"文件里确实有 Promise.all，但最慢的那个端点还在关键路径上")
	}
	if !strings.Contains(strings.Join(gotHalf, "\n"), "loadModelMultipliers()") {
		t.Errorf("半并行样本没有被报出『Promise.all 里没有 loadModelMultipliers()』。守卫输出：\n%s",
			strings.Join(gotHalf, "\n"))
	}

	// ---- 干净样本 2：本次改动后的真实写法 --------------------------------
	const cleanPanel = "" +
		"  async function loadModelsPanel() {\n" +
		"    const seq = ++modelsPanelSeq;\n" +
		"    $('mcount').innerHTML = '';\n" +
		"    $('models').innerHTML = '<span class=\"dim\">载入模型目录…</span>';\n" +
		"    const [, list] = await Promise.all([\n" +
		"      loadModelMultipliers(),\n" +
		"      api('/v1/models')\n" +
		"        .then(r => ({ ok: true, data: r && r.data }), e => ({ ok: false, err: e })),\n" +
		"    ]);\n" +
		"    if (seq !== modelsPanelSeq) return;\n" +
		"    if (!list.ok) { $('models').innerHTML = `<span class=\"err\">${esc(list.err.message)}</span>`; return; }\n" +
		"    try { renderModels(list.data); } catch (e) { $('models').innerHTML = `<span class=\"err\">${esc(e.message)}</span>`; }\n" +
		"  }\n"
	if got := modelsPanelSerialProblems(cleanPanel); len(got) != 0 {
		t.Fatalf("守卫对修复后的 loadModelsPanel 写法误报（会变成噪音）：\n%s", strings.Join(got, "\n"))
	}

	// ---- 反向验证降级判据：并行了但没有逐路失败分支 ------------------------
	// 脏样本与干净样本只差"目录那一路有没有失败分支 + 落点"，
	// 这样这条用例真正证明的是**降级判据**（而不是别的问题在报红）。
	const dirtyNoCatch = "" +
		"  async function loadModelsPanel() {\n" +
		"    const [, list] = await Promise.all([\n" +
		"      loadModelMultipliers(),\n" +
		"      api('/v1/models').then(r => ({ ok: true, data: r && r.data })),\n" +
		"    ]);\n" +
		"    try { renderModels(list.data); }\n" +
		"    catch (e) { $('models').innerHTML = `<span class=\"err\">${esc(e.message)}</span>`; }\n" +
		"  }\n"
	gotNoCatch := modelsPanelDegradeProblems(dirtyNoCatch)
	if len(gotNoCatch) == 0 {
		t.Fatal("守卫抓不住『目录那一路没有失败分支』——它是装饰品。" +
			"漏了它，/v1/models 一失败就是未捕获的 Promise rejection（refresh 故意不 await）")
	}
	if !strings.Contains(strings.Join(gotNoCatch, "\n"), "失败分支") {
		t.Errorf("回退样本被报了，但没报出『找不到失败分支』这一条。守卫输出：\n%s",
			strings.Join(gotNoCatch, "\n"))
	}

	// ---- 反向验证降级判据：失败外溢到别的面板 ------------------------------
	const dirtySpill = "" +
		"  async function loadModelsPanel() {\n" +
		"    const [, list] = await Promise.all([\n" +
		"      loadModelMultipliers(),\n" +
		"      api('/v1/models')\n" +
		"        .then(r => ({ ok: true, data: r && r.data }), e => ({ ok: false, err: e })),\n" +
		"    ]);\n" +
		"    if (!list.ok) { $('accts').innerHTML = `<span class=\"err\">${esc(list.err.message)}</span>`; return; }\n" +
		"    $('models').innerHTML = '';\n" +
		"    try { renderModels(list.data); } catch (e) { $('cards').innerHTML = esc(e.message); }\n" +
		"  }\n"
	gotSpill := modelsPanelDegradeProblems(dirtySpill)
	if len(gotSpill) == 0 {
		t.Fatal("守卫抓不住『失败外溢到别的面板』——它是装饰品。" +
			"漏了它，一个慢/挂的模型端点会把账号表一起改写，用户读成『整个控制台坏了』")
	}
	if !strings.Contains(strings.Join(gotSpill, "\n"), "外溢") {
		t.Errorf("回退样本被报了，但没报出『失败外溢』这一条。守卫输出：\n%s",
			strings.Join(gotSpill, "\n"))
	}

	// 干净样本 3：本组判据对上面的 cleanPanel 不报（否则会变成噪音）。
	if got := modelsPanelDegradeProblems(cleanPanel); len(got) != 0 {
		t.Fatalf("降级判据对修复后的写法误报（会变成噪音）：\n%s", strings.Join(got, "\n"))
	}

	// ---- 反向验证降级判据：#mcount 的清理写在了取数**之后** ----------------
	//
	// 这是最隐蔽的一种回退：清 #mcount 看起来"更整洁"（把上一轮的结论清掉），
	// 但排到取数之后就会把 `loadModelMultipliers()` **同步**挂上的
	//「倍率目录未加载」警告当场擦掉 —— 面板顶部一条提示都没有。
	const dirtyLateClear = "" +
		"  async function loadModelsPanel() {\n" +
		"    $('models').innerHTML = '<span class=\"dim\">载入模型目录…</span>';\n" +
		"    const [, list] = await Promise.all([\n" +
		"      loadModelMultipliers(),\n" +
		"      api('/v1/models')\n" +
		"        .then(r => ({ ok: true, data: r && r.data }), e => ({ ok: false, err: e })),\n" +
		"    ]);\n" +
		"    $('mcount').innerHTML = '';\n" +
		"    if (!list.ok) { $('models').innerHTML = `<span class=\"err\">${esc(list.err.message)}</span>`; return; }\n" +
		"    try { renderModels(list.data); } catch (e) { $('models').innerHTML = `<span class=\"err\">${esc(e.message)}</span>`; }\n" +
		"  }\n"
	gotLateClear := modelsPanelDegradeProblems(dirtyLateClear)
	if len(gotLateClear) == 0 {
		t.Fatal("守卫抓不住『#mcount 的清理排在取数之后』——它是装饰品。" +
			"漏了它，倍率目录整体失败时的面板级警告会被当场擦掉，用户看到满屏模型却没有倍率")
	}
	if !strings.Contains(strings.Join(gotLateClear, "\n"), "倍率目录未加载") {
		t.Errorf("回退样本被报了，但没报出『清理排在取数之后会擦掉警告』这一条。守卫输出：\n%s",
			strings.Join(gotLateClear, "\n"))
	}

	// ---- 反向验证陈旧回填判据：把守卫整条删掉 ------------------------------
	const dirtyNoSeq = "" +
		"  let modelsPanelSeq = 0;\n" +
		"  async function loadModelsPanel() {\n" +
		"    $('mcount').innerHTML = '';\n" +
		"    $('models').innerHTML = '<span class=\"dim\">载入模型目录…</span>';\n" +
		"    const [, list] = await Promise.all([\n" +
		"      loadModelMultipliers(),\n" +
		"      api('/v1/models')\n" +
		"        .then(r => ({ ok: true, data: r && r.data }), e => ({ ok: false, err: e })),\n" +
		"    ]);\n" +
		"    if (!list.ok) { $('models').innerHTML = `<span class=\"err\">${esc(list.err.message)}</span>`; return; }\n" +
		"    try { renderModels(list.data); } catch (e) { $('models').innerHTML = `<span class=\"err\">${esc(e.message)}</span>`; }\n" +
		"  }\n"
	gotNoSeq := modelsPanelStaleWriteProblems(dirtyNoSeq)
	if len(gotNoSeq) == 0 {
		t.Fatal("守卫抓不住『陈旧回填守卫被删掉』——它是装饰品。" +
			"漏了它，迟到的旧一轮会静默覆盖新一轮的目录（界面『刷新完又变回旧的』且不报错）")
	}
	if !strings.Contains(strings.Join(gotNoSeq, "\n"), "陈旧回填守卫") {
		t.Errorf("回退样本被报了，但没报出『找不到陈旧回填守卫』这一条。守卫输出：\n%s",
			strings.Join(gotNoSeq, "\n"))
	}

	// ---- 反向验证陈旧回填判据：守卫写在了回填**之后**（顺序反了） -----------
	const dirtyLateGuard = "" +
		"  let modelsPanelSeq = 0;\n" +
		"  async function loadModelsPanel() {\n" +
		"    const seq = ++modelsPanelSeq;\n" +
		"    const [, list] = await Promise.all([\n" +
		"      loadModelMultipliers(),\n" +
		"      api('/v1/models')\n" +
		"        .then(r => ({ ok: true, data: r && r.data }), e => ({ ok: false, err: e })),\n" +
		"    ]);\n" +
		"    $('mcount').innerHTML = '';\n" +
		"    $('models').innerHTML = '';\n" +
		"    try { renderModels(list.data); }\n" +
		"    catch (e) { $('models').innerHTML = `<span class=\"err\">${esc(e.message)}</span>`; }\n" +
		"    if (seq !== modelsPanelSeq) return;\n" +
		"  }\n"
	gotLate := modelsPanelStaleWriteProblems(dirtyLateGuard)
	if len(gotLate) == 0 {
		t.Fatal("守卫抓不住『陈旧回填守卫排在写 DOM 之后』——它是装饰品。" +
			"漏了它，旧一轮已经写进界面了，return 只能省下剩下的写入（界面半旧半新）")
	}
	if !strings.Contains(strings.Join(gotLate, "\n"), "排在陈旧守卫") {
		t.Errorf("回退样本被报了，但没报出『写 DOM 排在守卫之前』这一条。守卫输出：\n%s",
			strings.Join(gotLate, "\n"))
	}

	// ---- 干净样本 4：改动后的真实写法对陈旧回填判据不报 --------------------
	const cleanPanelWithSeq = "" +
		"  let modelsPanelSeq = 0;\n" +
		cleanPanel // 它自带 seq / 守卫 / 顺序
	if got := modelsPanelStaleWriteProblems(cleanPanelWithSeq); len(got) != 0 {
		t.Fatalf("陈旧回填判据对修复后的写法误报（会变成噪音）：\n%s", strings.Join(got, "\n"))
	}
}
