// models_catalog_fallback_test.go 倍率的**回落链**回归。
//
// # 这条测试是为一次真实回归补的（用户报"倍率全丢了"）
//
// 上一版把 modelCatalogFor 的两条数据源写成了"互不相通的独立路径"：
// 进了扩展点分支就在分支内解决，无论成功失败都不再往下走。而注册表里的
// 5 个上游 id **全部非空**（workbuddy/inkstone/jiyuan/loomy/trae），
// 于是每一个都进了那个分支 —— **不实现 ModelMultiplierExt 的上游
// （workbuddy / inkstone / jiyuan）再也拿不到倍率**，
// 下面那段 /v3/config 的代码成了死代码。
//
// 实测症状：/admin/models/preview 从"各上游都有倍率"掉到只剩 loomy 的 8 条；
// /admin/stats 的 model_catalog 变成 `unavailable / 0`（那是默认上游
// workbuddy 的状态）。
//
// # 上一版为什么没被测试抓住
//
// 当时只测了"扩展点这条路**也**走缓存"（models_catalog_ext_cache_test.go），
// 而那个用例的上游**实现了**扩展点。缺的是对照：
// **不实现扩展点的上游必须落到 /v3/config**。
//
// 所以本文件钉住的是**回落链本身**，两个方向都钉：
//
//	有扩展点   → 用扩展点的结果（不走 /v3/config）
//	无扩展点   → 落到 /v3/config（不能让它的倍率凭空消失）
package server

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NaiZi899/omni2api/internal/auth"
	"github.com/NaiZi899/omni2api/internal/gateway"
	"github.com/NaiZi899/omni2api/internal/pool"
)

// noExtProvider 一个**不实现** ModelMultiplierExt 的上游。
//
// 关键：只实现 gateway.Provider 的基础方法，刻意**没有** ModelMultipliers。
// 这正是 workbuddy/inkstone/jiyuan 的形态。
type noExtProvider struct {
	id string
}

var _ gateway.Provider = (*noExtProvider)(nil)

func (p *noExtProvider) ID() string               { return p.id }
func (p *noExtProvider) Caps() gateway.Capability { return gateway.CapChat }
func (p *noExtProvider) Models(context.Context, gateway.Credential) ([]gateway.ModelInfo, error) {
	return []gateway.ModelInfo{{ID: p.id + "-m"}}, nil
}
func (p *noExtProvider) Chat(context.Context, gateway.Credential, []byte) (gateway.ChatStream, error) {
	return gateway.ChatStream{}, nil
}

// countingCatalogUpstream 记录 /v3/config 被拉了几次（扮演 workbuddy 的客户端）。
type countingCatalogUpstream struct {
	hits   atomic.Int32
	models []gateway.ModelCatalogEntry
	fail   bool
}

func (s *countingCatalogUpstream) FetchModelCatalog(*auth.Auth) (*gateway.ModelCatalog, error) {
	s.hits.Add(1)
	if s.fail {
		return nil, errors.New("stub: /v3/config 不可用")
	}
	return &gateway.ModelCatalog{Models: s.models}, nil
}
func (s *countingCatalogUpstream) FetchModels(*auth.Auth) ([]DefaultModel, error) { return nil, nil }
func (s *countingCatalogUpstream) RefreshToken(*auth.Auth) error                  { return nil }
func (s *countingCatalogUpstream) ChatStreamWithIP(*auth.Auth, []byte, string) (io.ReadCloser, int, []byte, error) {
	return nil, 0, nil, errors.New("stub: 本用例不打对话")
}

// noExtRouter 一个只注册 noExtProvider 的路由（ModelMultipliers 恒 false）。
type noExtRouter struct {
	multiRouter
	id string
}

func (r *noExtRouter) ModelMultipliers(context.Context, string) (map[string]float64, bool) {
	return nil, false // 上游没实现扩展点
}

// fallbackFixture 造一个"上游无扩展点 + /v3/config 有数据"的部署。
func fallbackFixture(t *testing.T, up *countingCatalogUpstream) (*Handler, *pool.Pool) {
	t.Helper()
	p := pool.New("")
	p.SetDefaultProvider("workbuddy")
	p.SetMaxInFlight(10)
	a := &auth.Auth{UID: "u-fb", Nickname: "回落号"}
	p.SyncToDirWithSecrets("workbuddy", []*auth.Auth{a}, map[string]any{"u-fb": &struct{}{}})
	p.SetCredits("u-fb", 100000)

	rt := &noExtRouter{id: "workbuddy"}
	rt.def = "workbuddy"
	rt.p = p

	h := NewHandler(Config{
		Pool:            p,
		Provider:        rt,
		Upstream:        up,
		DefaultProvider: "workbuddy",
		StreamMax:       time.Minute,
		StreamFirstByte: time.Minute,
	})
	return h, p
}

// TestCatalogFallsBackToV3ConfigWhenNoExt 没有扩展点的上游必须落到 /v3/config。
//
// 这是那个回归的直接判据：若 modelCatalogFor 在扩展点分支里"解决"掉，
// 本用例会拿到 nil（倍率凭空消失）。
func TestCatalogFallsBackToV3ConfigWhenNoExt(t *testing.T) {
	ResetModelsCache()
	resetProviderModelsCaches()

	up := &countingCatalogUpstream{models: []gateway.ModelCatalogEntry{
		{ID: "glm-5.2", Multiplier: 3, MultiplierKnown: true},
	}}
	h, _ := fallbackFixture(t, up)

	cat := h.modelCatalogFor("workbuddy")
	if cat == nil {
		t.Fatal("★ 不实现 ModelMultiplierExt 的上游拿到了 nil —— 它的倍率会整片消失" +
			"（这正是用户报的「倍率全丢了」）。回落链断了：扩展点拿不到就该继续走 /v3/config。")
	}
	if len(cat.Models) != 1 || cat.Models[0].ID != "glm-5.2" {
		t.Errorf("目录内容不对: %+v", cat.Models)
	}
	if up.hits.Load() == 0 {
		t.Error("没有调用 /v3/config —— 说明走的是别的路（而那条路对这个上游没有数据）")
	}
}

// TestCatalogExtWinsOverV3Config 有扩展点时优先用它，不去打 /v3/config。
//
// 反面：否则"拿别家凭证打 copilot.tencent.com"会失败并惩罚无辜账号
// （这是仓库既有注释里写明的取舍）。同时钉住"顺序不能颠倒"。
func TestCatalogExtWinsOverV3Config(t *testing.T) {
	ResetModelsCache()
	resetProviderModelsCaches()

	up := &countingCatalogUpstream{models: []gateway.ModelCatalogEntry{
		{ID: "from-v3config", Multiplier: 9, MultiplierKnown: true},
	}}
	p := pool.New("")
	p.SetDefaultProvider("loomy")
	p.SetMaxInFlight(10)
	a := &auth.Auth{UID: "u-ext2", Nickname: "扩展点号"}
	p.SyncToDirWithSecrets("loomy", []*auth.Auth{a}, map[string]any{"u-ext2": &struct{}{}})

	ext := &countingMultiplierProvider{id: "loomy"}
	rt := &extCacheRouter{ext: ext}
	rt.def = "loomy"
	rt.p = p

	h := NewHandler(Config{
		Pool: p, Provider: rt, Upstream: up, DefaultProvider: "loomy",
		StreamMax: time.Minute, StreamFirstByte: time.Minute,
	})

	cat := h.modelCatalogFor("loomy")
	if cat == nil {
		t.Fatal("有扩展点却拿到 nil")
	}
	// 扩展点给的是 loomy-m:2.5（见 countingMultiplierProvider）
	found := false
	for _, m := range cat.Models {
		if m.ID == "loomy-m" {
			found = true
		}
		if m.ID == "from-v3config" {
			t.Error("用了 /v3/config 的结果 —— 有扩展点时不该走那条路" +
				"（拿别家凭证打 workbuddy 的 /v3/config 会失败并惩罚无辜账号）")
		}
	}
	if !found {
		t.Errorf("目录里没有扩展点给的模型: %+v", cat.Models)
	}
	if ext.calls.Load() == 0 {
		t.Error("没有调用扩展点")
	}
}

// TestCatalogFallbackStillCached 回落链**也要**受缓存约束。
//
// 性能收益不能因为加了回落而丢：第二次取目录不该再打上游。
func TestCatalogFallbackStillCached(t *testing.T) {
	ResetModelsCache()
	resetProviderModelsCaches()

	up := &countingCatalogUpstream{models: []gateway.ModelCatalogEntry{
		{ID: "glm-5.2", Multiplier: 3, MultiplierKnown: true},
	}}
	h, _ := fallbackFixture(t, up)

	if h.modelCatalogFor("workbuddy") == nil {
		t.Fatal("第一次取目录就失败了")
	}
	after1 := up.hits.Load()
	if after1 == 0 {
		t.Fatal("第一次没打 /v3/config")
	}
	// 再取若干次：都应命中缓存。
	for i := 0; i < 5; i++ {
		if h.modelCatalogFor("workbuddy") == nil {
			t.Fatalf("第 %d 次取目录返回 nil", i+2)
		}
	}
	if got := up.hits.Load(); got != after1 {
		t.Errorf("缓存命中时又打了上游 %d 次（第一次 %d，现在 %d）—— "+
			"回落链破坏了缓存收益", got-after1, after1, got)
	}
}
