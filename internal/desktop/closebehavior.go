package desktop

import "strings"

// CloseBehavior 点窗口 X 时的行为（存 window.json 的 close_behavior）。
//
// 值域是封闭的三个字符串而不是 iota：它会被写进用户能打开看的 JSON，
// 数字在那里不可读，而且以后加模式时数字的含义会漂。未知值一律收敛到
// CloseAsk —— 一个"点 X 不退出"的默认行为，比误判成 quit 丢掉后台服务温和。
type CloseBehavior string

const (
	// CloseAsk 每次弹框问（默认）。
	CloseAsk CloseBehavior = "ask"
	// CloseMinimize 隐藏窗口、进程与 WebView2 继续跑（后台保活）。
	CloseMinimize CloseBehavior = "minimize"
	// CloseQuit 直接退出（走正常收尾：落盘 → 停机 → 删 runtime.json）。
	CloseQuit CloseBehavior = "quit"
)

// closeBehaviors 托盘菜单的顺序 = 这里声明的顺序（询问 → 最小化 → 退出）。
var closeBehaviors = []CloseBehavior{CloseAsk, CloseMinimize, CloseQuit}

// normalizeCloseBehavior 把任意输入收敛到合法值；空/未知/大小写混杂都返回 CloseAsk。
func normalizeCloseBehavior(s string) CloseBehavior {
	v := CloseBehavior(strings.ToLower(strings.TrimSpace(s)))
	for _, b := range closeBehaviors {
		if v == b {
			return v
		}
	}
	return CloseAsk
}

// describe 日志与菜单提示用的中文说法。
func (b CloseBehavior) describe() string {
	switch normalizeCloseBehavior(string(b)) {
	case CloseMinimize:
		return "最小化到托盘（后台保活）"
	case CloseQuit:
		return "直接退出"
	default:
		return "每次询问"
	}
}

// closeAction WM_CLOSE 的实际处置。
type closeAction int

const (
	// closeDefer 交回原窗口过程：它会 DestroyWindow，消息循环随之返回，
	// 也就是"真的退出"。退出后的落盘/停机由 app.shutdown 负责。
	closeDefer closeAction = iota
	// closeHide 只隐藏窗口：进程、WebView2、网关服务全部继续。
	closeHide
	// closeAsk 弹原生 MessageBox 问一次。
	closeAsk
)

// closeActionFor 设置 → 动作。
func closeActionFor(b CloseBehavior) closeAction {
	switch normalizeCloseBehavior(string(b)) {
	case CloseMinimize:
		return closeHide
	case CloseQuit:
		return closeDefer
	default:
		return closeAsk
	}
}

// resolveAsk 把询问结果折算成动作：true = 最小化到托盘，false = 退出。
func resolveAsk(minimize bool) closeAction {
	if minimize {
		return closeHide
	}
	return closeDefer
}
