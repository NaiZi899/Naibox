// Command desktop omni2api 的 Windows 桌面壳。
//
// # 它做什么
//
// 双击出窗口（内嵌 WebView2），托盘常驻，数据固定在 %APPDATA%\omni2api。
// 装配逻辑一行都不在这里 —— 全部经 internal/boot.Run，与 cmd/server 同源。
//
// # 发布构建
//
// 想要"双击不闪黑框"，用 GUI 子系统链接（否则会先出现一个控制台窗口，
// 代码里的 FreeConsole 只能把它收掉，仍会闪一下）：
//
//	GOOS=windows CGO_ENABLED=0 go build -trimpath ^
//	  -ldflags "-s -w -H=windowsgui -X github.com/NaiZi899/omni2api/internal/desktop.Version=0.1.0" ^
//	  -o dist/omni2api-desktop.exe ./cmd/desktop
//
// 本仓库的验收命令刻意不带 -H windowsgui（见 task），构建照样通过；
// 只是那种构建从资源管理器双击会带一个控制台窗口。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/NaiZi899/omni2api/internal/desktop"
)

func main() {
	cfgPath := flag.String("config", "", "配置文件路径（默认 <数据根>\\config.json；显式指定时不迁移旧布局）")
	dataDir := flag.String("data-dir", "", "数据根目录（默认 %APPDATA%\\omni2api；exe 同级有 portable.flag 时用 exe 目录）")
	mode := flag.String("mode", "window", "window=内嵌窗口（WebView2 缺失时自动回落 browser）| browser=只用系统浏览器")
	port := flag.Int("port", 0, "监听端口（默认 7863，被占用顺延到 7873，仍占用由系统分配）")
	console := flag.Bool("console", false, "保留控制台窗口，并把日志同时打到 stderr")
	autostart := flag.Bool("autostart", false, "由开机自启项拉起（内部使用，窗口不抢前台）")
	restartWait := flag.Bool("restart-wait", false, "由托盘「重启服务」拉起：等待旧进程释放单实例锁（内部使用）")
	showVersion := flag.Bool("version", false, "打印版本后退出")
	flag.Parse()

	if *showVersion {
		fmt.Printf("omni2api desktop %s\n", desktop.Version)
		return
	}

	// Ctrl+C / 关控制台 / 系统注销都要走同一条收尾路径（落盘在 boot 的 Shutdown 里）。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := desktop.Run(ctx, desktop.Options{
		ConfigPath:  *cfgPath,
		DataDir:     *dataDir,
		Mode:        *mode,
		Port:        *port,
		Console:     *console,
		AutoStart:   *autostart,
		RestartWait: *restartWait,
		// 原始参数交给「重启服务」用：自我替换时以同样的参数拉起新进程。
		Args:   os.Args[1:],
		Stderr: os.Stderr,
		Exit:   os.Exit,
	})
	if err != nil {
		// Run 内部已把细节写进日志与消息框；这里只保证退出码非零（脚本可判）。
		fmt.Fprintf(os.Stderr, "omni2api desktop: %v\n", err)
		os.Exit(1)
	}
}
