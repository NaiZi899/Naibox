package server

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 源码级守卫：任务进度轮询不得再整页刷新
// ---------------------------------------------------------------------------
//
// # 这条守卫守的是什么事故
//
// `watchTask` 的 tick 在任务运行期间每 2 秒跑一次，曾经直接调 `refresh()`。
// 而 refresh() 里包含三件在 2 秒粒度上完全不该发生的事：
//
//	① 重拉 manifest → 重建导航与上游面板；
//	② 触发 loadModelsPanel() → 并发打 /admin/models/preview 与 /v1/models
//	   （全页最慢的两个端点，前者实测 max 27s）；
//	③ 无条件 await loadClientLogin() → 后端探测客户端进程。
//	   桌面壳是 -H windowsgui 子系统，探测用的 tasklist.exe 每次都会新建
//	   一个控制台 → 用户看到终端窗口每隔 ~3 秒闪一次。
//
// 20 分钟的任务 ≈ 600 轮：约 6000 个管理请求、约 1200 次进程探测。
// 用户报的是"窗口一直闪"，代价远不止观感。
//
// # 为什么做成源码断言而不是行为测试
//
// 行为上"每 2 秒没有整页刷新"要跑真浏览器 + 网络采样才能观测，而那套前端
// 探针不在版本库里（tests/ 已撤下）。这条判据的形态很稳定（一个 if 分支里
// 不许出现 refresh()），源码断言足以拦住"照着旁边旧代码抄回去"这种复发。
func TestWatchTaskDoesNotFullRefresh(t *testing.T) {
	src, err := readWebUIHTML()
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}
	body := sliceJSFunction(t, src, "function watchTask()")

	start := strings.Index(body, "if (t.running) {")
	if start < 0 {
		t.Fatal("watchTask 里找不到 `if (t.running) {` 分支 —— 守卫失去着力点")
	}
	returnAt := strings.Index(body[start:], "return;")
	if returnAt < 0 {
		t.Fatal("`if (t.running)` 分支里找不到 return; —— 结构已变")
	}
	running := stripJSComments(body[start : start+returnAt])

	if strings.Contains(running, "refresh()") {
		t.Error("任务运行期间的分支又调用了整页 refresh()：2 秒一轮会拉起模型目录与进程探测")
	}
	if !strings.Contains(running, "pollPoolValues()") {
		t.Error("任务运行期间应改用 pollPoolValues() 做数值对齐（否则额度/状态不再刷新）")
	}
}

// TestRefreshGatesClientLogin refresh() 只在成长面板可见时拉本机登录状态。
//
// loadClientLogin 是"会 spawn 探测"的端点，挂在整页刷新主路径上时，
// 任何一次刷新都变成一次进程探测。判据：它前面必须有 growthPanelVisible()。
func TestRefreshGatesClientLogin(t *testing.T) {
	src, err := readWebUIHTML()
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}
	if !strings.Contains(src, "function growthPanelVisible()") {
		t.Fatal("growthPanelVisible 不存在 —— 判据已丢失")
	}
	body := sliceJSFunction(t, src, "async function refresh()")

	at := strings.Index(body, "await loadClientLogin()")
	if at < 0 {
		t.Fatal("refresh 里找不到 loadClientLogin —— 守卫失去着力点")
	}
	head := body[:at]
	gate := strings.LastIndex(head, "if (growthPanelVisible())")
	if gate < 0 {
		t.Fatal("refresh 里的 loadClientLogin 没有 visible 门控：每次整页刷新都会探测进程")
	}
	// 门控与调用之间只允许有 `{ try` 这类过渡，不允许再出现别的语句把它隔开。
	between := strings.TrimSpace(head[gate:])
	want := "if (growthPanelVisible()) {"
	if !strings.HasPrefix(between, want) {
		t.Errorf("门控结构不符，期望以 %q 开头，实际：%q", want, between)
	}
	if strings.Contains(head[gate+len(want):], "}") {
		// `}` 出现在门控与调用之间 = 调用落在了 if 外面。
		t.Error("loadClientLogin 落在 if 块之外：门控没有生效")
	}
}

// sliceJSFunction 截出某个顶层函数的函数体。
//
// 约定：页面的顶层函数都以两个空格缩进 + `function ` 开头（或在 const 里
// 用 `function name(`），因此从下一处 "\n  function " 截断即可。找不到结尾
// 时返回剩余全文 —— 断言仍然成立，不会因为格式微调而假装通过。
func sliceJSFunction(t *testing.T, src, header string) string {
	t.Helper()
	start := strings.Index(src, header)
	if start < 0 {
		t.Fatalf("webui.html 里找不到 %q", header)
	}
	rest := src[start+len(header):]
	if i := strings.Index(rest, "\n  function "); i >= 0 {
		return rest[:i]
	}
	return rest
}
