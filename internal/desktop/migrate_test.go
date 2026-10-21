package desktop

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// legacySample 造假样本时要复现的旧布局：配置 + 账号 + 状态。
//
// 三样缺一不可 —— 迁移只要漏掉任意一类，用户的损失形态分别是：
//   - 漏 config.json：api_key 变了，所有客户端 401（看起来像"网关坏了"）；
//   - 漏 auths/：OAuth 凭证全丢，「账号」列表空（用户第一反应是"号被偷了"）；
//   - 漏 data/：state.json 里的冷却/配额/分组全没了，重登的号会被立刻打满。
//
// auths 里刻意放两层（根 + 上游子目录）与中文文件名：改造期的 auths 布局
// 两处都存在，而中文文件名在 Windows 上是最容易在复制/遍历里出错的一类。
func legacySample() map[string][]byte {
	return map[string][]byte{
		"config.json": []byte(`{
  "listen": "127.0.0.1:7863",
  "api_key": "sk-legacy-user-key",
  "auth_dir": "./auths",
  "state_file": "./data/state.json"
}
`),
		"auths/账号-根.json":           []byte(`{"uid":"uid-root","access_token":"at-root","refresh_token":"rt-root"}`),
		"auths/workbuddy/账号-甲.json": []byte(`{"uid":"uid-a","access_token":"at-a","refresh_token":"rt-a","email":"甲@example.com"}`),
		"auths/workbuddy/账号-乙.json": []byte(`{"uid":"uid-b","access_token":"at-b","refresh_token":"rt-b"}`),
		"data/state.json":           []byte(`{"version":2,"accounts":{"uid-a":{"cooldown_until":"2026-01-01T00:00:00Z"}}}`),
		"data/nested/deep.bin":      {0x00, 0x01, 0xff, 0xfe, 0x7f, 0x80},
	}
}

// notMigrated 旧目录里故意存在、但**不该**被迁移的内容。
//
// logs 是诊断材料而不是资产（见 Layout.Backup 的同一条理由）：把它一起搬
// 进数据根会让"新目录里为什么有一份去年的日志"变成一个说不清的状态。
func notMigrated() map[string][]byte {
	return map[string][]byte{
		"logs/server-20250101.log": []byte("old log line\n"),
	}
}

// writeTree 把 path→content 落到 root 下。
func writeTree(t *testing.T, root string, files map[string][]byte) {
	t.Helper()
	for rel, body := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("建目录 %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, body, 0o600); err != nil {
			t.Fatalf("写 %s: %v", full, err)
		}
	}
}

// chdirForTest 切换进程工作目录。
//
// MigrateLegacy 的旧布局来源只有两个候选（exe 同级、当前工作目录），而测试
// 二进制的同级目录是 go-build 的临时目录，无法预置样本；所以只能用 cwd 造
// "旧布局就在当前目录"这个真实场景。**这些用例不能并行**（chdir 是进程级
// 状态），因此全程不调 t.Parallel。
func chdirForTest(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("取当前工作目录: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("切到 %s: %v", dir, err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(old); err != nil {
			t.Fatalf("恢复工作目录 %s 失败（后续用例会被污染）: %v", old, err)
		}
	})
}

// newTargetLayout 造一个"新数据根"（全新目录，尚未有 config.json）。
func newTargetLayout(t *testing.T) Layout {
	t.Helper()
	l, err := ResolveLayout(filepath.Join(t.TempDir(), "新 数据根", "omni2api"), "")
	if err != nil {
		t.Fatalf("ResolveLayout: %v", err)
	}
	return l
}

// dumpTree 返回 dir 下所有普通文件的相对路径→内容（用于"逐字节比对"）。
func dumpTree(t *testing.T, dir string) (map[string][]byte, error) {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = body
		return nil
	})
	return out, err
}

// TestMigrateLegacyCopiesByteForByteAndKeepsSourceIntact 是本次改造第一优先级的用例。
//
// 它守的是"迁移写错 = 用户账号全丢"这个后果链上的四条断言：
//
//	(a) 目标三样齐全且逐字节一致 —— 少一个字节的凭证就是无效凭证；
//	(b) 源目录仍在且内容未被删除 —— 只复制不移动：用户可能还要用
//	    cmd/server 形态在同一份 auths 上跑；
//	(c) 源目录出现迁移标记 —— 用户翻到旧目录时能明白"数据已经搬走了"，
//	    否则会在错的地方找账号，进而手工"修复"成两份数据；
//	(d) 不该搬的东西（logs）确实没被搬 —— 数据根里不该出现来历不明的旧日志。
func TestMigrateLegacyCopiesByteForByteAndKeepsSourceIntact(t *testing.T) {
	legacyRoot := filepath.Join(t.TempDir(), "旧 安装目录")
	if err := os.MkdirAll(legacyRoot, 0o755); err != nil {
		t.Fatalf("建旧目录: %v", err)
	}
	want := legacySample()
	stray := notMigrated()
	writeTree(t, legacyRoot, want)
	writeTree(t, legacyRoot, stray)

	chdirForTest(t, legacyRoot)
	l := newTargetLayout(t)

	res, err := MigrateLegacy(l, false, log.New(&bytes.Buffer{}, "", 0))
	if err != nil {
		t.Fatalf("MigrateLegacy: %v", err)
	}
	if !res.Attempted || !res.Migrated {
		t.Fatalf("应识别并执行迁移：Attempted=%v Migrated=%v Source=%q", res.Attempted, res.Migrated, res.Source)
	}
	if filepath.Clean(res.Source) != filepath.Clean(legacyRoot) {
		t.Fatalf("Source=%q want %q", res.Source, legacyRoot)
	}

	// (a) 目标内容逐字节一致。
	for rel, expect := range want {
		got, err := os.ReadFile(filepath.Join(l.DataDir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("目标缺少 %s: %v", rel, err)
		}
		if !bytes.Equal(got, expect) {
			t.Fatalf("%s 内容不一致（少一个字节的凭证就是无效凭证）:\n got=%q\nwant=%q", rel, got, expect)
		}
	}
	if len(res.Copied) != len(want) {
		t.Errorf("Copied 记录 %d 个文件，样本有 %d 个：%v", len(res.Copied), len(want), res.Copied)
	}

	// (d) logs 不该被搬。
	if _, err := os.Stat(filepath.Join(l.DataDir, "logs")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("数据根下出现了 logs/ —— 旧日志不该跟着资产一起搬")
	}
	for rel := range stray {
		if _, err := os.Stat(filepath.Join(l.DataDir, filepath.FromSlash(rel))); err == nil {
			t.Errorf("不该迁移的 %s 出现在数据根", rel)
		}
	}

	// (b) 源目录原样保留。
	gotSrc, err := dumpTree(t, legacyRoot)
	if err != nil {
		t.Fatalf("扫源目录: %v", err)
	}
	for rel, expect := range want {
		if !bytes.Equal(gotSrc[filepath.ToSlash(rel)], expect) {
			t.Fatalf("源目录里的 %s 被改动或删除了（迁移必须是复制，不是移动）", rel)
		}
	}
	// (c) 源目录留标记。
	marker := filepath.Join(legacyRoot, migratedMarkerName)
	body, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("源目录没有迁移标记 %s: %v", marker, err)
	}
	var m struct {
		From   string   `json:"from"`
		To     string   `json:"to"`
		Copied []string `json:"copied"`
		Note   string   `json:"note"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("迁移标记不是合法 JSON: %v\n%s", err, body)
	}
	if filepath.Clean(m.To) != filepath.Clean(l.DataDir) {
		t.Errorf("标记里 to=%q，实际数据根 %q —— 用户会被指错方向", m.To, l.DataDir)
	}
	if !strings.Contains(strings.Join(m.Copied, ","), "config.json") {
		t.Errorf("标记的 copied 里没有 config.json：%v", m.Copied)
	}
	if strings.TrimSpace(m.Note) == "" {
		t.Errorf("标记缺少说明文字")
	}
	if len(m.Copied) != len(want) {
		// 标记是给人看的：少列一个文件不会丢数据，但会让人以为漏迁了。
		t.Errorf("标记 copied 覆盖 %d 个文件，样本 %d 个", len(m.Copied), len(want))
	}
}

// TestMigrateLegacySecondRunIsIdempotent 守"第二次启动再搬一遍"。
//
// 幂等的实现判据是"目标已有 config.json 就直接返回"（不靠标记文件）。
// 若这里退化，最坏的形态不是重复复制，而是**用旧数据覆盖新数据**：
// 用户在新版里删过/换过的账号，下次启动被旧 auths 复活。
func TestMigrateLegacySecondRunIsIdempotent(t *testing.T) {
	legacyRoot := filepath.Join(t.TempDir(), "旧 安装目录")
	writeTree(t, legacyRoot, legacySample())
	chdirForTest(t, legacyRoot)
	l := newTargetLayout(t)

	first, err := MigrateLegacy(l, false, nil)
	if err != nil {
		t.Fatalf("首次 MigrateLegacy: %v", err)
	}
	if !first.Migrated {
		t.Fatalf("首次迁移未发生：%+v", first)
	}
	before, err := dumpTree(t, l.DataDir)
	if err != nil {
		t.Fatalf("扫数据根: %v", err)
	}
	markerPath := filepath.Join(legacyRoot, migratedMarkerName)
	markerBefore, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("读标记: %v", err)
	}

	second, err := MigrateLegacy(l, false, nil)
	if err != nil {
		t.Fatalf("第二次 MigrateLegacy 应无错，得到 %v", err)
	}
	if second.Attempted || second.Migrated || len(second.Copied) != 0 {
		t.Fatalf("第二次不应再迁移：%+v", second)
	}
	after, err := dumpTree(t, l.DataDir)
	if err != nil {
		t.Fatalf("二次扫数据根: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("第二次迁移改变了文件集：前 %d 个，后 %d 个", len(before), len(after))
	}
	for rel, body := range before {
		if !bytes.Equal(after[rel], body) {
			t.Fatalf("%s 在第二次迁移后被覆盖（用旧数据砸新数据）", rel)
		}
	}
	markerAfter, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("二次读标记: %v", err)
	}
	if !bytes.Equal(markerBefore, markerAfter) {
		t.Errorf("幂等路径重写了迁移标记（标记应只在真正迁移时更新）")
	}
}

// TestMigrateLegacySkippedWhenExplicitConfig 守"-config 显式指定却照样搬家"。
//
// 用户说"配置在这里"，等于说"数据我自己管"。此时再复制一份旧布局，会得到
// 两套 auths：服务读 A、用户改 B，出现"改配置没反应"与"删号后复活"。
func TestMigrateLegacySkippedWhenExplicitConfig(t *testing.T) {
	legacyRoot := filepath.Join(t.TempDir(), "旧 安装目录")
	writeTree(t, legacyRoot, legacySample())
	chdirForTest(t, legacyRoot)

	l := newTargetLayout(t)
	explicit := filepath.Join(t.TempDir(), "自定义 配置", "cfg.json")
	l.ConfigPath = explicit

	res, err := MigrateLegacy(l, true, nil)
	if err != nil {
		t.Fatalf("MigrateLegacy: %v", err)
	}
	if res.Attempted || res.Migrated {
		t.Fatalf("-config 显式指定时不该迁移：%+v", res)
	}
	if _, err := os.Stat(explicit); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("-config 场景下不该生成 %s", explicit)
	}
	if _, err := os.Stat(filepath.Join(legacyRoot, migratedMarkerName)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("没迁移却写了迁移标记 —— 用户会以为数据已搬走")
	}
	if entries, err := os.ReadDir(l.DataDir); err == nil && len(entries) > 0 {
		t.Errorf("没迁移却往数据根写了东西：%v", entries)
	}
}

// TestMigrateLegacyNeverOverwritesExistingTargetFiles 守"旧数据砸新数据"。
//
// 目标已有同名文件时选择保守策略（谁在目标里谁赢）是刻意的：无法自动判断
// 哪份更新，而覆盖的后果比不迁移严重得多 —— 用户在新版里新登录的号会被
// 旧 auths 里的过期凭证顶掉，现象是"刚加的号又没了"。
func TestMigrateLegacyNeverOverwritesExistingTargetFiles(t *testing.T) {
	legacyRoot := filepath.Join(t.TempDir(), "旧 安装目录")
	writeTree(t, legacyRoot, legacySample())
	chdirForTest(t, legacyRoot)

	l := newTargetLayout(t)
	if err := l.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}
	// 目标里已有一份"更新"的凭证：内容与旧布局不同。
	newer := []byte(`{"uid":"uid-a","access_token":"at-newer","refresh_token":"rt-newer","note":"新版重新登录过"}`)
	if err := os.MkdirAll(filepath.Join(l.AuthDir, "workbuddy"), 0o755); err != nil {
		t.Fatalf("建 auths/workbuddy: %v", err)
	}
	if err := os.WriteFile(filepath.Join(l.AuthDir, "workbuddy", "账号-甲.json"), newer, 0o600); err != nil {
		t.Fatalf("写目标凭证: %v", err)
	}

	res, err := MigrateLegacy(l, false, nil)
	if err != nil {
		t.Fatalf("MigrateLegacy: %v", err)
	}
	if !res.Migrated {
		t.Fatalf("目标没有 config.json，仍应执行迁移：%+v", res)
	}
	got, err := os.ReadFile(filepath.Join(l.AuthDir, "workbuddy", "账号-甲.json"))
	if err != nil {
		t.Fatalf("读目标凭证: %v", err)
	}
	if !bytes.Equal(got, newer) {
		t.Fatalf("目标已有凭证被旧数据覆盖（用户新登录的号会被过期凭证顶掉）:\n got=%q", got)
	}
	// 其余文件仍应复制到位，不能因为一个冲突就整体放弃。
	if _, err := os.Stat(filepath.Join(l.AuthDir, "workbuddy", "账号-乙.json")); err != nil {
		t.Errorf("非冲突文件未复制: %v", err)
	}
	if _, err := os.Stat(l.ConfigPath); err != nil {
		t.Errorf("config.json 未复制: %v", err)
	}
	// Copied 是写进日志与迁移标记的账本：跳过的文件绝不能出现在"已复制"里，
	// 否则用户在迁移摘要里看到"账号-甲 已复制"，实际用的是自己的那份。
	for _, rel := range res.Copied {
		if strings.Contains(rel, "账号-甲") {
			t.Fatalf("Copied 把跳过的文件记成已复制：%v", res.Copied)
		}
	}
	// ⚠ 已上报的实现疑点（本测试不以此为准失败）：MigrationResult.Kept 的
	// 注释写的是"目标已存在、刻意没覆盖的相对路径"，但实现只在**整个目录
	// 一个文件都没复制**时才追加一条粗粒度记录；部分冲突（本用例）时
	// Kept 为空 —— 迁移摘要会显示"复制 N 个文件"，对那个被跳过的账号
	// 只字未提。数据本身是安全的（跳过正确），缺的是可审计性。
	if len(res.Kept) == 0 {
		t.Logf("实现现状：部分冲突时 Kept 为空（只有整目录都被跳过才会记录）")
	}
}

// TestMigrateLegacyNoopWhenNoLegacyLayout 守"空目录里凭空迁移"。
//
// 首次启动是最常见的路径：cwd 是空目录（例如用户从桌面快捷方式启动，
// 起始位置是某空文件夹）。此时任何误判都会写下一个指向不存在来源的标记，
// 或者在数据根里造出几个空文件。
func TestMigrateLegacyNoopWhenNoLegacyLayout(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "空 起始目录")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatalf("建空目录: %v", err)
	}
	chdirForTest(t, empty)
	l := newTargetLayout(t)

	res, err := MigrateLegacy(l, false, nil)
	if err != nil {
		t.Fatalf("MigrateLegacy: %v", err)
	}
	if res.Attempted || res.Migrated || res.Source != "" {
		t.Fatalf("没有旧布局时不该进入迁移：%+v", res)
	}
	if _, err := os.Stat(l.ConfigPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("没迁移却生成了 %s", l.ConfigPath)
	}
	if _, err := os.Stat(filepath.Join(empty, migratedMarkerName)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("空目录里被写了迁移标记")
	}
}

// TestMigrateLegacyDoesNotCopyIntoItself 守 portable 形态下的"自己迁自己"。
//
// portable.flag 让数据根 = exe 目录，而 exe 目录同时是旧布局的第一候选。
// 从外部无法制造"数据根里有 config.json 且同时被判为旧布局"的状态
// （目标已有 config.json 就先早退了，findLegacyRoot 里的 samePath 判断
// 是这一层的第二道闸），所以本用例断言的是**可观察结果**：当 cwd 就是
// 数据根时，迁移既不复制也不留标记，目录内容一个文件都不多。
// 若这条闸被拆掉，绿色版（U 盘/便携）用户会看到目录里出现自己的副本。
func TestMigrateLegacyDoesNotCopyIntoItself(t *testing.T) {
	root := filepath.Join(t.TempDir(), "绿色版 目录")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	only := map[string][]byte{
		"config.json":  []byte(`{"api_key":"sk-self"}`),
		"auths/a.json": []byte(`{"uid":"self"}`),
	}
	writeTree(t, root, only)
	chdirForTest(t, root)

	l, err := ResolveLayout(root, "")
	if err != nil {
		t.Fatalf("ResolveLayout: %v", err)
	}
	res, err := MigrateLegacy(l, false, nil)
	if err != nil {
		t.Fatalf("MigrateLegacy: %v", err)
	}
	if res.Migrated {
		t.Fatalf("目标就是数据根本身，不该复制任何东西：%+v", res)
	}
	files, err := dumpTree(t, root)
	if err != nil {
		t.Fatalf("扫目录: %v", err)
	}
	var names []string
	for rel := range files {
		names = append(names, rel)
	}
	sort.Strings(names)
	if len(names) != len(only) {
		t.Fatalf("目录里多出了文件（自己迁自己的迹象）：%v", names)
	}
	for rel, body := range only {
		if !bytes.Equal(files[rel], body) {
			t.Errorf("%s 内容被改动", rel)
		}
	}
	if _, err := os.Stat(filepath.Join(root, migratedMarkerName)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("在数据根本身写了迁移标记 —— 用户会以为数据搬走了")
	}
}

// TestMigrateLegacyFromRenamedAppDataRoot 守"改名后上一版用户的账号还在"。
//
// 数据根从 %APPDATA%\omni2api 改名成 %APPDATA%\NaiBox 之后，账号、api_key、
// 冷却状态全留在旧目录里。findLegacyRoot 若不认它，升级后账号池直接是空的，
// 日志只会说"按首次启动处理"，而用户的结论是"号被偷了"。
//
// 承重断言：Source 指向旧根、目标三样齐且逐字节一致、旧根一个文件都没少、
// 旧根留下标记。
func TestMigrateLegacyFromRenamedAppDataRoot(t *testing.T) {
	base := t.TempDir()
	setUserConfigBase(t, base) // %APPDATA% 指到临时目录，不碰真实用户数据
	oldRoot := filepath.Join(base, legacyAppDirName)
	want := legacySample()
	writeTree(t, oldRoot, want)

	empty := filepath.Join(t.TempDir(), "空 起始目录")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatalf("建空目录: %v", err)
	}
	chdirForTest(t, empty) // 抹掉 exe 同级与 cwd 两个候选的干扰

	l, err := ResolveLayout("", "")
	if err != nil {
		t.Fatalf("ResolveLayout: %v", err)
	}
	wantRoot := filepath.Join(base, appDirName)
	if l.DataDir != wantRoot {
		t.Fatalf("默认数据根=%q want %q", l.DataDir, wantRoot)
	}

	res, err := MigrateLegacy(l, false, nil)
	if err != nil {
		t.Fatalf("MigrateLegacy: %v", err)
	}
	if !res.Migrated {
		t.Fatalf("没有从改名前的 %s 迁移：%+v", oldRoot, res)
	}
	if filepath.Clean(res.Source) != filepath.Clean(oldRoot) {
		t.Fatalf("Source=%q want %q", res.Source, oldRoot)
	}
	for rel, expect := range want {
		got, err := os.ReadFile(filepath.Join(l.DataDir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("目标缺少 %s: %v", rel, err)
		}
		if !bytes.Equal(got, expect) {
			t.Fatalf("%s 内容不一致（少一个字节的凭证就是无效凭证）:\n got=%q\nwant=%q", rel, got, expect)
		}
	}
	// 旧目录原样保留：用户可能还要翻回去找东西，迁移是复制不是移动。
	for rel, expect := range want {
		got, err := os.ReadFile(filepath.Join(oldRoot, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("旧目录的 %s 不见了: %v", rel, err)
		}
		if !bytes.Equal(got, expect) {
			t.Fatalf("旧目录的 %s 被改动", rel)
		}
	}
	if _, err := os.Stat(filepath.Join(oldRoot, migratedMarkerName)); err != nil {
		t.Errorf("旧目录没有迁移标记（用户会在错的地方找账号）: %v", err)
	}
}

// TestMigrateLegacyPrefersRenamedRootOverCwd 守候选顺序。
//
// 改名前的 %APPDATA% 目录是**上一版的官方位置**，exe 同级/cwd 只是 v1.0 时代
// 数据落在安装目录的产物。两者都存在时按前者迁，否则用户会拿到一份年代更久
// 的账号，还以为新版把最近的号弄丢了。
func TestMigrateLegacyPrefersRenamedRootOverCwd(t *testing.T) {
	base := t.TempDir()
	setUserConfigBase(t, base)
	oldRoot := filepath.Join(base, legacyAppDirName)
	writeTree(t, oldRoot, map[string][]byte{
		"config.json":  []byte(`{"api_key":"sk-from-renamed-root"}`),
		"auths/a.json": []byte(`{"uid":"from-renamed-root"}`),
	})

	cwd := filepath.Join(t.TempDir(), "起点 目录")
	writeTree(t, cwd, map[string][]byte{
		"config.json":  []byte(`{"api_key":"sk-from-cwd"}`),
		"auths/a.json": []byte(`{"uid":"from-cwd"}`),
	})
	chdirForTest(t, cwd)

	l, err := ResolveLayout("", "")
	if err != nil {
		t.Fatalf("ResolveLayout: %v", err)
	}
	res, err := MigrateLegacy(l, false, nil)
	if err != nil {
		t.Fatalf("MigrateLegacy: %v", err)
	}
	if filepath.Clean(res.Source) != filepath.Clean(oldRoot) {
		t.Fatalf("Source=%q，应优先取改名前的数据根 %q（cwd 的年代更久）", res.Source, oldRoot)
	}
	got, err := os.ReadFile(l.ConfigPath)
	if err != nil {
		t.Fatalf("读目标配置: %v", err)
	}
	if !bytes.Contains(got, []byte("sk-from-renamed-root")) {
		t.Fatalf("迁过来的是 cwd 那份配置：%s", got)
	}
}

// TestMigrateLegacySkipsRenamedRootUnlessDefaultLayout 守"越界注入"。
//
// 旧根只在数据根是默认位置时才允许当来源：
//   - portable（数据跟着 exe 走）→ 注入 %APPDATA% 会让便携版变两套数据；
//   - -data-dir 显式指定 → 用户自己指了目录，替他搬家属于越界。
//
// 两条都必须"不迁移、不写标记、旧根一个字节都不动"。
func TestMigrateLegacySkipsRenamedRootUnlessDefaultLayout(t *testing.T) {
	cases := []struct {
		name   string
		layout func(t *testing.T, cwd string) Layout
	}{
		{"portable", func(t *testing.T, cwd string) Layout {
			return Layout{DataDir: cwd, Portable: true}
		}},
		{"explicit-data-dir", func(t *testing.T, cwd string) Layout {
			l, err := ResolveLayout(cwd, "")
			if err != nil {
				t.Fatalf("ResolveLayout: %v", err)
			}
			if !l.ExplicitDataDir {
				t.Fatalf("前提不满足：-data-dir 没被标记成显式")
			}
			return l
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base := t.TempDir()
			setUserConfigBase(t, base)
			oldRoot := filepath.Join(base, legacyAppDirName)
			writeTree(t, oldRoot, legacySample())

			cwd := filepath.Join(t.TempDir(), "起始 目录")
			if err := os.MkdirAll(cwd, 0o755); err != nil {
				t.Fatalf("建目录: %v", err)
			}
			chdirForTest(t, cwd)
			l := c.layout(t, cwd)

			res, err := MigrateLegacy(l, false, nil)
			if err != nil {
				t.Fatalf("MigrateLegacy: %v", err)
			}
			if res.Attempted || res.Migrated {
				t.Fatalf("%s 形态下不该从 %s 迁移：%+v", c.name, oldRoot, res)
			}
			if _, err := os.Stat(filepath.Join(oldRoot, migratedMarkerName)); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("没迁移却在旧根写了标记 —— 用户会以为数据搬走了")
			}
		})
	}
}
