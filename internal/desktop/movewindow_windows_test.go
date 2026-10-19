//go:build windows

package desktop

import "testing"

// moveWindowFlags 本身定义在 platform_windows.go（产品代码），这里只断言它。
// 放在产品侧而不是测试侧，是为了让"标志位"只有一份来源 —— 否则测试断言
// 自己定义的值，产品代码写成什么都绿。

// TestMoveWindowFlagsIncludeNoSize 「只挪位置」必须带 SWP_NOSIZE。
//
// # 守的是什么（实测事故，2026-10-19）
//
// SetWindowPos 的签名是 (hwnd, after, x, y, cx, cy, flags)。moveWindow 传
// cx=cy=0 —— 这**只有在带 SWP_NOSIZE 时**才表示"不动尺寸"。缺了它，0 就是
// 一个真实的尺寸请求，Windows 把窗口压到最小跟踪尺寸（实测 135×37）并
// 落在参数坐标上。
//
// 用户看到的形态：双击 exe，窗口在左上角只有标题栏那么大。而且**只有存在
// window.json 时才复现**（首次启动 HasPos=false 不调 moveWindow，一切正常），
// 所以"重装一次就好了"是假象，第二次启动必然复发。
//
// 实测对照（同一台机器、同一份 window.json）：
//
//	缺 SWP_NOSIZE  → rect=(133,80) 135x37
//	含 SWP_NOSIZE  → rect=(133,80) 787x547
func TestMoveWindowFlagsIncludeNoSize(t *testing.T) {
	if moveWindowFlags&swpNoSize == 0 {
		t.Fatalf("moveWindow 的标志位缺 SWP_NOSIZE(%#x)：SetWindowPos 会把 cx=cy=0 "+
			"当成真实尺寸，窗口启动即被压到最小跟踪尺寸（实测 135×37，落在左上角）。\n"+
			"当前标志位 = %#x", swpNoSize, moveWindowFlags)
	}
}

// TestMoveWindowFlagsDoNotMoveZOrderOrActivate 挪位置时不许抢前台/改 Z 序。
//
// 启动时窗口尚未显示，抢前台会让它压掉用户正在用的窗口；改 Z 序则会把它
// 提到最上层，看起来像"程序自己弹出来抢焦点"。
func TestMoveWindowFlagsDoNotMoveZOrderOrActivate(t *testing.T) {
	if moveWindowFlags&swpNoZOrder == 0 {
		t.Error("moveWindow 必须带 SWP_NOZORDER：启动时改 Z 序会让窗口压到别的窗口上面")
	}
	if moveWindowFlags&swpNoActivate == 0 {
		t.Error("moveWindow 必须带 SWP_NOACTIVATE：启动时抢前台会打断用户正在做的事")
	}
}

// TestMoveWindowDoesNotCarryNoMove SWP_NOMOVE 与"挪位置"这件事直接矛盾。
//
// 带上它 moveWindow 就彻底失效（位置不再被恢复），而且同样是**静默**的：
// 窗口照常出现、尺寸也对，只是每次启动都回到默认位置。用户会觉得
// "这程序记不住我摆的窗口位置"。
func TestMoveWindowDoesNotCarryNoMove(t *testing.T) {
	if moveWindowFlags&swpNoMove != 0 {
		t.Error("moveWindow 不能带 SWP_NOMOVE：那会让恢复窗口位置彻底失效（且不报错）")
	}
}
