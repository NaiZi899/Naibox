// runtime.go Runtime 的生命周期：退出顺序、清理链、Serve 循环的收束。
//
// # 为什么这里有一张按指针索引的表
//
// `Runtime` 的契约（api.go）只导出 `Addr` / `Server` 两个字段，而 `Shutdown`
// 需要账号池、清理链与 Serve 循环的退出信号。Go 没有"给已有类型补私有字段"
// 的机制，也不允许把字段声明拆到两个文件里，所以实例状态放在这张表里 —
// 契约语义一个字节都没动，换来的是 boot.go 继续按原样装配。
//
// 进程内通常只有一个 Runtime；`Shutdown` 之后条目即删除。
package boot

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/NaiZi899/Naibox/internal/pool"
)

// bootState 一个已启动实例的全部内部状态。
type bootState struct {
	// p 账号池。Shutdown 必须先 Flush 它再做优雅停机。
	p *pool.Pool

	// srv 与 Runtime.Server 是**同一个**指针。
	srv *http.Server

	// cleanups 原本 `defer` 链的替代品，按**注册顺序**存放，
	// 收尾时逆序执行（Go 的 defer 就是逆序）。
	//
	// 为什么必须搬走：Run 会立刻返回（桌面壳要拿到 Runtime 才能开窗口），
	// 而 defer 会随 Run 返回立刻触发 —— 那等于服务还没开始监听就把
	// 日志 sink 关掉、把 WASM 签名器销毁。
	cleanups []func()

	// done Serve 循环结束时关闭；serveErr 在 close 之前写入。
	done     chan struct{}
	serveErr error

	// fatalf 与 Run 期间生效的致命错误出口是同一个函数。
	fatalf func(format string, args ...any)

	once sync.Once
}

// Shutdown 收尾：先落盘，再优雅停机，随后按 defer 的逆序跑清理链。
//
// # 顺序为什么是硬要求
//
// 账号池的变更（删除账号、禁用账号）只在内存里生效，落盘由后台每 5 秒一次；
// **先停机再落盘**会留下一个已经不再接受请求、但状态还是旧的进程，
// 表现出来就是"删掉的号重启后复活"。
//
// 因此 `p.Flush()` 必须在 `srv.Shutdown` 之前 —— 与改造前 main.go 里
// `go func() { <-ctx.Done(); p.Flush(); srv.Shutdown(...) }()` 逐字一致。
//
// 幂等：多次调用只有第一次生效。
func (rt *Runtime) Shutdown() {
	if rt == nil {
		return
	}
	st := lookup(rt)
	if st == nil {
		return
	}
	st.once.Do(func() {
		st.p.Flush() // 信号触发：先落盘再做优雅停机

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = st.srv.Shutdown(shutdownCtx)

		// 逆序，与改造前 main 返回时 defer 链的执行顺序一致。
		for i := len(st.cleanups) - 1; i >= 0; i-- {
			st.cleanups[i]()
		}

		// ⚠ 这里**不能** detach：Wait 还要读 serveErr 才决定打 "bye" 还是
		// 走 fatal（cmd/server 就是这样用的 —— Shutdown 由 ctx 观察者触发，
		// Wait 在 main 里）。条目由 Wait 收尾时删除；这里只把重引用放掉，
		// 让"调用方只 Shutdown 不 Wait"的残留退化成几十字节，而不是一个账号池。
		st.p = nil
		st.cleanups = nil
		st.srv = nil
	})
}

// Wait 阻塞到 Serve 循环结束，并按改造前的文案收尾。
//
// 正常关停（ErrServerClosed）打 "bye"；真正的监听错误仍按原样走 fatal ——
// 文案 "http: %v" 与改造前 ListenAndServe 之后那一处完全一致。
func (rt *Runtime) Wait() {
	if rt == nil {
		return
	}
	st := lookup(rt)
	if st == nil {
		return
	}
	<-st.done
	err := st.serveErr
	detach(rt)
	if err != nil && err != http.ErrServerClosed {
		st.fatalf("http: %v", err)
		return
	}
	log.Printf("bye")
}

// runtimes 按 Runtime 指针索引实例状态。
var (
	runtimesMu sync.Mutex
	runtimes   = map[*Runtime]*bootState{}
)

func attach(rt *Runtime, st *bootState) {
	runtimesMu.Lock()
	runtimes[rt] = st
	runtimesMu.Unlock()
}

func lookup(rt *Runtime) *bootState {
	runtimesMu.Lock()
	defer runtimesMu.Unlock()
	return runtimes[rt]
}

func detach(rt *Runtime) {
	runtimesMu.Lock()
	delete(runtimes, rt)
	runtimesMu.Unlock()
}

// loggerWriter 把标准库日志转投到调用方给的 *log.Logger。
//
// 为什么不是 log.SetOutput(opt.Logger.Writer())：那样会丢掉调用方设的前缀与
// 标志位。这里走 Print，让 "谁写日志"与"日志长什么样"两件事各自归各自的主人。
type loggerWriter struct{ l *log.Logger }

func (w loggerWriter) Write(b []byte) (int, error) {
	w.l.Print(trimNewline(string(b)))
	return len(b), nil
}

func trimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
