//go:build windows

package desktop

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	webview2 "github.com/jchv/go-webview2"
)

// TestWebView2VisibilityOnHide 探针：窗口被 SW_HIDE 之后，页面里
// document.visibilityState / document.hidden 到底是什么；以及
// **本功能真正发给页面的那段 JS** 能不能把它们钉成 hidden。
//
// # 为什么是"人工探针"而不是常规单测
//
// 它要开一个真的 WebView2 窗口、依赖交互式桌面会话。放进常规 `go test` 会
// 在无头环境/CI 上弹窗或挂住，所以默认 skip，只在显式设了 OMNI2API_GUI_PROBE=1
// 时跑。它存在的意义只有一个：给「隐藏期间前端还在不在轮询」这个结论提供
// **实测证据**，而不是猜。
//
// # 实测结论（2026-10，WebView2 运行时本机版本）
//
//  1. 只调 ShowWindow(SW_HIDE) 隐藏宿主窗口，页面里 visibilityState 仍是
//     "visible"、hidden 仍是 false —— WebView2 不把宿主窗口的可见性传给
//     Page Visibility API。
//  2. 所以"最小化到托盘"如果什么都不做，页面会在后台继续全速轮询/保持 SSE，
//     与"省资源"的意图正好相反。
//  3. 发本仓库 desktop 侧的 jsPageVisibility（遮蔽 document.hidden /
//     visibilityState 的 getter + 派发 visibilitychange）之后，页面确实读到
//     hidden，并且可以 delete 还原回 visible。
func TestWebView2VisibilityOnHide(t *testing.T) {
	if os.Getenv("OMNI2API_GUI_PROBE") == "" {
		t.Skip("人工探针：设 OMNI2API_GUI_PROBE=1 才跑（要开真窗口 + 桌面会话）")
	}

	w := webview2.NewWithOptions(webview2.WebViewOptions{
		AutoFocus: false,
		// 每次跑用独立 profile：默认值（%AppData%\<测试exe名>）会被上一轮
		// 残留的 msedgewebview2 进程占着，导致环境创建失败（go-webview2 在
		// 这条路径上直接 panic，看起来像"测试本身坏了"）。
		DataPath:      filepath.Join(t.TempDir(), "webview2"),
		WindowOptions: webview2.WindowOptions{Title: "visibility probe", Width: 360, Height: 200},
	})
	if w == nil {
		t.Fatal("WebView2 窗口创建失败（运行时缺失？）")
	}
	defer w.Destroy()

	states := make(chan string, 16)
	// go-webview2 这个版本把**第一个实参原样**递进来（不是文档里写的
	// JSON 数组信封），所以这里既不当数组也不当 JSON 解析。
	if err := w.Bind("probe", func(req string) error { states <- req; return nil }); err != nil {
		t.Fatalf("Bind 失败：%v", err)
	}

	hwnd := uintptr(w.Window())
	watchdog := time.AfterFunc(30*time.Second, func() {
		t.Log("看门狗：消息循环 30 秒未返回，强制关窗")
		w.Destroy()
	})
	w.Navigate(`data:text/html,<html><body style="font:14px sans-serif">visibility probe</body></html>`)

	const probeJS = `probe(document.visibilityState + " / hidden=" + document.hidden)`
	hideJS := fmt.Sprintf(jsPageVisibility, true)
	showJS := fmt.Sprintf(jsPageVisibility, false)

	// step 把一串"排到 UI 线程执行的动作"串起来：Eval 是异步的，中间必须留
	// 时间给页面把 probe() 回调送回来。
	steps := []struct {
		what string
		do   func()
	}{
		{"可见态基线", func() { w.Eval(probeJS) }},
		{"SW_HIDE 隐藏宿主窗口（不发任何 JS）", func() {
			showWindow(hwnd, swHide)
		}},
		{"隐藏后原样采样", func() { w.Eval(probeJS) }},
		{"发 desktop 侧 jsPageVisibility(true)", func() { w.Eval(hideJS) }},
		{"钉成 hidden 后采样", func() { w.Eval(probeJS) }},
		{"SW_SHOW 恢复窗口", func() { showWindow(hwnd, swShow) }},
		{"发 jsPageVisibility(false) 还原", func() { w.Eval(showJS) }},
		{"还原后采样", func() { w.Eval(probeJS) }},
	}

	go func() {
		time.Sleep(1500 * time.Millisecond)
		for _, s := range steps {
			step := s
			w.Dispatch(step.do)
			time.Sleep(900 * time.Millisecond)
			t.Logf("── %s", step.what)
		}
		w.Dispatch(func() { w.Terminate() })
	}()

	w.Run()

	// 看门狗：消息循环偶尔会因为 WebView2 启动失败而不返回（第三方包的行为），
	// 那样 `go test` 会一直挂着。到点强制关窗，让测试以失败/通过收场。
	watchdog.Stop()

	// 不 close(states)：Bind 的回调跑在 WebView2 的消息线程上，关窗后仍可能有
	// 一次尾包进来，close 会让它 panic（"send on closed channel"）。改成定时排空。
	var got []string
	for done := false; !done; {
		select {
		case s := <-states:
			got = append(got, s)
		case <-time.After(700 * time.Millisecond):
			done = true
		}
	}
	if len(got) == 0 {
		t.Fatal("页面一次都没回报（Bind 没生效？）")
	}
	for i, s := range got {
		t.Logf("采样 %d：%s", i+1, s)
	}
}
