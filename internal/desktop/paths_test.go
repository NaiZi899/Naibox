package desktop

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// setUserConfigBase 把 os.UserConfigDir 的根临时改到 dir 下。
//
// 存在的理由：默认数据根那条分支**只能**通过 UserConfigDir 观察，而直接
// 让测试去读真实 %APPDATA% 既不可控（不同机器上可能真有一份 omni2api 目录，
// 断言就变成"我这台机器上恰好的事实"），也有"测试污染用户数据"的风险。
func setUserConfigBase(t *testing.T, dir string) {
	t.Helper()
	switch runtime.GOOS {
	case "windows":
		// 源码里读的是 AppData（os.UserConfigDir 的 Windows 分支），
		// Windows 环境变量名大小写不敏感，用 APPDATA 设置即可命中。
		t.Setenv("APPDATA", dir)
	case "darwin":
		t.Setenv("HOME", dir)
	default:
		t.Setenv("XDG_CONFIG_HOME", dir)
	}
}

// TestResolveLayoutExplicitDataDirWinsWithUnicodeAndSpace 守两个故障：
//
//  1. -data-dir 被忽略（数据跟着当前工作目录走）—— 升级/换快捷方式启动后
//     "账号没了"的原始成因；
//  2. 中文 + 空格路径下子目录没真正挂在数据根下（手拼字符串而非
//     filepath.Join 时的典型错法），表现为 auths 落在数据根外。
//
// 同时断言 ResolveLayout **不建目录**：它是纯解析，建目录与生成配置是
// EnsureDirs / EnsureConfig 的职责。解析阶段就落盘会让"要不要迁移旧布局"
// 的判据提前失效。
func TestResolveLayoutExplicitDataDirWinsWithUnicodeAndSpace(t *testing.T) {
	root := filepath.Join(t.TempDir(), "我的 网关 数据", "omni2api")
	setUserConfigBase(t, filepath.Join(t.TempDir(), "appdata-should-be-ignored"))

	l, err := ResolveLayout(root, "")
	if err != nil {
		t.Fatalf("ResolveLayout(%q): %v", root, err)
	}
	if l.DataDir != root {
		t.Fatalf("-data-dir 未生效：DataDir=%q want %q", l.DataDir, root)
	}
	if l.Portable {
		t.Fatalf("显式 -data-dir 不应被标成 portable（Portable=true）")
	}
	want := map[string]string{
		"ConfigPath": filepath.Join(root, "config.json"),
		"AuthDir":    filepath.Join(root, "auths"),
		"DataSubDir": filepath.Join(root, "data"),
		"LogDir":     filepath.Join(root, "logs"),
		"BackupDir":  filepath.Join(root, "backups"),
	}
	got := map[string]string{
		"ConfigPath": l.ConfigPath,
		"AuthDir":    l.AuthDir,
		"DataSubDir": l.DataSubDir,
		"LogDir":     l.LogDir,
		"BackupDir":  l.BackupDir,
	}
	for name, wantPath := range want {
		if got[name] != wantPath {
			t.Errorf("%s=%q want %q（中文/空格路径下子路径必须仍挂在数据根下）", name, got[name], wantPath)
		}
		if rel, err := filepath.Rel(root, got[name]); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Errorf("%s=%q 不在数据根 %q 之下（rel=%q err=%v）", name, got[name], root, rel, err)
		}
	}
	if _, err := os.Stat(root); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ResolveLayout 不应创建目录；%q 已存在（stat err=%v）", root, err)
	}
}

// TestResolveLayoutDefaultsUnderUserConfigDir 守"默认数据根跑错地方"。
//
// 默认分支错一步的后果最大：不是"配置读不到"，而是**新账号写进了安装目录**，
// 用户以为存下了，卸载/覆盖升级时连同目录一起没了。
func TestResolveLayoutDefaultsUnderUserConfigDir(t *testing.T) {
	base := t.TempDir()
	setUserConfigBase(t, base)

	l, err := ResolveLayout("", "")
	if err != nil {
		t.Fatalf("ResolveLayout 默认分支: %v", err)
	}
	want := filepath.Join(base, appDirName)
	if runtime.GOOS == "darwin" {
		want = filepath.Join(base, "Library", "Application Support", appDirName)
	}
	if l.DataDir != want {
		t.Fatalf("默认数据根=%q want %q", l.DataDir, want)
	}
	if l.Portable {
		t.Errorf("默认分支不应标记 portable")
	}
	if wd, err := os.Getwd(); err == nil && strings.EqualFold(l.DataDir, wd) {
		t.Errorf("数据根落到了当前工作目录 %q —— 这正是改造前的问题形态", wd)
	}
	if _, err := os.Stat(l.DataDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ResolveLayout 不应创建数据根；%q 已存在", l.DataDir)
	}
}

// TestResolveLayoutExplicitConfigKeepsDataRoot 守"-config 顺手把数据根也搬走"。
//
// 显式 -config 的语义是"配置在这里"，不是"数据全在这里"：数据根被一起改写
// 会让同一个用户出现两份 auths（见面录里一半的号"消失"）。
func TestResolveLayoutExplicitConfigKeepsDataRoot(t *testing.T) {
	dataRoot := filepath.Join(t.TempDir(), "数据 根")
	cfg := filepath.Join(t.TempDir(), "另一处 配置", "my-config.json")

	l, err := ResolveLayout(dataRoot, cfg)
	if err != nil {
		t.Fatalf("ResolveLayout: %v", err)
	}
	if l.ConfigPath != cfg {
		t.Fatalf("-config 未生效：ConfigPath=%q want %q", l.ConfigPath, cfg)
	}
	if l.DataDir != dataRoot {
		t.Fatalf("-config 不应改动数据根：DataDir=%q want %q", l.DataDir, dataRoot)
	}
	if rel, err := filepath.Rel(l.DataDir, l.ConfigPath); err == nil && !strings.HasPrefix(rel, "..") {
		t.Fatalf("ConfigPath=%q 落在数据根内（rel=%q），说明 -config 被 filepath.Join 覆盖了", l.ConfigPath, rel)
	}
}

// TestEnsureDirsCreatesSkeletonButNeverConfig 守迁移判据被提前破坏。
//
// MigrateLegacy 的整个判断建立在"目标还没有 config.json"上。若 EnsureDirs
// 顺手生成默认配置，老用户的 auths 永远迁不过来 —— 而界面上一切正常
// （服务能起、配置合法），只是账号全是空池。这类故障最难从现象追到原因。
func TestEnsureDirsCreatesSkeletonButNeverConfig(t *testing.T) {
	root := filepath.Join(t.TempDir(), "数据 根")
	l, err := ResolveLayout(root, "")
	if err != nil {
		t.Fatalf("ResolveLayout: %v", err)
	}

	if err := l.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}
	for _, dir := range []string{l.DataDir, l.AuthDir, l.DataSubDir, l.LogDir, l.BackupDir} {
		st, err := os.Stat(dir)
		if err != nil || !st.IsDir() {
			t.Fatalf("目录 %q 未创建：stat err=%v", dir, err)
		}
	}
	if fileExists(l.ConfigPath) {
		t.Fatalf("EnsureDirs 生成了 %q —— 迁移会因此认为「目标已有配置」而永不动手", l.ConfigPath)
	}

	// 幂等：重复调用不报错（每次启动都会调一次）。
	if err := l.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs 第二次失败（应幂等）: %v", err)
	}
	// 被用户/清理工具删掉一个目录后要能补回来。
	if err := os.Remove(l.AuthDir); err != nil {
		t.Fatalf("remove auths: %v", err)
	}
	if err := l.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs 修复缺失目录失败: %v", err)
	}
	if st, err := os.Stat(l.AuthDir); err != nil || !st.IsDir() {
		t.Fatalf("auths 目录未被补建：%v", err)
	}
}

// TestEnsureConfigGeneratesUniqueKeyAndNeverOverwrites 守两类故障：
//
//  1. 首启生成的 api_key 是空串/固定值 —— 空串等于网关裸奔，固定值等于
//     所有装机共用一把钥匙（复制一份配置就能互调）；
//  2. 已存在的 config.json 被默认配置覆盖 —— 用户改过的 api_key、
//     上游开关、凭证路径全部回到出厂值，且现象是"所有客户端突然 401"。
func TestEnsureConfigGeneratesUniqueKeyAndNeverOverwrites(t *testing.T) {
	keys := make([]string, 0, 2)
	for i := 0; i < 2; i++ {
		root := filepath.Join(t.TempDir(), "首启 数据")
		l, err := ResolveLayout(root, "")
		if err != nil {
			t.Fatalf("ResolveLayout: %v", err)
		}
		if err := l.EnsureDirs(); err != nil {
			t.Fatalf("EnsureDirs: %v", err)
		}
		created, err := l.EnsureConfig()
		if err != nil {
			t.Fatalf("EnsureConfig: %v", err)
		}
		if created != l.ConfigPath {
			t.Fatalf("EnsureConfig 返回 created=%q want %q", created, l.ConfigPath)
		}
		body, err := os.ReadFile(l.ConfigPath)
		if err != nil {
			t.Fatalf("读默认配置: %v", err)
		}
		var probe struct {
			APIKey string `json:"api_key"`
			Listen string `json:"listen"`
		}
		if err := json.Unmarshal(body, &probe); err != nil {
			t.Fatalf("默认配置不是合法 JSON: %v\n%s", err, body)
		}
		if !strings.HasPrefix(probe.APIKey, "sk-") || len(probe.APIKey) <= len("sk-") {
			t.Fatalf("默认配置的 api_key 不合规: %q", probe.APIKey)
		}
		if probe.Listen == "" {
			t.Errorf("默认配置缺 listen（内嵌模板被改坏？）")
		}
		keys = append(keys, probe.APIKey)

		// 已有配置绝不能被覆盖：改一处内容，再调一次 EnsureConfig。
		custom := bytes.Replace(body, []byte(probe.APIKey), []byte("sk-user-edited-key"), 1)
		if err := os.WriteFile(l.ConfigPath, custom, 0o600); err != nil {
			t.Fatalf("改配置: %v", err)
		}
		again, err := l.EnsureConfig()
		if err != nil {
			t.Fatalf("EnsureConfig 第二次: %v", err)
		}
		if again != "" {
			t.Errorf("配置已存在时 EnsureConfig 应返回空串（表示没生成），得到 %q", again)
		}
		after, err := os.ReadFile(l.ConfigPath)
		if err != nil {
			t.Fatalf("二次读取配置: %v", err)
		}
		if !bytes.Equal(after, custom) {
			t.Fatalf("已有 config.json 被覆盖（用户改过的 api_key 会丢）:\n got=%q", after)
		}
	}
	if keys[0] == keys[1] {
		t.Fatalf("两次首启生成同一个 api_key %q —— 所有装机共用一把钥匙", keys[0])
	}
}

// TestDataDirHashCaseInsensitiveAndDirSpecific 守单实例互斥体名的两个边界：
//
//   - 同一数据根的不同写法（大小写/尾部斜杠）必须得到同一个哈希，否则
//     "再点一次 exe"会变成**第二个实例**抢同一份 auths；
//   - 不同数据根必须不同，否则 portable（U 盘）版与 %APPDATA% 版互相顶掉，
//     后启的那个直接让位退出，用户看到"双击没反应"。
func TestDataDirHashCaseInsensitiveAndDirSpecific(t *testing.T) {
	base := t.TempDir()
	lower := filepath.Join(base, "data", "omni2api")
	upper := filepath.Join(base, "DATA", "OMNI2API")

	hLower := DataDirHash(lower)
	if hLower == "" || len(hLower) != 12 {
		t.Fatalf("DataDirHash 应返回 12 位十六进制，得到 %q", hLower)
	}
	if got := DataDirHash(upper); got != hLower {
		t.Errorf("同一目录的大小写变体哈希不同：%q vs %q", hLower, got)
	}
	if got := DataDirHash(lower + string(filepath.Separator)); got != hLower {
		t.Errorf("尾部路径分隔符改变了哈希：%q vs %q", hLower, got)
	}
	if other := DataDirHash(filepath.Join(base, "data", "other")); other == hLower {
		t.Errorf("不同数据根得到同一哈希 %q（两个实例会互相顶掉）", other)
	}
}

// TestRuntimeFileRoundTripAndRemoveIsIdempotent 守退出路径与"残留 runtime.json"。
//
// runtime.json 是第二实例找到主实例的唯一线索，因此它有两个必须成立的
// 性质：写进去的内容能原样读回来（端口/pid 错一位就会去打别人的端口），
// 删除必须幂等（退出路径会在 shutdown / fatalf / cleanup 三处都调它，
// 其中任何一处因为"文件已不存在"报错，都会把正常退出变成一条误导性的
// 错误日志）。路径里带中文与空格，顺带守住 Windows 下的编码口径。
func TestRuntimeFileRoundTripAndRemoveIsIdempotent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "用户 数据", "omni2api")
	l, err := ResolveLayout(root, "")
	if err != nil {
		t.Fatalf("ResolveLayout: %v", err)
	}
	if err := l.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}

	if _, err := l.ReadRuntime(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("runtime.json 不存在时 ReadRuntime 应返回 ErrNotExist，得到 %v", err)
	}

	want := RuntimeInfo{
		PID:       os.Getpid(),
		Addr:      "127.0.0.1:7863",
		Mode:      "window",
		Version:   "0.1.0-test",
		StartedAt: "2026-01-02T03:04:05Z",
	}
	if err := l.WriteRuntime(want); err != nil {
		t.Fatalf("WriteRuntime: %v", err)
	}
	got, err := l.ReadRuntime()
	if err != nil {
		t.Fatalf("ReadRuntime: %v", err)
	}
	if got != want {
		t.Fatalf("runtime.json 往返不一致：got=%+v want=%+v", got, want)
	}
	if filepath.Dir(l.RuntimePath()) != l.DataDir {
		t.Errorf("RuntimePath=%q 不在数据根 %q 下", l.RuntimePath(), l.DataDir)
	}
	// 二次写入覆盖（托盘"重启服务"会重写一次）。
	want.PID, want.Addr = want.PID+1, "127.0.0.1:7864"
	if err := l.WriteRuntime(want); err != nil {
		t.Fatalf("WriteRuntime 第二次: %v", err)
	}
	if got, _ := l.ReadRuntime(); got.Addr != want.Addr {
		t.Fatalf("重写 runtime.json 未生效：%q want %q", got.Addr, want.Addr)
	}

	if err := l.RemoveRuntime(); err != nil {
		t.Fatalf("RemoveRuntime: %v", err)
	}
	if err := l.RemoveRuntime(); err != nil {
		t.Fatalf("RemoveRuntime 第二次应幂等，得到 %v", err)
	}
	if _, err := l.ReadRuntime(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("删除后 ReadRuntime 应 ErrNotExist，得到 %v", err)
	}
	if _, err := os.Stat(l.RuntimePath() + ".tmp"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("原子写残留了 %s.tmp（Windows 上会被当成占用文件）", l.RuntimePath())
	}
}
