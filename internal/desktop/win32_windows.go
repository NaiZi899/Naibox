//go:build windows

package desktop

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 本文件是"窗口过程 + 托盘气泡"这一小块用到的 Win32 绑定。
//
// 单独成文件而不是继续堆进 platform_windows.go：那边是"进程/控制台/注册表"
// 的杂项，这里是**子类化窗口过程**这一条链路（SetWindowLongPtr/CallWindowProc
// 与 NOTIFYICONDATAW），放一起会让人以为它们与 FreeConsole 是同一类东西。

var (
	shell32 = windows.NewLazySystemDLL("shell32.dll")

	procSetWindowLongPtrW        = user32.NewProc("SetWindowLongPtrW")
	procSetWindowLongW           = user32.NewProc("SetWindowLongW")
	procCallWindowProcW          = user32.NewProc("CallWindowProcW")
	procDefWindowProcW           = user32.NewProc("DefWindowProcW")
	procIsWindowVisible          = user32.NewProc("IsWindowVisible")
	procFlashWindowEx            = user32.NewProc("FlashWindowEx")
	procFindWindowExW            = user32.NewProc("FindWindowExW")
	procGetWindowThreadProcessID = user32.NewProc("GetWindowThreadProcessId")
	procShellNotifyIconW         = shell32.NewProc("Shell_NotifyIconW")
	// 窗口图标：资源管理器读的是 exe 的**文件图标**（PE 资源），
	// 而标题栏 / 任务栏 / Alt-Tab 读的是**窗口类图标**（WM_SETICON）——
	// 两套东西。只做前者的话，标题栏会一直是 Windows 的默认空白图标。
	procSendMessageW = user32.NewProc("SendMessageW")
	procLoadImageW   = user32.NewProc("LoadImageW")
)

const (
	// gwlWndProc GWLP_WNDPROC 的索引（-4）。x/sys/windows 没有导出这两个
	// SetWindowLongPtr 包装（386 上要换成 SetWindowLongW），所以自己绑。
	gwlWndProc = ^uintptr(3) // -4

	swHide = 0

	// 托盘气泡：NIF_INFO 表示"这次 NIM_MODIFY 只更新气泡文本"。
	nifInfo    = 0x00000010
	nimModify  = 0x00000001
	niifInfo   = 0x00000001
	notifyIcon = 976 // sizeof(NOTIFYICONDATAW, v4)：布局对不上就别弹（见下）

	// systray 库给主图标用的固定 uID（energye/systray systray_windows.go）。
	// 它没有导出，改版本时会话里会失配 —— NIM_MODIFY 那时返回 FALSE，
	// 我们退化成"只记日志"，不会破坏已有的托盘图标。
	systrayWindowClass = "SystrayClass"
	systrayIconID      = 100

	flashwAll       = 0x00000003
	flashwTimerNoFG = 0x0000000C
)

// showWindow 是对 ShowWindow 的薄封装。
//
// 返回值是 Win32 的原义：**调用之前**窗口是否可见。对已经隐藏的窗口返回
// false —— 那不是失败。所以这里不返回 error：把 ShowWindow 的返回值当
// 成功判据是这类代码的经典错法（真被藏起来时反而"报错"）。
func showWindow(hwnd uintptr, cmd int32) bool {
	if hwnd == 0 {
		return false
	}
	r, _, _ := procShowWindow.Call(hwnd, uintptr(cmd))
	return r != 0
}

// procErr 归一 LazyProc.Call 的 errno。
//
// x/sys/windows 的 Call 把 `Errno` 直接塞进 error 接口，于是**成功调用**
// 也会得到一个非 nil 的 err，打印出来是 "The operation completed successfully."
// —— 正是这句话让人以为哪里坏了。所以只在非零 errno 时才认它是错误。
func procErr(what string, err error) error {
	if err == nil {
		return nil
	}
	if errno, ok := err.(syscall.Errno); ok && errno == 0 {
		return fmt.Errorf("%s 未成功（系统未给出错误码）", what)
	}
	return err
}

// isWindowVisible 窗口当前对用户是否可见（SW_HIDE 之后为 0）。
func isWindowVisible(hwnd uintptr) bool {
	if hwnd == 0 {
		return false
	}
	r, _, _ := procIsWindowVisible.Call(hwnd)
	return r != 0
}

// setWindowLongPtr 换窗口过程，返回原过程。
//
// 386 上没有 SetWindowLongPtrW（它是 SetWindowLongW 的宏），按 uintptr 宽度选。
// 本仓库目前只发 windows/amd64 与 arm64，但这条分支留着比在 386 上
// "找不到 proc 直接 panic"好。
func setWindowLongPtr(hwnd uintptr, index uintptr, newValue uintptr) (uintptr, error) {
	proc := procSetWindowLongPtrW
	if unsafe.Sizeof(hwnd) == 4 {
		proc = procSetWindowLongW
	}
	r, _, err := proc.Call(hwnd, index, newValue)
	if r == 0 {
		return 0, procErr("SetWindowLongPtr", err)
	}
	return r, nil
}

// callWindowProc 把消息交回原窗口过程。
//
// # 这一句是整个子类化里最不能省的一行
//
// 子类化只是"插一层"，不是"顶替"。凡是自己没处理的消息（拖动、缩放、
// DPI 变更、WM_GETMINMAXINFO、WM_ACTIVATE……）都必须原样交回原过程，
// 否则窗口会变成"能显示但拖不动、缩放后白屏"的僵尸 —— 这类故障看起来
// 像死锁，实际是消息被吞了。
func callWindowProc(prevProc, hwnd, msg, wParam, lParam uintptr) uintptr {
	if prevProc == 0 {
		return defaultWindowProc(hwnd, msg, wParam, lParam)
	}
	r, _, _ := procCallWindowProcW.Call(prevProc, hwnd, msg, wParam, lParam)
	return r
}

// defaultWindowProc 系统默认窗口过程（没有原过程可交时用）。
func defaultWindowProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
	return r
}

// flashTaskbar 闪一下任务栏按钮。
//
// 用途只有一个：前台锁抢不到时给用户一个"窗口在这儿"的信号。
// 刻意只闪**一次**（uCount=3，不循环重试）—— 死循环抢焦点会把用户从
// 正在做的事情里拽出来，比不闪更糟。
func flashTaskbar(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	fi := struct {
		cbSize    uint32
		_         uint32
		hwnd      uintptr
		dwFlags   uint32
		uCount    uint32
		dwTimeout uint32
	}{hwnd: hwnd, dwFlags: flashwAll | flashwTimerNoFG, uCount: 3}
	fi.cbSize = uint32(unsafe.Sizeof(fi))
	procFlashWindowEx.Call(uintptr(unsafe.Pointer(&fi)))
}

// notifyIconData Win32 NOTIFYICONDATAW（含 guidItem/hBalloonIcon 的 v4 布局）。
//
// 字段顺序与宽度必须与 energye/systray 里那份完全一致：NIM_MODIFY 靠
// (hWnd, uID) 定位图标，而 cbSize 决定系统怎么解释这块内存。写错的结果
// 不是报错，是"气泡不出现"。
type notifyIconData struct {
	Size            uint32
	_               uint32
	Wnd             uintptr
	ID              uint32
	Flags           uint32
	CallbackMessage uint32
	_               uint32
	Icon            uintptr
	Tip             [128]uint16
	State           uint32
	StateMask       uint32
	Info            [256]uint16
	Timeout         uint32
	InfoTitle       [64]uint16
	InfoFlags       uint32
	GuidItem        [16]byte
	BalloonIcon     uintptr
}

// copyWide 把 s 写进定长 UTF-16 缓冲（必然 NUL 结尾）。
func copyWide(dst []uint16, s string) {
	u, err := windows.UTF16FromString(s)
	if err != nil {
		return
	}
	if len(u) > len(dst) {
		u = u[:len(dst)-1]
	}
	copy(dst, u)
}

// ownTrayWindow 找**本进程**的托盘窗口。
//
// 不能直接用 FindWindowW("SystrayClass", nil)：同一个桌面会话里可能有第二个
// omni2api（另一个数据根），那样气泡会弹到别人的图标上。所以按窗口所属
// 进程号筛一遍。
func ownTrayWindow() uintptr {
	cls, err := windows.UTF16PtrFromString(systrayWindowClass)
	if err != nil {
		return 0
	}
	self := uint32(os.Getpid())
	var prev uintptr
	for i := 0; i < 64; i++ { // 顶层的 SystrayClass 窗口正常只有 1 个；上限只为防死循环
		h, _, _ := procFindWindowExW.Call(0, prev, uintptr(unsafe.Pointer(cls)), 0)
		if h == 0 {
			return 0
		}
		prev = h
		var pid uint32
		procGetWindowThreadProcessID.Call(h, uintptr(unsafe.Pointer(&pid)))
		if pid == self {
			return h
		}
	}
	return 0
}

// showTrayBalloon 在**已有的**托盘图标上弹一个系统气泡（NIF_INFO）。
//
// 做法是对同一个 (hWnd, uID) 发 NIM_MODIFY + NIF_INFO：不会新增第二个图标，
// 只是让系统在现有图标上浮一段文字。Win10/11 上它可能被"专注助手"静音，
// 所以调用方**无论成败**都要另外写一行日志 —— 气泡是提示的增强，不是唯一通道。
func showTrayBalloon(title, text string) error {
	if unsafe.Sizeof(notifyIconData{}) != notifyIcon {
		return fmt.Errorf("NOTIFYICONDATAW 布局与预期不符（%d != %d）", unsafe.Sizeof(notifyIconData{}), notifyIcon)
	}
	hwnd := ownTrayWindow()
	if hwnd == 0 {
		return fmt.Errorf("找不到本进程的托盘窗口（托盘可能未就绪）")
	}
	nid := notifyIconData{
		Wnd:       hwnd,
		ID:        systrayIconID,
		Flags:     nifInfo,
		InfoFlags: niifInfo,
	}
	nid.Size = uint32(unsafe.Sizeof(nid))
	copyWide(nid.Info[:], text)
	copyWide(nid.InfoTitle[:], title)
	ok, _, err := procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&nid)))
	if ok == 0 {
		return procErr("Shell_NotifyIconW(NIM_MODIFY)", err)
	}
	return nil
}
