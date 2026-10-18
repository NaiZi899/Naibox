package desktop

import "errors"

// ErrNoGUI 平台没有内嵌窗口（非 Windows），调用方据此回落浏览器模式。
var ErrNoGUI = errors.New("当前平台不支持内嵌窗口")

// WindowView 一扇内嵌窗口。Windows 实现是 WebView2，其它平台返回 ErrNoGUI。
//
// 抽成接口而不是让 app.go 直接 import webview2：`GOOS=linux go build ./...`
// 必须过，而 go-webview2 整个包是 //go:build windows 的 —— 直接在公共文件里
// 引用它会让交叉编译找不到符号。
type WindowView interface {
	// Navigate 换地址（重启服务后端口可能变）。
	Navigate(url string)
	// Show 把窗口提到前台（第二实例唤醒、托盘"打开控制台"）。
	// 窗口若在托盘里藏着，这里负责把它恢复出来。
	Show()
	// Close 请求关闭；幂等，可从任意 goroutine 调用。
	Close()
	// Destroy 兜底销毁（Close 之后仍未退出时）。
	Destroy()
	// Run 跑消息循环，直到窗口关闭才返回。**必须**在主 goroutine 调用。
	Run()
	// ConfigureClose 安装关窗策略，**必须在 Run 之前**调用。
	// 未安装时窗口行为与旧版一致：点 X 就退出。
	ConfigureClose(p ClosePolicy)
}

// ClosePolicy 关窗策略：桌面壳注入，窗口过程在收到 WM_CLOSE 时回调。
//
// # 线程约束（写错会卡死界面）
//
// 除 Hidden/Quit 外，这些回调都运行在**窗口消息循环线程**上（WndProc 内部）。
// 在 WndProc 里阻塞 = 整扇窗口失去响应，所以：
//   - Behavior 只许读内存（读文件/加锁等 IO 都不行）；
//   - Ask 会弹模态框，属于有意为之的阻塞（MessageBox 自带消息泵）；
//   - Hidden / Quit 的实现方必须立刻返回，真正的工作甩到 goroutine。
type ClosePolicy struct {
	// Behavior 当前设置。
	Behavior func() CloseBehavior
	// Ask 弹框问用户；hwnd 用作父窗口。返回 true = 最小化到托盘，false = 退出。
	Ask func(hwnd uintptr) bool
	// Hidden 窗口刚刚被藏进托盘（记日志、弹一次性气泡）。
	Hidden func()
	// Quit 系统要关机/注销了，尽力先把状态落盘。**必须异步**。
	Quit func()
}
