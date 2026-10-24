package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// localPrefix / localSuffix 本地通道认的包名。
//
// 与 backupName 的前缀一致：清理**只**删符合这个形状的文件，绝不动用户
// 自己放进 backups/ 的其它东西（那可能是他手工留的一份）。
const (
	localPrefix = "omni2api-backup-"
	localSuffix = ".zip"
)

// LocalChannel 写到数据根下的 backups/ 目录。
//
// # 为什么它是默认通道
//
// 远端通道随时可能不可用（对象存储改了密钥、网盘掉线），而"备份"这件事的
// 价值在于**出事那一刻能拿到包**。本地那份的代价几乎为零，所以它是唯一
// 在 config.example.json 里预置的通道。
type LocalChannel struct {
	id   string
	dir  string
	keep int
}

// NewLocalChannel 构造。dir 是备份目录（通常是 Layout.BackupDir）。
func NewLocalChannel(id, dir string) *LocalChannel {
	if strings.TrimSpace(id) == "" {
		id = "local"
	}
	return &LocalChannel{id: id, dir: dir, keep: 10}
}

func (c *LocalChannel) ID() string   { return c.id }
func (c *LocalChannel) Kind() string { return "local" }

// SetKeep 由 Manager.Configure 下发保留份数。
func (c *LocalChannel) SetKeep(n int) {
	if n > 0 {
		c.keep = n
	}
}

// Test 建目录并写一个探针文件再删掉。
//
// 只 Stat 目录是不够的：备份目录常常被同步盘/网盘接管，**存在但写不进去**
// 才是常见故障形态（只读挂载、配额满）。探针能把这种情况在"测试"按钮上
// 如实报出来，而不是等到用户备份时才发现。
func (c *LocalChannel) Test(ctx context.Context) error {
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return fmt.Errorf("创建备份目录: %w", err)
	}
	probe := filepath.Join(c.dir, ".naibox-write-probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		return fmt.Errorf("备份目录不可写: %w", err)
	}
	os.Remove(probe)
	return nil
}

// Put 落到本地目录。
//
// 先写 `.part` 再 rename：备份目录常被同步盘盯着，直接写目标名会让同步端
// 看到半包并可能先传上去 —— 恢复时拿到一个截断的 zip。
func (c *LocalChannel) Put(ctx context.Context, name string, r io.Reader, size int64) (string, error) {
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return "", fmt.Errorf("创建备份目录: %w", err)
	}
	dst := filepath.Join(c.dir, filepath.Base(name))
	tmp := dst + ".part"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("创建备份文件: %w", err)
	}
	n, copyErr := io.Copy(f, ctxReader(ctx, r))
	closeErr := f.Close()
	if copyErr != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("写入备份文件: %w", copyErr)
	}
	if closeErr != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("关闭备份文件: %w", closeErr)
	}
	if size > 0 && n != size {
		os.Remove(tmp)
		return "", fmt.Errorf("写入字节数不符（期望 %d，实际 %d）", size, n)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("落盘备份文件: %w", err)
	}
	return dst, nil
}

// List 列出已有备份包（新的在前）。
func (c *LocalChannel) List(ctx context.Context) ([]Object, error) {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Object
	for _, e := range entries {
		if e.IsDir() || !isBackupName(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Object{Name: e.Name(), Size: info.Size(), ModTime: info.ModTime()})
	}
	sortNewestFirst(out)
	return out, nil
}

// Get 打开本地包。
func (c *LocalChannel) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	if !isBackupName(name) {
		return nil, errors.New("不是备份包文件名")
	}
	return os.Open(filepath.Join(c.dir, filepath.Base(name)))
}

// Prune 只保留最近 keep 份。
//
// 按文件名排序（名字里是 yyyyMMdd-HHmmss，字典序 = 时间序），不按 mtime：
// 备份目录被同步盘搬动过之后 mtime 会全变。
func (c *LocalChannel) Prune(keep int) error {
	if keep <= 0 {
		keep = c.keep
	}
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !isBackupName(e.Name()) {
			continue
		}
		names = append(names, e.Name())
	}
	if len(names) <= keep {
		return nil
	}
	sort.Strings(names)
	for _, n := range names[:len(names)-keep] {
		if err := os.Remove(filepath.Join(c.dir, n)); err != nil {
			return err
		}
	}
	return nil
}

func isBackupName(name string) bool {
	base := filepath.Base(name)
	return strings.HasPrefix(base, localPrefix) && strings.HasSuffix(base, localSuffix) && base == name
}

func sortNewestFirst(list []Object) {
	sort.Slice(list, func(i, j int) bool { return list[i].Name > list[j].Name })
}

// ctxReader 每读一次就检查一次取消。
//
// 标准库没有"可取消的 Reader"，而大包上传正是最需要响应用户取消的地方
// （用户点了关闭窗口 / 请求被中断）。包一层比让 S3 那边自己判 context 简单。
func ctxReader(ctx context.Context, r io.Reader) io.Reader {
	if ctx == nil {
		return r
	}
	return &cancelReader{ctx: ctx, r: r}
}

type cancelReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *cancelReader) Read(p []byte) (int, error) {
	select {
	case <-c.ctx.Done():
		return 0, c.ctx.Err()
	default:
	}
	return c.r.Read(p)
}

// nowFunc 便于测试固定时间。
func nowFunc(f func() time.Time) func() time.Time {
	if f == nil {
		return time.Now
	}
	return f
}
