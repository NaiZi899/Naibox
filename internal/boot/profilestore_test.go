package boot

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NaiZi899/Naibox/internal/auth"
	"github.com/NaiZi899/Naibox/internal/browserprofile"
	"github.com/NaiZi899/Naibox/internal/pool"
)

// newProfileFixture 造一个带两个账号的 pool + 一份线路表 + 空的档案存储。
func newProfileFixture(t *testing.T) (*profileStore, string) {
	t.Helper()
	root := t.TempDir()
	p := pool.New("")
	p.AddFor("workbuddy", &auth.Auth{UID: "u1", Nickname: "阿猫", Channel: "cn", EgressID: ""}, nil)
	p.AddFor("codearts", &auth.Auth{UID: "u2", Nickname: "", Channel: "intl"}, nil)

	cfg := &Config{}
	cfg.Egress = []EgressConfig{
		{ID: "cn-1", Proxy: "socks5://127.0.0.1:11080", Realm: "cn", MaxAccounts: 1, Region: "CN-ZJ"},
		{ID: "auth-1", Proxy: "socks5://user:pass@127.0.0.1:11081", Realm: "cn", MaxAccounts: 1},
	}
	store := browserprofile.NewStore(root)
	ps := newProfileStore(cfg, p, store, nil, func(string, ...any) {})
	return ps, root
}

// TestProfileUpsertBindsUIDAndName 用户要求：绑具体 uid，并把账号名一起绑上。
func TestProfileUpsertBindsUIDAndName(t *testing.T) {
	ps, _ := newProfileFixture(t)
	view, err := ps.Upsert(context.Background(), json.RawMessage(`{"id":"p1","uid":"u1"}`))
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if view.UID != "u1" {
		t.Fatalf("uid 没绑上: %+v", view)
	}
	if view.Label != "阿猫" {
		t.Fatalf("账号名应当自动带进标签，实际 %q", view.Label)
	}
	if view.Provider != "workbuddy" {
		t.Fatalf("上游应当随账号带出，实际 %q", view.Provider)
	}
	if view.AccountName != "阿猫" {
		t.Fatalf("列表要能显示账号名: %q", view.AccountName)
	}

	// 昵称空时回落 uid（否则标签是空的，界面上只剩一串数字）
	v2, err := ps.Upsert(context.Background(), json.RawMessage(`{"id":"p2","uid":"u2"}`))
	if err != nil {
		t.Fatalf("Upsert u2: %v", err)
	}
	if v2.Label != "u2" {
		t.Fatalf("昵称空时标签应当回落 uid，实际 %q", v2.Label)
	}
}

// TestProfileUpsertKeepsUserLabel 用户自己起的名字不该被昵称覆盖（保存一次丢一次）。
func TestProfileUpsertKeepsUserLabel(t *testing.T) {
	ps, _ := newProfileFixture(t)
	if _, err := ps.Upsert(context.Background(), json.RawMessage(`{"id":"p1","uid":"u1","label":"主号档案"}`)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	v, ok := ps.store.Get("p1")
	if !ok || v.Label != "主号档案" {
		t.Fatalf("用户起的标签被覆盖: %+v", v)
	}
}

// TestProfileUpsertRejects 三种非法输入必须在保存时就拒（而不是启动时才炸）。
func TestProfileUpsertRejects(t *testing.T) {
	ps, _ := newProfileFixture(t)
	cases := []struct{ name, body, wantIn string }{
		{"不存在的账号", `{"id":"p1","uid":"ghost"}`, "不在池里"},
		{"不存在的线路", `{"id":"p1","egress_id":"ghost"}`, "不存在"},
		{"附加参数覆盖档案目录", `{"id":"p1","extra_args":["--user-data-dir=/tmp/x"]}`, "user-data-dir"},
		{"附加参数覆盖代理", `{"id":"p1","extra_args":["--proxy-server=socks5://x:1"]}`, "proxy-server"},
		{"非法 id", `{"id":"../etc"}`, "id"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			_, err := ps.Upsert(context.Background(), json.RawMessage(c.body))
			if err == nil {
				t.Fatalf("应当拒绝: %s", c.body)
			}
			if !strings.Contains(err.Error(), c.wantIn) {
				t.Fatalf("错误信息应当提到 %q，实际 %q", c.wantIn, err.Error())
			}
		})
	}
}

// TestProfileUpsertPreservesBookkeeping 编辑不该清掉"上次使用/调试端口"这些记账字段。
func TestProfileUpsertPreservesBookkeeping(t *testing.T) {
	ps, _ := newProfileFixture(t)
	if _, err := ps.Upsert(context.Background(), json.RawMessage(`{"id":"p1","uid":"u1"}`)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := ps.store.Touch("p1", "https://a.example", 51234); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	// 只改标签
	if _, err := ps.Upsert(context.Background(), json.RawMessage(`{"id":"p1","uid":"u1","label":"改名了"}`)); err != nil {
		t.Fatalf("Upsert 第二次: %v", err)
	}
	v, _ := ps.store.Get("p1")
	if v.LastDebugPort != 51234 || v.LastURL != "https://a.example" || v.LastUsedAt.IsZero() {
		t.Fatalf("记账字段被编辑清掉了: %+v", v)
	}
}

// TestProfileListShowsDiskUsage 目录里真有东西时，列表要报体积（页面用来提示"几百 MB"）。
func TestProfileListShowsDiskUsage(t *testing.T) {
	ps, root := newProfileFixture(t)
	if _, err := ps.Upsert(context.Background(), json.RawMessage(`{"id":"p1","uid":"u1"}`)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	dir := filepath.Join(root, "browser", "p1", "Default")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Cookies"), make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	list, browser, accounts := ps.List(context.Background())
	if len(list) != 1 {
		t.Fatalf("档案数不对: %d", len(list))
	}
	if list[0].DiskBytes < 4096 {
		t.Fatalf("目录体积没算出来: %d", list[0].DiskBytes)
	}
	if list[0].AccountName != "阿猫" {
		t.Fatalf("账号名没并进来: %+v", list[0])
	}
	if !browser.OK {
		t.Fatalf("本机应当能探测到浏览器（测试机装了 Chrome/Edge）: %+v", browser)
	}
	if len(accounts) != 2 {
		t.Fatalf("可绑定账号应当有 2 个，实际 %d", len(accounts))
	}
	// 排序稳定：u1 在 u2 前
	if accounts[0].UID != "u1" || accounts[1].UID != "u2" {
		t.Fatalf("账号投影没按 uid 排序: %+v", accounts)
	}
}

// TestProfileLaunchErrors 启动前把"必然失败"的三种情况说清楚。
func TestProfileLaunchErrors(t *testing.T) {
	ps, _ := newProfileFixture(t)

	if _, err := ps.Launch(context.Background(), "ghost", ""); err == nil {
		t.Fatal("不存在的档案应当报错")
	}

	// 绑了带认证的线路：启动必须拒绝并解释（Chrome 不处理代理认证）
	if _, err := ps.Upsert(context.Background(), json.RawMessage(`{"id":"p-auth","egress_id":"auth-1"}`)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	_, err := ps.Launch(context.Background(), "p-auth", "")
	if err == nil {
		t.Fatal("带认证的代理应当在启动时被拒绝")
	}
	if !strings.Contains(err.Error(), "认证") {
		t.Fatalf("错误信息应当说清是认证问题，实际 %q", err.Error())
	}
}

// TestProfileOpenDirWithoutDesktop 服务端形态（openPath=nil）要明确"不支持"。
func TestProfileOpenDirWithoutDesktop(t *testing.T) {
	ps, _ := newProfileFixture(t)
	if _, err := ps.Upsert(context.Background(), json.RawMessage(`{"id":"p1"}`)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	err := ps.OpenDir(context.Background(), "p1")
	if err == nil {
		t.Fatal("没有桌面时应当报不支持")
	}
	if !strings.Contains(err.Error(), "服务端形态") {
		t.Fatalf("错误信息应当说明形态限制，实际 %q", err.Error())
	}
}

// fakeVariantExe 造一个"像变体"的可执行文件：里面带 --fingerprint-* 的开关字符串。
// 探测看的就是这些字符串，所以不需要真的跑它。
func fakeVariantExe(t *testing.T, dir string, variant bool) string {
	t.Helper()
	path := filepath.Join(dir, map[bool]string{true: "variant.exe", false: "plain.exe"}[variant])
	body := "MZ fake browser binary"
	if variant {
		body += "\x00--fingerprint-canvas-noise\x00--fingerprint-platform=Win32"
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestProfileFingerprintRequiresVariant 原版浏览器 + 指纹参数 = 保存时就拒。
//
// 这条守的是最难查的失败形态：参数被静默忽略，窗口照开，指纹没变。
func TestProfileFingerprintRequiresVariant(t *testing.T) {
	ps, root := newProfileFixture(t)
	plain := fakeVariantExe(t, root, false)
	body := `{"id":"p1","browser_path":` + bodyQuote(plain) +
		`,"fingerprint":{"canvas_noise":true}}`
	_, err := ps.Upsert(context.Background(), json.RawMessage(body))
	if err == nil {
		t.Fatal("原版浏览器不该允许保存指纹参数")
	}
	if !strings.Contains(err.Error(), "变体") {
		t.Fatalf("错误信息应当指向变体，实际 %q", err.Error())
	}
}

// TestProfileFingerprintWithVariant 变体上保存成功，并把"支持"如实回给界面。
func TestProfileFingerprintWithVariant(t *testing.T) {
	ps, root := newProfileFixture(t)
	variant := fakeVariantExe(t, root, true)
	body := `{"id":"p1","browser_path":` + bodyQuote(variant) +
		`,"fingerprint":{"canvas_noise":true,"screen_width":1920,"lang":"zh-CN"}}`
	view, err := ps.Upsert(context.Background(), json.RawMessage(body))
	if err != nil {
		t.Fatalf("变体上应当保存成功: %v", err)
	}
	if view.Fingerprint == nil || view.Fingerprint.ScreenWidth != 1920 {
		t.Fatalf("指纹没存进档案: %+v", view.Fingerprint)
	}
	if !view.FingerprintOK {
		t.Fatalf("应当报支持: %+v", view)
	}
	if !strings.Contains(view.FingerprintNote, "支持") {
		t.Fatalf("说明文字不对: %q", view.FingerprintNote)
	}
	// 启动参数里真的带上（不启动进程，只看 Plan）
	p, _ := ps.store.Get("p1")
	cmd, err := browserprofile.Plan("linux", p, t.TempDir(), browserprofile.Options{Explicit: variant})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	joined := strings.Join(cmd.Args, " ")
	if !strings.Contains(joined, "--fingerprint-canvas-noise") || !strings.Contains(joined, "--fingerprint-screen-width=1920") {
		t.Fatalf("指纹参数没进命令行: %v", cmd.Args)
	}
}

// TestProfileFingerprintRejectsBadValues 越界值在保存时就拒。
func TestProfileFingerprintRejectsBadValues(t *testing.T) {
	ps, root := newProfileFixture(t)
	variant := fakeVariantExe(t, root, true)
	body := `{"id":"p1","browser_path":` + bodyQuote(variant) +
		`,"fingerprint":{"screen_width":100}}`
	_, err := ps.Upsert(context.Background(), json.RawMessage(body))
	if err == nil || !strings.Contains(err.Error(), "指纹参数非法") {
		t.Fatalf("越界屏幕宽度应当被拒，实际 %v", err)
	}
}

// TestProfileFingerprintEmptyBecomesNil 前端传一个空对象等于"不启用指纹"。
func TestProfileFingerprintEmptyBecomesNil(t *testing.T) {
	ps, _ := newProfileFixture(t)
	view, err := ps.Upsert(context.Background(), json.RawMessage(`{"id":"p1","fingerprint":{}}`))
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if view.Fingerprint != nil {
		t.Fatalf("空指纹应当存成 nil，实际 %+v", view.Fingerprint)
	}
	p, _ := ps.store.Get("p1")
	if p.Fingerprint != nil {
		t.Fatalf("档案里也不该留一个空指纹对象: %+v", p.Fingerprint)
	}
}

// bodyQuote 把路径安全地包成 JSON 字符串（Windows 反斜杠要转义）。
func bodyQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
