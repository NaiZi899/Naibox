//go:build windows

package desktop

import (
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	webview2 "github.com/jchv/go-webview2"
)

// windowTitle 窗口标题。同时是第二实例 FindWindow 之外的人工识别标志。
const windowTitle = "NaiBox 控制台"

// webviewView go-webview2 的封装。
type webviewView struct {
	w      webview2.WebView
	hwnd   uintptr
	layout Layout
	store  *windowStore
	log    *log.Logger

	closeOnce sync.Once
	destroyed sync.Once

	// policy ConfigureClose 装进来的关窗策略。零值 = 不拦截（旧行为）。
	policy ClosePolicy
	// prevProc 子类化之前的窗口过程。**所有没处理的消息都要交回它**。
	prevProc uintptr
	// hidden 窗口当前是否藏在托盘里（SW_HIDE 之后）。
	hidden atomic.Bool
	// asking 询问框是否正开着（防止叠第二个对话框）。
	asking atomic.Bool
	// closeAskShown 页面内弹窗是否已经真的显示出来。
	// 关窗时先 Eval 让页面弹窗，若页面还没加载好，这个标志会一直是 false，
	// 超时后回退到原生 MessageBox（不能让用户点 X 没反应）。
	closeAskShown atomic.Bool
	// closeConfirmed 用户已经明确选择退出。重投 WM_CLOSE 时用它跳过询问，
	// 直接走原来的销毁路径（落盘/停机逻辑不绕过）。
	closeConfirmed atomic.Bool
	// closeAskReady 三个 JS 回调都绑定成功。绑定失败时不再尝试页面弹窗，
	// 直接走原生 MessageBox，避免"页面弹窗 + 超时兜底"同时出现两个框。
	closeAskReady atomic.Bool
}

// 子类化窗口过程的全局登记表。
//
// 为什么要全局：syscall.NewCallback 每调用一次就占一个 Windows 回调槽
// （上限约 2000，用完 panic），所以只能建**一个**回调，再按 hwnd 分发到
// 各自的 view。go-webview2 自己也是这个套路（windowContext map）。
var (
	subclassMu    sync.RWMutex
	subclassViews = map[uintptr]*webviewView{}
	subclassProc  = syscall.NewCallback(subclassWndProc)
)

// subclassWndProc 唯一的子类化回调。
func subclassWndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	subclassMu.RLock()
	v := subclassViews[hwnd]
	subclassMu.RUnlock()
	if v == nil {
		// 理论上到不了：说明窗口被销毁时没把登记摘干净。退回默认过程，
		// 绝不能返回 0 —— 那等于把这个窗口的所有消息都吞掉。
		return defaultWindowProc(hwnd, msg, wParam, lParam)
	}
	return v.handleWindowMessage(hwnd, msg, wParam, lParam)
}

// newView 建窗并导航到 url。
//
// 注意：WebView2 的 CreateWithOptions 内部对"拿不到 settings"调用的是
// log.Fatal（第三方包的，接不进我们的 Fatalf）。因此调用前必须先过
// canShowWindow 的运行时检测 —— 那条路径才是真正会失败的路径。
func newView(l Layout, url string, store *windowStore, logger *log.Logger) (WindowView, error) {
	st := store.Get()
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		// DataPath 放进数据根：WebView2 的 profile（cookie、缓存）会占几十 MB，
		// 丢在默认位置（<exe>\.webview 或 系统临时目录）既难找也难清。
		DataPath:  filepath.Join(l.DataDir, "webview2"),
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title:  windowTitle,
			Width:  uint(st.Width),
			Height: uint(st.Height),
			// 用户摆过窗口就别再居中，否则每次启动都跳回屏幕中间。
			Center: !st.HasPos,
		},
	})
	if w == nil {
		return nil, fmt.Errorf("WebView2 窗口创建失败（运行时可能已损坏，可尝试重装 WebView2 运行时）")
	}
	v := &webviewView{w: w, layout: l, store: store, log: logger}
	v.bindCloseAsk()
	v.hwnd = uintptr(w.Window())
	if st.HasPos {
		moveWindow(v.hwnd, st.X, st.Y)
	}
	// 上次是最大化就恢复最大化：否则会按普通窗口建一个"铺满屏幕还多一点"
	// 的窗口（最大化的几何比可见屏幕大，含边框），位置偏负又压住整屏。
	if st.Maximized {
		showWindow(v.hwnd, swMaximize)
	}
	w.Navigate(url)
	return v, nil
}

// Navigate 换地址。
func (v *webviewView) Navigate(url string) { v.w.Navigate(url) }

// eval 在页面里执行一段脚本（异步，不等待结果）。
func (v *webviewView) eval(js string) { v.w.Eval(js) }

// bindCloseAsk 把页面内关窗弹窗需要的三个回调注册到 JS。
//
//	closeChoice(choice)  用户选了 hide / quit / cancel
//	closeFallback()      页面里没有弹窗实现时，走原生 MessageBox
//	closeShown()         弹窗真的显示出来了（用于取消超时兜底）
//
// Bind 失败不致命：关窗超时兜底仍会弹原生框。
func (v *webviewView) bindCloseAsk() {
	ok := true
	if err := v.w.Bind("nbCloseChoice", v.onCloseChoice); err != nil {
		v.logf("绑定关窗弹窗回调失败（将回退原生对话框）：%v", err)
		ok = false
	}
	if err := v.w.Bind("nbCloseFallback", v.onCloseFallback); err != nil {
		v.logf("绑定关窗兜底回调失败（将回退原生对话框）：%v", err)
		ok = false
	}
	if err := v.w.Bind("nbCloseShown", func() { v.closeAskShown.Store(true) }); err != nil {
		v.logf("绑定关窗弹窗显示回调失败（超时兜底仍可用）：%v", err)
		ok = false
	}
	v.closeAskReady.Store(ok)
}

func (v *webviewView) onCloseChoice(choice string) {
	v.logf("关窗询问：页面选择 %s", choice)
	switch choice {
	case "hide":
		v.resolveCloseAsk(true)
	case "quit":
		v.resolveCloseAsk(false)
	default:
		v.cancelCloseAsk()
	}
}

func (v *webviewView) onCloseFallback() {
	v.fallbackCloseAsk()
}

// Show 提到前台；若窗口藏在托盘里就恢复出来。
func (v *webviewView) Show() {
	showAndFocus(v.hwnd)
	if v.hidden.Swap(false) {
		// 从托盘回来：把页面从"后台"语义里放出来（恢复轮询/SSE）。
		v.syncPageVisibility(false)
		v.logf("窗口已从托盘恢复")
	}
}

// Close 请求退出消息循环。
//
// Dispatch 把函数排到消息循环线程执行：PostQuitMessage 只作用于**调用它的
// 线程**，从别的 goroutine 直接调等于往空气里发消息（表现是"点退出没反应"）。
func (v *webviewView) Close() {
	v.closeOnce.Do(func() {
		v.w.Dispatch(func() { v.w.Terminate() })
		// 兜底：万一消息循环已经不在跑（Dispatch 的 PostMessage 无人处理），
		// 两秒后直接关窗，避免进程退不干净。
		time.AfterFunc(2*time.Second, func() { v.Destroy() })
	})
}

// Destroy 关窗（幂等）。
func (v *webviewView) Destroy() {
	v.destroyed.Do(func() {
		defer func() { _ = recover() }() // 已销毁时第三方包可能 panic，忽略
		v.w.Destroy()
	})
}

// Run 跑消息循环并周期性保存窗口位置。
//
// 为什么是轮询而不是"退出时读一次"：Run 返回时窗口往往已经被销毁，
// GetWindowRect 拿到的是垃圾值（或者直接失败）。轮询每 5 秒落一次盘，
// 用户摆好窗口后就算程序被强杀，下次也还在原位。
func (v *webviewView) Run() {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				v.snapshot()
			}
		}
	}()
	v.w.Run()
	close(done)
	v.snapshot()
}

// snapshot 读当前窗口几何并落盘。
//
// 只写几何：关窗行为/气泡标记由 windowStore 单独更新。旧版这里是
// "拼一个全新的 WindowState 写回"，那样会**顺手把关窗行为清空**（每 5 秒一次）。
//
// ⚠ 最大化状态必须一起记（见 WindowState.Maximized 的注释）：最大化时
// GetWindowRect 给的是"铺满屏幕 + 边框"的尺寸，只存几何不存这个位，
// 下次启动就会按普通窗口建一个比屏幕还大的窗口。
func (v *webviewView) snapshot() {
	r, ok := windowRect(v.hwnd)
	if !ok {
		return
	}
	v.store.SetGeometryMax(int(r.left), int(r.top), int(r.right-r.left), int(r.bottom-r.top),
		isMaximized(v.hwnd))
}

// ConfigureClose 安装关窗策略（子类化窗口过程）。
//
// 失败不阻断启动：点 X 会退回"直接退出"的老行为，日志里留一条。
func (v *webviewView) ConfigureClose(p ClosePolicy) {
	if v.hwnd == 0 {
		v.logf("无法安装关窗策略：窗口句柄为空")
		return
	}
	if p.Behavior == nil {
		p.Behavior = func() CloseBehavior { return CloseAsk }
	}
	v.policy = p
	if err := v.installSubclass(); err != nil {
		v.logf("安装关窗拦截失败（点 X 将直接退出）：%v", err)
	}
	v.applyWindowIcon()
}

// 窗口图标相关常量（winuser.h）。
const (
	wmSetIcon      = 0x0080
	iconSmall      = 0
	iconBig        = 1
	imageIcon      = 1
	lrDefaultColor = 0x0000
	// rsrcGroupIconID 是 rsrc 生成的 RT_GROUP_ICON 资源 ID。
	//
	// # 为什么是 2 而不是 1（实测，别猜）
	//
	// rsrc 把 **manifest 放在 ID 1**、图标组放在 ID 2：
	//
	//	RT_GROUP_ICON(14) → ID=2
	//	RT_ICON(3)        → 6 帧（ID 1..6，不同资源类型 ID 空间独立）
	//	RT_MANIFEST(24)   → ID=1
	//
	// 我第一版按"常见约定 = 1"写，`LoadImage` 返回 0，窗口保持默认空白图标，
	// **不报任何错** —— 只能靠日志里那句"没找到 RT_GROUP_ICON"反推。
	// 用 Windows 的 EnumResourceNames 一查就清楚了（命令见 CHANGELOG）。
	rsrcGroupIconID = 2
)

// applyWindowIcon 把 exe 内嵌的图标设成窗口图标。
//
// # 为什么必须显式设（实测）
//
// go-webview2 建的是普通 Win32 窗口，**没有** Icon 字段、也不发 WM_SETICON，
// 于是 Windows 给一个默认的空白窗口图标。而 exe 的 PE 资源里其实有图标
// （rsrc.syso 打的），但那是"文件图标"：资源管理器读它，标题栏/任务栏/Alt-Tab
// 读的是**窗口图标**，两者是两套机制。
//
// 所以必须 LoadImage 把资源读成 HICON 再 SendMessage(WM_SETICON)。
// 大/小两个尺寸都要设：ICON_SMALL 给标题栏，ICON_BIG 给任务栏与 Alt-Tab。
func (v *webviewView) applyWindowIcon() {
	if v.hwnd == 0 {
		return
	}
	hInst, _, _ := kernel32.NewProc("GetModuleHandleW").Call(0)
	if hInst == 0 {
		v.logf("设置窗口图标失败：GetModuleHandle 返回 0")
		return
	}
	// 先按已知 ID 取；取不到就扫 1..16 兜底 —— 换资源生成器时不会静默失效。
	// 扫描只在启动时跑一次，代价可忽略。
	// cx=cy=0 表示"用资源里的实际尺寸"，取到的是第一帧（16×16）。
	// 所以探测用一个 0 尺寸的调用，真正设图标时再按目标尺寸各取一次 ——
	// 否则 ICON_BIG 拿到的也是 16px 帧，任务栏与 Alt-Tab 里会被放大成糊的。
	load := func(id uintptr, cx, cy int32) uintptr {
		h, _, _ := procLoadImageW.Call(hInst, id, imageIcon, uintptr(cx), uintptr(cy), lrDefaultColor)
		return h
	}
	hIcon, used := load(rsrcGroupIconID, 0, 0), uintptr(rsrcGroupIconID)
	if hIcon == 0 {
		for id := uintptr(1); id <= 16; id++ {
			if h := load(id, 0, 0); h != 0 {
				hIcon, used = h, id
				break
			}
		}
	}
	if hIcon == 0 {
		// 不阻断启动：图标是观感问题，但日志要留痕 —— 否则"图标怎么还是白的"
		// 会变成一个只能读源码才能回答的问题。
		v.logf("设置窗口图标失败：exe 里 1..16 都没有 RT_GROUP_ICON（检查 rsrc.syso 是否被链接）")
		return
	}
	// 分尺寸设置：ICON_SMALL(16) 给标题栏，ICON_BIG(32) 给任务栏与 Alt-Tab。
	// 取不到对应尺寸时回落探测到的那个句柄（宁可用 16px 放大，也不要空白）。
	hSmall := load(used, 16, 16)
	if hSmall == 0 {
		hSmall = hIcon
	}
	hBig := load(used, 32, 32)
	if hBig == 0 {
		hBig = hIcon
	}
	procSendMessageW.Call(v.hwnd, wmSetIcon, iconSmall, hSmall)
	procSendMessageW.Call(v.hwnd, wmSetIcon, iconBig, hBig)
	v.logf("窗口图标已应用（RT_GROUP_ICON ID=%d，16px+32px）", used)
}

func (v *webviewView) installSubclass() error {
	subclassMu.Lock()
	subclassViews[v.hwnd] = v
	subclassMu.Unlock()

	// SetWindowLongPtr 返回的是**原过程**，必须留着 —— 它是"未处理消息"的
	// 唯一去处，丢了这扇窗口就不再响应拖动/缩放/DPI 变更。
	prev, err := setWindowLongPtr(v.hwnd, gwlWndProc, subclassProc)
	if err != nil {
		subclassMu.Lock()
		delete(subclassViews, v.hwnd)
		subclassMu.Unlock()
		return err
	}
	v.prevProc = prev
	return nil
}

// removeSubclass 窗口销毁前把原过程还回去。
func (v *webviewView) removeSubclass(hwnd uintptr) {
	subclassMu.Lock()
	delete(subclassViews, hwnd)
	subclassMu.Unlock()
	if v.prevProc != 0 {
		if _, err := setWindowLongPtr(hwnd, gwlWndProc, v.prevProc); err != nil {
			v.logf("恢复原窗口过程失败（窗口已销毁时属正常）：%v", err)
		}
	}
}

// handleWindowMessage 子类化的窗口过程。
func (v *webviewView) handleWindowMessage(hwnd, msg, wParam, lParam uintptr) uintptr {
	switch msg {
	case wmQueryEndSession, wmEndSession:
		d := decideSystemShutdown(uint32(msg), wParam)
		if msg == wmQueryEndSession {
			// 恒返回 TRUE：拦住关机会让用户强制结束进程，反而丢数据。
			v.logf("系统询问能否结束会话（WM_QUERYENDSESSION）：放行")
		}
		if d.quitAsync {
			v.logf("系统正在关机/注销（WM_ENDSESSION）：尽力先落盘")
			// 必须异步：在这个线程上等 Flush 会把关机流程卡在这里。
			if v.policy.Quit != nil {
				go v.policy.Quit()
			}
		}
		if d.handled {
			return d.result
		}
		return callWindowProc(v.prevProc, hwnd, msg, wParam, lParam)

	case wmClose:
		return v.handleClose(hwnd, wParam, lParam)

	case wmNCDestroy:
		// 窗口真要没了：先把原过程还回去，再从登记表摘掉。
		v.removeSubclass(hwnd)
	}
	// 其余一律交回原过程 —— 包括 go-webview2 自己处理的 WM_SIZE/WM_ACTIVATE
	// 和它没处理的 WM_GETMINMAXINFO 之类。吞掉任何一条都会让窗口"看起来卡死"。
	return callWindowProc(v.prevProc, hwnd, msg, wParam, lParam)
}

// handleClose WM_CLOSE：按设置决定藏起来还是退出。
func (v *webviewView) handleClose(hwnd, wParam, lParam uintptr) uintptr {
	// 用户已经在页面内明确选了退出：这次 WM_CLOSE 是 resolveCloseAsk 重投的，
	// 直接交回原过程走销毁，不能再问一遍。
	if v.closeConfirmed.Swap(false) {
		return callWindowProc(v.prevProc, hwnd, wmClose, wParam, lParam)
	}

	act := closeActionFor(v.behavior())
	if act == closeAsk {
		// JS 桥接没有装好：直接走原来的原生对话框，不尝试页面弹窗。
		if !v.closeAskReady.Load() {
			minimize := false
			if v.policy.Ask != nil {
				minimize = v.policy.Ask(hwnd)
			}
			act = resolveAsk(minimize)
		} else {
			// 已经有一次询问在路上（用户或脚本又发了一次 WM_CLOSE）：
			// 吞掉这一次，别叠第二个弹窗。
			if !v.asking.CompareAndSwap(false, true) {
				return 0
			}
			v.closeAskShown.Store(false)
			// 页面还没加载完 / 脚本被禁用时，Eval 不会产生任何回调。
			// 超时后回退原生 MessageBox，保证点 X 一定有反应。
			go v.closeAskWatchdog()
			v.eval(`(function () {
				if (typeof window.__nbCloseAsk === 'function') { window.__nbCloseAsk(); return; }
				if (window.nbCloseFallback) { window.nbCloseFallback(); }
			})()`)
			return 0
		}
	}

	if act == closeHide {
		v.hideToTray()
		return 0
	}

	// 退出：**交回原过程**，让 go-webview2 的窗口过程 DestroyWindow →
	// WM_DESTROY → PostQuitMessage，Run 返回后由 app.shutdown 走
	// 「先落盘再停机」。这里绝不能自己 PostQuitMessage 或 os.Exit ——
	// 那条路绕过 boot.Runtime.Shutdown，账号池状态直接丢。
	return callWindowProc(v.prevProc, hwnd, wmClose, wParam, lParam)
}

// closeAskWatchdog 页面内弹窗没显示出来时回退原生 MessageBox。
//
// 正常路径下 JS 一打开弹窗就会回调 nbCloseShown，这个 goroutine 醒来后
// 什么也不做。只有页面还没加载完、脚本报错、Bind 失败时才真正兜底。
func (v *webviewView) closeAskWatchdog() {
	time.Sleep(2 * time.Second)
	if v.asking.Load() && !v.closeAskShown.Load() {
		v.fallbackCloseAsk()
	}
}

// fallbackCloseAsk 用原生 MessageBox 完成这次询问。
func (v *webviewView) fallbackCloseAsk() {
	if !v.asking.Load() {
		return
	}
	v.logf("页面内关窗弹窗不可用，回退到原生消息框")
	minimize := false
	if v.policy.Ask != nil {
		minimize = v.policy.Ask(v.hwnd)
	}
	v.resolveCloseAsk(minimize)
}

// resolveCloseAsk 收尾一次询问：先关页面弹窗，再执行隐藏或退出。
func (v *webviewView) resolveCloseAsk(minimize bool) {
	if !v.asking.CompareAndSwap(true, false) {
		return
	}
	v.eval("window.__nbCloseHide && window.__nbCloseHide()")
	if minimize {
		v.hideToTray()
		return
	}
	// 退出必须回到窗口消息循环里做：这里可能跑在 WebView2 的回调线程上。
	v.closeConfirmed.Store(true)
	if !postMessage(v.hwnd, wmClose, 0, 0) {
		v.closeConfirmed.Store(false)
		v.logf("投递退出消息失败（窗口句柄 %#x），本次关闭已取消", v.hwnd)
	}
}

// cancelCloseAsk 用户点了取消：只收弹窗，不动窗口。
func (v *webviewView) cancelCloseAsk() {
	if !v.asking.CompareAndSwap(true, false) {
		return
	}
	v.eval("window.__nbCloseHide && window.__nbCloseHide()")
}

// hideToTray 只隐藏窗口（不销毁 WebView）。
//
// # 为什么不 Destroy
//
// WebView2 的渲染进程与窗口生命周期是解耦的（MicrosoftEdge/WebView2Feedback#1424
// 说的正是"关窗后进程还活着"）。隐藏正好利用这一点：窗口没了但页面、
// 会话、正在跑的请求全在，恢复是零成本的一次 ShowWindow。反过来 Destroy
// 之后再恢复要重建整个 WebView2（白屏风险 + 重新登录态），代价完全不成比例。
func (v *webviewView) hideToTray() {
	v.hidden.Store(true)
	showWindow(v.hwnd, swHide)
	if isWindowVisible(v.hwnd) {
		// 极少数情况下（窗口被别的工具接管/句柄已失效）藏不掉：说清楚，
		// 别让用户以为"点了 X 却没反应"。
		v.logf("隐藏窗口之后它仍然可见（句柄 %#x）", v.hwnd)
	}
	// 日志 + 首次托盘气泡甩到别的 goroutine：它们要调 shell，不能占消息循环线程。
	if v.policy.Hidden != nil {
		go v.policy.Hidden()
	}
	v.syncPageVisibility(true)
}

// behavior 当前关窗行为（只读内存，不会阻塞消息循环）。
func (v *webviewView) behavior() CloseBehavior {
	if v.policy.Behavior != nil {
		return v.policy.Behavior()
	}
	return v.store.Get().CloseBehavior
}

// jsPageVisibility 把页面的"可见性"钉成指定状态，并派发 visibilitychange。
//
// # 为什么要手动改
//
// webui.html 已经把"后台"语义接好了：轮询与日志 SSE 都在 document.hidden /
// visibilityState 上早退（pollPoolValues、pollAPIKeys、shouldStreamLogs）。
// 但窗口被 SW_HIDE 之后，页面里的 document.visibilityState **是否**跟着变
// hidden 属于 WebView2/Chromium 的实现细节，实测结论留在这个仓库的探针
// （visibility_probe_windows_test.go）里。
//
// 不依赖那条链的理由：它一旦在某版运行时上不成立，"最小化到托盘"就变成
// "在后台全速轮询"——正是这个功能要避免的。所以这里直接给 document 打
// 两个 own property 遮蔽原型上的 getter（可逆：delete 掉就回到原型），
// 再派发一次事件让页面里已有的监听器跑起来。
//
// ⚠ 只碰 document 的可见性属性，不碰页面里的任何函数/状态：恢复时页面
// 自己会按 shouldStreamLogs 重新决定要不要连 SSE。
const jsPageVisibility = `(function(){try{
  var d=document;
  var hide=%t;
  if(hide){
    Object.defineProperty(d,'hidden',{configurable:true,get:function(){return true;}});
    Object.defineProperty(d,'visibilityState',{configurable:true,get:function(){return 'hidden';}});
  }else{
    delete d.hidden;
    delete d.visibilityState;
  }
  d.dispatchEvent(new Event('visibilitychange'));
}catch(e){}})()`

// syncPageVisibility 把隐藏/恢复同步给页面。
//
// Eval 必须排到消息循环线程（Dispatch）：WebView2 的脚本执行接口不是
// 并发安全的，从别的 goroutine 直接调会偶发崩溃。
func (v *webviewView) syncPageVisibility(hide bool) {
	js := fmt.Sprintf(jsPageVisibility, hide)
	v.w.Dispatch(func() { v.w.Eval(js) })
}

// logf 记一行日志（logger 可能为 nil：测试里会直接构造 view）。
func (v *webviewView) logf(format string, args ...any) {
	if v.log != nil {
		v.log.Printf(format, args...)
	}
}
