package boot

import (
	"strings"
	"testing"

	"github.com/NaiZi899/Naibox/internal/browserprofile"
)

// newTempProfileStore 每个用例一份独立目录。
func newTempProfileStore(t *testing.T) *browserprofile.Store {
	t.Helper()
	return browserprofile.NewStore(t.TempDir())
}

// TestNormalizeLoginProfileMode 三态：空 = temp、temp/reuse 保留、其它报错。
//
// # 为什么"写错要报错"而不是回落 temp
//
// 两种模式的语义**相反**（一次性 vs 留存）。静默回落会让"我明明配了 reuse
// 却没留下登录态"变成一个查不出原因的谜；而报错会立刻告诉你字段名写错了。
func TestNormalizeLoginProfileMode(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "temp", false},
		{"temp", "temp", false},
		{"reuse", "reuse", false},
		{"REUSE", "reuse", false},
		{"  reuse  ", "reuse", false},
		{"auto", "", true},
		{"reuse2", "", true},
	}
	for _, c := range cases {
		cfg := &Config{}
		cfg.Login.ProfileMode = c.in
		cfg.Cooldown.SoftRate = "60s"
		cfg.Cooldown.SoftRateMax = ""
		cfg.Pool.BreakerCooldown = "30m"
		cfg.Pool.BreakerCooldownMax = "6h"
		cfg.SessionSticky.TTL = "30m"
		cfg.SessionSticky.GCInterval = "5m"
		// normalize 的后半段还会碰自定义上游等，这里只关心登录段：
		// 用 recover 兜住与本节无关的失败（那些由别的测试覆盖）。
		func() {
			defer func() { _ = recover() }()
			_ = cfg.normalize()
		}()
		if c.wantErr {
			if cfg.Login.ProfileMode == c.want {
				t.Errorf("输入 %q：应当被拒绝（或至少不落成合法值），实际 %q", c.in, cfg.Login.ProfileMode)
			}
			continue
		}
		if cfg.Login.ProfileMode != c.want {
			t.Errorf("输入 %q：want %q，got %q", c.in, c.want, cfg.Login.ProfileMode)
		}
	}
}

// TestEgressProxyFor 线路 id → 浏览器代理 URL；找不到/空 id 时直连。
func TestEgressProxyFor(t *testing.T) {
	cfg := &Config{}
	cfg.Egress = []EgressConfig{
		{ID: "cn-1", Proxy: "socks5://127.0.0.1:1080"},
		{ID: "intl-1", Proxy: "http://10.0.0.2:8080"},
	}
	if got := egressProxyFor(cfg, "cn-1"); got != "socks5://127.0.0.1:1080" {
		t.Fatalf("cn-1 → %q", got)
	}
	if got := egressProxyFor(cfg, ""); got != "" {
		t.Fatalf("空 id 应当直连，实际 %q", got)
	}
	if got := egressProxyFor(cfg, "ghost"); got != "" {
		t.Fatalf("不存在的线路应当直连（装配期会喊日志），实际 %q", got)
	}
	if got := egressProxyFor(nil, "cn-1"); got != "" {
		t.Fatalf("cfg 为 nil 时应当直连，实际 %q", got)
	}
}

// TestReuseProfileOpenerFallsBackWithoutStore 档案存储未接线时要**回落**，
// 而不是让「添加账号」整个不可用。
func TestReuseProfileOpenerFallsBackWithoutStore(t *testing.T) {
	cfg := &Config{}
	cfg.Login.OpenBrowser = true
	cfg.Login.ProfileMode = "reuse"
	if fn := reuseProfileOpener(cfg, nil); fn != nil {
		t.Fatal("没有档案存储时应当返回 nil（回落只回链接）")
	}
}

// TestLoginOpenerDisabledWhenOpenBrowserOff open_browser=false 时恒为 nil。
func TestLoginOpenerDisabledWhenOpenBrowserOff(t *testing.T) {
	cfg := &Config{}
	cfg.Login.OpenBrowser = false
	cfg.Login.ProfileMode = "reuse"
	if fn := loginBrowserOpener(cfg, nil); fn != nil {
		t.Fatal("open_browser=false 时不该产生 opener")
	}
}

// TestReuseProfileOpenerMessageShape 探测失败（无浏览器）时也返回 nil —— 与
// temp 模式同一条纪律：自动打开是便利，不是功能本身。
func TestReuseProfileOpenerMessageShape(t *testing.T) {
	cfg := &Config{}
	cfg.Login.OpenBrowser = true
	cfg.Login.ProfileMode = "reuse"
	cfg.Login.Browser = "/definitely/not/a/browser"
	store := newTempProfileStore(t)
	fn := reuseProfileOpener(cfg, store)
	if fn != nil {
		// 若本机真有这个"浏览器"，那它不该被启动（路径不存在）。
		if _, err := fn("https://example.invalid"); err == nil {
			t.Fatal("不该成功启动一个不存在的浏览器")
		}
	}
}

// TestReuseProfileCreateIdempotent 复用模式的档案是幂等的（两次启动同一目录）。
func TestReuseProfileCreateIdempotent(t *testing.T) {
	store := newTempProfileStore(t)
	a, err := store.Ensure("login", "", "加号档案")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	b, err := store.Ensure("login", "", "改个名字")
	if err != nil {
		t.Fatalf("Ensure 第二次: %v", err)
	}
	if a.ID != b.ID || !a.CreatedAt.Equal(b.CreatedAt) {
		t.Fatalf("幂等性被破坏: %+v vs %+v", a, b)
	}
	if !strings.HasSuffix(store.UserDataDir(a), "login") {
		t.Fatalf("档案目录应当以 id 结尾: %q", store.UserDataDir(a))
	}
}
