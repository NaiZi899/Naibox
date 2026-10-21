package server

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 源码级守卫：「自定义上游」管理页必须在，且接线方式不退化
// ---------------------------------------------------------------------------
//
// # 这条守卫守的是什么 bug
//
// 自定义上游（OpenAI 兼容端点）是**后端加、前端几乎零逻辑**的一块：
// 三个端点（GET / PUT / POST test）、一张表、一个内联编辑器。它最容易出的
// 不是"写错渲染"，而是下面这四类**沉默**的退化 —— 每一类都能正常点、
// 不报错，只是不再工作：
//
//	① section 少了 data-key="custom-upstreams"
//	   导航由 `#content > section[data-key]` 生成，showPanel 按 data-key 分派。
//	   少了它：面板在，导航里没有 → 用户永远打不开（"加了个功能但看不见"）。
//
//	② showPanel 里的 `if (key === 'custom-upstreams') loadCustomUpstreams();` 掉了
//	   面板能打开、表格永远是「加载中…」。本项目有过同款：
//	   PROVIDER_PANEL_RENDERERS 漏登记时面板停在「加载中…」（见那里的注释）。
//
//	③ PUT / POST 的方法串被写错（'put' / 默认 GET）
//	   admin() 带 Content-Type，GET 打 PUT 端点 → 405，而前端只在 toast 里
//	   说一句失败。这条断言把「写操作确实声明了方法」钉住。
//
//	④ 行内按钮改成"每行绑一次"或滚动改用 scrollIntoView
//	   前者随每次渲染累积监听器（同一个动作点一次触发多次，本项目踩过），
//	   后者会连带滚动最近的可滚动祖先，把表头推走、落点看不见标题。
//
// # 为什么断言用"结构切片"而不是大括号配对
//
// 这段代码里有**嵌套模板字符串**：
//
//	`… ${it.key_tail ? `<div …>${esc(it.key_tail)}</div>` : ''} …`
//
// 一个半吊子的花括号扫描器会在嵌套反引号上数错；而数错的切片**仍可能
// 通过 contains 断言** —— 那是最坏的一种守卫：看起来在守，实际在自证。
// 所以这里用两个结构性锚点切出这一段（状态变量声明 → 下一节的分隔注释），
// 锚点找不到就 Fatal（守卫被搬走/改名必须让人看见，不能回落成全文匹配，
// 否则 `method: 'PUT'` 会命中设置页的保存函数，永远绿）。
//
// # 这里为什么读源码文件
//
// 与 readWebUIHTML 的注释同一条理由：section、data-key、方法串都只在
// 源文件里有意义。读内嵌变量会让"改了文件忘了同步"这类漂移漏网。

// webUICustomBlock 切出 webui.html 里「自定义上游」那段 JS。
func webUICustomBlock(t *testing.T, src string) string {
	t.Helper()
	const start = "let customItems = []"
	// 下一节的分隔注释。它同时是"这段代码到哪儿结束"的边界 ——
	// 用分隔注释而不是"最后一行"，是因为这段以后可能继续加函数。
	const end = "// ---------------------------------------------------------------- 成长计划"

	i := strings.Index(src, start)
	if i < 0 {
		t.Fatalf("webui.html 里找不到 %q —— 自定义上游那段 JS 被搬走或改名了；"+
			"请同步更新本守卫的锚点（不要改成全文匹配，那会让断言永远绿）", start)
	}
	j := strings.Index(src[i:], end)
	if j < 0 {
		t.Fatalf("webui.html 里找不到本节结束锚点 %q —— 请同步更新本守卫的锚点", end)
	}
	return src[i : i+j]
}

// webUIWS 去掉所有空白，用于"不因排版变化而红"的断言。
//
// 例：`{ method: 'PUT', … }` 与 `{method:'PUT',…}` 归一后都是 `{method:'PUT',…}`。
// 只用于短 token；不要拿它断言跨行文案（那会把可读性牺牲掉）。
func webUIWS(s string) string { return strings.Join(strings.Fields(s), "") }

// TestWebUICustomUpstreamsSection 面板骨架必须完整。
//
// 少了任何一项，用户看到的是"面板存在但没法用"：没有表 → 看不到已配的端点；
// 没有编辑器字段 → 新增表单填不全；没有 6 列表头 → 空态行 colspan 对不上、错位。
func TestWebUICustomUpstreamsSection(t *testing.T) {
	src, err := readWebUIHTML()
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}

	// ① data-key 必须在（导航与 showPanel 都按它分派）。
	const open = `<section data-key="custom-upstreams">`
	i := strings.Index(src, open)
	if i < 0 {
		t.Fatalf("找不到 %s —— 导航不会出现该面板，showPanel 也分派不到", open)
	}

	// 它在「设置」之后（本轮要求的落点：settings 之后、动态面板宿主之前）。
	if k := strings.Index(src, `data-key="settings"`); k < 0 || k > i {
		t.Errorf("自定义上游 section 不在设置之后（settings=%d, custom=%d）", k, i)
	}

	// ② 切出这个 section 本体（本段内没有嵌套 section，第一个 </section> 就是它的结尾）。
	rest := src[i:]
	if j := strings.Index(rest, "</section>"); j >= 0 {
		rest = rest[:j]
	} else {
		t.Fatalf("自定义上游 section 没有闭合")
	}
	sec := webUIWS(rest)

	wantIn := []struct {
		needle string
		why    string
	}{
		{"自定义上游", "面板标题"},
		{"保存后需重启网关", "标题上的生效时机提示（不写等于让人以为保存即生效）"},
		{`id="btnCustomAdd"`, "「新增上游」按钮"},
		{`id="customRows"`, "表格 tbody（行渲染与事件委托都挂在它上面）"},
		{`colspan="6"`, "空态/失败态占位行的跨列数（与 6 个表头对齐）"},
		{`id="customEditor"hidden`, "编辑器必须默认隐藏（点新增/编辑才展开）"},
		{`id="customEditorTitle"`, "编辑器标题（新增 / 编辑：name 两态）"},
		{`id="cfId"`, "ID 字段"},
		{`id="cfName"`, "显示名字段"},
		{`id="cfBase"`, "基址字段"},
		{`id="cfKey"`, "API Key 字段"},
		{`id="cfKeys"`, "多密钥字段（一行一个）"},
		{`id="cfKeyEnv"`, "api_key_env 字段（与 api_key 二选一的另一半）"},
		{`id="cfModels"`, "模型列表 textarea（一行一个）"},
		{`id="cfTimeout"`, "超时（秒）"},
		{`id="cfShards"`, "分片数"},
		{`id="cfAuthHeader"`, "auth_header"},
		{`id="cfAuthScheme"`, "auth_scheme"},
		{`id="cfHeaders"`, "附加请求头 textarea（一行一条 Key: Value）"},
		{`id="cfInsecure"`, "insecure_skip_verify"},
		{`id="cfEnabled"`, "enabled"},
		{`id="customTest"`, "「测试连通性」"},
		{`id="customSave"`, "「保存」"},
		{`id="customCancel"`, "「取消」"},
		// 三条要浮到界面上的规则文案（后端契约的投影，改了措辞要一起想清楚）。
		{"^[a-z][a-z0-9-]*$", "id 正则提示（它同时是模型前缀）"},
		{"api_key_env", "api_key / api_key_env 二选一（名字出现即说明提示在）"},
	}
	for _, w := range wantIn {
		if !strings.Contains(sec, w.needle) {
			t.Errorf("自定义上游面板缺少 %s（找不到 %q）", w.why, w.needle)
		}
	}

	// ③ 表头六列：改列数时这里会红，提示同步改占位行的 colspan。
	if n := strings.Count(rest, "<th>"); n != 6 {
		t.Errorf("表头列数 = %d，期望 6（名称 / ID、基址、模型、最近探测、状态、操作）", n)
	}

	// ④ API Key 必须是 password：本面板里只允许**一个**密码框（就是它）。
	// 写 type="text" 会让明文 key 直接显示在管理页上 —— 这是"残余风险最高"的一处。
	if n := strings.Count(sec, `type="password"`); n != 1 {
		t.Errorf("密码框数量 = %d，期望 1（只有 API Key 字段该是 password）", n)
	}
}

// TestWebUICustomUpstreamsEndpoints 三个端点的字面量与写方法都必须在。
//
// 这是"契约被悄悄写歪"的最小钉子：
//
//	· GET/PUT 是同一个路径（/admin/custom-upstreams），只靠 method 区分
//	· 草稿探测是子路径 /test
//
// 方法串用 ws 归一后断言（`method:'PUT'`）：只忽略排版，不忽略值 ——
// 写成 'put' 或漏掉 method（默认 GET）都会红。
func TestWebUICustomUpstreamsEndpoints(t *testing.T) {
	src, err := readWebUIHTML()
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}
	block := webUICustomBlock(t, src)
	ws := webUIWS(block)

	if !strings.Contains(block, "/admin/custom-upstreams/test") {
		t.Error("缺少草稿探测端点字面量 /admin/custom-upstreams/test")
	}
	// 列表/PUT 那条路径在代码里是 admin('/admin/custom-upstreams' —— 断言到引号处，
	// 避免被 /test 子路径顺带命中。
	if !strings.Contains(block, "/admin/custom-upstreams'") {
		t.Error("缺少列表/保存端点字面量 /admin/custom-upstreams")
	}
	if !strings.Contains(ws, "method:'PUT'") {
		t.Error("自定义上游的写操作没有声明 method: 'PUT'（默认 GET 打 PUT 端点会 405）")
	}
	if !strings.Contains(ws, "method:'POST'") {
		t.Error("自定义上游的探测没有声明 method: 'POST'")
	}
	// 函数名单独钉一遍：契约可以换实现，但这几个入口名是注释与测试共用的锚点。
	for _, fn := range []string{
		"function loadCustomUpstreams(",
		"function renderCustomUpstreams(",
		"function openCustomEditor(",
		"function closeCustomEditor(",
		"function collectCustomSpec(",
		"async function saveCustomUpstreams(",
		"async function testCustomDraft(",
	} {
		if !strings.Contains(block, fn) {
			t.Errorf("缺少 %q", fn)
		}
	}
}

// TestWebUICustomUpstreamsRestartNotice 保存回执必须说出"要重启"。
//
// 为什么值得一条守卫：这块写的是 config.json 的 custom_upstreams，进程内的
// 上游注册表在启动时构建 —— 保存成功但没重启，等于**什么都没发生**，
// 而界面已经说"已保存"。不说清重启，用户会以为功能坏了（或者以为已经生效，
// 再去 /v1/models 里找不到模型）。
func TestWebUICustomUpstreamsRestartNotice(t *testing.T) {
	src, err := readWebUIHTML()
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}
	ws := webUIWS(webUICustomBlock(t, src))
	if !strings.Contains(ws, "重启") {
		t.Error("保存成功的回执里没有「重启」字样 —— 用户会以为保存即生效")
	}
	// need_restart 是**后端说的**；前端不该自己编一份"哪些项要重启"。
	if !strings.Contains(ws, "need_restart") {
		t.Error("回执文案没有读 need_restart —— 重启清单必须来自后端，不能前端猜")
	}
	// 面板标题上也得有（不点保存也要能看见这件事）。
	if !strings.Contains(webUIWS(src), "保存后需重启网关") {
		t.Error("面板标题缺少「保存后需重启网关」提示")
	}
}

// TestWebUICustomUpstreamsShowPanelBranch 切面板必须按需加载。
//
// 面板能打开但永远「加载中…」，是本项目最典型的一类沉默缺陷
// （见 loadProviderPanel 与 buildProviderPanels 的注释）。
// 这条断言把"按 data-key 分派"钉在 showPanel 里面，而不是全文件任意位置。
func TestWebUICustomUpstreamsShowPanelBranch(t *testing.T) {
	src, err := readWebUIHTML()
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}
	i := strings.Index(src, "function showPanel(idx)")
	if i < 0 {
		t.Fatal("找不到 showPanel(idx)")
	}
	// 用下一个函数定义当右边界：showPanel 之后紧跟的是 loadProviderPanel
	//（它有完整注释块，但函数头的字面量是稳定的）。
	j := strings.Index(src[i:], "function loadProviderPanel(")
	if j < 0 {
		t.Fatal("找不到 loadProviderPanel —— showPanel 的右边界锚点失效，请同步本守卫")
	}
	panel := webUIWS(src[i : i+j])

	if !strings.Contains(panel, "'custom-upstreams'") {
		t.Error("showPanel 里没有 'custom-upstreams' 分支 —— 切到该面板不会加载数据")
	}
	if !strings.Contains(panel, "loadCustomUpstreams()") {
		t.Error("showPanel 的 custom-upstreams 分支没有调用 loadCustomUpstreams()")
	}
}

// TestWebUICustomUpstreamsWiring 接线方式不得退化。
//
// 三件事：
//  1. 四个按钮 + key 显隐**在 boot 里绑一次**（不随渲染绑 → 监听器不累积）
//  2. 行内动作只有**一处** addEventListener（委托），且委托函数只被调用一次
//  3. 滚动用 window.scrollTo，**不用** scrollIntoView（会连带滚动祖先）
func TestWebUICustomUpstreamsWiring(t *testing.T) {
	src, err := readWebUIHTML()
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}
	block := webUICustomBlock(t, src)
	// 与实现细节有关的判据一律在**剥掉注释**的代码上判：
	// 本段的注释必然会提到 scrollIntoView（解释为什么不用它），
	// 拿原文判会让守卫被自己的说明文字弄红，而下一个人会去删注释而不是改代码。
	code := webUIWS(stripJSComments(block))

	for _, w := range []string{
		`$('btnCustomAdd').onclick`,
		`$('customSave').onclick`,
		`$('customCancel').onclick`,
		`$('customTest').onclick`,
		`$('cfKeyToggle').onclick`,
	} {
		if !strings.Contains(webUIWS(src), webUIWS(w)) {
			t.Errorf("boot 里没有接线 %s", w)
		}
	}

	// 行内动作只允许一处监听（委托）。多出来的那处通常就是"顺手在渲染里绑一次"。
	if n := strings.Count(code, "addEventListener"); n != 1 {
		t.Errorf("自定义上游这段里 addEventListener 出现 %d 次，期望 1（只允许 customRows 上那一次委托）", n)
	}
	// 委托函数：1 次定义 + 1 次调用。变 3 次说明被挪进了渲染路径。
	// 判据带括号 —— 注释里提到函数名是正常的，提到 `名字()` 才是调用。
	if n := strings.Count(src, "installCustomUpstreamActions()"); n != 2 {
		t.Errorf("installCustomUpstreamActions() 出现 %d 次，期望 2（定义 + boot 里调用一次）；"+
			"多于 2 通常意味着被绑到了每次渲染上（监听器会累积）", n)
	}

	if strings.Contains(code, "scrollIntoView") {
		t.Error("自定义上游这段用了 scrollIntoView —— 会连带滚动最近的可滚动祖先，把表头推走；请用 window.scrollTo")
	}
	if !strings.Contains(code, "window.scrollTo") {
		t.Error("打开编辑器时没有滚动到表单（window.scrollTo）—— 低分辨率下编辑器可能在视口外")
	}
}

// TestWebUICustomRowAttrsEscapeQuotes 行内按钮的 data-* 必须走 escAttr。
//
// # 这条守的是什么
//
// 本文件的 esc() 只转 & < >（对文本内容够用），不转引号。而 id 会被塞进
// `data-cedit="${esc(id)}"` —— 一个带 `"` 的 id 就能截断属性并注入新属性。
// 停用条目不过后端校验（NormalizeAll 只校验 enabled 的条目），所以
// `{"id":"a\"><img onerror=...>","enabled":false}` 是能落盘的。
//
// escAttr 早就存在（title 属性在用），但行内按钮漏了它 —— 这类"工具写了
// 却没用上"的缺口无法靠渲染测试发现，只能在源码层钉住。
func TestWebUICustomRowAttrsEscapeQuotes(t *testing.T) {
	src, err := readWebUIHTML()
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}
	block := webUICustomBlock(t, src)
	for _, attr := range []string{"data-cedit", "data-ctest", "data-ctoggle", "data-cdel"} {
		if !strings.Contains(block, attr+`="${escAttr(`) {
			t.Errorf("%s 没有用 escAttr —— id 里的引号会截断属性（XSS/属性注入）", attr)
		}
		if strings.Contains(block, attr+`="${esc(`) {
			t.Errorf("%s 仍在用 esc（不转引号）", attr)
		}
	}
}
