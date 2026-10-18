package boot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NaiZi899/omni2api/internal/server"
)

// bootDesktopConfig 桌面形态的最小配置：监听 127.0.0.1:0（端口交给系统），
// 所有会自动打上游的开关全部关掉。
//
// 为什么必须关：调度器的守卫轮**首轮立即执行**（scheduler.Jobs.RunOnce 对
// lastRun 零值的任务不做间隔等待），签到/保活/上游刷新一旦被注册就会在启动
// 瞬间发起真实 HTTP。本包的测试必须能离线跑，否则"全绿"要依赖网络。
// auths 目录为空本身也会让这些任务空转，但两重保险才不依赖别人的实现细节。
const bootDesktopConfig = `{
  "listen": "127.0.0.1:0",
  "api_key": "sk-boot-test-key",
  "service_name": "omni2api",
  "auth_dir": "./auths",
  "state_file": "./data/state.json",
  "login": {"open_browser": false},
  "schedule": {"checkin_enabled": false, "keepalive_enabled": false, "activity_hours": []},
  "admin": {"travel_auto_claim": false},
  "codearts": {"enabled": false},
  "workbuddy_intl": {"enabled": false},
  "loomy": {"enabled": false},
  "trae": {"enabled": false},
  "cline": {"enabled": false},
  "raccoon": {"enabled": false},
  "lobsterai": {"enabled": false},
  "qoder": {"enabled": false},
  "inkstone": {"enabled": false},
  "jiyuan": {"enabled": false}
}`

// syncBuffer 并发安全的日志缓冲。
//
// 必须加锁：Run 之后有 pool 后台 flush、调度器、HTTP handler 多条 goroutine
// 同时写日志，plain bytes.Buffer 会在 -race 下直接报数据竞争。
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// isolateRunGlobals 保存并恢复 Run 会改写的包级状态。
//
// Run 的两处副作用是**故意**的（桌面壳要它们）：log.SetOutput 把 stdlib 日志
// 收进文件、fatalf 换成注入的出口。但包级变量会跨测试泄漏，把后面那些
// "捕获日志断言文案"的既有测试打乱，所以这里显式还原。
func isolateRunGlobals(t *testing.T) {
	t.Helper()
	oldOut := log.Writer()
	oldFatal := fatalf
	t.Cleanup(func() {
		log.SetOutput(oldOut)
		fatalf = oldFatal
	})
}

// writeBootConfig 把测试配置落到 dir 下并返回路径。
func writeBootConfig(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(bootDesktopConfig), 0o600); err != nil {
		t.Fatalf("写测试配置: %v", err)
	}
	return path
}

// leakyTempDir 与 t.TempDir() 相同，但清理失败**不判测试失败**。
//
// 用于"启动必然失败"的用例：那种路径上 boot.Run 在 net.Listen 之前就已经
// 打开了请求日志文件，而失败返回时清理链不会执行（sink.Close 属于
// Runtime.Shutdown 的职责，没有 Runtime 就没有收尾），句柄留到进程结束。
// Windows 下带着未关闭句柄的目录 RemoveAll 必然失败 —— 用 t.TempDir()
// 会让一条**实现正确**的行为把测试判红。
func leakyTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "omni2api-boot-fatal-*")
	if err != nil {
		t.Fatalf("建临时目录: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// healthSample 一次 /healthz 探测的结果。
type healthSample struct {
	status  int
	service string // 响应头 X-Service
	body    []byte
}

// waitHealthz 轮询 /healthz 直到服务应答（最多 5 秒）。
//
// 判据是"能拿到 /healthz 的 JSON"，**不是** 200：/healthz 的语义是
// "这个实例现在能不能受理对话"（server.healthz 用 pool.ServableNow 决定
// 200/503），全新安装 0 个账号时它恒为 503。桌面壳若把"200"当成"服务起来了"
// 就会让第一次使用的用户永远卡在"启动中"。
//
// 用轮询而不是 sleep：Run 返回时 Serve goroutine 已经起跑，但"立刻可连"
// 与毫秒级抖动是两件事，固定 sleep 要么慢要么偶发失败。
func waitHealthz(t *testing.T, addr string) healthSample {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(5 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + addr + "/healthz")
		if err == nil {
			body, rerr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if rerr == nil && (resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusServiceUnavailable) {
				return healthSample{status: resp.StatusCode, service: resp.Header.Get("X-Service"), body: body}
			}
			last = fmt.Sprintf("status=%d err=%v body=%q", resp.StatusCode, rerr, body)
		} else {
			last = err.Error()
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s 在 5 秒内没有可解析的 /healthz 应答：%s", addr, last)
	return healthSample{}
}

// waitPortBindable 轮询断言 addr 重新可以 bind。
//
// Shutdown 的 srv.Shutdown 是异步收束：返回时监听已经关闭，但为了不依赖
// 这个时序细节，这里轮询到成功为止。
func waitPortBindable(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			ln.Close()
			return
		}
		last = err
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("Shutdown 之后 %s 仍不可 bind（监听/端口未释放）: %v", addr, last)
}

// TestDesktopBootServesHealthzThenShutdownReleasesPort 守桌面壳赖以工作的三条契约。
//
// 桌面壳的启动路径是：boot.Run 返回 → 立刻用 rt.Addr 拼 /ui 地址开窗口 →
// 退出时 Shutdown（落盘在它内部）。所以这三件事任何一条坏了，用户看到的
// 分别是：
//
//	(a) Run 返回时服务还没在监听 → 窗口打开是白屏，稍后刷新才正常；
//	(b) /healthz 的 service 字段不是自家名字 → 第二实例的"防连错服务"
//	    判据失效（ProbeService 拿这个名字比对，见 desktop.PortProbe）；
//	(c) Shutdown 之后端口没释放 / 不可重复调用 → 托盘"重启服务"必然失败，
//	    而且重启失败时窗口已经关了，用户以为程序崩了。
func TestDesktopBootServesHealthzThenShutdownReleasesPort(t *testing.T) {
	isolateRunGlobals(t)
	dir := t.TempDir()
	cfgPath := writeBootConfig(t, dir)

	logs := &syncBuffer{}
	var fatalMu sync.Mutex
	var fatalMsgs []string

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rt, err := Run(ctx, Options{
		Mode:       ModeDesktop,
		ConfigPath: cfgPath,
		DataDir:    dir,
		Port:       0, // 用配置里的 127.0.0.1:0：端口冲突与本用例无关
		Logger:     log.New(logs, "", 0),
		Fatalf: func(format string, args ...any) {
			fatalMu.Lock()
			fatalMsgs = append(fatalMsgs, fmt.Sprintf(format, args...))
			fatalMu.Unlock()
		},
	})
	if err != nil {
		cancel()
		t.Fatalf("Run: %v\n--- 日志 ---\n%s", err, logs.String())
	}
	if rt == nil || rt.Server == nil {
		cancel()
		t.Fatalf("Run 返回了空 Runtime: %+v", rt)
	}
	if got := func() []string { fatalMu.Lock(); defer fatalMu.Unlock(); return append([]string(nil), fatalMsgs...) }(); len(got) > 0 {
		t.Fatalf("启动期出现了致命错误（桌面壳会弹框退出）: %v", got)
	}
	if _, port, err := net.SplitHostPort(rt.Addr); err != nil || port == "0" {
		t.Fatalf("rt.Addr=%q 不是可直接连接的地址（窗口地址要靠它拼）", rt.Addr)
	}

	got := waitHealthz(t, rt.Addr)
	var health struct {
		Service string `json:"service"`
	}
	if err := json.Unmarshal(got.body, &health); err != nil {
		t.Fatalf("/healthz 不是 JSON: %v body=%q", err, got.body)
	}
	// 这就是"防连错服务"的判定来源：desktop.ProbeService 拿 runtime.json 里的
	// addr 打 /healthz，只有这个字段等于期望服务名才肯唤醒主实例。
	if health.Service != server.ServiceName {
		t.Fatalf("/healthz 的 service=%q，本程序期望 %q —— 第二实例会把陌生服务当成自家主实例",
			health.Service, server.ServiceName)
	}
	// 双保险的第二路：ProbeService 只读 body，但运维/脚本会读头。
	if got.service != server.ServiceName {
		t.Errorf("X-Service=%q 与 /healthz 的 service 字段（%q）不一致", got.service, health.Service)
	}
	if got.status != http.StatusOK && got.status != http.StatusServiceUnavailable {
		t.Errorf("/healthz 返回了意料之外的 %d", got.status)
	}
	if got.status == http.StatusServiceUnavailable {
		// 0 账号 → 无可用实例 → 503。桌面首次启动就是这个状态，
		// 所以"就绪判据"绝不能等同"200"（见 waitHealthz 的注释）。
		t.Logf("空账号池下 /healthz=%d（符合 server.healthz 的 ServableNow 口径）", got.status)
	}

	// 数据根重定位：装配层把配置里的**相对**落点（这里默认是
	// data/request-log.jsonl）挪到 DataDir 下。这是桌面形态的核心承诺 ——
	// 相对路径留在 cwd 意味着"换个方式启动，账号就写进了安装目录"。
	// 注：auths/ 由桌面壳（Layout.EnsureDirs）负责建，boot 只读它，
	// 所以这里不能断言 auths/ 存在（那是另一层的契约，见 desktop 包测试）。
	reqLog := filepath.Join(dir, "data", "request-log.jsonl")
	if _, err := os.Stat(reqLog); err != nil {
		t.Errorf("相对路径未重定位到数据根：%s 不存在（%v）", reqLog, err)
	}

	// Shutdown 幂等：托盘"退出"与 ctx 取消两条路径都会调它。
	rt.Shutdown()
	rt.Shutdown() // 重复调用不得 panic、不得挂起

	waitDone := make(chan struct{})
	go func() {
		rt.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(20 * time.Second):
		t.Fatalf("Shutdown 之后 Wait 20 秒未返回（Serve 循环没收敛）")
	}

	waitPortBindable(t, rt.Addr)

	// Wait 之后条目已 detach：再调 Shutdown/Wait 应安全返回（不 panic）。
	rt.Shutdown()
	rt.Wait()

	// 退出路径必须留下证据：桌面形态没有控制台，日志文件是唯一诊断材料。
	if !strings.Contains(logs.String(), "bye") {
		t.Errorf("正常关闭没有打 bye：日志末尾=%q", tail(logs.String(), 400))
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// TestDesktopBootPortCollisionReturnsErrorThroughFatalf 守"端口被占时的退出方式"。
//
// 桌面形态下装配层的 17 处 log.Fatalf 已被 opt.Fatalf 接管（弹框 + 写日志 +
// 退出码）。端口冲突走的正是这条链：net.Listen 失败 → fatalf("http: %v") →
// 返回 error。若哪天这个出口退回 log.Fatalf，双击 exe 的表现会变成
// "窗口一闪、什么都没有"——用户拿不到任何可诊断的信息，我们也没有日志。
//
// 顺带固定"不静默漂移"：端口被占时**不许**自己换个端口起来，那会让
// 用户在防火墙/客户端里写死的 7863 变成一次安静的不一致。
func TestDesktopBootPortCollisionReturnsErrorThroughFatalf(t *testing.T) {
	isolateRunGlobals(t)
	dir := leakyTempDir(t)
	cfgPath := writeBootConfig(t, dir)

	// 构造占用：先自己 bind 一个端口，再让 Run 指定同一个端口。
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占位监听: %v", err)
	}
	defer occupied.Close()
	_, portStr, err := net.SplitHostPort(occupied.Addr().String())
	if err != nil {
		t.Fatalf("解析占位地址: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("解析端口: %v", err)
	}

	var fatalMu sync.Mutex
	var fatalMsgs []string
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rt, err := Run(ctx, Options{
		Mode:       ModeDesktop,
		ConfigPath: cfgPath,
		DataDir:    dir,
		Port:       port,
		Logger:     log.New(io.Discard, "", 0),
		Fatalf: func(format string, args ...any) {
			fatalMu.Lock()
			fatalMsgs = append(fatalMsgs, fmt.Sprintf(format, args...))
			fatalMu.Unlock()
		},
	})
	if err == nil {
		if rt != nil {
			rt.Shutdown()
		}
		t.Fatalf("端口 %d 已被占用，Run 却成功了（服务听在了别处？）", port)
	}
	if rt != nil {
		t.Errorf("失败时不应返回 Runtime：%+v", rt)
	}
	got := func() []string { fatalMu.Lock(); defer fatalMu.Unlock(); return append([]string(nil), fatalMsgs...) }()
	if len(got) != 1 {
		t.Fatalf("启动期致命错误应恰好走一次注入的 Fatalf，实际 %d 次: %v", len(got), got)
	}
	if !strings.HasPrefix(got[0], "http:") {
		t.Errorf("致命错误文案变了（桌面壳用弹框展示它，文案要与改造前一致）: %q", got[0])
	}
}

// TestResolveListenPortOverrideOnlyInDesktopMode 守两件事：
//
//  1. "服务器形态被 Port 悄悄改端口" —— Options.Port 只属于桌面壳
//     （多实例/端口被占自选）。如果 ModeServer 也认它，一台真实部署会出现
//     "config 写 7863、实际听 8000"：防火墙、反代、客户端全部错位，
//     而且这个漂移在日志里只有一行听地址，极易被忽略。
//
//  2. "桌面形态把控制台暴露到局域网" —— 空 host（":7863"）在 Go 里等于
//     监听所有网卡，而 /ui 对回环来源会自动注入 api_key。个人电脑上
//     绑通配等于同网段任何人打开 http://<内网IP>:7863/ui 就能进控制台。
//     所以桌面形态下空 host 必须收到 127.0.0.1。
//
// ⚠ 反方向同样要守：用户**显式**写 0.0.0.0 或某个网卡地址时不得被悄悄改掉
// —— 那是明确意图（他可能就想在局域网里用），静默改写会让他排查半天
// "为什么别的机器连不上"。风险提示由桌面壳在日志里给。
func TestResolveListenPortOverrideOnlyInDesktopMode(t *testing.T) {
	cases := []struct {
		name   string
		listen string
		opt    Options
		want   string
	}{
		{"server 形态忽略 Port", ":7863", Options{Mode: ModeServer, Port: 9999}, ":7863"},
		{"server 零值 Port", "127.0.0.1:7863", Options{Mode: ModeServer}, "127.0.0.1:7863"},
		{"server 形态不收敛 host（Docker 需要通配）", ":7863", Options{Mode: ModeServer}, ":7863"},
		{"desktop 零值 Port 用配置端口但收敛 host", ":7863", Options{Mode: ModeDesktop, Port: 0}, "127.0.0.1:7863"},
		{"desktop 换纯端口段", ":7863", Options{Mode: ModeDesktop, Port: 9999}, "127.0.0.1:9999"},
		{"desktop 保留显式回环 host", "127.0.0.1:7863", Options{Mode: ModeDesktop, Port: 9999}, "127.0.0.1:9999"},
		{"desktop 配置无端口段", "127.0.0.1", Options{Mode: ModeDesktop, Port: 9999}, "127.0.0.1:9999"},
		{"desktop 配置写 :0", ":0", Options{Mode: ModeDesktop, Port: 9999}, "127.0.0.1:9999"},
		// 显式通配必须被尊重 —— 静默改写会让用户查不出"局域网连不上"。
		{"desktop 显式 0.0.0.0 不被改写", "0.0.0.0:7863", Options{Mode: ModeDesktop, Port: 0}, "0.0.0.0:7863"},
		{"desktop 显式 0.0.0.0 仅换端口", "0.0.0.0:7863", Options{Mode: ModeDesktop, Port: 9999}, "0.0.0.0:9999"},
		// 显式回环 + 零值 Port：只该收敛 host 之外什么都不变。
		{"desktop 显式回环零值 Port", "127.0.0.1:7863", Options{Mode: ModeDesktop}, "127.0.0.1:7863"},
	}
	for _, tc := range cases {
		if got := resolveListen(tc.listen, tc.opt); got != tc.want {
			t.Errorf("%s: resolveListen(%q, %+v)=%q want %q", tc.name, tc.listen, tc.opt, got, tc.want)
		}
	}
}
