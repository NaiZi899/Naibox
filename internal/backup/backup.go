// Package backup 把数据根里的资产打成一个包，并通过若干通道送到别处。
//
// # 为什么单独一个包，而不是留在 desktop
//
// 改造前打包逻辑写在 `internal/desktop/backup.go` 的 `Layout.Backup()` 里，
// 三个限制焊死在一起：**只有本地目录**、**保留份数写死 10**、**只有托盘能触发**。
// 服务器形态（cmd/server）与 Linux/macOS 根本没有托盘，于是那条路径对它们
// 等于不存在 —— 而"账号池"恰恰是最该异地留一份的东西。
//
// 拆开之后有三层，各自可以单独测：
//
//	Scope    备什么（声明式，不含 IO 策略）
//	Build   怎么打包（zip + manifest，只在临时目录里产生副作用）
//	Channel 送到哪（local / s3 / openlist，可插拔）
//
// # 为什么打包结果是**磁盘上的临时文件**而不是内存 []byte
//
// `data/state.json` 在多账号部署里能到几十 MB，把整包放在内存里会让备份
// 与网关的峰值内存叠加。写临时文件也让"上传失败重试"不必重新打包。
package backup

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// FormatVersion 清单格式版本。将来换包结构时靠它判"能不能恢复"。
const FormatVersion = 1

// ErrRunning 已有一次备份在执行。
//
// 定时与手动撞车是常态（用户看到 3 点没备份就点了按钮）。这里的判据是
// **不排队**：排队会让第二次备份在第一次结束后再跑一遍，用户等到的结果
// 与"备份进行中"完全一样，却多花一份带宽。
var ErrRunning = errors.New("backup: 已有一次备份正在进行")

// Scope 声明"备什么"。
//
// 路径规则：Files / Dirs 都是**相对 Root** 的路径，Root 是数据根。
// 用相对路径是因为数据根有三种形态（`-data-dir`、portable、%APPDATA%），
// 绝对路径会把"同一份备份描述"焊死在某一台机器上。
type Scope struct {
	// Root 数据根（%APPDATA%\NaiBox 或 -data-dir 指定的目录）。
	Root string
	// Files 要备份的单个文件（相对 Root），如 "config.json"。
	Files []string
	// Dirs 要备份的目录（相对 Root），如 "auths" / "data"。
	Dirs []string
	// ConfigPath 配置文件绝对路径（可空）。非空且与 Root 不同时，
	// 清单里记为 "config.json"，内容取自它 —— -config 指向别处就是这么来的。
	ConfigPath string
}

// ManifestFile 清单里的一条文件。
type ManifestFile struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest 包内的 manifest.json。恢复方据此判断"这个包完整不完整、是谁的"。
type Manifest struct {
	FormatVersion int            `json:"format_version"`
	CreatedAt     time.Time      `json:"created_at"`
	Host          string         `json:"host,omitempty"`
	Root          string         `json:"root,omitempty"`
	FileCount     int            `json:"file_count"`
	TotalBytes    int64          `json:"total_bytes"`
	Files         []ManifestFile `json:"files"`
	// Note 给恢复方看的一句话（当前用于声明"日志不在包里"）。
	Note string `json:"note,omitempty"`
}

// Package 一次打包的结果。
//
// Path 是**调用方负责删除**的临时文件（用 Cleanup），不要长期持有。
type Package struct {
	Name     string
	Path     string
	Size     int64
	Manifest Manifest
}

// Cleanup 删除临时包文件。可重复调用。
func (p *Package) Cleanup() {
	if p == nil || p.Path == "" {
		return
	}
	os.Remove(p.Path)
	p.Path = ""
}

// backupName 生成包文件名。
//
// ⚠ 前缀保持 `omni2api-backup-` 不变（虽然产品已改名 NaiBox）：
// 本地通道的历史包都是这个名字，改名会让 ① 老包永远不被清理（prune 按前缀选）、
// ② 用户写死的脚本/同步盘规则找不到文件。改名要单独做一次迁移，不能顺手改。
func backupName(now time.Time) string {
	return "omni2api-backup-" + now.Format("20060102-150405") + ".zip"
}

// Build 按 Scope 打一个包，返回到临时文件。
//
// 顺序固定（文件按名字排序、manifest 最后写），所以同一份数据在同一秒内
// 打两次包得到的内容是稳定的 —— 这让"备份是否真的变了"可以被测试断言。
func Build(scope Scope, now time.Time) (*Package, error) {
	if strings.TrimSpace(scope.Root) == "" {
		return nil, errors.New("backup: scope.Root 为空")
	}
	entries, err := collect(scope)
	if err != nil {
		return nil, err
	}

	f, err := os.CreateTemp("", "naibox-backup-*.zip")
	if err != nil {
		return nil, fmt.Errorf("创建临时包: %w", err)
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(tmp)
		}
	}()

	zw := zip.NewWriter(f)
	man := Manifest{
		FormatVersion: FormatVersion,
		CreatedAt:     now,
		Root:          scope.Root,
		Note:          "logs/ 不在备份内：日志是诊断材料，不是资产",
	}
	man.Host, _ = os.Hostname()
	for _, e := range entries {
		hdr := &zip.FileHeader{Name: filepath.ToSlash(e.Name), Method: zip.Deflate}
		hdr.Modified = e.ModTime
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return nil, fmt.Errorf("写入 %s: %w", e.Name, err)
		}
		sum, n, err := copyHashed(w, e.Src)
		if err != nil {
			return nil, fmt.Errorf("打包 %s: %w", e.Name, err)
		}
		man.Files = append(man.Files, ManifestFile{Name: e.Name, Size: n, SHA256: sum})
		man.TotalBytes += n
	}
	man.FileCount = len(man.Files)

	raw, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("序列化清单: %w", err)
	}
	w, err := zw.Create("manifest.json")
	if err != nil {
		return nil, fmt.Errorf("写入清单: %w", err)
	}
	if _, err := w.Write(append(raw, '\n')); err != nil {
		return nil, fmt.Errorf("写入清单: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("收尾 zip: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("关闭临时包: %w", err)
	}

	st, err := os.Stat(tmp)
	if err != nil {
		return nil, fmt.Errorf("读取临时包: %w", err)
	}
	ok = true
	return &Package{Name: backupName(now), Path: tmp, Size: st.Size(), Manifest: man}, nil
}

type entry struct {
	Name    string // 包内名字（相对 Root，正斜杠）
	Src     string // 磁盘绝对路径
	ModTime time.Time
}

// resolveFile 把 Scope.Files 的一项解析成 (磁盘路径, 包内名字)。
//
// 绝对路径按"文件名进包、目录不带上"处理：`auth_dir` 可以配到数据根之外
// （部署者的既有习惯），而包内结构不该因此变成 `/home/someone/…` 那种形态。
func (s Scope) resolveFile(p string) (src, name string) {
	if filepath.IsAbs(p) {
		return p, filepath.Base(p)
	}
	if p == "config.json" && s.ConfigPath != "" {
		return s.ConfigPath, "config.json"
	}
	return filepath.Join(s.Root, filepath.FromSlash(p)), p
}

// resolveDir 把 Scope.Dirs 的一项解析成 (磁盘目录, 包内前缀)。
func (s Scope) resolveDir(p string) (dir, prefix string) {
	if filepath.IsAbs(p) {
		return p, filepath.Base(filepath.Clean(p))
	}
	return filepath.Join(s.Root, filepath.FromSlash(p)), p
}

// collect 列出要打包的文件。
//
// 目录不存在**不算错误**（首次启动就点备份时 auths 可能还没建），
// 但读不动（权限）要如实返回 —— 静默跳过会让用户拿到一个"看起来成功、
// 其实少了账号目录"的包，那比报错糟得多。
func collect(scope Scope) ([]entry, error) {
	var out []entry
	seen := map[string]bool{}

	addFile := func(name, src string) error {
		if seen[name] {
			return nil
		}
		st, err := os.Stat(src)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("读取 %s: %w", name, err)
		}
		if st.IsDir() {
			return nil
		}
		seen[name] = true
		out = append(out, entry{Name: name, Src: src, ModTime: st.ModTime()})
		return nil
	}

	for _, p := range scope.Files {
		src, name := scope.resolveFile(p)
		if err := addFile(name, src); err != nil {
			return nil, err
		}
	}
	for _, p := range scope.Dirs {
		root, prefix := scope.resolveDir(p)
		st, err := os.Stat(root)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("读取目录 %s: %w", prefix, err)
		}
		if !st.IsDir() {
			continue
		}
		err = filepath.WalkDir(root, func(full string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(root, full)
			if err != nil {
				return err
			}
			return addFile(filepath.ToSlash(filepath.Join(prefix, rel)), full)
		})
		if err != nil {
			return nil, fmt.Errorf("遍历 %s: %w", prefix, err)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// copyHashed 拷贝并同时算 sha256，避免为了填清单再读一遍源文件。
func copyHashed(w io.Writer, path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// Channel 一个"送到哪"的实现。
//
// 约定：所有方法都必须尊重 ctx；Put 的 r 只需在调用期间可用
// （Manager 不会重试时复用同一个 reader，而是重新打开临时包）。
type Channel interface {
	// ID 配置里的通道标识（日志与结果里用它）。
	ID() string
	// Kind 通道类型：local / s3 / openlist。
	Kind() string
	// Test 连通性与凭据自检（管理台的「测试」按钮）。
	Test(ctx context.Context) error
	// Put 上传一个包，返回可展示的位置（本地路径 / 对象 URL）。
	Put(ctx context.Context, name string, r io.Reader, size int64) (string, error)
	// List 列出该通道里已有的备份包（新的在前）。
	List(ctx context.Context) ([]Object, error)
	// Get 取回一个包（管理台「下载」用）。
	Get(ctx context.Context, name string) (io.ReadCloser, error)
}

// Object 通道里的一个对象。
type Object struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

// Result 单个通道的一次执行结果。
type Result struct {
	Channel  string    `json:"channel"`
	Kind     string    `json:"kind"`
	Name     string    `json:"name,omitempty"`
	Location string    `json:"location,omitempty"`
	Bytes    int64     `json:"bytes,omitempty"`
	Err      string    `json:"error,omitempty"`
	At       time.Time `json:"at"`
}

// OK 该通道是否成功。
func (r Result) OK() bool { return r.Err == "" }

// Manager 串行执行"打包 + 分发"。
//
// # 为什么配置可以运行期替换
//
// 管理台保存通道后立刻生效（`Configure`），不要求重启：备份是"用户点了就想
// 看到结果"的动作，逼他重启一次才生效属于可用性问题。代价是内部要加锁，
// 而这把锁的粒度是"整次备份"（本来就串行）。
type Manager struct {
	mu       sync.Mutex
	running  bool
	scope    func() Scope
	channels []Channel
	keep     int
	maxWarn  int64
	last     []Result
	logf     func(format string, args ...any)
	now      func() time.Time
}

// NewManager 构造。
//
// scope 用函数而不是值：数据根与配置文件路径在 portable/`-data-dir`/热重载
// 下会变，构造期取一份会让周末的长跑进程一直备份旧目录。
func NewManager(scope func() Scope, channels []Channel, keep int,
	logf func(string, ...any)) *Manager {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if keep <= 0 {
		keep = 10
	}
	return &Manager{
		scope:    scope,
		channels: channels,
		keep:     keep,
		maxWarn:  100 << 20, // 100 MB 起记告警（不阻断）
		logf:     logf,
		now:      time.Now,
	}
}

// Configure 运行期替换通道表与保留份数（管理台保存后调用）。
func (m *Manager) Configure(channels []Channel, keep int) {
	if keep <= 0 {
		keep = 10
	}
	m.mu.Lock()
	m.channels, m.keep = channels, keep
	m.mu.Unlock()
	for _, c := range channels {
		if l, ok := c.(interface{ SetKeep(int) }); ok {
			l.SetKeep(keep)
		}
	}
}

// Channels 当前通道表的只读快照（管理台展示用）。
func (m *Manager) Channels() []Channel {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Channel, len(m.channels))
	copy(out, m.channels)
	return out
}

// Last 上一次执行的结果（可能为空）。
func (m *Manager) Last() []Result {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Result, len(m.last))
	copy(out, m.last)
	return out
}

// Run 打包并按通道分发。
//
// ids 为空 = 跑全部已启用通道。任一通道失败不影响其它通道，也不返回错误
// （结果在 []Result 里逐条给出）—— 备份是**尽力而为**的动作：S3 挂了不该
// 连累"本地还留了一份"。
//
// 唯一的错误是 ErrRunning（已有一次在跑）与打包本身失败（那时没有包可传，
// 所有通道都无从执行）。
func (m *Manager) Run(ctx context.Context, ids []string) ([]Result, error) {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return nil, ErrRunning
	}
	m.running = true
	channels := append([]Channel(nil), m.channels...)
	keep := m.keep
	scopeFn := m.scope
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.running = false
		m.mu.Unlock()
	}()

	// 通道为空时连临时包都不必打：本地通道也是显式配置的一项（默认 config
	// 里就带着它），所以空通道表只可能是用户全删了。此时如实报出来，
	// 而不是打一个谁也不收的包然后返回"成功 0 个通道"。
	if len(channels) == 0 {
		return nil, errors.New("backup: 没有配置任何通道")
	}

	pkg, err := Build(scopeFn(), m.now())
	if err != nil {
		return nil, err
	}
	defer pkg.Cleanup()

	if pkg.Size > m.maxWarn {
		m.logf("备份包较大（%d MB，%d 个文件）：通道上传可能超时，检查 data/ 是否被日志污染",
			pkg.Size>>20, pkg.Manifest.FileCount)
	}

	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var results []Result
	for _, c := range channels {
		if len(want) > 0 && !want[c.ID()] {
			continue
		}
		r := Result{Channel: c.ID(), Kind: c.Kind(), Name: pkg.Name, Bytes: pkg.Size, At: m.now()}
		f, openErr := os.Open(pkg.Path)
		if openErr != nil {
			r.Err = "打开临时包失败: " + openErr.Error()
			results = append(results, r)
			continue
		}
		loc, putErr := c.Put(ctx, pkg.Name, f, pkg.Size)
		f.Close()
		if putErr != nil {
			r.Err = putErr.Error()
			m.logf("备份通道 %s(%s) 失败: %v", c.ID(), c.Kind(), putErr)
		} else {
			r.Location = loc
			m.logf("备份通道 %s(%s) 完成: %s（%d 字节，%d 个文件）",
				c.ID(), c.Kind(), loc, pkg.Size, pkg.Manifest.FileCount)
		}
		if l, ok := c.(interface{ Prune(int) error }); ok && r.OK() {
			if err := l.Prune(keep); err != nil {
				m.logf("备份通道 %s 清理旧包失败: %v", c.ID(), err)
			}
		}
		results = append(results, r)
	}

	m.mu.Lock()
	m.last = results
	m.mu.Unlock()
	return results, nil
}
