package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/NaiZi899/Naibox/internal/boot"
)

// Version 桌面壳版本（发布构建可用 -ldflags "-X .../desktop.Version=x.y.z" 覆盖）。
//
// 这里的值是**开发期默认值**：release.yml 用 tag 名注入真实版本
// （见该文件 Build desktop shell 一步）。两者不一致时以注入值为准 ——
// 所以本地 go build 出来的二进制显示的是这个默认值，别拿它判断发布版本。
var Version = "1.2.0"

// defaultServiceName 配置里没写 service_name 时的身份标识。
//
// 与 internal/server.ServiceName 的默认值一致；这里刻意不 import server
// 只为取一个常量 —— 桌面壳不该拖入整个 handler 包的编译依赖。
const defaultServiceName = "omni2api"

// ErrInstanceHandoff 已有实例占着同一个数据根，但身份校验或唤醒失败。
//
// 与"干净让位"（acquireInstance 返回 nil）严格区分：让位是正常路径，
// 这个错误要让用户看到一句话并以非零码退出 —— 静默消失等于"双击没反应"。
var ErrInstanceHandoff = errors.New("检测到已有实例，但无法唤醒")

// Options 桌面壳启动参数（对应 cmd/desktop 的命令行 flag）。
type Options struct {
	// ConfigPath -config。空 = 数据根下的 config.json（且允许旧布局迁移）。
	ConfigPath string
	// DataDir -data-dir。空 = %APPDATA%\NaiBox（或 portable.flag 的 exe 目录）。
	DataDir string
	// Mode "window" | "browser"。
	Mode string
	// Port -port。0 = 自动（7863 起顺延）。
	Port int
	// Console -console：保留控制台。
	Console bool
	// AutoStart 由开机自启项拉起（决定是否把窗口抢到前台）。
	AutoStart bool
	// Args 本次启动的原始命令行参数（不含程序名）。托盘「重启服务」用它
	// 以**同样的参数**拉起新进程 —— 重启后配置路径、模式、端口都保持不变。
	Args []string
	// RestartWait 由「重启服务」拉起的子进程置位：等旧进程释放单实例锁，
	// 而不是把它当成"已有实例"去唤醒。见 acquireInstance 的注释。
	RestartWait bool
	// Stderr 控制台日志出口。nil = os.Stderr。
	Stderr io.Writer
	// Exit 进程退出出口（致命错误时用）。
	//
	// # 为什么不在这里直接 os.Exit
	//
	// internal/desktop 内禁止裸 os.Exit（唯一例外是单实例让位，而那条路径
	// 其实也只是 return）。把退出动作注入进来，好处是测试/嵌入场景可以
	// 拦下"程序要死了"这件事，而不是让一个库函数擅自结束宿主进程。
	// 空 = 不退出（只记录），由调用方决定。
	Exit func(code int)
}

// app 一次桌面会话的全部状态。
type app struct {
	opt    Options
	layout Layout
	log    *log.Logger
	logw   *DailyLogWriter

	mode    string
	service string
	version string

	cancelAll context.CancelFunc

	mu       sync.Mutex
	rt       *boot.Runtime
	cancelRt context.CancelFunc
	addr     string

	view     WindowView
	tray     trayHandle
	guard    *instanceGuard
	shutOnce sync.Once

	// winState window.json 的唯一读写入口（窗口几何 + 关窗行为 + 气泡标记）。
	winState *windowStore
}

// Run 启动桌面壳：解析数据根 → 迁移旧布局 → 抢单实例 → 起服务 → 起托盘/窗口 → 等退出 → 收尾。
//
// 返回值只在"启动失败"时非 nil；正常退出（用户关窗/托盘退出/第二实例让位）返回 nil。
func Run(ctx context.Context, opt Options) error {
	if opt.Stderr == nil {
		opt.Stderr = os.Stderr
	}
	if opt.Exit == nil {
		opt.Exit = func(int) {}
	}

	layout, err := ResolveLayout(opt.DataDir, opt.ConfigPath)
	if err != nil {
		return err
	}
	if opt.Mode == "window" && !opt.Console {
		hideConsole()
	}

	// 日志最先就绪：之后任何一步失败都必须留下证据（桌面形态没有控制台，
	// 日志文件是用户唯一能提供的诊断材料）。
	logw := NewDailyLogWriter(layout.LogDir, opt.Stderr)
	a := &app{
		opt:     opt,
		layout:  layout,
		log:     log.New(logw, "", log.LstdFlags|log.Lmicroseconds),
		logw:    logw,
		mode:    normalizeMode(opt.Mode),
		version: Version,
	}
	defer logw.Close() // 幂等：正常退出路径里 shutdown 已经关过一次

	// 顺序是硬要求：先建目录 → 再迁移旧布局 → 最后才生成默认配置。
	// 反过来的话（先 EnsureConfig），迁移会看到"目标已有 config.json"，
	// 老用户的账号就被一份全新的默认配置挡在门外。
	if err := layout.EnsureDirs(); err != nil {
		a.log.Printf("准备数据根失败：%v", err)
		return err
	}
	a.log.Printf("NaiBox desktop %s 启动", a.version)
	a.log.Printf("数据根 %s（portable=%v）；配置 %s", layout.DataDir, layout.Portable, layout.ConfigPath)
	a.winState = newWindowStore(layout, a.log)
	a.log.Printf("关窗行为：%s（window.json 的 close_behavior）", a.winState.Get().CloseBehavior.describe())

	// 旧布局迁移：决定老用户升级后账号是否还在。
	mig, err := MigrateLegacy(layout, opt.ConfigPath != "", a.log)
	if err != nil {
		// 迁移失败不阻断启动：服务照跑，用户至少还能手动把文件搬过来。
		a.log.Printf("旧布局迁移未完成（服务继续启动）：%v", err)
	} else if mig.Migrated && mig.Source != "" {
		a.log.Printf("迁移摘要：来自 %s，复制 %d 个文件，保留 %d 项", mig.Source, len(mig.Copied), len(mig.Kept))
	}

	created, err := layout.EnsureConfig()
	if err != nil {
		a.log.Printf("生成默认配置失败：%v", err)
		return err
	}
	if created != "" {
		a.log.Printf("首次启动：已生成默认配置 %s（api_key 为随机值，可在 /ui 里查看）", created)
	}

	a.service = readServiceName(layout.ConfigPath)
	a.warnIfBoundToAllInterfaces(layout.ConfigPath)

	// 窗口能力检测放在最前面：WebView2 缺失时不是"建窗失败"，而是
	// 一开始就走浏览器模式 —— 用户看到的差别是"没有窗口"，不是白屏。
	if a.mode == "window" {
		if ok, reason := canShowWindow(); !ok {
			a.log.Printf("内嵌窗口不可用：%s → 回落浏览器模式", reason)
			messageBox("NaiBox 桌面版",
				"内嵌窗口不可用："+reason+"\n\n本次改用浏览器模式，网关服务照常运行。\n"+
					"想要窗口模式请安装 WebView2 运行时（Microsoft Edge WebView2 Runtime）。",
				mbIconWarning)
			a.mode = "browser"
		} else {
			a.log.Printf("%s，使用内嵌窗口", reason)
		}
	}

	// 自我替换式重启时给旧进程 30 秒释放单实例锁的窗口：
	// 旧进程要先落盘（Flush）、优雅停机（srv.Shutdown 5s）、跑清理链、
	// 删 runtime.json，实测都在 1 秒级，30 秒是给慢磁盘/杀软留的余量。
	waitForLock := time.Duration(0)
	if opt.RestartWait {
		waitForLock = 30 * time.Second
		a.log.Printf("由「重启服务」拉起：等待旧实例释放单实例锁")
	}

	guard, first, err := acquireInstance(layout, logw.Path(), a.service, waitForLock, a.log, a.activate)
	if err != nil {
		// 只有"已有实例但唤不醒"会走到这里（互斥体建不出来是另一条
		// 降级路径，返回 isFirst=true、err=nil）。用户已经在弹框里看过
		// 说明，这里再把摘要写进日志并以非零码退出。
		a.log.Printf("启动中止：%v", err)
		logw.Close()
		return err
	}
	if !first {
		// 第二实例：已请求主实例显示窗口，本进程干净退出（不走 os.Exit）。
		a.log.Printf("已有实例在运行，本次启动让位")
		logw.Close()
		return nil
	}
	a.guard = guard
	defer guard.Close()

	if err := a.run(ctx); err != nil {
		a.log.Printf("启动失败：%v", err)
		a.cleanup()
		return err
	}
	return nil
}

// run 起服务并进入事件等待。
func (a *app) run(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	a.cancelAll = cancel
	defer cancel()

	port, err := PickPort(a.opt.Port, a.log)
	if err != nil {
		return err
	}
	if _, err := a.startRuntime(ctx, port); err != nil {
		return err
	}

	a.startTray()
	defer a.stopTray()

	if a.mode == "window" {
		if err := a.openWindow(); err != nil {
			a.log.Printf("内嵌窗口创建失败：%v → 回落浏览器模式", err)
			a.mode = "browser"
		}
	}
	if a.mode == "browser" {
		a.openBrowser()
	}

	if a.view != nil {
		// 托盘退出 / ctx 取消 → 关窗，让 Run 返回。
		go func() {
			<-ctx.Done()
			a.view.Close()
		}()
		// 窗口消息循环必须在主 goroutine：go-webview2 的 Run 里跑的是
		// 该线程自己的 GetMessage 循环。用户关窗时它返回，即"用户要退出"。
		a.view.Run()
		a.log.Printf("窗口已关闭")
	} else {
		<-ctx.Done()
	}

	a.shutdown()
	return nil
}

// startRuntime 装配并启动网关服务。
func (a *app) startRuntime(parent context.Context, port int) (*boot.Runtime, error) {
	rctx, cancel := context.WithCancel(parent)
	readyCh := make(chan string, 1)

	rt, err := boot.Run(rctx, boot.Options{
		Mode:       boot.ModeDesktop,
		ConfigPath: a.layout.ConfigPath,
		DataDir:    a.layout.DataDir,
		Port:       port,
		Logger:     a.log,
		OnReady: func(addr string, _ *boot.Runtime) {
			select {
			case readyCh <- addr:
			default:
			}
		},
		Fatalf: a.fatalf,
	})
	if err != nil {
		cancel()
		return nil, err
	}

	// OnReady 一定在 Run 返回前被调用（boot.Run 里先 Listen、再回调、
	// 再 return），所以这里读得到实际地址。兜底用 rt.Addr。
	//
	// ⚠ 就绪判据只有"Run 已返回 + OnReady 给了 addr"，**绝不能**改成
	// "去请求一次 /healthz，200 就算就绪"。那个接口在账号池为空（新装、
	// 账号全禁用/全冷却）时返回的是 **503**，而且它的语义是"现在能不能
	// 受理聊天请求"，不是"进程是否已监听"。拿它当就绪信号会让全新安装
	// 永远起不来。同理第二实例的身份校验只看 body 的 service 字段、
	// 不看状态码（见 ProbeService）。
	addr := rt.Addr
	select {
	case got := <-readyCh:
		if got != "" {
			addr = got
		}
	default:
	}
	addr = normalizeAddr(addr)

	a.mu.Lock()
	a.rt, a.cancelRt, a.addr = rt, cancel, addr
	a.mu.Unlock()

	info := RuntimeInfo{
		PID:       os.Getpid(),
		Addr:      addr,
		Mode:      a.mode,
		Version:   a.version,
		StartedAt: time.Now().Format(time.RFC3339),
	}
	if err := a.layout.WriteRuntime(info); err != nil {
		a.log.Printf("写 runtime.json 失败（不影响服务）：%v", err)
	}
	a.log.Printf("控制台地址 http://%s/ui", addr)
	a.refreshTrayTooltip()
	return rt, nil
}

// openWindow 建内嵌窗口。
func (a *app) openWindow() error {
	if a.winState.Get().CloseBehavior == CloseMinimize && a.tray == nil {
		// 没有托盘就没有恢复入口：藏起来的窗口只能靠杀进程找回。
		// 这里降级成"每次询问"，让用户至少还能选退出。
		a.log.Printf("托盘不可用：本次「最小化到托盘」降级为「每次询问」（藏起来的窗口没有恢复入口）")
	}
	v, err := newView(a.layout, a.uiURL(), a.winState, a.log)
	if err != nil {
		return err
	}
	a.view = v
	// 装关窗策略：Windows 上就是子类化窗口过程（WM_CLOSE → 询问/隐藏/退出）。
	// 必须在 Run 之前 —— Run 里跑的是阻塞的消息循环，之后就没机会了。
	v.ConfigureClose(a.closePolicy())
	if a.opt.AutoStart {
		// 开机自启时不抢前台：用户刚登录，桌面还在加载。
		a.log.Printf("由开机自启拉起：窗口不前置")
	}
	return nil
}

// closePolicy 把桌面壳的行为接进窗口过程。
//
// 这些回调大多运行在窗口消息循环线程上，所以这里只做"读内存 + 转调"，
// 真正的活儿（日志、气泡、落盘）都在被调用的函数里甩出去。
func (a *app) closePolicy() ClosePolicy {
	return ClosePolicy{
		// 每次 WM_CLOSE 现取：用户在托盘里改完设置，下一次点 X 立刻生效，
		// 不需要重启窗口。
		Behavior: a.effectiveCloseBehavior,
		Ask: func(hwnd uintptr) bool {
			minimize := askMinimizeToTray(hwnd)
			choice := "退出"
			if minimize {
				choice = "最小化到托盘"
			}
			a.log.Printf("关窗询问：用户选择 %s", choice)
			return minimize
		},
		Hidden: a.minimizedToTray,
		// 系统关机/注销：抢在进程被结束之前落盘。取消 ctx 就是 boot 里
		//「<-ctx.Done() → Runtime.Shutdown」那条路（内部先 Flush 再停机）。
		Quit: func() {
			a.log.Printf("系统正在关机/注销：开始落盘收尾")
			if a.cancelAll != nil {
				a.cancelAll()
			}
		},
	}
}

// effectiveCloseBehavior 本次会话真正生效的关窗行为。
func (a *app) effectiveCloseBehavior() CloseBehavior {
	b := normalizeCloseBehavior(string(a.winState.Get().CloseBehavior))
	if b == CloseMinimize && a.tray == nil {
		// 托盘没起来（图标坏了/初始化失败）：藏起来等于丢窗口。
		return CloseAsk
	}
	return b
}

// minimizedToTray 窗口刚被藏进托盘：记日志、弹一次气泡、更新托盘提示。
//
// 从 ClosePolicy.Hidden 进来，跑在独立 goroutine 上，不占消息循环线程。
func (a *app) minimizedToTray() {
	a.log.Printf("已最小化到托盘：窗口已隐藏，进程与网关继续在后台运行（托盘「打开控制台」恢复）")
	if a.tray != nil {
		a.tray.SetTooltip(a.trayTooltip() + "\n窗口已隐藏到托盘")
	}
	if a.winState.Get().TrayHintShown {
		return
	}
	if err := a.winState.MarkTrayHintShown(); err != nil {
		a.log.Printf("记录托盘提示状态失败（下次还会再弹一次）：%v", err)
	}
	// 气泡在 Win10/11 可能被"专注助手"静音，所以上面那行日志才是主通道，
	// 气泡只是让用户**当场**看见的那一下。
	if err := showTrayBalloon("NaiBox 仍在后台运行",
		"窗口已最小化到通知区域，网关继续工作。\n右键托盘图标 →「打开控制台」可以恢复窗口。"); err != nil {
		a.log.Printf("托盘气泡未能显示（%v）", err)
	} else {
		a.log.Printf("托盘气泡已弹出（只在第一次隐藏时弹，之后靠窗口提示与日志）")
	}
}

// setCloseBehavior 托盘菜单切换关窗行为。
func (a *app) setCloseBehavior(b CloseBehavior) {
	b = normalizeCloseBehavior(string(b))
	if err := a.winState.SetCloseBehavior(b); err != nil {
		a.log.Printf("保存关窗行为失败：%v", err)
		messageBox("NaiBox", "保存关窗行为失败：\n"+err.Error(), mbIconError)
	}
	a.log.Printf("关窗行为已设为：%s", b.describe())
	a.syncTrayCloseChecks(b)
	if a.mode != "window" {
		a.log.Printf("（当前是浏览器模式，关窗行为不影响托盘「退出」）")
	}
}

// syncTrayCloseChecks 三个勾选项互斥。
func (a *app) syncTrayCloseChecks(b CloseBehavior) {
	if a.tray == nil {
		return
	}
	a.tray.SetChecked(actCloseAsk, b == CloseAsk)
	a.tray.SetChecked(actCloseMinimize, b == CloseMinimize)
	a.tray.SetChecked(actCloseQuit, b == CloseQuit)
}

// openBrowser 用系统默认浏览器打开控制台。
func (a *app) openBrowser() {
	url := a.uiURL()
	if err := openURL(url); err != nil {
		a.log.Printf("打开浏览器失败（%v）：请手动访问 %s", err, url)
		return
	}
	a.log.Printf("已在浏览器中打开 %s", url)
}

// activate 另一实例请求"把控制台显示出来"。
//
// 与托盘动作同理：这段跑在单实例等待 goroutine 上，panic 会带走整个进程。
func (a *app) activate() {
	defer func() {
		if r := recover(); r != nil {
			a.log.Printf("处理实例唤醒时 panic（已拦截）：%v", r)
		}
	}()
	if a.view != nil {
		a.view.Show()
		return
	}
	a.openBrowser()
}

// startTray 起托盘。失败只记日志（服务照跑）。
func (a *app) startTray() {
	iconBytes, err := trayIconBytes(a.layout)
	if err != nil {
		// 没图标的托盘等于没有托盘（图标不可见就点不到菜单），
		// 所以这里按"托盘不可用"处理，而不是带着空图标硬起。
		a.log.Printf("托盘图标不可用：%v", err)
		a.warnNoTrayExit(err)
		return
	}
	h, err := startTray(newTraySpec(a.trayTooltip(), trayState{
		Autostart:     autostartEnabled(),
		CloseBehavior: a.winState.Get().CloseBehavior,
	}), iconBytes, a.onTrayAction)
	if err != nil {
		a.log.Printf("托盘不可用（不影响服务）：%v", err)
		a.warnNoTrayExit(err)
		return
	}
	a.tray = h
	a.log.Printf("托盘已就绪")
}

// warnNoTrayExit 浏览器模式下托盘是唯一退出入口，没有它要明确告诉用户怎么退出。
func (a *app) warnNoTrayExit(cause error) {
	if a.mode != "browser" || a.opt.Console {
		return
	}
	messageBox("NaiBox 桌面版",
		"托盘不可用："+cause.Error()+"\n\n网关仍在运行，但当前没有退出入口。\n"+
			"请在任务管理器里结束 omni2api-desktop.exe；\n"+
			"以 -console 启动时按 Ctrl+C 也会正常落盘退出。",
		mbIconWarning)
}

func (a *app) stopTray() {
	if a.tray != nil {
		a.tray.Quit()
	}
}

// onTrayAction 托盘菜单点击。运行在托盘消息循环派生的 goroutine 上，可阻塞。
//
// 整个分发带 recover：托盘动作是"界面侧"的操作（打开目录、打 zip、改注册表），
// 其中任何一处 panic 都不该把正在服务的网关带走 —— 那是最坏的结果
// （用户点了一下"备份数据"，网关了）。这里拦下来记日志，继续活着。
func (a *app) onTrayAction(act trayAction) {
	defer func() {
		if r := recover(); r != nil {
			a.log.Printf("托盘动作 %d 发生 panic（已拦截，服务未受影响）：%v", act, r)
		}
	}()
	switch act {
	case actOpenConsole:
		if a.view != nil {
			a.view.Show()
			return
		}
		a.openBrowser()
	case actOpenBrowser:
		a.openBrowser()
	case actOpenDataDir:
		if err := openPath(a.layout.DataDir); err != nil {
			a.log.Printf("打开数据目录失败：%v", err)
			messageBox("NaiBox", "打开数据目录失败：\n"+a.layout.DataDir+"\n\n"+err.Error(), mbIconError)
		}
	case actOpenTodayLog:
		logPath := a.logw.Path()
		if !fileExists(logPath) {
			// 当天还没有日志（刚启动且没有输出）：打开目录而不是报错。
			_ = openPath(a.layout.LogDir)
			return
		}
		if err := openPath(logPath); err != nil {
			a.log.Printf("打开日志失败：%v", err)
		}
	case actBackupNow:
		a.backupNow()
	case actToggleAutostart:
		a.toggleAutostart()
	case actCloseAsk:
		a.setCloseBehavior(CloseAsk)
	case actCloseMinimize:
		a.setCloseBehavior(CloseMinimize)
	case actCloseQuit:
		a.setCloseBehavior(CloseQuit)
	case actRestartService:
		a.restart()
	case actQuit:
		a.log.Printf("托盘：退出")
		if a.cancelAll != nil {
			a.cancelAll()
		}
		if a.view != nil {
			a.view.Close()
		}
	}
}

// backupNow 打包备份并打开备份目录。
func (a *app) backupNow() {
	path, err := a.layout.Backup()
	if err != nil {
		a.log.Printf("备份失败：%v", err)
		messageBox("NaiBox 备份失败", err.Error(), mbIconError)
		return
	}
	a.log.Printf("已备份到 %s", path)
	if err := openPath(a.layout.BackupDir); err != nil {
		a.log.Printf("打开备份目录失败：%v", err)
	}
}

// toggleAutostart 切换开机自启（以注册表实际状态为准，不信内存）。
func (a *app) toggleAutostart() {
	enable := !autostartEnabled()
	exe, err := os.Executable()
	if err != nil {
		a.log.Printf("取 exe 路径失败，无法设置开机自启：%v", err)
		messageBox("NaiBox", "设置开机自启失败："+err.Error(), mbIconError)
		return
	}
	if err := setAutostart(enable, exe); err != nil {
		a.log.Printf("设置开机自启失败：%v", err)
		messageBox("NaiBox", "设置开机自启失败：\n"+err.Error(), mbIconError)
		return
	}
	a.log.Printf("开机自启：%v（%s）", enable, exe)
	if a.tray != nil {
		a.tray.SetChecked(actToggleAutostart, enable)
	}
}

// restart 托盘「重启服务」：**自我替换进程**，不在同进程里二次 boot.Run。
//
// # 为什么不用"同进程再 Run 一次"
//
// Runtime.Shutdown 会跑完整条清理链：Flush 落盘、关日志 sink、销毁 WASM
// 签名器、detach。这些清理**没有逆操作**，二次 Run 能不能把它们全部重建
// 出来没有验证过。而"重启"一旦半途失败，结果就是服务停摆 —— 比不提供
// 重启糟得多。所以选择进程级重启：新进程从零装配，旧进程只负责收尾退出。
//
// # 顺序（这是这类重启最容易错的地方）
//
//  1. 起新进程，带 -restart-wait（它会轮询等锁，不把"锁被占"当让位）
//  2. 旧进程 cancel → rt.Shutdown 落盘 → 释放监听端口
//  3. Run 返回时 defer 里的 guard.Close() **释放单实例锁**
//  4. 旧进程退出；新进程拿到锁 → 成为主实例 → 绑端口
//
// 第 3 步必须在退出前发生：如果先退出再靠内核回收锁，新进程很容易在
// 这几十毫秒里看到"锁被占"，然后去唤醒一个正在消失的进程 —— 表现是
// 两边都没了。反过来，如果先释放锁再收尾，新进程会抢到端口/写同一份
// state.json。所以顺序只能是「先落盘停机，再释放锁，最后退出」。
func (a *app) restart() {
	exe, err := os.Executable()
	if err != nil {
		a.log.Printf("重启失败：取自身路径失败：%v", err)
		messageBox("NaiBox 重启失败", "无法取到程序自身路径：\n"+err.Error(), mbIconError)
		return
	}

	args := make([]string, 0, len(a.opt.Args)+2)
	for _, arg := range a.opt.Args {
		// 去掉上一轮可能带进来的标记，避免层层叠加。
		if arg == "-restart-wait" || strings.HasPrefix(arg, "-restart-wait=") {
			continue
		}
		args = append(args, arg)
	}
	args = append(args, "-restart-wait")

	cmd := exec.Command(exe, args...)
	cmd.Dir = cwdOr(".")
	// Stdout/Stderr 留空 = 子进程输出丢弃（桌面形态本来也没有控制台；
	// 它自己会写 %APPDATA% 的日志文件）。写成继承父进程的句柄的话，
	// 父进程退出后子进程会持有一个已失效的控制台句柄。
	if err := cmd.Start(); err != nil {
		// 起不来就**不要动现有服务**：让它继续跑，比停摆强。
		a.log.Printf("重启失败：无法启动新进程：%v（服务继续运行）", err)
		messageBox("NaiBox 重启失败",
			"无法启动新进程：\n"+err.Error()+"\n\n当前服务继续运行，未受影响。", mbIconError)
		return
	}
	a.log.Printf("重启：已拉起新进程 pid=%d，本进程开始收尾并释放单实例锁", cmd.Process.Pid)

	// 触发 run() 退出 → shutdown()（落盘 + 停机 + 删 runtime.json）
	// → Run 返回 → defer 里的 guard.Close() 释放锁 → 进程退出。
	if a.cancelAll != nil {
		a.cancelAll()
	}
	if a.view != nil {
		a.view.Close()
	}
}

// fatalf 注入给装配层的致命错误出口。
//
// 行为对齐原来的 log.Fatalf（写完就死），但多做了三件事：写进日志文件、
// 弹一个用户看得见的框、清掉 runtime.json。
func (a *app) fatalf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	a.log.Printf("致命错误：%s", msg)
	// 先清 runtime.json：否则下次启动会把一个已死进程当成"活实例"。
	if err := a.layout.RemoveRuntime(); err != nil {
		a.log.Printf("清理 runtime.json 失败：%v", err)
	}
	a.stopTray()
	messageBox("NaiBox 启动失败", msg+"\n\n日志："+a.logw.Path(), mbIconError)
	a.opt.Exit(1)
}

// shutdown 正常退出路径。幂等。
func (a *app) shutdown() {
	a.shutOnce.Do(func() {
		a.log.Printf("正在退出（先落盘再停机）")
		if a.cancelAll != nil {
			a.cancelAll() // boot 内部的 <-ctx.Done() → rt.Shutdown()
		}

		a.mu.Lock()
		rt := a.rt
		cancelRt := a.cancelRt
		a.rt, a.cancelRt = nil, nil
		a.mu.Unlock()

		// 先取消这个 runtime 的 ctx（boot 内部的 <-ctx.Done() 会触发它自己的
		// Shutdown），再显式调一次 Shutdown 兜底 —— 两条路都幂等，但顺序
		// 保证了"落盘先于停机"由 boot 那条路径执行。
		if cancelRt != nil {
			cancelRt()
		}

		if rt != nil {
			rt.Shutdown() // 幂等：ctx 取消那条路径可能已经跑过
			done := make(chan struct{})
			go func() { rt.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(20 * time.Second):
				a.log.Printf("等待后台收尾超时（继续退出）")
			}
		}
		if err := a.layout.RemoveRuntime(); err != nil {
			a.log.Printf("删除 runtime.json 失败：%v", err)
		}
		a.stopTray()
		a.log.Printf("bye")
		a.logw.Close()
	})
}

// cleanup 启动失败后的收尾（与 shutdown 的区别：不等待、不重复落盘）。
func (a *app) cleanup() {
	if a.cancelAll != nil {
		a.cancelAll()
	}
	_ = a.layout.RemoveRuntime()
	a.stopTray()
	a.logw.Close()
}

// uiURL 控制台地址。
func (a *app) uiURL() string {
	a.mu.Lock()
	addr := a.addr
	a.mu.Unlock()
	return "http://" + addr + "/ui"
}

func (a *app) trayTooltip() string {
	a.mu.Lock()
	addr := a.addr
	a.mu.Unlock()
	return fmt.Sprintf("omni2api %s\n控制台 http://%s/ui", a.version, addr)
}

func (a *app) refreshTrayTooltip() {
	if a.tray != nil {
		a.tray.SetTooltip(a.trayTooltip())
	}
}

// warnIfBoundToAllInterfaces 只在用户**显式**写了非回环 host 时提醒。
//
// # 口径为什么这样定
//
// boot 的 resolveListen 在 ModeDesktop 下会把**空 host** 收敛成
// 127.0.0.1（`:7863` → `127.0.0.1:7863`），显式 `0.0.0.0` 或具体网卡
// 地址则原样尊重。也就是说默认配置已经是安全的，这里再喊"你在监听所有
// 网卡"就是假警报 —— 会把日志噪音教成"忽略它"。
//
// 真正需要提醒的是用户**明确写了** `0.0.0.0` / `::` / 某个内网 IP：
// 那时 `/ui` 对回环来源自动注入 api_key 的便利会扩展到同网段任何人
// （打开 http://<内网IP>:7863/ui 直接进控制台）。提醒但不拦 —— 那是
// 用户的明确意图（比如他就是要给局域网用）。
func (a *app) warnIfBoundToAllInterfaces(configPath string) {
	listen := readConfigString(configPath, "listen")
	host, _, err := net.SplitHostPort(listen)
	if err != nil || host == "" {
		// 没有 listen 字段、或写的是纯 ":port"（空 host）：
		// 前者由 boot 用默认值，后者会被 boot 收敛到回环，都不需要提醒。
		return
	}
	if strings.EqualFold(host, "localhost") {
		return
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil && ip.IsLoopback() {
		return
	}
	a.log.Printf("提醒：配置 listen=%q 显式绑定了非回环地址 %s；"+
		"同网段的其他人可以直接打开 http://<本机内网IP>:%s/ui 访问控制台。"+
		"只想本机使用请改成 127.0.0.1:7863", listen, host, portOf(listen))
}

// portOf 取 listen 里的端口段（取不到时给个提示性占位）。
func portOf(listen string) string {
	if _, port, err := net.SplitHostPort(listen); err == nil && port != "" {
		return port
	}
	return "7863"
}

// normalizeMode 只认 window / browser，其它一律 window（默认体验）。
func normalizeMode(m string) string {
	if strings.EqualFold(strings.TrimSpace(m), "browser") {
		return "browser"
	}
	return "window"
}

// normalizeAddr 把监听地址归一成"能用浏览器打开"的形式。
//
// net.Listen("tcp", ":7863") 的实际地址是 "[::]:7863"，直接拼 URL 会得到
// http://[::]:7863/ui —— 浏览器能打开，但走 IPv6 且部分系统代理会拦。
// 桌面形态只需本机访问，统一落回 127.0.0.1。
func normalizeAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// readServiceName 读配置里的 service_name（缺失/损坏回落默认值）。
func readServiceName(configPath string) string {
	if v := readConfigString(configPath, "service_name"); strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return defaultServiceName
}

// readConfigString 从任意 JSON 里取一个**顶层**字符串字段。
//
// 刻意不 import cmd/server 的 Config：桌面壳只需要三个字段
// （service_name / listen / 未来的字段），把 4000 行的配置结构体拉进来
// 会让"配置结构一改，桌面壳跟着重编"成为必然。JSON 宽松读，缺字段就空串。
func readConfigString(configPath, key string) string {
	body, err := os.ReadFile(configPath)
	if err != nil {
		return ""
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	raw, ok := probe[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}
