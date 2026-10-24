package browserprofile

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeLookup 固定返回一个假浏览器，测试因此完全离线。
func fakeLookup(id string) (string, bool) {
	if id == "msedge" {
		return "/fake/msedge", true
	}
	return "", false
}

// TestPlanUsesPersistentProfileAndNoIncognito 参数向量的三件必做事：
// 持久目录、调试端口、**不带无痕**。
//
// 不带无痕是本包最容易被"顺手加回去"的东西（browseropen 那边就是无痕），
// 一旦加上，登录态就留不住了 —— 而表现出来只是"下次又要重新登录"，
// 没人会联想到参数。
func TestPlanUsesPersistentProfileAndNoIncognito(t *testing.T) {
	dir := t.TempDir() + "/profile-a"
	cmd, err := Plan("linux", Profile{ID: "a"}, dir, Options{
		URL:    "https://example.com/auth",
		Lookup: fakeLookup,
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if cmd.Private {
		t.Fatal("持久档案不能带无痕")
	}
	joined := ""
	for _, a := range cmd.Args {
		if a == "--incognito" || a == "--inprivate" {
			t.Fatalf("持久档案不该带无痕参数: %v", cmd.Args)
		}
		joined += a + " "
	}
	for _, want := range []string{"--user-data-dir=" + dir, "--remote-debugging-port=0", "--no-first-run"} {
		if !containsStr(joined, want) {
			t.Fatalf("参数缺 %q：%v", want, cmd.Args)
		}
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("档案目录应当被建出来（否则浏览器可能改用默认 profile）: %v", err)
	}
	if got := cmd.Argv(); got[len(got)-1] != "https://example.com/auth" {
		t.Fatalf("URL 必须在末位，实际 %v", got)
	}
}

// TestPlanRejectsAuthenticatedProxy 带认证的代理必须报错而不是静默无效。
//
// Chrome 的 --proxy-server 不处理认证：给它 socks5://u:p@h:port，它会忽略
// 认证（甚至整条代理），表现是"配了代理但出口没变"。宁可报错。
func TestPlanRejectsAuthenticatedProxy(t *testing.T) {
	dir := t.TempDir() + "/p"
	_, err := Plan("linux", Profile{ID: "a"}, dir, Options{
		Lookup:   fakeLookup,
		ProxyURL: "socks5://user:pass@127.0.0.1:1080",
	})
	if err == nil {
		t.Fatal("带认证的代理应当被拒绝")
	}
	if !containsStr(err.Error(), "认证") {
		t.Fatalf("错误信息应当说清原因，实际 %q", err)
	}
	// 免认证的同协议要能用
	if _, err := Plan("linux", Profile{ID: "a"}, dir, Options{
		Lookup:   fakeLookup,
		ProxyURL: "socks5://127.0.0.1:1080",
	}); err != nil {
		t.Fatalf("免认证 socks5 应当可用: %v", err)
	}
	// 不支持的协议（比如 vmess）要拒
	if _, err := Plan("linux", Profile{ID: "a"}, dir, Options{
		Lookup:   fakeLookup,
		ProxyURL: "vmess://abc",
	}); err == nil {
		t.Fatal("vmess 不是浏览器原生支持的代理协议，应当拒绝")
	}
}

// TestPlanDarwinUsesOpen open 的 --args 分界符是硬要求（少了它参数被 open 吃掉）。
func TestPlanDarwinUsesOpen(t *testing.T) {
	cmd, err := Plan("darwin", Profile{ID: "a"}, t.TempDir()+"/p", Options{
		URL:    "https://example.com",
		Lookup: func(string) (string, bool) { return "/Applications/Google Chrome.app", true },
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if cmd.Path != "/usr/bin/open" {
		t.Fatalf("darwin 应当经 open 启动，实际 %q", cmd.Path)
	}
	if !containsStr(cmd.Args[0]+" "+cmd.Args[1]+" "+cmd.Args[2]+" "+cmd.Args[3], "-n -a") {
		t.Fatalf("应当带 -n -a，实际 %v", cmd.Args)
	}
	found := false
	for _, a := range cmd.Args {
		if a == "--args" {
			found = true
		}
	}
	if !found {
		t.Fatalf("必须有 --args 分界符，实际 %v", cmd.Args)
	}
}

// TestStoreCRUD 索引的增删改查 + 删档案要连目录一起删。
func TestStoreCRUD(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)

	p, err := s.Ensure("workbuddy", "workbuddy", "WorkBuddy")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if p.ID != "workbuddy" || p.CreatedAt.IsZero() {
		t.Fatalf("Ensure 结果不对: %+v", p)
	}
	// 幂等
	again, err := s.Ensure("workbuddy", "workbuddy", "别的名字")
	if err != nil || !again.CreatedAt.Equal(p.CreatedAt) {
		t.Fatalf("Ensure 应当幂等（不覆盖 CreatedAt）: %v %+v", err, again)
	}

	// 目录与索引文件位置
	dir := s.UserDataDir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Cookies"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "browser-profiles.json")); err != nil {
		t.Fatalf("索引文件应当写在数据根下: %v", err)
	}

	if err := s.Touch("workbuddy", "https://copilot.tencent.com/x", 42222); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	got, ok := s.Get("workbuddy")
	if !ok || got.LastDebugPort != 42222 || got.LastURL == "" || got.LastUsedAt.IsZero() {
		t.Fatalf("Touch 没写进索引: %+v", got)
	}

	// 新建一个 Store 实例读回来（验持久化，而不是只信内存）
	s2 := NewStore(root)
	list, err := s2.List()
	if err != nil || len(list) != 1 || list[0].ID != "workbuddy" {
		t.Fatalf("重新加载失败: %v %+v", err, list)
	}

	// 删除连目录
	if err := s2.Delete("workbuddy", false); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("删档案应当把浏览器目录一起删掉（否则磁盘上留几百 MB 死 cookie）")
	}
	if _, ok := s2.Get("workbuddy"); ok {
		t.Fatal("删完不该还能查到")
	}
	if err := s2.Delete("workbuddy", false); err == nil {
		t.Fatal("删不存在的档案应当报错")
	}
}

// TestStoreRejectsPathTraversal id 同时是目录名，必须挡住穿越。
func TestStoreRejectsPathTraversal(t *testing.T) {
	s := NewStore(t.TempDir())
	for _, bad := range []string{"", "..", ".", "../etc", "a/b", `a\b`, "名字"} {
		if _, err := s.Upsert(Profile{ID: bad}); err == nil {
			t.Fatalf("应当拒绝 id=%q", bad)
		}
	}
}

// TestStoreCorruptIndex 索引损坏时不能把网关拖死：另存后当空表继续。
func TestStoreCorruptIndex(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "browser-profiles.json")
	if err := os.WriteFile(path, []byte("{ 不是 JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewStore(root)
	list, err := s.List()
	if err == nil {
		t.Fatal("读损坏索引应当返回错误（让调用方知道数据有问题）")
	}
	if len(list) != 0 {
		t.Fatalf("损坏后应当当空表，实际 %+v", list)
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Fatal("损坏的索引必须另存备份，不能直接覆盖丢掉")
	}
}

// TestReadDevToolsPort 端口从 DevToolsActivePort 读第一行。
func TestReadDevToolsPort(t *testing.T) {
	dir := t.TempDir()
	if _, ok := readDevToolsPort(dir, 150*time.Millisecond); ok {
		t.Fatal("文件不存在时应当超时返回 false")
	}
	if err := os.WriteFile(filepath.Join(dir, "DevToolsActivePort"), []byte("51234\n/devtools/browser/abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	port, ok := readDevToolsPort(dir, time.Second)
	if !ok || port != 51234 {
		t.Fatalf("应当读到 51234，实际 %d %v", port, ok)
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
