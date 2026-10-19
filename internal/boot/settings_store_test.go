package boot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/NaiZi899/Naibox/internal/admin"
)

// 复现并守住阶段 0 评审的 F1：**部分 PUT 不得覆盖未提交的配置**。
//
// # 缺陷原貌
//
// parse 用 `var p admin.Settings; json.Unmarshal(patch, &p)` ——
// 补丁里没出现的键保持零值，随后整个 p 被写回 config.json。
// 于是：
//
//	PUT {"checkin_enabled":false}
//	  → upstream.timeout_seconds 120 → 0
//	  → pool.max_in_flight         8 → 0
//	  → cooldown.soft_rate     "60s" → ""
//	  → session_sticky.enabled  true → false
//
// 用户只改一个开关，其它配置静默丢失。改造前就存在（非本次引入），
// 但危害真实，故一并修掉。

// settingsStoreForTest 造一个最小的 settingsStore（不启动调度器/日志）。
func settingsStoreForTest(t *testing.T, cfg *Config) *settingsStore {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &settingsStore{path: path, cfg: cfg}
}

// TestParseKeepsUnmentionedKeys 补丁里没提到的键必须保持原值。
func TestParseKeepsUnmentionedKeys(t *testing.T) {
	cfg := &Config{}
	cfg.Upstream.TimeoutSeconds = 120
	cfg.Upstream.HeaderTimeoutSeconds = 30
	cfg.Upstream.StreamMaxSeconds = 900
	cfg.Upstream.StreamFirstByteSeconds = 120
	cfg.Pool.MaxInFlight = 8
	cfg.Pool.BreakerThreshold = 5
	cfg.Cooldown.SoftRate = "60s"
	cfg.SessionSticky.Enabled = true
	cfg.Schedule.CheckinHours = []int{7, 19}
	cfg.Schedule.KeepaliveHours = []int{23}

	s := settingsStoreForTest(t, cfg)

	p, err := s.parse(json.RawMessage(`{"checkin_enabled":false}`))
	if err != nil {
		t.Fatal(err)
	}

	// 被提交的那个键要生效
	if p.CheckinEnabled {
		t.Error("补丁里的 checkin_enabled=false 应生效")
	}
	// 未提交的键一个都不许被清零
	if p.UpstreamTimeoutSec != 120 {
		t.Errorf("upstream.timeout_seconds 被冲掉了: %d，期望 120", p.UpstreamTimeoutSec)
	}
	if p.UpstreamHeaderTimeout != 30 {
		t.Errorf("upstream.header_timeout_seconds 被冲掉了: %d", p.UpstreamHeaderTimeout)
	}
	// 流式闸门也必须不被冲掉（新字段最容易漏进 currentSettings 基底）。
	if p.UpstreamStreamMax != 900 {
		t.Errorf("upstream.stream_max_seconds 被冲掉了: %d，期望 900", p.UpstreamStreamMax)
	}
	if p.UpstreamFirstByte != 120 {
		t.Errorf("upstream.stream_first_byte_seconds 被冲掉了: %d，期望 120", p.UpstreamFirstByte)
	}
	if p.PoolMaxInFlight != 8 {
		t.Errorf("pool.max_in_flight 被冲掉了: %d，期望 8", p.PoolMaxInFlight)
	}
	if p.BreakerThreshold != 5 {
		t.Errorf("pool.breaker_threshold 被冲掉了: %d", p.BreakerThreshold)
	}
	if p.SoftRate != "60s" {
		t.Errorf("cooldown.soft_rate 被冲掉了: %q", p.SoftRate)
	}
	if !p.SessionStickyEnabled {
		t.Error("session_sticky.enabled 被冲掉了（true → false）")
	}
	if len(p.CheckinHours) != 2 || p.CheckinHours[0] != 7 {
		t.Errorf("checkin_hours 被冲掉了: %v", p.CheckinHours)
	}
	if len(p.KeepaliveHours) != 1 || p.KeepaliveHours[0] != 23 {
		t.Errorf("keepalive_hours 被冲掉了: %v", p.KeepaliveHours)
	}
}

// TestParseAppliesPatchOnTopOfCurrent 补丁要**覆盖**基底（而不是被基底盖掉）。
func TestParseAppliesPatchOnTopOfCurrent(t *testing.T) {
	cfg := &Config{}
	cfg.Upstream.TimeoutSeconds = 120
	cfg.Pool.MaxInFlight = 8

	s := settingsStoreForTest(t, cfg)
	p, err := s.parse(json.RawMessage(`{"upstream_timeout_seconds":300,"pool_max_in_flight":99}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.UpstreamTimeoutSec != 300 {
		t.Errorf("补丁应覆盖基底: 得到 %d，期望 300", p.UpstreamTimeoutSec)
	}
	if p.PoolMaxInFlight != 99 {
		t.Errorf("补丁应覆盖基底: 得到 %d，期望 99", p.PoolMaxInFlight)
	}
}

// TestParseEmptyPatchKeepsEverything 空补丁 = 不改任何东西。
func TestParseEmptyPatchKeepsEverything(t *testing.T) {
	cfg := &Config{}
	cfg.Upstream.TimeoutSeconds = 77
	cfg.Pool.MaxInFlight = 3
	s := settingsStoreForTest(t, cfg)

	for _, patch := range []string{``, `{}`} {
		p, err := s.parse(json.RawMessage(patch))
		if err != nil {
			t.Fatalf("patch=%q: %v", patch, err)
		}
		if p.UpstreamTimeoutSec != 77 || p.PoolMaxInFlight != 3 {
			t.Errorf("patch=%q 不该改动任何值: timeout=%d maxInFlight=%d",
				patch, p.UpstreamTimeoutSec, p.PoolMaxInFlight)
		}
	}
}

// TestParseRejectsMalformed 坏 JSON 要报错，而不是静默返回零值基座。
func TestParseRejectsMalformed(t *testing.T) {
	s := settingsStoreForTest(t, &Config{})
	if _, err := s.parse(json.RawMessage(`{"checkin_enabled":`)); err == nil {
		t.Error("损坏的 JSON 应报错")
	}
}

// TestCurrentSettingsCoversAllWritableFields currentSettings 与 applyToMemory
// 必须**字段对称**：一个是读、一个是写。
//
// # 为什么需要这条
//
// 若 currentSettings 漏了某个 applyToMemory 会写的字段，
// 那个字段每次保存都会被"基底的零值"覆盖 —— 也就是 F1 的复发形态。
// 这条测试用"写进去再读出来"的往返来证明对称性，
// 将来加字段时若只改了一边，这里会红。
func TestCurrentSettingsCoversAllWritableFields(t *testing.T) {
	// 先把所有可写字段设成**非零且互不相同**的值
	src := admin.Settings{
		SoftRate:              "45s",
		UpstreamTimeoutSec:    101,
		UpstreamHeaderTimeout: 102,
		UpstreamIdleTimeout:   103,
		UpstreamStreamMax:     110,
		UpstreamFirstByte:     111,
		PoolMaxInFlight:       104,
		BreakerThreshold:      105,
		BreakerCooldown:       "106s",
		BreakerCooldownMax:    "107s",
		SanitizeFingerprints:  true,
		UpstashURL:            "https://upstash.example",
		SessionStickyEnabled:  true,
		SessionStickyTTL:      "109s",
		CheckinEnabled:        true,
		KeepaliveEnabled:      true,
		CheckinHours:          []int{1, 2},
		KeepaliveHours:        []int{3},
	}

	cfg := &Config{}
	s := settingsStoreForTest(t, cfg)

	// 用 applyToMemory 写入内存（这是"保存后生效"的路径）
	s.applyToMemory(src)

	// 再用 currentSettings 读回来
	got := s.currentSettings()

	// 逐字段比对：漏掉的字段会读出零值 → 报错
	check := func(name string, want, have any) {
		t.Helper()
		wj, _ := json.Marshal(want)
		hj, _ := json.Marshal(have)
		if string(wj) != string(hj) {
			t.Errorf("%s 不对称: applyToMemory 写入 %s，currentSettings 读出 %s —— "+
				"该字段每次保存都会被零值冲掉（F1 复发形态）", name, wj, hj)
		}
	}
	check("SoftRate", src.SoftRate, got.SoftRate)
	check("UpstreamTimeoutSec", src.UpstreamTimeoutSec, got.UpstreamTimeoutSec)
	check("UpstreamHeaderTimeout", src.UpstreamHeaderTimeout, got.UpstreamHeaderTimeout)
	check("UpstreamIdleTimeout", src.UpstreamIdleTimeout, got.UpstreamIdleTimeout)
	// 本次新增的流式两道闸门：与上面三项同一条链路，必须同样对称。
	check("UpstreamStreamMax", src.UpstreamStreamMax, got.UpstreamStreamMax)
	check("UpstreamFirstByte", src.UpstreamFirstByte, got.UpstreamFirstByte)
	check("PoolMaxInFlight", src.PoolMaxInFlight, got.PoolMaxInFlight)
	check("BreakerThreshold", src.BreakerThreshold, got.BreakerThreshold)
	check("BreakerCooldown", src.BreakerCooldown, got.BreakerCooldown)
	check("BreakerCooldownMax", src.BreakerCooldownMax, got.BreakerCooldownMax)
	check("SanitizeFingerprints", src.SanitizeFingerprints, got.SanitizeFingerprints)
	check("UpstashURL", src.UpstashURL, got.UpstashURL)
	check("SessionStickyEnabled", src.SessionStickyEnabled, got.SessionStickyEnabled)
	check("SessionStickyTTL", src.SessionStickyTTL, got.SessionStickyTTL)
	check("CheckinEnabled", src.CheckinEnabled, got.CheckinEnabled)
	check("KeepaliveEnabled", src.KeepaliveEnabled, got.KeepaliveEnabled)
	check("CheckinHours", src.CheckinHours, got.CheckinHours)
	check("KeepaliveHours", src.KeepaliveHours, got.KeepaliveHours)
}

// TestHiddenProvidersRoundTrip 守「上游显示过滤」这一项的登记链路。
//
// # 为什么这条必须单独测
//
// 设置项的登记是**五处**分散的（Settings 字段 / Snapshot / currentSettings /
// applyToMemory / persist），漏任何一处都不报错：
//   - 漏 persist       → 当场生效、重启后丢失（用户以为"有时管用"）
//   - 漏 applyToMemory → 内存与文件不一致，同一次会话里前后两次读不同
//   - 漏 json tag      → 设置页 PUT 上来直接被丢弃，而且**不留任何痕迹**
//
// 这个仓库已经在同类"五处登记"上栽过（见 settings.go 里 UpstreamStreamMax
// 的注释），所以这里用一条端到端断言把链路串起来。
//
// ⚠ 不走 Apply：它会调用 scheduler（本 helper 没有调度器，nil 指针会 panic）。
// 走的正是 Apply 内部那三步 —— parse → applyToMemory → persist。
func TestHiddenProvidersRoundTrip(t *testing.T) {
	cfg := &Config{}
	st := settingsStoreForTest(t, cfg)

	// 1) parse：设置页 PUT 的 JSON 必须能落到字段上（json tag 对不上就静默丢弃）
	p, err := st.parse(json.RawMessage(`{"hidden_providers":["jiyuan","inkstone"]}`))
	if err != nil {
		t.Fatalf("parse 失败: %v", err)
	}
	if len(p.HiddenProviders) != 2 || p.HiddenProviders[0] != "jiyuan" {
		t.Fatalf("parse 后 HiddenProviders = %v，want [jiyuan inkstone]", p.HiddenProviders)
	}

	// 2) applyToMemory：内存 cfg 必须同步
	st.applyToMemory(p)
	if got := cfg.UI.HiddenProviders; len(got) != 2 || got[1] != "inkstone" {
		t.Fatalf("内存 cfg.UI.HiddenProviders = %v，want [jiyuan inkstone]", got)
	}

	// 3) persist：config.json 的 ui.hidden_providers 必须真的写进去（重启才不丢）
	if err := st.persist(p); err != nil {
		t.Fatalf("persist 失败: %v", err)
	}
	raw, err := os.ReadFile(st.path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		UI struct {
			HiddenProviders []string `json:"hidden_providers"`
		} `json:"ui"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("config.json 不是合法 JSON: %v", err)
	}
	if len(doc.UI.HiddenProviders) != 2 {
		t.Fatalf("config.json 的 ui.hidden_providers = %v，want 2 项", doc.UI.HiddenProviders)
	}

	// 4) currentSettings：读回路径必须与写对称（漏这处会让 Snapshot 永远空）
	if got := st.currentSettings().HiddenProviders; len(got) != 2 {
		t.Fatalf("currentSettings().HiddenProviders = %v，want 2 项", got)
	}

	// 5) 清空：空列表必须真的清掉（写回 [] 而不是保留旧值），
	//    否则用户"全部恢复显示"永远失效。
	p.HiddenProviders = nil
	st.applyToMemory(p)
	if err := st.persist(p); err != nil {
		t.Fatalf("persist 清空失败: %v", err)
	}
	if got := cfg.UI.HiddenProviders; len(got) != 0 {
		t.Fatalf("清空后内存仍为 %v", got)
	}
	raw2, _ := os.ReadFile(st.path)
	var doc2 struct {
		UI struct {
			HiddenProviders []string `json:"hidden_providers"`
		} `json:"ui"`
	}
	_ = json.Unmarshal(raw2, &doc2)
	if len(doc2.UI.HiddenProviders) != 0 {
		t.Fatalf("清空后文件仍为 %v", doc2.UI.HiddenProviders)
	}
}

// TestHiddenProvidersAreCopied 返回值与内部状态不得共享底层数组。
//
// 不拷贝的话，调用方（前端序列化、测试、或未来的过滤逻辑）只要 append
// 一次，就会改写配置里的切片长度/元素 —— 表现为"隐藏列表莫名其妙多一项"。
func TestHiddenProvidersAreCopied(t *testing.T) {
	cfg := &Config{}
	cfg.UI.HiddenProviders = []string{"jiyuan"}
	st := settingsStoreForTest(t, cfg)

	// 用 currentSettings 而不是 Snapshot：后者要读调度器槽位（本 helper 没有）。
	// 两者都用 append([]string(nil), ...) 拷贝，语义同一份。
	got := st.currentSettings().HiddenProviders
	_ = append(got, "inkstone") // 典型误用：调用方在返回值上 append
	if n := len(cfg.UI.HiddenProviders); n != 1 {
		t.Fatalf("append 返回的切片污染了配置：cfg 现在有 %d 项", n)
	}
}
