//go:build windows

package desktop

// Win32 窗口消息号（只列本文件关心的几条，其余交给原过程）。
const (
	wmClose           = 0x0010
	wmQueryEndSession = 0x0011
	wmEndSession      = 0x0016
	wmNCDestroy       = 0x0082
)

// systemShutdownDecision 窗口过程对关机/注销类消息的处置。
type systemShutdownDecision struct {
	// handled false = 交给原窗口过程。
	handled bool
	// result handled 时的返回值（只有 WM_QUERYENDSESSION 的返回值有语义）。
	result uintptr
	// quitAsync 需要异步触发收尾（落盘）。
	quitAsync bool
}

// decideSystemShutdown WM_QUERYENDSESSION / WM_ENDSESSION 的处理决定。
//
// # 为什么 WM_QUERYENDSESSION 必须无条件返回 TRUE
//
// 这条消息是"系统即将关机/注销，你同意吗"。返回 FALSE 就把用户卡在
// "有程序阻止关机"的界面上：轻则多等 5 秒，重则用户直接点"强制关机"——
// 那时进程是被 TerminateProcess 干掉的，state.json / request-log 都来不及
// 落盘，账号池状态就丢了。也就是说：**想保住数据，恰恰不能拦关机**。
//
// 正确做法是放行 + 在 WM_ENDSESSION(wParam!=0) 时抢几秒钟把状态刷下去。
// 这里刻意不看 CloseBehavior：用户选了"最小化到托盘"也不影响关机。
func decideSystemShutdown(msg uint32, wParam uintptr) systemShutdownDecision {
	switch msg {
	case wmQueryEndSession:
		// TRUE = 我同意，别拦我。
		return systemShutdownDecision{handled: true, result: 1}
	case wmEndSession:
		// wParam != 0：关机已成定局，抢时间落盘。
		// wParam == 0：这次关机被取消了（别的程序拦下或用户点了取消），
		// 什么都别做 —— 提前收尾会把"用户只是想关个别的程序"变成"我们的服务没了"。
		return systemShutdownDecision{handled: true, result: 0, quitAsync: wParam != 0}
	default:
		return systemShutdownDecision{}
	}
}
