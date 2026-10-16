// chattool_watchdog_test.go 工具循环那条出站路径的闸门回归。
//
// # 为什么单独一个文件
//
// 普通 chat 路径的闸门在 handler 层（见 chatwatchdog.go），一次覆盖
// 默认上游与其余上游 —— 但**工具循环在 chatVia 之前就分流走了**
// （见 chatwatchdog.go 文件头的"已知边界"），绕过了那道闸门。
//
// 它的唯一流式出口是 chattool_stream.go 的 consumeRound，所以闸门补在那里。
// 本文件钉住"那条路径确实被闸门包住了"，防止将来有人重构 consumeRound
// 时把包装点丢掉 —— 丢掉的表现是"工具循环里的请求又能挂到天荒地老"，
// 而那正是本次要消灭的形态（实测 380s / 325s）。
package server

import (
	"context"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/NaiZi899/omni2api/internal/auth"
	"github.com/NaiZi899/omni2api/internal/gateway"
	"github.com/NaiZi899/omni2api/internal/pool"
)

// hangingBody 一个"连上了但永不产出"的上游流。
//
// 它的 Read 阻塞到 Close 被调用为止 —— 正是闸门要抓的形态
// （上游不停发心跳/保持连接，但没有任何内容帧）。
//
// 用 channel 而不是 sleep：Close 一到就立刻醒，测试不必等满超时，
// 也就不会因为"等太久"而变成慢测试或 flaky。
type hangingBody struct {
	closed chan struct{}
	once   chan struct{}
}

func newHangingBody() *hangingBody {
	return &hangingBody{closed: make(chan struct{}), once: make(chan struct{}, 1)}
}

func (b *hangingBody) Read(p []byte) (int, error) {
	<-b.closed
	return 0, io.EOF
}

func (b *hangingBody) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

// hangingProvider 返回一个永不产出内容的 body，并记录 Chat 被调次数。
type hangingProvider struct {
	id    string
	body  *hangingBody
	calls int
}

var _ gateway.Provider = (*hangingProvider)(nil)

func (p *hangingProvider) ID() string { return p.id }
func (p *hangingProvider) Caps() gateway.Capability {
	// 声明 CapChat 与工具能力，好让 shouldRunToolLoop 认为它可用
	// （本测试直接调 consumeRound，不走分流，但保持桩的语义完整）。
	return gateway.CapChat
}

func (p *hangingProvider) Chat(_ context.Context, _ gateway.Credential, _ []byte) (gateway.ChatStream, error) {
	p.calls++
	return gateway.ChatStream{Status: 200, Body: p.body}, nil
}

func (p *hangingProvider) Models(_ context.Context, _ gateway.Credential) ([]gateway.ModelInfo, error) {
	return []gateway.ModelInfo{{ID: p.id + "-m"}}, nil
}

// toolLoopWatchdogHandler 造一个"已接线"的 handler，Provider 是一个
// 永不产出内容的上游。
func toolLoopWatchdogHandler(t *testing.T, streamMax, firstByte time.Duration) (*Handler, *hangingBody) {
	t.Helper()
	p := pool.New("")
	p.SetDefaultProvider("loomy")
	p.SetMaxInFlight(10)
	a := &auth.Auth{UID: "u-wd", Nickname: "闸门号"}
	p.SyncToDirWithSecrets("loomy", []*auth.Auth{a}, map[string]any{"u-wd": &struct{}{}})
	p.SetCredits("u-wd", 100000)

	hb := newHangingBody()
	prov := &hangingProvider{id: "loomy", body: hb}
	r := &singleProviderRouter{def: "loomy", prov: prov, p: p}

	return NewHandler(Config{
		Pool:            p,
		Provider:        r,
		DefaultProvider: "loomy",
		StreamMax:       streamMax,
		StreamFirstByte: firstByte,
	}), hb
}

// singleProviderRouter 一个最小 ProviderRouter：只有一个上游。
//
// 不复用 multiRouter 是因为它按 reg map 分发，而本测试要的正是
// "把 Chat 直接指向这个会挂住的桩"。
type singleProviderRouter struct {
	def  string
	prov *hangingProvider
	p    *pool.Pool
}

var _ ProviderRouter = (*singleProviderRouter)(nil)

func (r *singleProviderRouter) Has(id string) bool { return id == r.def }
func (r *singleProviderRouter) Default() string    { return r.def }
func (r *singleProviderRouter) IDs() []string      { return []string{r.def} }

func (r *singleProviderRouter) Models(context.Context, string) ([]gateway.ModelInfo, bool) {
	return []gateway.ModelInfo{{ID: "loomy-m"}}, true
}

func (r *singleProviderRouter) ModelMultipliers(context.Context, string) (map[string]float64, bool) {
	return nil, false
}

func (r *singleProviderRouter) Chat(ctx context.Context, id string, cred gateway.Credential, body []byte) (gateway.ChatStream, bool, error) {
	if id != r.def {
		return gateway.ChatStream{}, false, nil
	}
	cs, err := r.prov.Chat(ctx, cred, body)
	return cs, true, err
}

func (r *singleProviderRouter) Credential(id, uid string) (gateway.Credential, bool) {
	if id != r.def {
		return gateway.Credential{}, false
	}
	if r.p == nil {
		return gateway.Credential{}, false
	}
	secret, ok := r.p.SecretOf(uid)
	if !ok || secret == nil {
		return gateway.Credential{}, false
	}
	return gateway.Credential{Provider: id, UID: uid, Secret: secret}, true
}

func (r *singleProviderRouter) RefreshCredential(context.Context, string, gateway.Credential) (bool, error) {
	return true, nil
}

func (r *singleProviderRouter) RefreshSkew(string, gateway.Credential) (time.Duration, bool) {
	return 0, false
}

func (r *singleProviderRouter) SoftRateReset(string, int, string) (time.Time, bool) {
	return time.Time{}, false
}

func (r *singleProviderRouter) ResetAt(string, gateway.Credential) (time.Time, bool) {
	return time.Time{}, false
}

func (r *singleProviderRouter) Classify(string, int, string) (gateway.ErrorKind, bool) {
	return gateway.ErrKindNone, false
}

func (r *singleProviderRouter) Explain(string, int, string) (gateway.ErrorExplain, bool) {
	return gateway.ErrorExplain{}, false
}

// TestToolLoopStreamHasTotalTimeout 工具循环的流也必须受总时长闸门约束。
//
// # 判据
//
// 上游永不产出，总时长上限设为 300ms。断言 consumeRound 在远小于
// "永远"的时间里返回，且带一个错误（不是静默成功）。
// 若有人把 consumeRound 里的 newStreamWatchdog 包装去掉，这条会挂到
// 测试自身的守护超时（几秒）才返回 —— 于是变红。
func TestToolLoopStreamHasTotalTimeout(t *testing.T) {
	h, hb := toolLoopWatchdogHandler(t, 300*time.Millisecond, 0)

	sw := newSSEWriter(httptest.NewRecorder())
	stats := newToolLoopStats(time.Now())

	done := make(chan roundOutcome, 1)
	go func() {
		p := toolLoopParams{
			ctx:        context.Background(),
			uid:        "u-wd",
			providerID: "loomy",
			start:      time.Now(),
		}
		done <- h.consumeRound(sw, p, []byte(`{"model":"loomy/auto"}`), map[string]bool{}, stats)
	}()

	select {
	case out := <-done:
		if out.err == nil {
			t.Error("上游永不产出，consumeRound 却无声返回（总时长闸门没生效）")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("consumeRound 挂住了：工具循环这条路径没有被总时长闸门覆盖")
	}
	_ = hb
}

// TestToolLoopStreamHasFirstByteWatchdog 工具循环也要有首帧看门狗。
//
// 与总时长那条互补：总时长上限设得很宽松（1 小时，等于不设），
// 只留首帧闸门 300ms。若首帧闸门没接上，这里就会挂到守护超时。
func TestToolLoopStreamHasFirstByteWatchdog(t *testing.T) {
	h, _ := toolLoopWatchdogHandler(t, time.Hour, 300*time.Millisecond)

	sw := newSSEWriter(httptest.NewRecorder())
	stats := newToolLoopStats(time.Now())

	done := make(chan struct{}, 1)
	go func() {
		p := toolLoopParams{
			ctx:        context.Background(),
			uid:        "u-wd",
			providerID: "loomy",
			start:      time.Now(),
		}
		_ = h.consumeRound(sw, p, []byte(`{"model":"loomy/auto"}`), map[string]bool{}, stats)
		done <- struct{}{}
	}()

	select {
	case <-done:
		// 正常：首帧闸门到点断流。
	case <-time.After(5 * time.Second):
		t.Fatal("首帧闸门没生效：连接建立了但上游不吐内容，consumeRound 一直挂着")
	}
}

// TestToolLoopHealthyStreamNotKilled 反向断言：正常吐字的流不该被闸门掐掉。
//
// 没有这条，"把闸门设成 1ms"也能让上面两条过 —— 那是把功能掐死而不是修好。
func TestToolLoopHealthyStreamNotKilled(t *testing.T) {
	p := pool.New("")
	p.SetDefaultProvider("loomy")
	p.SetMaxInFlight(10)
	a := &auth.Auth{UID: "u-ok", Nickname: "正常号"}
	p.SyncToDirWithSecrets("loomy", []*auth.Auth{a}, map[string]any{"u-ok": &struct{}{}})
	p.SetCredits("u-ok", 100000)

	prov := &recordingProvider{id: "loomy", status: 200, body: sseOK}
	r := &singleProviderRouter{def: "loomy", p: p}
	// 复用 hangingProvider 的 router 形状，但换一个正常返回的 provider：
	// singleProviderRouter 的 Chat 走 r.prov，这里改用直接构造的 router。
	_ = prov
	_ = r

	// 直接用能返回正常 SSE 的 router（multiRouter 的 Chat 按 reg 分发）。
	okProv := &recordingProvider{id: "loomy", status: 200, body: sseOK}
	mr := &multiRouter{
		def: "loomy",
		p:   p,
		reg: map[string]*recordingProvider{"loomy": okProv},
	}
	h := NewHandler(Config{
		Pool:            p,
		Provider:        mr,
		DefaultProvider: "loomy",
		StreamMax:       30 * time.Second,
		StreamFirstByte: 5 * time.Second,
	})

	sw := newSSEWriter(httptest.NewRecorder())
	stats := newToolLoopStats(time.Now())
	p2 := toolLoopParams{
		ctx:        context.Background(),
		uid:        "u-ok",
		providerID: "loomy",
		start:      time.Now(),
	}
	out := h.consumeRound(sw, p2, []byte(`{"model":"loomy/auto"}`), map[string]bool{}, stats)
	if out.err != nil {
		t.Errorf("正常的短流不该被闸门掐掉，得到 err=%v", out.err)
	}
}
