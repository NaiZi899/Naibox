//go:build !windows

package desktop

import "errors"

// 非 Windows：全部退化为"浏览器模式"所需的空实现。
//
// 这些函数存在的唯一目的，是让 GOOS=linux 的整仓编译（build tag 隔离
// 是否有效的验收项）成立。它们不做任何平台相关的事。

func hideConsole() {}

func messageBox(title, text string, flags uint32) {}

func messageBoxOwner(hwnd uintptr, title, text string, flags uint32) int32 { return 0 }

// askMinimizeToTray 非 Windows 恒返回 false（= 退出）：没有托盘，藏起来就
// 再也找不回来，那比"点 X 就退出"糟得多。
func askMinimizeToTray(hwnd uintptr) bool { return false }

// showTrayBalloon 非 Windows 没有通知区域。
func showTrayBalloon(title, text string) error { return errors.New("当前平台没有系统托盘") }

func showWindow(hwnd uintptr, cmd int32) bool { return false }

func isWindowVisible(hwnd uintptr) bool { return false }

func flashTaskbar(hwnd uintptr) {}

func processAlive(pid int) bool { return false }

func openURL(u string) error { return errors.New("当前平台不支持直接打开浏览器") }

func openPath(p string) error { return errors.New("当前平台不支持打开文件") }

func interactiveDesktop() bool { return false }

func hasWebView2Runtime() (bool, string) { return false, "非 Windows 平台没有 WebView2" }

func canShowWindow() (bool, string) { return false, "非 Windows 平台不支持内嵌窗口" }

type winRect struct {
	left, top, right, bottom int32
}

func windowRect(hwnd uintptr) (winRect, bool) { return winRect{}, false }

func moveWindow(hwnd uintptr, x, y int) {}

func showAndFocus(hwnd uintptr) {}
