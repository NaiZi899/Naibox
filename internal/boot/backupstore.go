// backupstore.go 管理台「备份」页的宿主实现。
//
// # 为什么它在这里而不是 internal/admin 里
//
// admin 只定义接口（`admin.BackupStore`）与 HTTP 形状；"通道怎么建、配置写回
// 哪一行、定时怎么排"都是**宿主的事实** —— 与 `settings_store.go` /
// `customstore.go` 同一条纪律。admin 不得知道 config.json 的字段长什么样。
package boot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"

	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/NaiZi899/Naibox/internal/admin"
	"github.com/NaiZi899/Naibox/internal/backup"
)

// backupStore 备份管理器 + 配置读写 + 定时排班。
type backupStore struct {
	path     string // config.json 路径
	dataRoot string // 数据根（local 通道默认落在 <dataRoot>/backups）
	logf     func(string, ...any)

	mu     sync.Mutex
	cfg    *Config
	mgr    *backup.Manager
	lastAt time.Time
}

// newBackupStore 按当前配置建通道，并把定时排班接上。
func newBackupStore(cfgPath, dataRoot string, cfg *Config, logf func(string, ...any)) (*backupStore, error) {
	if logf == nil {
		logf = log.Printf
	}
	s := &backupStore{path: cfgPath, dataRoot: dataRoot, cfg: cfg, logf: logf}
	// 老配置（升级上来的 config.json）根本没有 backup 段：此时**必须**仍然
	// 有一份本地备份，否则托盘那个按钮会从"打一个 zip"变成"报错没有通道"。
	// 这是升级路径上的行为回归，所以在这里补一条隐式 local 通道（不落盘）。
	if len(cfg.Backup.Channels) == 0 {
		cfg.Backup.Channels = []BackupChannelConfig{{ID: "local", Kind: "local"}}
	}
	channels, err := buildBackupChannels(cfg, dataRoot)
	if err != nil {
		return nil, err
	}
	s.mgr = backup.NewManager(s.scope, channels, cfg.Backup.Keep, logf)
	return s, nil
}

// Manager 暴露给桌面壳（托盘「备份数据」走同一条链路）。
func (s *backupStore) Manager() *backup.Manager { return s.mgr }

// current 返回当前生效配置的只读引用（Save 后会整体替换）。
func (s *backupStore) current() *Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// scope 声明"备什么"。
//
// 路径取**装配后**的值（cfg.AuthsBase / cfg.StateFile 已经是绝对路径）：
// 它们在 portable 与 -data-dir 形态下都不同，写死相对路径会备到错目录。
func (s *backupStore) scope() backup.Scope {
	cfg := s.current()
	sc := backup.Scope{Root: s.dataRoot, Files: []string{"config.json"}, ConfigPath: s.path}
	if cfg == nil {
		return sc
	}
	if cfg.AuthsBase != "" {
		sc.Dirs = append(sc.Dirs, cfg.AuthsBase)
	} else if cfg.AuthDir != "" {
		sc.Dirs = append(sc.Dirs, cfg.AuthDir)
	}
	if cfg.StateFile != "" {
		// state.json 与其它运行态（如 checkin-log）同目录：备份整个 data/。
		sc.Dirs = append(sc.Dirs, filepath.Dir(cfg.StateFile))
	}
	return sc
}

// buildBackupChannels 把配置翻译成通道实例。
//
// 停用的通道**不进表**（`enabled=false` 的 s3 不该在每次备份时拖一个超时）。
// 但它们在管理台上仍要能看见 —— 那是 admin 侧从配置读的事，与这里无关。
func buildBackupChannels(cfg *Config, dataRoot string) ([]backup.Channel, error) {
	var out []backup.Channel
	for _, ch := range cfg.Backup.Channels {
		if !ch.IsEnabled() {
			continue
		}
		switch ch.Kind {
		case "local":
			dir := ch.Dir
			if dir == "" {
				dir = filepath.Join(dataRoot, "backups")
			}
			out = append(out, backup.NewLocalChannel(ch.ID, dir))
		case "s3":
			c, err := backup.NewS3Channel(ch.ID, backup.S3Options{
				Endpoint: ch.Endpoint, Bucket: ch.Bucket, Region: ch.Region,
				Prefix: ch.Prefix, AccessKey: ch.AccessKey, SecretKey: ch.SecretKey,
			})
			if err != nil {
				return nil, fmt.Errorf("backup.channels[%s]: %w", ch.ID, err)
			}
			out = append(out, c)
		case "openlist":
			c, err := backup.NewOpenListChannel(ch.ID, backup.OpenListOptions{
				Endpoint: ch.Endpoint, Token: ch.Token, Dir: ch.RemoteDir,
			})
			if err != nil {
				return nil, fmt.Errorf("backup.channels[%s]: %w", ch.ID, err)
			}
			out = append(out, c)
		default:
			return nil, fmt.Errorf("backup.channels[%s]: 未知 kind=%q", ch.ID, ch.Kind)
		}
	}
	return out, nil
}

// channel 按 id 找通道实例。
func (s *backupStore) channel(id string) (backup.Channel, error) {
	for _, c := range s.mgr.Channels() {
		if c.ID() == id {
			return c, nil
		}
	}
	return nil, fmt.Errorf("通道 %q 未启用或不存在", id)
}

// Snapshot 管理台的读快照：配置回声（凭据只留尾 4 位）+ 上一次执行结果。
//
// # 为什么不在快照里顺带列远端对象
//
// 列对象要打网络（S3 ListObjectsV2 / OpenList /api/fs/list）。把网络放进
// "打开设置页"这条路径上，会让一次网盘抖动变成整页转圈 —— 那正是本项目
// 反复避免的"读页面触发上游请求"。对象列表由 `GET /admin/backup/objects`
// 单独取。
func (s *backupStore) Snapshot(context.Context) admin.BackupView {
	cfg := s.current()
	v := admin.BackupView{Keep: 10, Channels: []admin.BackupChannelView{}}
	if cfg != nil {
		v.Enabled = cfg.Backup.Enabled
		v.Hours = append([]int(nil), cfg.Backup.Hours...)
		v.Keep = cfg.Backup.Keep
		for _, ch := range cfg.Backup.Channels {
			v.Channels = append(v.Channels, admin.BackupChannelView{
				ID: ch.ID, Kind: ch.Kind, Enabled: ch.IsEnabled(),
				Dir: ch.Dir, Endpoint: ch.Endpoint, Bucket: ch.Bucket,
				Region: ch.Region, Prefix: ch.Prefix, RemoteDir: ch.RemoteDir,
				AccessKey: maskTail(ch.AccessKey), SecretKey: maskTail(ch.SecretKey),
				Token: maskTail(ch.Token),
			})
		}
	}
	for _, r := range s.mgr.Last() {
		v.Last = append(v.Last, admin.BackupResultView{
			Channel: r.Channel, Kind: r.Kind, Name: r.Name, Location: r.Location,
			Bytes: r.Bytes, Error: r.Err, At: r.At.Format(time.RFC3339), OK: r.OK(),
		})
	}
	return v
}

// Run 立即备份。ids 为空 = 全部已启用通道。
func (s *backupStore) Run(ctx context.Context, ids []string) ([]admin.BackupResultView, error) {
	res, err := s.mgr.Run(ctx, ids)
	if err != nil {
		// 把 backup.ErrRunning 翻译成 admin 侧的哨兵（HTTP 层据此回 409）。
		if errors.Is(err, backup.ErrRunning) {
			return nil, admin.ErrBackupRunning
		}
		return nil, err
	}
	out := make([]admin.BackupResultView, 0, len(res))
	for _, r := range res {
		out = append(out, admin.BackupResultView{
			Channel: r.Channel, Kind: r.Kind, Name: r.Name, Location: r.Location,
			Bytes: r.Bytes, Error: r.Err, At: r.At.Format(time.RFC3339), OK: r.OK(),
		})
	}
	return out, nil
}

// TestChannel 单通道连通性自检。
func (s *backupStore) TestChannel(ctx context.Context, id string) (admin.BackupProbeView, error) {
	c, err := s.channel(id)
	if err != nil {
		return admin.BackupProbeView{At: time.Now().Format(time.RFC3339)}, err
	}
	start := time.Now()
	terr := c.Test(ctx)
	v := admin.BackupProbeView{
		OK:        terr == nil,
		LatencyMS: time.Since(start).Milliseconds(),
		At:        time.Now().Format(time.RFC3339),
	}
	if terr != nil {
		v.Error = terr.Error()
	}
	return v, nil
}

// Objects 列某个通道里的备份包。
func (s *backupStore) Objects(ctx context.Context, id string) ([]admin.BackupObjectView, error) {
	c, err := s.channel(id)
	if err != nil {
		return nil, err
	}
	list, err := c.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]admin.BackupObjectView, 0, len(list))
	for _, o := range list {
		mod := ""
		if !o.ModTime.IsZero() {
			mod = o.ModTime.Format(time.RFC3339)
		}
		out = append(out, admin.BackupObjectView{Name: o.Name, Size: o.Size, ModTime: mod})
	}
	return out, nil
}

// Open 取回一个包（管理台「下载」）。
func (s *backupStore) Open(ctx context.Context, id, name string) (io.ReadCloser, int64, error) {
	c, err := s.channel(id)
	if err != nil {
		return nil, 0, err
	}
	rc, err := c.Get(ctx, name)
	if err != nil {
		return nil, 0, err
	}
	var size int64
	if list, lerr := c.List(ctx); lerr == nil {
		for _, o := range list {
			if o.Name == name {
				size = o.Size
				break
			}
		}
	}
	return rc, size, nil
}

// Save 保存 backup 段：校验 → 写回 config.json → 热生效。
//
// # 为什么热生效（不要求重启）
//
// 备份是"点了就想看到结果"的动作。改个保留份数就要重启一次网关，代价与
// 收益完全不成比例 —— 而 Manager.Configure 已经能安全替换通道表。
//
// # 凭据的空串语义
//
// 与自定义上游同一手法：补丁里 `access_key`/`secret_key`/`token` 传空串
// 表示**沿用旧值**。否则前端每次保存都要把明文密钥回填一次，等于强迫
// 页面持有它 —— 而页面只需要知道"配过了"。
func (s *backupStore) Save(raw json.RawMessage) (applied, needRestart []string, err error) {
	var patch BackupPatch
	if err := json.Unmarshal(raw, &patch); err != nil {
		return nil, nil, fmt.Errorf("备份配置不是合法 JSON: %w", err)
	}
	old := s.current()
	if old == nil {
		return nil, nil, errors.New("配置尚未加载")
	}
	merged := Config{}
	merged.Backup.Enabled = patch.Enabled
	merged.Backup.Hours = patch.Hours
	merged.Backup.Keep = patch.Keep
	oldByID := map[string]BackupChannelConfig{}
	for _, ch := range old.Backup.Channels {
		oldByID[ch.ID] = ch
	}
	for _, ch := range patch.Channels {
		if prev, ok := oldByID[ch.ID]; ok {
			if ch.AccessKey == "" {
				ch.AccessKey = prev.AccessKey
			}
			if ch.SecretKey == "" {
				ch.SecretKey = prev.SecretKey
			}
			if ch.Token == "" {
				ch.Token = prev.Token
			}
		}
		merged.Backup.Channels = append(merged.Backup.Channels, ch)
	}
	if err := merged.normalizeBackup(); err != nil {
		return nil, nil, err
	}
	channels, err := buildBackupChannels(&merged, s.dataRoot)
	if err != nil {
		return nil, nil, err
	}
	rawBackup, err := json.Marshal(merged.Backup)
	if err != nil {
		return nil, nil, err
	}
	if err := writeConfigKey(s.path, "backup", rawBackup); err != nil {
		return nil, nil, err
	}

	s.mu.Lock()
	old.Backup = merged.Backup
	s.mu.Unlock()
	s.mgr.Configure(channels, merged.Backup.Keep)
	s.logf("备份配置已更新：%d 条通道（其中启用 %d 条），定时=%v 时点=%v 保留=%d",
		len(merged.Backup.Channels), len(channels), merged.Backup.Enabled,
		merged.Backup.Hours, merged.Backup.Keep)
	return []string{"backup"}, nil, nil
}

// BackupPatch 管理台提交的 backup 段（字段与 config 一一对应）。
//
// 单独声明而不是复用 Config 的内嵌结构：PUT 的语义是**整段替换**，
// 复用内嵌结构会让人以为可以只发一半字段（那会把没发的键清零）。
type BackupPatch struct {
	Enabled  bool                  `json:"enabled"`
	Hours    []int                 `json:"hours"`
	Keep     int                   `json:"keep"`
	Channels []BackupChannelConfig `json:"channels"`
}

// StartSchedule 按 backup.hours 定时跑（整点语义，与签到/保活一致）。
//
// # 为什么自带 ticker 而不是挂调度器槽位
//
// 调度器的整点槽位已经停用（`scheduler.New` 传 `Slots: nil`，签到/保活
// 改成 30 分钟 JobExt 扫描）。挂一个已经不存在的机制不会报错，只会让
// "定时备份"永远不触发 —— 那种失败没有任何痕迹。
//
// 检查周期取 5 分钟：整点触发的最小判定粒度必须小于 1 小时，否则 3 点的
// 备份会在 3:00 之后某个不确定的时刻才跑（甚至跨过整点后判成 4 点已跑过）。
func (s *backupStore) StartSchedule(ctx context.Context) {
	go func() {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.tick(ctx, time.Now())
			}
		}
	}()
}

// tick 判断"这个小时该不该跑"，并保证每小时最多一次。
func (s *backupStore) tick(ctx context.Context, now time.Time) {
	cfg := s.current()
	if cfg == nil || !cfg.Backup.Enabled {
		return
	}
	want := false
	for _, h := range cfg.Backup.Hours {
		if h == now.Hour() {
			want = true
			break
		}
	}
	if !want {
		return
	}
	s.mu.Lock()
	if !s.lastAt.IsZero() && s.lastAt.Hour() == now.Hour() &&
		s.lastAt.YearDay() == now.YearDay() && s.lastAt.Year() == now.Year() {
		s.mu.Unlock()
		return
	}
	s.lastAt = now
	s.mu.Unlock()

	res, err := s.mgr.Run(ctx, nil)
	if err != nil {
		if errors.Is(err, backup.ErrRunning) {
			s.logf("定时备份跳过：已有一次备份在进行")
			return
		}
		s.logf("定时备份失败：%v", err)
		return
	}
	ok, bad := 0, 0
	for _, r := range res {
		if r.OK() {
			ok++
		} else {
			bad++
		}
	}
	s.logf("定时备份完成：成功 %d 条通道，失败 %d 条", ok, bad)
}

// maskTail 只留尾 4 位。
//
// 不足 8 位时整串掩掉：短密钥露出尾 4 位等于露出它的一半。
func maskTail(s string) string {
	if s == "" {
		return ""
	}
	if len(s) < 8 {
		return strings.Repeat("*", len(s))
	}
	return "…" + s[len(s)-4:]
}
