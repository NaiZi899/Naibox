// Package boot 装配层：把配置、账号池、上游注册表、出口服务与调度器接起来。
//
// # 为什么需要这个包（而不是留在 cmd/server）
//
// 装配逻辑原来全部住在 `cmd/server/main.go`（1428 行 `package main`），
// 而 `main` 包**无法被另一个 main 包 import**。桌面壳（`cmd/desktop`）
// 需要同一套装配 —— 复制一份会立刻产生两个装配源，而"加一个上游要改两处"
// 正是本仓库判据 1 要避免的。
//
// 因此装配体搬到这里，`cmd/server` 与 `cmd/desktop` 都只调 `Run`。
//
// # 与架构约束的关系
//
// 本包**必须** import 全部上游实现（它就是装配层），因此它天然"像"一个上游。
// `internal/gateway/arch_test.go` 为此新增第三类登记 `assemblyPackages`：
// 它允许依赖上游，但**不被任何上游依赖**（防环，有测试守）。
//
// # 本文件只放契约
//
// `Run` / `Runtime.Shutdown` / `Runtime.Wait` 的实现在 boot.go / runtime.go。
// 本文件保持"只有类型"，让 `cmd/desktop` 可以在实现完成前就按签名开工。
package boot

import (
	"log"
	"net/http"

	"github.com/NaiZi899/Naibox/internal/backup"
)

// Mode 运行形态。影响的是"谁来决定监听地址与数据根"，不影响装配内容。
type Mode int

const (
	// ModeServer 与改造前的 cmd/server 行为逐字节一致：
	// 监听地址取自配置、数据根是当前工作目录。
	ModeServer Mode = iota
	// ModeDesktop 由桌面壳驱动：监听地址可被 Port 覆盖（0 = 用配置值），
	// 数据根由调用方给（通常是 %APPDATA%\NaiBox）。
	ModeDesktop
)

// Options 装配参数。
type Options struct {
	// Mode 运行形态。只影响 Port 是否生效，不影响装配内容。
	// 零值 = ModeServer，即改造前的 cmd/server 行为。
	Mode Mode

	// ConfigPath 配置文件路径。空 = "config.json"（与 cmd/server 的 flag 默认一致）。
	ConfigPath string

	// DataDir 数据根目录（auths / data / logs 的基准）。
	//
	// 空 = 使用当前工作目录，即改造前的语义（cmd/server 永远传空）。
	// 非空时装配层会把相对路径的 auth_dir / state_file / 日志路径
	// **重定位**到该目录下（绝对路径不动）。
	DataDir string

	// Port 覆盖监听端口。0 = 用配置里的 listen（改造前语义）。
	// 仅 ModeDesktop 使用；ModeServer 传任何非零值也按 0 处理，
	// 以免 server 形态出现"配置说 7863、实际听 8000"的静默漂移。
	Port int

	// OnReady 服务**已开始接受连接**之后回调。
	//
	// # 为什么不是"ListenAndServe 之前"
	//
	// 监听地址在 net.Listen 返回时就已经确定，但 listener 在 Serve 循环
	// 跑起来之前不会 Accept。若在 Serve 之前同步回调，桌面壳在 OnReady
	// 里打一次 `GET /healthz` 就会**永久阻塞**（没有人 Accept），
	// 表现是"双击后窗口永远不出来"。所以回调点放在 Serve goroutine 之后。
	//
	// addr 是**实际**监听地址（Port 顺延或取 0 时与配置不同）；
	// 桌面壳应当直接用它拼 /ui 的 URL，不要自己再算一遍端口。
	// nil = 不回调（cmd/server 不需要）。
	OnReady func(addr string, rt *Runtime)

	// OpenPath 用系统默认程序打开一个路径（桌面壳注入；服务端留空）。
	//
	// 只有"浏览器档案 → 打开目录"这类**桌面专属**动作用它。服务端形态下
	// 对应的端点会明确回 501，而不是假装成功。
	OpenPath func(path string) error

	// Logger 日志出口。nil = 标准库默认（stderr）。
	// 桌面模式会传一个写文件的 logger（同时 fan-out 到控制台）。
	Logger *log.Logger

	// Fatalf 启动期致命错误的出口。
	//
	// # 为什么用注入而不是把 log.Fatalf 改写成 error 返回链
	//
	// 装配体里有 17 处 `log.Fatalf`，它们都是"配置/凭证坏了，进程不该继续跑"
	// 的启动期判定。改写成层层返回 error 会动到几乎每一个装配步骤
	// （风险评估远高于收益），而且容易在某一层把"致命"降级成"警告"——
	// 那种静默降级正是本仓库反复踩过的坑。
	//
	// 注入一个函数指针，搬迁就是机械替换（log.Fatalf → opt.Fatalf），
	// 行为逐字节保持；同时桌面侧可以接管成"写日志 + 托盘提示 + 退出"，
	// 而不是双击后窗口一闪什么也没留下。
	//
	// nil = log.Fatalf（cmd/server 的现有行为）。
	Fatalf func(format string, args ...any)
}

// Runtime 一个已启动的装配实例。
//
// 生命周期：Run 返回时服务已在监听（或已失败返回 error）。
// 调用方负责在 ctx 取消后调 Shutdown 收尾；Shutdown 是幂等的。
//
// # 方法契约（实现在 runtime.go，此处只登记，便于调用方按签名写代码）
//
//	func (rt *Runtime) Shutdown()
//	    幂等收尾。顺序是硬要求：先 p.Flush() 落盘，再 srv.Shutdown(5s)，
//	    最后按注册的逆序跑清理链（与改造前 main 的 defer 链一致）。
//	    **先停机再落盘**会留下"已经不再接受请求、状态还是旧的"进程，
//	    表现就是删掉的账号重启后复活 —— 这是本函数不允许调换顺序的理由。
//	    刻意无 ctx 参数：优雅停机窗口固定 5 秒（与改造前逐字一致），
//	    免得调用方传一个已取消的 ctx 把停机跳过去。
//
//	func (rt *Runtime) Wait()
//	    阻塞到 Serve 循环结束。正常关停打 "bye"；真正的监听错误
//	    仍按改造前的文案 "http: %v" 走致命出口。
type Runtime struct {
	// Addr 实际监听地址（形如 "127.0.0.1:7863"）。
	Addr string

	// Server 底层 HTTP 服务。供需要"改 Handler / 关 KeepAlive"的调用方使用；
	// 常规收尾请走 Shutdown（它保证落盘顺序）。
	Server *http.Server

	// Backup 备份管理器（nil = 未接线）。
	//
	// 桌面壳托盘「备份数据」走它，而不是自己再调一次 Layout.Backup()：
	// 保留策略与通道配置必须只有一个执行点，否则托盘与管理台会各留一份。
	Backup *backup.Manager
}
