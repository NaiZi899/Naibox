//go:build windows

package desktop

import (
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

const (
	// eventModifyState 打开已有事件并 SetEvent 所需的最小权限。
	eventModifyState = 0x0002
	// waitObject0 WaitForSingleObject 的"已触发"返回值。
	waitObject0 = 0
)

// instanceGuard 单实例句柄。
//
// # 为什么用"命名互斥体 + 命名事件"两个对象
//
// 互斥体只回答"有没有别的实例"；用户真正要的是"再点一次 exe，把已经
// 在后台的那个窗口叫出来"。事件是跨进程通知里最轻、最不容易被安全软件
// 拦下的一招（不需要窗口句柄、不需要网络、不需要权限提升）。
type instanceGuard struct {
	mutex windows.Handle
	event windows.Handle
	stop  chan struct{}
	once  sync.Once
}

// acquireInstance 抢单实例。
//
// 返回值：
//   - (guard, true, nil)  本进程是主实例
//   - (nil,   false, nil) 已有主实例；已请求它显示窗口，本进程应当退出
//   - (nil,   false, err) 已有主实例但**无法确认/唤醒**它：调用方应当提示用户并失败退出
//   - (nil,   true,  nil) 单实例机制不可用（互斥体建不出来），仍然继续启动
//
// # 名字按数据根区分
//
// 名字里带 DataDirHash：portable 版（数据在 U 盘）与 %APPDATA% 版可以
// 同时运行、各自打自己的数据，不会互相顶掉。
//
// # waitForLock：托盘"重启服务"用的让位窗口
//
// 自我替换式重启的顺序是「新进程先起来等锁 → 旧进程落盘收尾、释放锁、退出」。
// 新进程带着 -restart-wait 启动时会把 waitForLock 设成正数，于是"锁被占"
// 不再按让位处理，而是轮询到旧进程释放为止。没有这条，新进程会立刻判定
// "已有实例"并去唤醒一个正在退出的进程 —— 结果是两边都没了。
//
// # 为什么"唤不醒"要失败退出，而且必须弹框
//
// 锁被一个存活进程持有、但 runtime.json 的 pid 校验或 /healthz 身份校验
// 失败时，最省事的做法是"静默让位"：新进程退出，用户看到的是双击没反应
// （日志里才有一行"无法唤醒已有实例"）。另一种省事做法是"那就起第二个
// 服务"—— 同一数据根两个进程写 state.json，数据风险不可接受。
//
// 所以选择第三条：弹一个用户看得懂的框（说明现状 + 数据根 + 日志路径），
// 然后以非零码退出。原则是**宁可让用户看到一句看得懂的话，也不要让他
// 看到双击没反应**。
func acquireInstance(l Layout, logPath, expectService string, waitForLock time.Duration, logger *log.Logger, onActivate func()) (*instanceGuard, bool, error) {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	if onActivate == nil {
		onActivate = func() {}
	}
	base := "NaiBox-" + DataDirHash(l.DataDir)
	deadline := time.Now().Add(waitForLock)
	announcedWait := false

	for {
		mu, existed, ns, err := createMutexNamed(base)
		if err != nil {
			// 单实例是体验增强，不是启动前提：拿不到就让它跑，
			// 总比"什么都没发生"好（用户至少能看到窗口）。
			logger.Printf("单实例互斥体创建失败（继续启动，可能出现双实例）：%v", err)
			return nil, true, nil
		}
		if !existed {
			ev, _, err := createEventNamed(base + "-activate")
			if err != nil {
				logger.Printf("激活事件创建失败（不影响主实例，但第二实例叫不醒窗口）：%v", err)
				return &instanceGuard{mutex: mu, stop: make(chan struct{})}, true, nil
			}
			g := &instanceGuard{mutex: mu, event: ev, stop: make(chan struct{})}
			go g.wait(onActivate, logger)
			if announcedWait {
				logger.Printf("旧实例已释放单实例锁，本进程接管（%s%s）", ns, base)
			} else {
				logger.Printf("单实例锁已持有（%s%s）", ns, base)
			}
			return g, true, nil
		}

		// 锁被占用。
		windows.CloseHandle(mu)
		if waitForLock > 0 && time.Now().Before(deadline) {
			if !announcedWait {
				logger.Printf("检测到旧实例正在退出（%s%s），等待它释放单实例锁（最多 %.0fs）", ns, base, waitForLock.Seconds())
				announcedWait = true
			}
			time.Sleep(200 * time.Millisecond)
			continue
		}

		logger.Printf("检测到已有实例（%s%s）", ns, base)
		if herr := signalExisting(l.DataDir, expectService, base+"-activate", logger); herr != nil {
			logger.Printf("检测到已有实例但无法唤醒：%v；为避免两个进程同时写同一份数据，本次启动中止", herr)
			messageBox("NaiBox 桌面版",
				"检测到已有实例，但无法唤醒它：\n"+herr.Error()+"\n\n"+
					"为避免两个进程同时写同一份数据（可能损坏账号状态），本次启动已取消。\n\n"+
					"数据根："+l.DataDir+"\n"+
					"日志："+logPath+"\n\n"+
					"处理办法：用任务管理器结束残留的 omni2api-desktop.exe，再重新打开。",
				mbIconWarning)
			return nil, false, fmt.Errorf("%w：%v（数据根 %s，日志 %s）", ErrInstanceHandoff, herr, l.DataDir, logPath)
		}
		return nil, false, nil
	}
}

// Close 释放单实例对象。幂等。
func (g *instanceGuard) Close() {
	if g == nil {
		return
	}
	g.once.Do(func() {
		close(g.stop)
		if g.event != 0 {
			windows.CloseHandle(g.event)
		}
		if g.mutex != 0 {
			windows.CloseHandle(g.mutex)
		}
	})
}

// wait 阻塞等激活事件。
//
// 500ms 一轮而不是 INFINITE：进程要退出时得能看见 stop 通道，
// 用 INFINITE 会卡住一个 goroutine 到关机（无害但脏，且 Close 里的
// CloseHandle 会让它变成 wait-on-invalid-handle 的怪日志）。
func (g *instanceGuard) wait(onActivate func(), logger *log.Logger) {
	if g.event == 0 {
		return
	}
	for {
		select {
		case <-g.stop:
			return
		default:
		}
		st, err := windows.WaitForSingleObject(g.event, 500)
		if err != nil {
			logger.Printf("等待激活事件失败：%v", err)
			return
		}
		if st == waitObject0 {
			logger.Printf("收到另一个实例的唤醒请求")
			onActivate()
		}
	}
}

// createMutexNamed 建互斥体，返回是否"已存在"。
//
// 先试 Global\ 再退 Local\：Global 命名空间在部分受控环境（无
// SeCreateGlobalPrivilege 的普通用户）会 ERROR_ACCESS_DENIED，
// 那时退到 Local\ —— 语义从"整机一个"降级为"本登录会话一个"，
// 对桌面应用完全够用。
func createMutexNamed(name string) (windows.Handle, bool, string, error) {
	var lastErr error
	for _, ns := range []string{`Global\`, `Local\`} {
		ptr, err := windows.UTF16PtrFromString(ns + name)
		if err != nil {
			return 0, false, ns, err
		}
		h, err := windows.CreateMutex(nil, false, ptr)
		switch {
		case err == nil:
			return h, false, ns, nil
		case err == windows.ERROR_ALREADY_EXISTS && h != 0:
			return h, true, ns, nil
		default:
			if h != 0 {
				windows.CloseHandle(h)
			}
			lastErr = err
		}
	}
	return 0, false, "", lastErr
}

// createEventNamed 建自动复位事件（供等待方 WaitForSingleObject）。
func createEventNamed(name string) (windows.Handle, string, error) {
	var lastErr error
	for _, ns := range []string{`Global\`, `Local\`} {
		ptr, err := windows.UTF16PtrFromString(ns + name)
		if err != nil {
			return 0, ns, err
		}
		h, err := windows.CreateEvent(nil, 0 /*auto-reset*/, 0, ptr)
		if err == nil || err == windows.ERROR_ALREADY_EXISTS {
			return h, ns, nil
		}
		if h != 0 {
			windows.CloseHandle(h)
		}
		lastErr = err
	}
	return 0, "", lastErr
}

// signalExisting 校验"活着的那个实例确实是自家服务"，然后触发激活事件。
//
// 三道门全过才挥手：
//  1. runtime.json 里的 pid 还活着（否则就是残留文件）
//  2. /healthz 的 service 字段等于本程序的服务名（否则端口上是别人）
//  3. 事件对象打得开
func signalExisting(dataDir, expectService, eventName string, logger *log.Logger) error {
	l := Layout{DataDir: dataDir}
	info, err := l.ReadRuntime()
	if err != nil {
		return fmt.Errorf("读 runtime.json：%w", err)
	}
	if !processAlive(info.PID) {
		return fmt.Errorf("runtime.json 里的 pid=%d 已退出（残留文件）", info.PID)
	}
	if err := ProbeService(info.Addr, expectService, 2*time.Second); err != nil {
		return fmt.Errorf("端口 %s 上不是本程序的服务：%w", info.Addr, err)
	}
	h, _, err := openEventNamed(eventName)
	if err != nil {
		return fmt.Errorf("打开激活事件：%w", err)
	}
	defer windows.CloseHandle(h)
	if err := windows.SetEvent(h); err != nil {
		return fmt.Errorf("触发激活事件：%w", err)
	}
	logger.Printf("已验证实例 pid=%d addr=%s，已请求它显示控制台", info.PID, info.Addr)
	return nil
}

func openEventNamed(name string) (windows.Handle, string, error) {
	var lastErr error
	for _, ns := range []string{`Global\`, `Local\`} {
		ptr, err := windows.UTF16PtrFromString(ns + name)
		if err != nil {
			return 0, ns, err
		}
		h, err := windows.OpenEvent(eventModifyState, false, ptr)
		if err == nil {
			return h, ns, nil
		}
		lastErr = err
	}
	return 0, "", lastErr
}
