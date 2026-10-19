// main.go omni2api 服务入口。
//
// 装配体一行都不在这里：它住在 `internal/boot`，因为桌面壳（cmd/desktop）
// 需要同一套装配，而 `package main` 无法被 import。本文件只负责
// 「解析 flag → 建立信号 ctx → 装配 → 等到服务停下」。
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/NaiZi899/Naibox/internal/boot"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Options 其余字段留零值即改造前的语义：数据根是当前工作目录，
	// 监听地址取配置值，致命错误照旧走 log.Fatalf。
	rt, err := boot.Run(ctx, boot.Options{ConfigPath: *cfgPath})
	if err != nil {
		// 默认 Fatalf 是 log.Fatalf（进程已在 Run 内部退出），这里只兜住
		// 「装配失败但不是致命判定」的情形，避免静默返回 0。
		log.Fatalf("boot: %v", err)
	}

	// 阻塞到 Serve 循环收束：正常关停打 "bye"，监听错误仍按原样 fatal。
	rt.Wait()
}
