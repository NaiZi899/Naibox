package backup

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeFile 就是"造一个数据根里的文件"，测试里用得最多。
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func readZip(t *testing.T, path string) map[string][]byte {
	t.Helper()
	r, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer r.Close()
	out := map[string][]byte{}
	for _, f := range r.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %s: %v", f.Name, err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read %s: %v", f.Name, err)
		}
		out[f.Name] = b
	}
	return out
}

// TestBuildScopeAndManifest 打包内容与清单必须一致：
// ① assets 进包、② 没声明的目录（logs）不进包、③ 清单里的 sha256 对得上。
func TestBuildScopeAndManifest(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "config.json", `{"listen":":7863"}`)
	writeFile(t, root, "auths/workbuddy/a.json", `{"account":{"uid":"u1"}}`)
	writeFile(t, root, "data/state.json", `{"rev":1}`)
	writeFile(t, root, "logs/server.log", "should not be backed up")

	pkg, err := Build(Scope{
		Root:  root,
		Files: []string{"config.json"},
		Dirs:  []string{"auths", "data"},
	}, time.Unix(1761691000, 0))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer pkg.Cleanup()

	if !strings.HasPrefix(pkg.Name, localPrefix) || !strings.HasSuffix(pkg.Name, localSuffix) {
		t.Fatalf("包名不符合本地通道约定: %s", pkg.Name)
	}
	files := readZip(t, pkg.Path)
	for _, want := range []string{"config.json", "auths/workbuddy/a.json", "data/state.json", "manifest.json"} {
		if _, ok := files[want]; !ok {
			t.Fatalf("包内缺少 %s，实际有 %v", want, keysOf(files))
		}
	}
	if _, ok := files["logs/server.log"]; ok {
		t.Fatal("logs/ 不该进包（日志是诊断材料，不是资产）")
	}

	var man Manifest
	if err := json.Unmarshal(files["manifest.json"], &man); err != nil {
		t.Fatalf("清单不是合法 JSON: %v", err)
	}
	if man.FormatVersion != FormatVersion {
		t.Fatalf("清单版本 = %d，期望 %d", man.FormatVersion, FormatVersion)
	}
	if man.FileCount != len(man.Files) || man.FileCount == 0 {
		t.Fatalf("清单文件数不自洽: count=%d files=%d", man.FileCount, len(man.Files))
	}
	for _, f := range man.Files {
		sum := sha256.Sum256(files[f.Name])
		if got := hex.EncodeToString(sum[:]); got != f.SHA256 {
			t.Fatalf("%s 的 sha256 不符：清单 %s，实际 %s", f.Name, f.SHA256, got)
		}
		if int64(len(files[f.Name])) != f.Size {
			t.Fatalf("%s 的 size 不符", f.Name)
		}
	}
}

// TestBuildMissingDirsOK 首次启动就点备份：目录还没建也不算错误。
func TestBuildMissingDirsOK(t *testing.T) {
	root := t.TempDir()
	pkg, err := Build(Scope{Root: root, Files: []string{"config.json"}, Dirs: []string{"auths"}}, time.Now())
	if err != nil {
		t.Fatalf("空数据根应当打包成功: %v", err)
	}
	defer pkg.Cleanup()
	files := readZip(t, pkg.Path)
	if _, ok := files["manifest.json"]; !ok {
		t.Fatal("即使没有任何文件也要有清单")
	}
	if len(files) != 1 {
		t.Fatalf("期望只有清单，实际 %v", keysOf(files))
	}
}

// TestBuildConfigFromElsewhere -config 指向别处时，包内名字仍是 config.json。
func TestBuildConfigFromElsewhere(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "custom-config.json")
	if err := os.WriteFile(outside, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pkg, err := Build(Scope{Root: root, Files: []string{"config.json"}, ConfigPath: outside}, time.Now())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer pkg.Cleanup()
	files := readZip(t, pkg.Path)
	if string(files["config.json"]) != `{"a":1}` {
		t.Fatalf("config.json 内容取自 -config 路径，实际 %q", files["config.json"])
	}
}

func TestLocalChannelPutGetListPrune(t *testing.T) {
	dir := t.TempDir()
	c := NewLocalChannel("local", dir)
	ctx := context.Background()
	if err := c.Test(ctx); err != nil {
		t.Fatalf("Test: %v", err)
	}

	name := "omni2api-backup-20261024-120000.zip"
	loc, err := c.Put(ctx, name, strings.NewReader("payload"), int64(len("payload")))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if loc != filepath.Join(dir, name) {
		t.Fatalf("返回位置 = %q", loc)
	}
	if _, err := os.Stat(filepath.Join(dir, name+".part")); !os.IsNotExist(err) {
		t.Fatal("临时 .part 文件应当被重命名掉")
	}

	rc, err := c.Get(ctx, name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "payload" {
		t.Fatalf("取回内容 = %q", b)
	}

	// 字节数不符必须失败（上传中断的典型形态）
	if _, err := c.Put(ctx, name, strings.NewReader("short"), 999); err == nil {
		t.Fatal("写入字节数与期望不符时应当报错")
	}

	// 非备份包名一律拒绝（避免把用户放进去的其它文件当备份读/删）
	if _, err := c.Get(ctx, "notes.txt"); err == nil {
		t.Fatal("非备份包名应当被拒绝")
	}

	for i := 0; i < 12; i++ {
		n := "omni2api-backup-202610" + pad2(i) + "-000000.zip"
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "keep-me.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := c.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 13 {
		t.Fatalf("List 应当只列备份包（13 个），实际 %d 个", len(list))
	}
	if err := c.Prune(3); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	list, _ = c.List(ctx)
	if len(list) != 3 {
		t.Fatalf("Prune(3) 之后应当剩 3 个，实际 %d 个", len(list))
	}
	if _, err := os.Stat(filepath.Join(dir, "keep-me.txt")); err != nil {
		t.Fatal("Prune 不得删除非备份包文件")
	}
}

func pad2(i int) string {
	if i < 10 {
		return "0" + string(rune('0'+i))
	}
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}

// fakeChannel 用于测 Manager：可控失败、记录调用次数。
type fakeChannel struct {
	id   string
	fail bool
	puts int
}

func (f *fakeChannel) ID() string                 { return f.id }
func (f *fakeChannel) Kind() string               { return "fake" }
func (f *fakeChannel) Test(context.Context) error { return nil }
func (f *fakeChannel) Put(ctx context.Context, name string, r io.Reader, size int64) (string, error) {
	f.puts++
	n, _ := io.Copy(io.Discard, r)
	if f.fail {
		return "", io.ErrUnexpectedEOF
	}
	if n != size {
		return "", io.ErrShortWrite
	}
	return "fake://" + name, nil
}
func (f *fakeChannel) List(context.Context) ([]Object, error) { return nil, nil }
func (f *fakeChannel) Get(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

// TestManagerRunIsolatesChannelFailures 一个通道失败不影响另一个：
// "S3 挂了不该连累本地还留了一份"。
func TestManagerRunIsolatesChannelFailures(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "config.json", "{}")
	ok := &fakeChannel{id: "ok"}
	bad := &fakeChannel{id: "bad", fail: true}
	m := NewManager(func() Scope { return Scope{Root: root, Files: []string{"config.json"}} },
		[]Channel{ok, bad}, 5, nil)

	res, err := m.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("Run 不该因为单通道失败而返回错误: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("期望 2 条结果，实际 %d", len(res))
	}
	byID := map[string]Result{}
	for _, r := range res {
		byID[r.Channel] = r
	}
	if !byID["ok"].OK() {
		t.Fatalf("ok 通道应当成功: %+v", byID["ok"])
	}
	if byID["bad"].OK() {
		t.Fatalf("bad 通道应当失败: %+v", byID["bad"])
	}
	if ok.puts != 1 || bad.puts != 1 {
		t.Fatalf("两个通道都应被调用一次: ok=%d bad=%d", ok.puts, bad.puts)
	}
	if len(m.Last()) != 2 {
		t.Fatal("Last() 应当保留上一次结果")
	}
}

func TestManagerRunFiltersByID(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "config.json", "{}")
	a := &fakeChannel{id: "a"}
	b := &fakeChannel{id: "b"}
	m := NewManager(func() Scope { return Scope{Root: root, Files: []string{"config.json"}} },
		[]Channel{a, b}, 5, nil)
	res, err := m.Run(context.Background(), []string{"b"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res) != 1 || res[0].Channel != "b" || a.puts != 0 {
		t.Fatalf("指定 ids 时只该跑那一条: res=%d a.puts=%d", len(res), a.puts)
	}
}

func TestManagerNoChannels(t *testing.T) {
	m := NewManager(func() Scope { return Scope{Root: t.TempDir()} }, nil, 5, nil)
	if _, err := m.Run(context.Background(), nil); err == nil {
		t.Fatal("没有通道时应当如实报错，而不是假装成功")
	}
}

// TestManagerBusy Manager 是"不排队"语义：重入直接返回 ErrRunning。
func TestManagerBusy(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "config.json", "{}")
	block := make(chan struct{})
	slow := &blockingChannel{id: "slow", entered: make(chan struct{}), release: block}
	m := NewManager(func() Scope { return Scope{Root: root, Files: []string{"config.json"}} },
		[]Channel{slow}, 5, nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := m.Run(context.Background(), nil); err != nil {
			t.Errorf("第一次 Run 不该失败: %v", err)
		}
	}()
	<-slow.entered
	if _, err := m.Run(context.Background(), nil); err != ErrRunning {
		t.Fatalf("并发 Run 应当返回 ErrRunning，实际 %v", err)
	}
	close(block)
	<-done
}

type blockingChannel struct {
	id      string
	entered chan struct{}
	release chan struct{}
}

func (b *blockingChannel) ID() string   { return b.id }
func (b *blockingChannel) Kind() string { return "blocking" }
func (b *blockingChannel) Test(context.Context) error {
	return nil
}
func (b *blockingChannel) Put(ctx context.Context, name string, r io.Reader, size int64) (string, error) {
	io.Copy(io.Discard, r)
	signalOnce(b.entered)
	<-b.release
	return "blocking://" + name, nil
}
func (b *blockingChannel) List(context.Context) ([]Object, error) { return nil, nil }
func (b *blockingChannel) Get(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

// signalOnce 一次性通知（重复 close 会 panic，故用 select 兜住）。
func signalOnce(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
