// profilestore.go 浏览器档案的宿主实现。
//
// # 三件事只有宿主能做
//
//  1. **账号名**：档案表里只有 uid，昵称在账号池里。绑定时要把昵称记进 Label
//     （用户要求："绑定具体 UID，如果没绑定账号名字也一起绑了"），否则界面上
//     只有一串 uid，人看不出这是哪个号。
//  2. **线路 → 代理 URL**：档案存 egress_id，浏览器要的是 URL。
//  3. **打开目录**：只有桌面壳能开资源管理器；服务端形态如实回 501。
package boot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/NaiZi899/Naibox/internal/admin"
	"github.com/NaiZi899/Naibox/internal/browseropen"
	"github.com/NaiZi899/Naibox/internal/browserprofile"
	"github.com/NaiZi899/Naibox/internal/pool"
)

// profileDiskScanLimit 算档案目录体积时最多看多少条。
//
// 一个用久了的 Chromium 档案能有几万个文件（Cache/、Code Cache/）。
// 为了在页面上显示一个"大概多大"，为此遍历十万条文件不值 —— 到上限就停，
// 显示成 "≥" 由前端处理（这里只给出已统计到的字节）。
const profileDiskScanLimit = 20000

// profileStore 浏览器档案 + 账号绑定 + 启动。
type profileStore struct {
	store    *browserprofile.Store
	pool     *pool.Pool
	cfg      *Config
	openPath func(string) error // nil = 服务端形态（不支持打开目录）
	logf     func(string, ...any)
}

func newProfileStore(cfg *Config, p *pool.Pool, store *browserprofile.Store, openPath func(string) error, logf func(string, ...any)) *profileStore {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &profileStore{store: store, pool: p, cfg: cfg, openPath: openPath, logf: logf}
}

// accountIndex uid → (provider, nickname)。一次遍历，避免每个档案各查一遍池。
func (s *profileStore) accountIndex() map[string]admin.EgressAccountView {
	out := map[string]admin.EgressAccountView{}
	if s.pool == nil {
		return out
	}
	for _, st := range s.pool.List() {
		v := admin.EgressAccountView{UID: st.UID, Nickname: st.Nickname, Provider: st.Provider}
		if a := s.pool.AuthByUID(st.UID); a != nil {
			v.Channel = a.Channel
			v.Egress = a.EgressID
		}
		out[st.UID] = v
	}
	return out
}

// List 档案 + 浏览器探测 + 可绑定账号。
func (s *profileStore) List(ctx context.Context) ([]admin.BrowserProfileView, admin.BrowserProbeView, []admin.EgressAccountView) {
	profiles, err := s.store.List()
	if err != nil {
		s.logf("浏览器档案索引读取失败：%v", err)
	}
	idx := s.accountIndex()
	out := make([]admin.BrowserProfileView, 0, len(profiles))
	for _, p := range profiles {
		out = append(out, s.viewOf(p, idx))
	}
	accounts := make([]admin.EgressAccountView, 0, len(idx))
	for _, v := range idx {
		accounts = append(accounts, v)
	}
	// 排序：按 uid 稳定，界面不跳行。
	sortAccounts(accounts)
	return out, s.probeBrowser(), accounts
}

// viewOf 单条档案 → 展示形态（含账号名与目录体积）。
func (s *profileStore) viewOf(p browserprofile.Profile, idx map[string]admin.EgressAccountView) admin.BrowserProfileView {
	v := admin.BrowserProfileView{
		ID: p.ID, UID: p.UID, Provider: p.Provider, Label: p.Label,
		EgressID: p.EgressID, StartURL: p.StartURL, ExtraArgs: p.ExtraArgs,
		LastURL: p.LastURL, DebugPort: p.LastDebugPort,
		Dir: s.store.UserDataDir(p),
	}
	if !p.CreatedAt.IsZero() {
		v.CreatedAt = p.CreatedAt.Format(time.RFC3339)
	}
	if !p.LastUsedAt.IsZero() {
		v.LastUsedAt = p.LastUsedAt.Format(time.RFC3339)
	}
	if acc, ok := idx[p.UID]; ok && p.UID != "" {
		v.AccountName = firstNonEmpty(acc.Nickname, acc.UID)
		if v.Provider == "" {
			v.Provider = acc.Provider
		}
	}
	v.DiskBytes = dirSize(s.store.UserDataDir(p), profileDiskScanLimit)
	return v
}

// probeBrowser 本机浏览器探测（编辑器里"留空 = 自动探测"的依据）。
func (s *profileStore) probeBrowser() admin.BrowserProbeView {
	explicit := ""
	if s.cfg != nil {
		explicit = s.cfg.Login.Browser
	}
	l, err := browseropen.Resolve(runtime.GOOS, explicit, nil)
	if err != nil {
		return admin.BrowserProbeView{OK: false, Err: err.Error()}
	}
	return admin.BrowserProbeView{OK: true, Name: l.Name, Path: l.Path}
}

// profilePatch 提交的档案补丁。
type profilePatch struct {
	ID        string   `json:"id"`
	UID       string   `json:"uid"`
	Provider  string   `json:"provider"`
	Label     string   `json:"label"`
	EgressID  string   `json:"egress_id"`
	StartURL  string   `json:"start_url"`
	ExtraArgs []string `json:"extra_args"`
}

// Upsert 新建/更新档案；绑 uid 时把账号名一并写进 Label。
//
// # 为什么 Label 只在为空时自动填
//
// 用户可能自己给它起了名字（"主号档案"）。自动填只在 Label 为空时发生 ——
// 否则每次保存都会把用户起的名字覆盖成昵称，那是"保存一次丢一次"的经典体感。
func (s *profileStore) Upsert(ctx context.Context, raw json.RawMessage) (admin.BrowserProfileView, error) {
	var patch profilePatch
	if err := json.Unmarshal(raw, &patch); err != nil {
		return admin.BrowserProfileView{}, fmt.Errorf("档案不是合法 JSON: %w", err)
	}
	patch.ID = strings.TrimSpace(patch.ID)
	patch.UID = strings.TrimSpace(patch.UID)
	patch.EgressID = strings.TrimSpace(patch.EgressID)

	// extra_args 不允许覆盖那两个决定性参数（否则档案/线路会静默失效）。
	for _, a := range patch.ExtraArgs {
		l := strings.ToLower(strings.TrimSpace(a))
		if strings.HasPrefix(l, "--user-data-dir") || strings.HasPrefix(l, "--proxy-server") {
			return admin.BrowserProfileView{}, fmt.Errorf(
				"%q 不能写进附加参数：档案目录与出口由面板决定，手写会覆盖掉它们", a)
		}
	}
	// 线路必须存在（否则启动时才炸，用户已经以为配好了）。
	if patch.EgressID != "" {
		if s.egressLine(patch.EgressID) == nil {
			return admin.BrowserProfileView{}, fmt.Errorf("线路 %q 不存在（先在「出口线路」里加，或留空=直连）", patch.EgressID)
		}
	}

	idx := s.accountIndex()
	if patch.UID != "" {
		acc, ok := idx[patch.UID]
		if !ok {
			return admin.BrowserProfileView{}, fmt.Errorf("账号 %s 不在池里（先加号，或留空=不绑定）", patch.UID)
		}
		if patch.Provider == "" {
			patch.Provider = acc.Provider
		}
		if patch.Label == "" {
			// 用户要求：绑 uid 时把账号名也一起绑上。
			patch.Label = firstNonEmpty(acc.Nickname, acc.UID)
		}
	}

	prev, had := s.store.Get(patch.ID)
	next := browserprofile.Profile{
		ID: patch.ID, UID: patch.UID, Provider: patch.Provider, Label: patch.Label,
		EgressID: patch.EgressID, StartURL: patch.StartURL, ExtraArgs: patch.ExtraArgs,
	}
	if had {
		// 保留记账字段：它们由启动路径写，不该被一次编辑清掉。
		next.CreatedAt = prev.CreatedAt
		next.LastUsedAt = prev.LastUsedAt
		next.LastURL = prev.LastURL
		next.LastDebugPort = prev.LastDebugPort
	}
	saved, err := s.store.Upsert(next)
	if err != nil {
		return admin.BrowserProfileView{}, err
	}
	s.logf("浏览器档案已保存：%s（uid=%s 线路=%s）", saved.ID, orDash(saved.UID), orDash(saved.EgressID))
	return s.viewOf(saved, idx), nil
}

// Delete 删档案（keepDir=true 只清索引）。
func (s *profileStore) Delete(ctx context.Context, id string, keepDir bool) error {
	if err := s.store.Delete(id, keepDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("档案 %s 不存在", id)
		}
		return err
	}
	s.logf("浏览器档案已删除：%s（保留目录=%v）", id, keepDir)
	return nil
}

// Launch 启动档案里的浏览器。
func (s *profileStore) Launch(ctx context.Context, id, url string) (admin.LaunchResult, error) {
	p, ok := s.store.Get(id)
	if !ok {
		return admin.LaunchResult{}, fmt.Errorf("档案 %s 不存在", id)
	}
	if url == "" {
		url = p.StartURL
	}
	explicit := ""
	if s.cfg != nil {
		explicit = s.cfg.Login.Browser
	}
	opts := browserprofile.Options{
		URL:       url,
		Explicit:  explicit,
		ExtraArgs: p.ExtraArgs,
	}
	if p.EgressID != "" {
		line := s.egressLine(p.EgressID)
		if line == nil {
			return admin.LaunchResult{}, fmt.Errorf("档案绑的线路 %q 已不存在（改绑或清空后再启动）", p.EgressID)
		}
		opts.ProxyURL = line.Proxy
	}
	cmd, err := browserprofile.Open(s.store, p, opts)
	if err != nil {
		return admin.LaunchResult{}, err
	}
	res := admin.LaunchResult{OK: true, Browser: cmd.Browser, Command: redactCommand(cmd)}
	// 端口是浏览器自己选的（--remote-debugging-port=0），读 DevToolsActivePort。
	// 等 3 秒：正常机器 200–600ms 就写好了；等不到不算失败（浏览器可能还在起）。
	if port, ok := browserprofile.WaitDebugPort(s.store.UserDataDir(p), 3*time.Second); ok {
		res.DebugPort = port
	}
	s.logf("浏览器档案已启动：%s（%s，调试端口 %d）", p.ID, cmd.Browser, res.DebugPort)
	return res, nil
}

// OpenDir 打开档案目录（服务端形态不支持）。
func (s *profileStore) OpenDir(ctx context.Context, id string) error {
	if s.openPath == nil {
		return fmt.Errorf("%w：服务端形态没有桌面，请在文件管理器里打开数据根下的 browser/%s",
			admin.ErrProfilesUnsupported, id)
	}
	p, ok := s.store.Get(id)
	if !ok {
		return fmt.Errorf("档案 %s 不存在", id)
	}
	dir := s.store.UserDataDir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("档案目录不可用: %w", err)
	}
	return s.openPath(dir)
}

// egressLine 按 id 取线路（nil = 不存在）。
func (s *profileStore) egressLine(id string) *EgressConfig {
	if s.cfg == nil {
		return nil
	}
	for i := range s.cfg.Egress {
		if s.cfg.Egress[i].ID == id {
			return &s.cfg.Egress[i]
		}
	}
	return nil
}

// redactCommand 命令行摘要（代理 URL 里的凭据换成 ***）。
//
// 为什么不给"原始命令行"：里面有代理口令。界面上够用的是
// "哪个浏览器、档案目录、有没有代理、调试端口"这四件事。
func redactCommand(cmd browserprofile.Cmd) string {
	parts := make([]string, 0, len(cmd.Args)+1)
	for _, a := range cmd.Args {
		if strings.HasPrefix(a, "--proxy-server=") {
			a = "--proxy-server=" + redactProxy(strings.TrimPrefix(a, "--proxy-server="))
		}
		parts = append(parts, a)
	}
	if cmd.URL != "" {
		parts = append(parts, cmd.URL)
	}
	return cmd.Browser + ": " + strings.Join(parts, " ")
}

// dirSize 目录体积（有上限，避免为显示一个数字遍历十万文件）。
func dirSize(dir string, limit int) int64 {
	var total int64
	seen := 0
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 权限/竞态：跳过，别让整个列表失败
		}
		if d.IsDir() {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil {
			total += info.Size()
		}
		seen++
		if seen >= limit {
			return fs.SkipAll
		}
		return nil
	})
	return total
}

func sortAccounts(list []admin.EgressAccountView) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].UID < list[j-1].UID; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}
