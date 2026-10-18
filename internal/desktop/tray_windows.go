//go:build windows

package desktop

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/energye/systray"
)

// trayBackend 基于 github.com/energye/systray 的托盘实现。
//
// # 为什么选它（而不是自写 Shell_NotifyIconW）
//
// 实测结论（见汇报）：它是**纯 Go**，Windows 分支只用 syscall + unsafe，
// `GOOS=windows CGO_ENABLED=0 go build` 直接过，满足"无 gcc/windres"的
// 构建约束；平台差异已经由 build tag 隔离好（非 Windows 才走 cgo 的
// darwin 分支与 dbus 的 linux 分支）。自写一版 300 行 Shell_NotifyIconW
// 的边际收益只有"少一个依赖"，却要自己处理菜单消息、DPI、任务栏重建
// （explorer.exe 重启后图标会消失，这是自写实现最常见的坑）。
type systrayTray struct {
	mu      sync.Mutex
	items   map[trayAction]*systray.MenuItem
	handle  map[trayAction]bool
	quitOne sync.Once
}

// startTray 起托盘。返回 error 表示托盘不可用（调用方只记日志，不影响服务）。
func startTray(spec traySpec, icon []byte, onAction func(trayAction)) (trayHandle, error) {
	t := &systrayTray{
		items:  map[trayAction]*systray.MenuItem{},
		handle: map[trayAction]bool{},
	}
	ready := make(chan error, 1)

	// report 只投递第一个结果；onReady 里再 panic 也不会把缓冲区堵死。
	report := func(err error) {
		select {
		case ready <- err:
		default:
		}
	}

	go func() {
		// 托盘代码跑在独立 goroutine 上，任何 panic 都会带走整个进程
		// （其中包括正在工作的网关）。托盘是锦上添花的东西，不能有这个
		// 破坏力 —— 所以在这里兜住，转成一条 error。
		defer func() {
			if r := recover(); r != nil {
				report(fmt.Errorf("托盘初始化 panic: %v", r))
			}
		}()

		// 托盘要有自己的消息循环线程：菜单消息只投递给创建窗口的那个线程。
		// 不 LockOSThread 的话 goroutine 可能被调度到别的 OS 线程，
		// GetMessageW 会拿不到消息 —— 表现是"菜单点了没反应"。
		runtime.LockOSThread()
		systray.Run(func() {
			defer func() {
				if r := recover(); r != nil {
					report(fmt.Errorf("构建托盘菜单 panic: %v", r))
				}
			}()
			if len(icon) > 0 {
				systray.SetIcon(icon)
			}
			systray.SetTooltip(spec.Tooltip)
			for _, item := range spec.Items {
				if item.SeparatorBefore {
					systray.AddSeparator()
				}
				var mi *systray.MenuItem
				if item.Checkable {
					mi = systray.AddMenuItemCheckbox(item.Title, item.Tooltip, item.Checked)
				} else {
					mi = systray.AddMenuItem(item.Title, item.Tooltip)
				}
				action := item.Action
				// Click 回调在消息循环线程执行：任何阻塞操作都必须甩到
				// 新 goroutine，否则托盘菜单会卡住（连"退出"都点不动）。
				mi.Click(func() { go onAction(action) })
				t.mu.Lock()
				t.items[action] = mi
				t.handle[action] = item.Checkable
				t.mu.Unlock()
			}
			report(nil)
		}, func() {})
	}()

	select {
	case err := <-ready:
		if err != nil {
			return nil, err
		}
		return t, nil
	case <-time.After(8 * time.Second):
		// 卡住也不致命：托盘起不来，服务照跑，用户还能用浏览器打开。
		return nil, errors.New("托盘初始化超时（消息循环未就绪）")
	}
}

func (t *systrayTray) SetChecked(action trayAction, checked bool) {
	t.mu.Lock()
	mi, ok := t.items[action]
	checkable := t.handle[action]
	t.mu.Unlock()
	if !ok || !checkable {
		return
	}
	if checked {
		mi.Check()
	} else {
		mi.Uncheck()
	}
}

func (t *systrayTray) SetTooltip(text string) {
	systray.SetTooltip(text)
}

func (t *systrayTray) Quit() {
	t.quitOne.Do(func() {
		// Quit 内部向托盘窗口 PostMessage(WM_CLOSE)：跨线程安全，
		// 且不会阻塞调用方（退出路径上不能在消息循环里等自己）。
		systray.Quit()
	})
}
