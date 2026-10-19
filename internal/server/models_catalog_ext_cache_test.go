// models_catalog_ext_cache_test.go 扩展点这条倍率路径**也**必须走缓存。
//
// # 为什么单独一条（这是本次唯一由 Lead 发现的缺口）
//
// modelCatalogFor 有两条数据源：workbuddy 的 /v3/config，与其余上游的
// gateway.ModelMultiplierExt 扩展点。改造前是"先问扩展点，命中就 return"，
// 于是扩展点内部**无缓存**的上游调用（loomy 的 /models，实测 p90 10s /
// max 27.5s）每次都落在请求路径上 —— SWR 只覆盖了另一条路，形同虚设。
//
// 实测症状：/admin/models/preview 冷启动 60s（打满 curl 超时），
// 之后 0.2-4.5s 抖动。
//
// 本文件钉住：同一 provider 连续两次取目录，扩展点只被调**一次**。
package server

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NaiZi899/Naibox/internal/auth"
	"github.com/NaiZi899/Naibox/internal/gateway"
	"github.com/NaiZi899/Naibox/internal/pool"
)

// countingMultiplierProvider 记录 ModelMultipliers 被调次数。
type countingMultiplierProvider struct {
	id    string
	calls atomic.Int32
	// delay 模拟上游重尾（第二次不该再等它）。
	delay time.Duration
}

var _ gateway.Provider = (*countingMultiplierProvider)(nil)

func (p *countingMultiplierProvider) ID() string               { return p.id }
func (p *countingMultiplierProvider) Caps() gateway.Capability { return gateway.CapChat }
func (p *countingMultiplierProvider) Models(context.Context, gateway.Credential) ([]gateway.ModelInfo, error) {
	return []gateway.ModelInfo{{ID: p.id + "-m"}}, nil
}
func (p *countingMultiplierProvider) Chat(context.Context, gateway.Credential, []byte) (gateway.ChatStream, error) {
	return gateway.ChatStream{}, nil
}

// ModelMultipliers 实现 gateway.ModelMultiplierExt。
func (p *countingMultiplierProvider) ModelMultipliers(context.Context, gateway.Credential) (map[string]float64, bool) {
	p.calls.Add(1)
	if p.delay > 0 {
		time.Sleep(p.delay)
	}
	return map[string]float64{p.id + "-m": 2.5}, true
}

// extCacheRouter 让 ModelMultipliers 经 router 分派到桩上。
type extCacheRouter struct {
	multiRouter
	ext *countingMultiplierProvider
}

func (r *extCacheRouter) ModelMultipliers(_ context.Context, id string) (map[string]float64, bool) {
	if id != r.ext.id {
		return nil, false
	}
	return r.ext.ModelMultipliers(context.Background(), gateway.Credential{})
}

// TestMultiplierExtPathIsCached 扩展点路径的第二次取目录不得再打上游。
func TestMultiplierExtPathIsCached(t *testing.T) {
	ResetModelsCache()
	resetProviderModelsCaches()

	p := pool.New("")
	p.SetDefaultProvider("loomy")
	p.SetMaxInFlight(10)
	a := &auth.Auth{UID: "u-ext", Nickname: "扩展点号"}
	p.SyncToDirWithSecrets("loomy", []*auth.Auth{a}, map[string]any{"u-ext": &struct{}{}})

	ext := &countingMultiplierProvider{id: "loomy"}
	rt := &extCacheRouter{ext: ext}
	rt.def = "loomy"
	rt.p = p

	// ⚠ 必须注入 Upstream：modelCatalogFor 开头就要求它非 nil。
	// 用一个"永远失败"的桩：一旦代码走错到 /v3/config 那条路，
	// 用例会拿到 nil 而不是扩展点的结果 —— 失败信息直接指向走错了路。
	up := &slowCatalogUpstream{fail: true}
	h := NewHandler(Config{
		Pool:            p,
		Provider:        rt,
		Upstream:        up,
		DefaultProvider: "loomy",
		StreamMax:       time.Minute,
		StreamFirstByte: time.Minute,
	})

	first := h.modelCatalogFor("loomy")
	if first == nil {
		t.Fatal("第一次取目录应当拿到扩展点的结果")
	}
	if got := ext.calls.Load(); got != 1 {
		t.Fatalf("第一次取目录后扩展点调用次数 = %d，want 1", got)
	}

	// 第二次：必须命中缓存，不再打上游。
	second := h.modelCatalogFor("loomy")
	if second == nil {
		t.Fatal("第二次取目录应当命中缓存")
	}
	if got := ext.calls.Load(); got != 1 {
		t.Errorf("第二次取目录又打了扩展点（调用 %d 次，want 1）—— 这条路没走缓存，"+
			"loomy 的重尾会原样留在请求路径上", got)
	}
}

// TestMultiplierExtPathDoesNotBlockOnSlowUpstream 扩展点慢时不该拖住调用方。
//
// 与上一条互补：那条测"缓存命中"，这条测"缓存未命中时的等待有上界"。
// 上游 sleep 的时间远超首次预算，调用方必须在预算内返回（拿不到系数），
// 而不是等满上游 —— 这正是 27s 阻塞要消灭的形态。
func TestMultiplierExtPathDoesNotBlockOnSlowUpstream(t *testing.T) {
	ResetModelsCache()
	resetProviderModelsCaches()

	p := pool.New("")
	p.SetDefaultProvider("loomy")
	p.SetMaxInFlight(10)
	a := &auth.Auth{UID: "u-slow", Nickname: "慢号"}
	p.SyncToDirWithSecrets("loomy", []*auth.Auth{a}, map[string]any{"u-slow": &struct{}{}})

	ext := &countingMultiplierProvider{id: "loomy", delay: 10 * time.Second}
	rt := &extCacheRouter{ext: ext}
	rt.def = "loomy"
	rt.p = p

	h := NewHandler(Config{
		Pool:            p,
		Provider:        rt,
		Upstream:        &slowCatalogUpstream{fail: true},
		DefaultProvider: "loomy",
		StreamMax:       time.Minute,
		StreamFirstByte: time.Minute,
	})

	start := time.Now()
	_ = h.modelCatalogFor("loomy")
	elapsed := time.Since(start)

	// 预算 2.5s，留 1.5s 余量给调度。
	if elapsed > 4*time.Second {
		t.Errorf("扩展点上游 sleep 10s 时，调用方等了 %v —— 首屏被拖住了"+
			"（首次预算 modelCatalogFirstFetchBudget 没生效）", elapsed)
	}
}
