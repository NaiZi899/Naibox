//go:build windows

package desktop

import (
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	user32   = windows.NewLazySystemDLL("user32.dll")

	procGetConsoleWindow          = kernel32.NewProc("GetConsoleWindow")
	procFreeConsole               = kernel32.NewProc("FreeConsole")
	procGetProcessWindowStation   = user32.NewProc("GetProcessWindowStation")
	procGetUserObjectInformationW = user32.NewProc("GetUserObjectInformationW")
	procGetWindowRect             = user32.NewProc("GetWindowRect")
	procSetWindowPos              = user32.NewProc("SetWindowPos")
	procShowWindow                = user32.NewProc("ShowWindow")
	procSetForegroundWindow       = user32.NewProc("SetForegroundWindow")
)

const (
	swShow          = 5
	swRestore       = 9
	swpNoZOrder     = 0x0004
	swpNoActivate   = 0x0010
	mbSetForeground = 0x00010000

	stillActive = 259 // STILL_ACTIVE

	webView2ClientGUID = `{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}`
)

// hideConsole 让窗口模式不出现黑框。
//
// 双保险：发布构建用 -H windowsgui（GUI 子系统，根本没有控制台），
// 但从命令行/脚本启动时仍然是控制台子系统，会先闪一个黑窗。这里
// FreeConsole 把它收掉；-console 时不调用，调试时还能看到 stderr。
func hideConsole() {
	if h, _, _ := procGetConsoleWindow.Call(); h != 0 {
		procFreeConsole.Call()
	}
}

// messageBox 弹一个系统消息框（不需要窗口已存在）。
//
// 桌面形态没有控制台，错误必须找一个用户看得见的出口，否则"双击没反应"
// 是唯一的体验。
func messageBox(title, text string, flags uint32) {
	messageBoxOwner(0, title, text, flags)
}

// messageBoxOwner 同 messageBox，但指定父窗口：有 hwnd 时对话框会居中在
// 该窗口上，并且**模态禁用**它（用户在回答之前点不到背景窗口，避免
// "关窗询问框还开着，用户又去点了一次 X"）。
func messageBoxOwner(hwnd uintptr, title, text string, flags uint32) int32 {
	t, err := windows.UTF16PtrFromString(text)
	if err != nil {
		return 0
	}
	c, err := windows.UTF16PtrFromString(title)
	if err != nil {
		return 0
	}
	r, _ := windows.MessageBox(windows.HWND(hwnd), t, c, flags|mbSetForeground)
	return r
}

// askMinimizeToTray 关窗询问框：是 = 最小化到托盘，否 = 退出。
//
// 只有两个按钮（MB_YESNO）：三按钮要 TaskDialog + COM，为了"取消"多引一整
// 套接口不值 —— "先别关"这件事本来就可以用窗口右上角的取消语义表达：
// 用户点否就是退出，点了是就藏起来，两个都是他想要的。
func askMinimizeToTray(hwnd uintptr) bool {
	const text = "关闭窗口时最小化到托盘，让程序继续在后台运行？\n\n" +
		"是：隐藏窗口，网关继续跑（从托盘图标恢复）\n" +
		"否：退出程序（会先落盘再停机）\n\n" +
		"这个选择可以在托盘菜单「关闭窗口时」里改。"
	r := messageBoxOwner(hwnd, "NaiBox 控制台", text, mbYesNo|mbIconQuestion)
	return r == idYes
}

// processAlive 判断 pid 是否还活着。
//
// 不用 os.FindProcess + Signal(0)：Windows 上那个组合不报错也不代表进程存在
// （实现是空操作），会把残留的 runtime.json 当成活实例。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}

// openURL 用系统默认浏览器打开链接。
func openURL(u string) error { return shellOpen(u) }

// openPath 用系统关联程序打开文件/目录（日志用记事本、目录用资源管理器）。
func openPath(p string) error { return shellOpen(p) }

func shellOpen(target string) error {
	file, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	verb, err := windows.UTF16PtrFromString("open")
	if err != nil {
		return err
	}
	// ShellExecute 的返回值 <= 32 才是失败（x/sys 已把它包成 error）。
	return windows.ShellExecute(0, verb, file, nil, nil, swShow)
}

// interactiveDesktop 当前会话是否有可见桌面。
//
// 判据是窗口站的 WSF_VISIBLE：服务会话（session 0）、无头 RDP 断开后的
// 会话都拿不到可见窗口站。没有它时建 GUI 窗口只会得到一个"看不见的
// 死进程"，所以宁可回落浏览器模式。
func interactiveDesktop() bool {
	ws, _, _ := procGetProcessWindowStation.Call()
	if ws == 0 {
		return false
	}
	var flags struct {
		inherit  int32
		reserved int32
		flags    uint32
	}
	var needed uint32
	const uoiFlags = 1
	ok, _, _ := procGetUserObjectInformationW.Call(
		ws, uoiFlags,
		uintptr(unsafe.Pointer(&flags)), unsafe.Sizeof(flags),
		uintptr(unsafe.Pointer(&needed)),
	)
	return ok != 0 && flags.flags&0x0001 != 0
}

// hasWebView2Runtime 检测 WebView2 运行时。
//
// 判定完全按微软官方文档的注册表口径：EdgeUpdate\Clients\{GUID}\pv，
// 三个位置都要看（32 位视图、64 位视图、按用户安装）。pv 为 "0.0.0.0"
// 表示"装了但没装成"，必须当作缺失。
//
// 注册表全没命中时再补一条目录探测：某些企业部署用固定目录拷贝运行时，
// 不写 EdgeUpdate 键。宁可多认一次"有"，也别让用户明明装了还被降级。
func hasWebView2Runtime() (bool, string) {
	const rel = `Microsoft\EdgeUpdate\Clients\` + webView2ClientGUID
	for _, probe := range []struct {
		root registry.Key
		path string
	}{
		{registry.LOCAL_MACHINE, `SOFTWARE\WOW6432Node\` + rel},
		{registry.LOCAL_MACHINE, `SOFTWARE\` + rel},
		{registry.CURRENT_USER, `Software\` + rel},
	} {
		k, err := registry.OpenKey(probe.root, probe.path, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		pv, _, err := k.GetStringValue("pv")
		k.Close()
		if err != nil {
			continue
		}
		pv = strings.TrimSpace(pv)
		if pv == "" || pv == "0.0.0.0" {
			continue
		}
		return true, "WebView2 运行时 " + pv
	}
	for _, base := range []string{os.Getenv("ProgramFiles(x86)"), os.Getenv("ProgramFiles")} {
		if base == "" {
			continue
		}
		dir := filepath.Join(base, "Microsoft", "EdgeWebView", "Application")
		if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
			return true, "WebView2 运行时目录 " + dir
		}
	}
	return false, "未检测到 WebView2 运行时（注册表 pv 缺失或为 0.0.0.0）"
}

// canShowWindow 能不能开内嵌窗口。第二个返回值是给用户/日志看的原因。
func canShowWindow() (bool, string) {
	if !interactiveDesktop() {
		return false, "当前会话没有可见桌面（服务会话或无头会话）"
	}
	return hasWebView2Runtime()
}

// winRect GetWindowRect 的返回结构。
type winRect struct {
	left, top, right, bottom int32
}

func windowRect(hwnd uintptr) (winRect, bool) {
	var r winRect
	if hwnd == 0 {
		return r, false
	}
	ok, _, _ := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	return r, ok != 0
}

// moveWindow 只挪位置，不改大小。
func moveWindow(hwnd uintptr, x, y int) {
	if hwnd == 0 {
		return
	}
	procSetWindowPos.Call(hwnd, 0, uintptr(x), uintptr(y), 0, 0, swpNoZOrder|swpNoActivate)
}

// showAndFocus 把窗口显示出来并提到前台。
//
// # 为什么不能只调 SetForegroundWindow
//
// Windows 的前台锁规则：只有"当前前台进程"或"刚收到用户输入的进程"才能
// 抢前台。托盘点击（用户输入的进程就是自己）通常没问题；但第二实例通过
// 事件唤醒时是**另一个进程**在请求，SetForegroundWindow 会被静默忽略，
// 用户看到"点了图标没反应"。所以先 ShowWindow(SW_RESTORE) 再抢，
// 抢不到至少窗口已展开在任务栏里。
//
// # 从托盘里恢复隐藏窗口（本轮新增）
//
// SW_HIDE 之后的窗口是"不可见但仍存在"：SW_RESTORE 能把它带回来，但
// SW_SHOW 才是语义上正确的那一步（恢复最小化 ≠ 显示隐藏）。两个都发，
// 顺序无关，代价是一次多余的窗口消息。
//
// 前台抢不到时**只闪一次**任务栏（FlashWindowEx），不重试、不循环 ——
// 抢焦点循环会把用户从正在做的事情里拽出来。
func showAndFocus(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	if !isWindowVisible(hwnd) {
		procShowWindow.Call(hwnd, swShow)
	}
	procShowWindow.Call(hwnd, swRestore)
	if r, _, _ := procSetForegroundWindow.Call(hwnd); r == 0 {
		flashTaskbar(hwnd)
	}
}
