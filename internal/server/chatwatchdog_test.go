// chatwatchdog_test.go 流式出站两道时间闸门的回归测试。
//
// # 这一组要钉住什么
//
// 它们对应 request-log 里实测的那批记录（一个 token 都没产出却挂满全程）：
//
//	workbuddy/deepseek-v4.1-flash  stream  total=380282ms  tokens=0
//
// 三条正向断言（闸门真的会断、断得能区分原因、断了客户端立刻知道）与
// 一条**反向**断言（正常长回答不得被误杀 —— 防止有人把上限调得太激进）。
//
// # 为什么用"分段可控的桩 body"而不是 httptest.Server
//
// 要造的形状是"连接已建立、响应头已 200、然后**什么都不再发**"。
// 真 HTTP 服务器上做到这一点要么靠永不 handler 返回（连接语义会干扰
// 我们对 body 的观察），要么靠 sleep 一个很大的值（测试变慢且不确定）。
// 直接给 body 一个"吐完前缀就永久阻塞直到被 Close"的实现，形状精确、
// 时间可控，而且**Close 就是中断手段**这一条与生产完全一致
// （见 chatwatchdog.go 的文件头：两条出站路径的 body 都遵守"Close 即中断"）。
package server

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NaiZi899/Naibox/internal/auth"
	"github.com/NaiZi899/Naibox/internal/upstream"
)

// pacedStep 桩 body 的一步：等 delay 之后吐出 data。
type pacedStep struct {
	delay time.Duration
	data  string
}

// pacedBody 按步骤吐数据的上游 body 桩。
//
//	hangs=true  步骤走完后**永久阻塞**直到 Close（模拟"上游再也不发东西"）
//	hangs=false 步骤走完后返回 io.EOF（模拟正常结束的流）
//
// 阻塞中的 Close 会让 Read 立刻返回错误 —— 这正是被测闸门依赖的中断机制。
type pacedBody struct {
	steps []pacedStep
	hangs bool

	mu     sync.Mutex
	i      int
	buf    []byte
	closed chan struct{}
	once   sync.Once
}

func newPacedBody(hangs bool, steps ...pacedStep) *pacedBody {
	return &pacedBody{steps: steps, hangs: hangs, closed: make(chan struct{})}
}

func (b *pacedBody) Read(p []byte) (int, error) {
	for {
		b.mu.Lock()
		if len(b.buf) > 0 {
			n := copy(p, b.buf)
			b.buf = b.buf[n:]
			b.mu.Unlock()
			return n, nil
		}
		if b.i >= len(b.steps) {
			b.mu.Unlock()
			if !b.hangs {
				return 0, io.EOF
			}
			// 永久阻塞，直到闸门（或被测代码）关掉 body。
			<-b.closed
			return 0, errors.New("read on closed body")
		}
		step := b.steps[b.i]
		b.i++
		b.buf = []byte(step.data)
		b.mu.Unlock()

		if step.delay > 0 {
			select {
			case <-time.After(step.delay):
			case <-b.closed:
				return 0, errors.New("read on closed body")
			}
		}
	}
}

func (b *pacedBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

// pacedUpstream 造一个 body 由 pacedBody 决定的上游 client。
//
// ⚠ 刻意**不设** HTTP 的 Timeout、也不设 IdleTimeout：被测的是"上层闸门"，
// 下层任何一层自己先超时都会让测试通过得毫无意义（盖住真正的回归）。
func pacedUpstream(t *testing.T, body io.ReadCloser, status int) (upstreamAdapter, *pacedBody) {
	t.Helper()
	pb, _ := body.(*pacedBody)
	up := upstreamAdapter{c: &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       body,
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}}
	return up, pb
}

// openingFrame 是 OpenAI 流的第一帧：只有 role，**没有任何内容**。
//
// 它是本组测试的关键素材：一个只发了它然后卡住的上游，看起来"有 data 帧"，
// 但一个 token 都没产出 —— 首帧看门狗必须判定它"还没开始产出"。
const openingFrame = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\"," +
	"\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n"

// runStreamWithGuard 在守护时限内跑一次请求。
//
// 为什么要守护而不是直接调 ServeHTTP：**变异验证**（去掉闸门）时请求会
// 永久挂住。直接调会让测试卡到 go test 的全局超时（10 分钟），
// 于是"测试抓住了回归"这件事在 CI 上表现为超时而不是一条清晰的失败。
// 有了它，变异后会得到一条明确的 "没有被闸门终止" 失败。
func runStreamWithGuard(t *testing.T, guard time.Duration, h *Handler, body string) (int, string, time.Duration) {
	t.Helper()
	type result struct {
		code int
		body string
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		h.ServeHTTP(rec, req)
		done <- result{code: rec.Code, body: rec.Body.String()}
	}()
	select {
	case r := <-done:
		return r.code, r.body, time.Since(start)
	case <-time.After(guard):
		t.Fatalf("请求在守护时限 %s 内没有返回 —— 流被无限挂住了。"+
			"★ 这正是本组测试要防的回归（实测 380282ms / tokens=0）。", guard)
		return 0, "", 0
	}
}

// newStreamHealthHandler 造一个只挂默认上游的 handler，闸门时限显式给定。
func newStreamHealthHandler(up DefaultUpstream, max, firstByte time.Duration) *Handler {
	return NewHandler(Config{
		Pool:            testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:        up,
		StreamMax:       max,
		StreamFirstByte: firstByte,
	})
}

const streamReq = `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`

// TestStreamTotalTimeoutTerminatesHungStream 是本次修复在**总时长**这条上的主闸门。
//
// 桩：连上了、响应头已 200、一个字节都不发、永不结束。
// 断言：请求在配置上限附近被终止（而不是无限挂起），且客户端拿到一个
// **明确的**错误（504 + 可 grep 的 code），而不是一个空洞的 200。
//
// 变异验证：把 handler 里 newStreamWatchdog 的 total 参数换成 0（或删掉
// streamTimeoutHit 分支），本用例会以"请求没有被终止"失败。
func TestStreamTotalTimeoutTerminatesHungStream(t *testing.T) {
	body := newPacedBody(true) // 没有任何步骤 → 直接永久阻塞
	up, pb := pacedUpstream(t, body, 200)
	t.Cleanup(func() { _ = pb.Close() })

	const limit = 400 * time.Millisecond
	h := newStreamHealthHandler(up, limit, 0) // 首帧闸门关闭，只测总时长

	code, respBody, elapsed := runStreamWithGuard(t, 10*time.Second, h, streamReq)

	if code != http.StatusGatewayTimeout {
		t.Errorf("状态码=%d，期望 504 —— 一个从没产出过的流超时后必须明确报错，"+
			"而不是让客户端收到空洞的 200（改造前的形态）。body=%q", code, respBody)
	}
	if !strings.Contains(respBody, string(timeoutTotal)) {
		t.Errorf("响应体里没有可 grep 的超时原因 %q，body=%q", timeoutTotal, respBody)
	}
	if elapsed > 5*time.Second {
		t.Errorf("请求耗时 %s，远超配置上限 %s —— 闸门没有按时生效", elapsed, limit)
	}
	// 上限是 400ms，给足调度余量后仍要求"明显早于"它没生效时会发生的时长。
	if elapsed < limit {
		t.Errorf("请求耗时 %s 早于上限 %s —— 说明它不是因为这道闸门结束的，"+
			"本用例没在测它该测的东西", elapsed, limit)
	}
}

// TestStreamFirstByteWatchdogWritesInbandErrorAndDone 是**首帧看门狗**的主闸门。
//
// 桩：吐出开场帧（只有 role，没有内容）之后永久阻塞。
// 这是最阴的一种形态 —— 上游看起来"活着、有 data 帧"，但一个 token 都不产。
//
// 断言：在看门狗时限内断流，并且**状态码已经提交**（开场帧已写出去）时
// 仍按仓库既有机制写流内错误信封 + [DONE]，而不是静默截断。
//
// 变异验证：去掉 newStreamWatchdog 的 firstByte 参数，本用例会失败。
func TestStreamFirstByteWatchdogWritesInbandErrorAndDone(t *testing.T) {
	body := newPacedBody(true, pacedStep{data: openingFrame})
	up, pb := pacedUpstream(t, body, 200)
	t.Cleanup(func() { _ = pb.Close() })

	const firstByte = 400 * time.Millisecond
	// 总时长给得很大：本条只想测首帧看门狗，不让总时长闸门抢答。
	h := newStreamHealthHandler(up, time.Minute, firstByte)

	code, respBody, elapsed := runStreamWithGuard(t, 10*time.Second, h, streamReq)

	// 开场帧已经写出去了 → 状态码 200 已提交，收不回来。
	if code != http.StatusOK {
		t.Errorf("状态码=%d，期望 200 —— 开场帧已经写出，状态码此时已不可回收", code)
	}
	if !strings.Contains(respBody, string(timeoutFirstByte)) {
		t.Errorf("没有写出流内错误信封（找不到 %q）—— 客户端会把静默截断当成"+
			"回答结束，body=%q", timeoutFirstByte, respBody)
	}
	if !strings.Contains(respBody, "[DONE]") {
		t.Errorf("没有补 [DONE] —— 客户端会挂到自己那边的超时，body=%q", respBody)
	}
	if elapsed > 5*time.Second {
		t.Errorf("请求耗时 %s，远超看门狗时限 %s", elapsed, firstByte)
	}
}

// TestStreamLongHealthyAnswerNotKilled 是**反向断言**：正常的长回答不许被误杀。
//
// 桩：先吐一行内容（立刻），然后**慢速持续**吐很多行，整体时长明显超过
// 首帧看门狗时限，最后正常 [DONE]。
//
// 这条同时钉住设计上的关键一点：首帧看门狗是**一次性只查一次**的 ——
// "已经产出过"就永久放行。若有人把它写成"每 N 秒复查一次当前是否在产出"
// （看似更严格），本用例会红：那条实现会把"思考了 30 秒才继续吐字"的
// 正常长回答掐掉，正是把安全阀变成误杀器。
func TestStreamLongHealthyAnswerNotKilled(t *testing.T) {
	var steps []pacedStep
	// 首个内容帧几乎立刻到达（远早于看门狗时限）。
	steps = append(steps, pacedStep{data: contentFrame("第一段")})
	// 之后持续吐字，总时长刻意**超过**首帧看门狗时限。
	for i := 0; i < 12; i++ {
		steps = append(steps, pacedStep{delay: 100 * time.Millisecond, data: contentFrame("继续")})
	}
	steps = append(steps, pacedStep{data: "data: [DONE]\n\n"})

	body := newPacedBody(false, steps...) // hangs=false → 正常结束
	up, pb := pacedUpstream(t, body, 200)
	t.Cleanup(func() { _ = pb.Close() })

	// 首帧看门狗只有 300ms，而整条流要 1.2s 以上 —— 全靠"见过产出即放行"。
	h := newStreamHealthHandler(up, 30*time.Second, 300*time.Millisecond)

	code, respBody, _ := runStreamWithGuard(t, 15*time.Second, h, streamReq)

	if code != http.StatusOK {
		t.Fatalf("正常长回答被拒绝了：code=%d body=%q", code, respBody)
	}
	if strings.Contains(respBody, string(timeoutFirstByte)) ||
		strings.Contains(respBody, string(timeoutTotal)) {
		t.Errorf("正常长回答被闸门误杀（出现超时信封）：body=%q", respBody)
	}
	// 12 段内容一个都不能少：闸门收工之后不得再截断数据。
	if n := strings.Count(respBody, "继续"); n != 12 {
		t.Errorf("内容被截断：只收到 %d 段「继续」，期望 12 段。body=%q", n, respBody)
	}
	if !strings.Contains(respBody, "[DONE]") {
		t.Errorf("正常流该有上游自己的 [DONE]，body=%q", respBody)
	}
}

// contentFrame 造一帧带内容的 chunk。
func contentFrame(text string) string {
	return "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\"," +
		"\"choices\":[{\"index\":0,\"delta\":{\"content\":\"" + text + "\"}}]}\n\n"
}

// TestStreamTimeoutDoesNotPunishAccount 守住"超时不罚账号"这条红线。
//
// 超时是"这条流没跑完"，与这个账号有没有问题无关。若在这里给账号记一笔，
// 一个上游的慢就会把健康账号池拖进冷却 —— 池子耗尽后所有请求一起 503。
// 这与仓库里 transport error / 内容拦截那两条的既有立场一致。
func TestStreamTimeoutDoesNotPunishAccount(t *testing.T) {
	body := newPacedBody(true)
	up, pb := pacedUpstream(t, body, 200)
	t.Cleanup(func() { _ = pb.Close() })

	h := newStreamHealthHandler(up, 300*time.Millisecond, 0)

	code, _, _ := runStreamWithGuard(t, 10*time.Second, h, streamReq)
	if code != http.StatusGatewayTimeout {
		t.Fatalf("前置条件不成立：code=%d，期望 504（闸门应已命中）", code)
	}

	st := statusOf(t, h, "u1")
	if st.ErrTotal != 0 {
		t.Errorf("err_total=%d，期望 0 —— 流超时不该记在账号头上", st.ErrTotal)
	}
	if st.Cooling {
		t.Errorf("账号被冷却（reason=%q）—— 超时不是账号的过错", st.Reason)
	}
	if st.Disabled {
		t.Error("账号被禁用 —— 超时绝不该永久禁用账号")
	}
}

// TestStreamWatchdogNoLeakAfterClose 守住"闸门不泄漏 goroutine/定时器"。
//
// # 为什么这条是硬要求（任务书明确写了）
//
// 每个流式请求都会装一次闸门。若 Close 不停掉定时器，一个长连接部署下
// 定时器会随请求数累积（time.AfterFunc 的定时器在触发前一直被 runtime 持有），
// 直到到点才被回收 —— 15 分钟的上限意味着"到点前一直占着"。
//
// 判据：Close 之后 goroutine 数必须回到基线。本实现刻意**不用 goroutine**
// （两道闸门都是 time.AfterFunc，Read 在调用方 goroutine 里直通），
// 所以这里同时也钉住了"将来有人把它改成后台 goroutine 轮询"这种退化。
func TestStreamWatchdogNoLeakAfterClose(t *testing.T) {
	// 让运行时把已退出 goroutine 的统计刷新一下，再取基线。
	runtime.GC()
	base := runtime.NumGoroutine()

	for i := 0; i < 50; i++ {
		body := newPacedBody(true)
		wd := newStreamWatchdog(body, time.Now(), 30*time.Second, 10*time.Second)
		if err := wd.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		// 幂等：再关一次不得 panic（生产路径确实会关两次，
		// 见 Close 的注释：显式收尾 + 闸门自己 trip）。
		if err := wd.Close(); err != nil {
			t.Fatalf("第二次 Close: %v", err)
		}
	}

	// 给调度器一点时间回收。
	var after int
	for i := 0; i < 20; i++ {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
		after = runtime.NumGoroutine()
		if after <= base {
			return
		}
	}
	t.Errorf("50 次装/卸闸门后 goroutine 数 %d > 基线 %d —— 有泄漏", after, base)
}

// TestStreamWatchdogCloseIsIdempotent 单独钉幂等（不依赖上面的计数是否稳定）。
func TestStreamWatchdogCloseIsIdempotent(t *testing.T) {
	var closes int
	body := &countCloseBody{pacedBody: newPacedBody(true), closes: &closes}
	wd := newStreamWatchdog(body, time.Now(), time.Minute, time.Minute)

	_ = wd.Close()
	_ = wd.Close()
	_ = wd.Close()

	if closes != 1 {
		t.Errorf("底层 Close 被调了 %d 次，期望恰好 1 次 —— "+
			"上游 body 的 Close 不保证幂等，重复调用可能 panic 或破坏连接复用", closes)
	}
}

// countCloseBody 统计底层 Close 被调用了几次。
type countCloseBody struct {
	*pacedBody
	closes *int
}

func (b *countCloseBody) Close() error {
	*b.closes++
	return b.pacedBody.Close()
}

// TestStreamWatchdogTripThenCloseDoesNotPanic 闸门命中后仍然 Close 的路径。
//
// 这是生产的真实顺序：trip 时已经 Close 过底座，随后 handler 在
// wire.Stream 之后又 Close 一次。
func TestStreamWatchdogTripThenCloseDoesNotPanic(t *testing.T) {
	body := newPacedBody(true)
	wd := newStreamWatchdog(body, time.Now().Add(-time.Hour), time.Second, 0)

	// 预算早已耗尽 → 立刻 trip（会 Close 底座）。
	_, err := io.ReadAll(wd)
	var hit *streamTimeoutError
	if !errors.As(err, &hit) {
		t.Fatalf("err=%v，期望 *streamTimeoutError", err)
	}
	if err := wd.Close(); err != nil {
		t.Fatalf("trip 之后 Close 出错: %v", err)
	}
}

// # 为什么这是一条独立的判据（实测 380s 的真正形状）
//
// 380s 不是"一条流挂了 380s"，而是**多个上游各挂一段、每次都重新开始计时**：
//
//	预算按轮重开 → MaxRotate 次重试 = 上限 × 3
//
// 所以"上限是从现在起算"这种看起来更宽容的写法，恰好复现了这个 bug。
// 本用例不靠"跑满两轮再比时长"（那样阈值只能放得很松、抓不住回归），
// 而是直接构造一个**已经预支过的**起始时刻：
//
//	since = 2 秒前，total = 1 秒  →  预算早已耗尽
//	正确的实现（绝对时刻）应当**立刻**到点
//	错误的实现（从现在起算）会再等满 1 秒
//
// 变异验证：把 newStreamWatchdog 里的 `remainUntil(since.Add(total))`
// 改成 `total`，本用例变红。
func TestStreamMaxBudgetIsAbsoluteNotPerRound(t *testing.T) {
	t.Run("预算已耗尽则立刻到点", func(t *testing.T) {
		body := newPacedBody(true)
		t.Cleanup(func() { _ = body.Close() })

		// since 在过去 2 秒，而预算是 1 秒 → 剩余预算为 0。
		wd := newStreamWatchdog(body, time.Now().Add(-2*time.Second), time.Second, 0)
		t.Cleanup(func() { _ = wd.Close() })

		done := make(chan error, 1)
		start := time.Now()
		go func() {
			_, err := io.ReadAll(wd)
			done <- err
		}()

		select {
		case err := <-done:
			var hit *streamTimeoutError
			if !errors.As(err, &hit) || hit.Reason != timeoutTotal {
				t.Fatalf("错误=%v，期望 *streamTimeoutError(timeoutTotal)", err)
			}
			// 立刻 = 远小于一个完整预算（1s）。给足调度余量仍要求 <700ms。
			if elapsed := time.Since(start); elapsed > 700*time.Millisecond {
				t.Errorf("耗时 %s —— 预算已耗尽却还等了接近一整个 total，"+
					"说明上限是「从现在起算」而不是绝对时刻。\n"+
					"★ 那会让每次换号重试都重开预算，等于把上限乘 MaxRotate"+
					"（实测 380s 的形态）。", elapsed)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("预算已耗尽却始终没到点 —— 总时长闸门没有生效")
		}
	})

	t.Run("预算未耗尽则不得提前到点", func(t *testing.T) {
		body := newPacedBody(true)
		t.Cleanup(func() { _ = body.Close() })

		wd := newStreamWatchdog(body, time.Now(), 5*time.Second, 0)
		t.Cleanup(func() { _ = wd.Close() })

		done := make(chan error, 1)
		go func() {
			_, err := io.ReadAll(wd)
			done <- err
		}()

		select {
		case err := <-done:
			t.Fatalf("闸门提前到点了（err=%v）—— 正常流会被误杀", err)
		case <-time.After(500 * time.Millisecond):
			// 正确：预算还剩 4.5s，这里什么都不该发生。
		}
	})
}
