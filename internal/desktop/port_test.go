package desktop

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// verifyPortBindable 断言端口真的还能 bind。
//
// PickPort 的返回值会被直接交给装配层的 net.Listen（探测与真正 bind 之间
// 还有一小段竞态），所以"探测说空闲"不等于"一定能起来"；反过来若这里
// 已经 bind 不上，用户的结局就是一条启动失败 —— 桌面形态下表现为
// 双击后窗口一闪/一个错误框。
func verifyPortBindable(t *testing.T, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("选出的端口 %d 实际 bind 失败: %v", port, err)
	}
	ln.Close()
}

// TestPickPortKeepsExplicitPort 守"用户显式 -port 被私自改写"。
//
// 显式给端口的人通常是在防火墙/反代/客户端配置里已经写死了那个号；
// 悄悄换端口比启动失败更难排查（客户端报的是连接被拒，而不是端口变了）。
func TestPickPortKeepsExplicitPort(t *testing.T) {
	const want = 12345
	got, err := PickPort(want, nil)
	if err != nil {
		t.Fatalf("PickPort(%d): %v", want, err)
	}
	if got != want {
		t.Fatalf("显式端口被改写：got=%d want=%d", got, want)
	}
}

// TestPickPortSkipsOccupiedDefaultPort 守"默认端口被占还硬用"。
//
// 7863 很可能已被上一次没退干净的实例、或用户自己跑的 cmd/server 占着。
// 探测的目的就是把"启动失败"换成"顺延一个端口"。
//
// 本机本来就可能有人占着 7863（仓库自带的 restart-7863.sh 会跑服务），
// 所以"占领"失败不算前提不成立 —— 那种情况下端口同样是不可用的，
// 断言改为"结果必须真的可 bind 且不是被占的那个号"。
func TestPickPortSkipsOccupiedDefaultPort(t *testing.T) {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(DefaultPort)))
	occupiedByUs := err == nil
	if occupiedByUs {
		t.Cleanup(func() { ln.Close() })
	} else {
		t.Logf("默认端口 %d 本机已被占用（%v）：直接用现成占用做断言", DefaultPort, err)
	}

	got, err := PickPort(0, nil)
	if err != nil {
		t.Fatalf("PickPort: %v", err)
	}
	if got == 0 {
		t.Fatalf("PickPort 返回 0 —— 调用方会拿它去 bind :0，地址与日志都不再可预测")
	}
	if got == DefaultPort {
		t.Fatalf("端口 %d 已被占用，PickPort 仍返回它（启动会失败）", DefaultPort)
	}
	verifyPortBindable(t, got)
}

// TestPickPortFallsBackOutsideRangeWhenRangeBusy 守"顺延段全占时没有出路"。
//
// 顺延上限之后只剩两条路：系统分配，或者启动失败。实现选了系统分配
// （退无可退），本用例固定住这个语义：**不能**返回 0、**不能**返回
// 范围内某个已知不可用的号。
//
// 顺延段里可能有一部分端口已被别的进程占着（本机常态），这不影响断言：
// 无论那 11 个端口是被本用例占的还是别人占的，PickPort 都必须落到范围外。
func TestPickPortFallsBackOutsideRangeWhenRangeBusy(t *testing.T) {
	var held []net.Listener
	t.Cleanup(func() {
		for _, l := range held {
			l.Close()
		}
	})
	for p := DefaultPort; p <= PortScanEnd; p++ {
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
		if err != nil {
			continue // 已被别的进程占用：同样构成"不可用"，正是我们要的条件
		}
		held = append(held, ln)
	}

	got, err := PickPort(0, nil)
	if err != nil {
		t.Fatalf("PickPort: %v", err)
	}
	if got == 0 {
		t.Fatalf("顺延段全占时返回 0（调用方会 bind :0，端口不可预测且不进日志）")
	}
	if got >= DefaultPort && got <= PortScanEnd {
		t.Fatalf("顺延段 %d..%d 全部不可用，PickPort 却返回了 %d", DefaultPort, PortScanEnd, got)
	}
	verifyPortBindable(t, got)
}

// healthzServer 起一个假 /healthz。
func healthzServer(t *testing.T, body string) (addr string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// TestProbeServiceAcceptsOwnService 守"正常路径反而被拒"。
//
// 探测只在第二实例启动时走一次；误拒的后果不是报错，而是"再点一次 exe
// 时旧实例没被唤出、本进程让位退出"—— 用户看到的是双击完全没有反应。
func TestProbeServiceAcceptsOwnService(t *testing.T) {
	addr := healthzServer(t, `{"service":"omni2api","healthy":0,"total":1}`)
	if err := ProbeService(addr, "omni2api", 2*time.Second); err != nil {
		t.Fatalf("自家服务被拒: %v", err)
	}
}

// TestProbeServiceAcceptsOwnServiceEvenWhenUnhealthy 守住"不看状态码"这个语义。
//
// /healthz 的 200/503 表示"能不能受理对话"（server.healthz 用 pool.ServableNow），
// **不是**"进程活着"。全新安装 0 账号时它恒为 503。若哪天有人给 ProbeService
// 加上"必须 2xx 才算在线"，后果是：首启的主实例正常活着，第二实例却因为
// 503 判定"端口上不是本程序"，于是一次双击既没唤出窗口、也没起自己的服务 ——
// 用户看到"双击完全没反应"，而且日志里只有一条"不是本程序的服务"。
func TestProbeServiceAcceptsOwnServiceEvenWhenUnhealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable) // 0 账号：healthy=0,total=0
		io.WriteString(w, `{"healthy":0,"total":0,"service":"omni2api"}`)
	}))
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")

	if err := ProbeService(addr, "omni2api", 2*time.Second); err != nil {
		t.Fatalf("503（0 账号）时被拒：%v —— 桌面首启就是这个状态", err)
	}
}

// TestProbeServiceRejectsForeignService 是"防连错服务"的核心判据。
//
// 场景：runtime.json 里的端口被别的程序抢到（上次崩溃残留 + 端口复用），
// 或用户把本程序换成了另一个 omni2api 分支/仿制品。只按端口判断会把
// 「打开控制台」变成一次发给陌生服务的外部请求，并可能把带 api_key 的
// 请求送给第三方。因此 service 字段不一致必须判失败。
func TestProbeServiceRejectsForeignService(t *testing.T) {
	addr := healthzServer(t, `{"service":"some-other-gateway","status":"ok"}`)
	err := ProbeService(addr, "omni2api", 2*time.Second)
	if err == nil {
		t.Fatalf("端口上是别人的服务，ProbeService 却通过了")
	}
	if !strings.Contains(err.Error(), "some-other-gateway") {
		t.Errorf("错误信息未指出实际 service 名（用户无从判断连到了谁）: %v", err)
	}
}

// TestProbeServiceRejectsNonHealthzPayload 守"任何 200 都算通过"。
//
// 目标端口上可能是个普通网站或反代的错误页：HTTP 200 + HTML。
// 必须在解析阶段就失败，而不是把 HTML 当成"服务在线"。
func TestProbeServiceRejectsNonHealthzPayload(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"html 200", `<!doctype html><html><body>welcome</body></html>`},
		{"json 但没有 service 字段", `{"healthy":0}`},
		{"空响应", ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr := healthzServer(t, tc.body)
			if err := ProbeService(addr, "omni2api", 2*time.Second); err == nil {
				t.Fatalf("%s 被当成自家服务通过了", tc.name)
			}
		})
	}
}

// TestProbeServiceEmptyWantServiceOnlyChecksJSON 固定当前语义：
// wantService 为空时只验"响应是带 service 的 JSON"，不验身份。
//
// 刻意把这条写进测试而不是留给注释：它是**唯一**允许"不校验身份"的入口，
// 生产路径（singleinstance）永远传具体名字。若哪天有人把调用处的
// expectService 传成空串，本用例不会报警 —— 但读代码的人能在这里看到
// 这个洞的存在条件。
func TestProbeServiceEmptyWantServiceOnlyChecksJSON(t *testing.T) {
	addr := healthzServer(t, `{"service":"totally-other-app"}`)
	if err := ProbeService(addr, "", 2*time.Second); err != nil {
		t.Fatalf("wantService 为空时应跳过身份校验，得到 %v", err)
	}
}

// TestProbeServiceFailsOnDeadPort 守"残留 runtime.json 让第二实例白等"。
//
// 上次崩溃后 runtime.json 没删、端口已无人监听 —— 探测必须快速失败，
// 让第二实例继续自己启动（而不是把用户挂在一次 2 秒起步的超时里，
// 或者更糟：认为主实例活着而让位退出）。
func TestProbeServiceFailsOnDeadPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占位监听: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close() // 关掉：端口上从此没人应答

	err = ProbeService(addr, "omni2api", 500*time.Millisecond)
	if err == nil {
		t.Fatalf("无人应答的端口被判定为自家服务在线")
	}
}
