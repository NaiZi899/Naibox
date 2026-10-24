// Package browserprofile 持久浏览器档案：一个账号一份独立 user-data-dir，
// 登录态可以留存并复用。
//
// # 为什么需要它（与 internal/browseropen 的分工）
//
// browseropen 解决的是 OAuth 加号的**一次性**问题：用无痕 + 临时 profile
// 打开授权页，确保"你授权的是新号而不是浏览器里已登录的老号"。它的不变式
// 是"恒无痕、恒临时"，48 小时后临时目录被清掉 —— 登录态因此**不会留存**。
//
// 于是每加一个号就要重新走一次 OAuth：要密码、要短信、要重新确认设备。
// 多账号场景下这是主要的人力成本，也是"刚加的号还没热就被判设备异常"的
// 结构性原因之一（同一台机器、每次都是全新的浏览器画像）。
//
// 本包提供另一半：**持久档案**。一个档案 = 一份独立的浏览器配置目录
// （cookie / localStorage / 扩展全在里面），加号时用它、之后续期与签到
// 也可以用它。
//
// # 明确不做什么（边界）
//
//   - **不做指纹伪造**：那要自建 Chromium（改 UA/Canvas/WebGL 的产物），
//     属于另一条成本线。这里只做"独立的真实浏览器画像"。
//   - **不起本地 CDP 服务**：自动化脚本那条路在本期不做，所以不引入
//     一个监听端口的攻击面。调试端口只写进档案供排障查看。
//   - **不代管进程**：启动后即撒手（与 browseropen 同一策略），
//     进程生命周期由用户/系统管。
package browserprofile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Profile 一份持久浏览器档案。
type Profile struct {
	// ID 档案标识（也是目录名）。建议用 `<provider>` 或 `<provider>-<label>`。
	ID string `json:"id"`
	// Provider 归属上游（可空 = 不绑定）。
	Provider string `json:"provider,omitempty"`
	// Label 展示名（可空）。
	Label string `json:"label,omitempty"`
	// UID 该档案绑定的账号（空 = 不绑定）。
	//
	// # 为什么要绑到具体账号（用户要求）
	//
	// 只绑上游还不够：同一个上游下常有多个号，而浏览器档案是**一份设备画像**。
	// 绑到 uid 之后，"这个号的登录态存在哪份档案里"是确定的 ——
	// 否则加号 A 用的是档案 X、续期 B 又落到档案 X，两个号的会话互相踩。
	//
	// 绑定时会一并把账号名（昵称）记进 Label（见 boot 侧的 Upsert），
	// 这样界面上不只有一串 uid。
	UID string `json:"uid,omitempty"`

	// StartURL 启动时打开的地址（空 = about:blank）。
	StartURL string `json:"start_url,omitempty"`
	// Fingerprint 这套档案的指纹参数（nil = 不启用）。
	//
	// 只有"认识 --fingerprint-* 的变体"才吃这些参数；原版 Chrome/Edge 会静默
	// 忽略，所以启动前会先探测（见 fingerprint.go 的 SupportsFingerprint）。
	Fingerprint *Fingerprint `json:"fingerprint,omitempty"`

	// BrowserPath 该档案固定用哪个浏览器可执行文件（空 = 用 login.browser 或自动探测）。
	//
	// # 为什么档案级而不是全局
	//
	// 指纹变体往往是单独下载的一份 chromium，而用户平时用的是 Edge/Chrome。
	// 全局只留一个路径的话，"指纹档案"与"普通档案"没法共存。
	BrowserPath string `json:"browser_path,omitempty"`

	// ExtraArgs 追加参数（排障用：--window-size / --lang 等）。
	//
	// ⚠ 不允许覆盖 --user-data-dir / --proxy-server：那两个由档案与线路决定，
	// 手写会让"档案"或"线路"静默失效（见 launch.go 的 Plan）。
	ExtraArgs []string `json:"extra_args,omitempty"`

	// EgressID 该档案走哪条出站线路（空 = 直连；线路不存在时启动会拒绝）。
	//
	// # 为什么它属于档案而不是账号
	//
	// 浏览器是"设备"，出口 IP 是设备所在网络。同一个账号在网关里走一条线、
	// 在浏览器里走另一条，风控看到的是两个矛盾的画像。
	EgressID string `json:"egress_id,omitempty"`
	// CreatedAt / LastUsedAt / LastURL 记账字段（排障用：这个档案上次拿去干什么了）。
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at,omitempty"`
	LastURL    string    `json:"last_url,omitempty"`
	// LastDebugPort 上一次启动时读到的 DevTools 端口（0 = 未知/未启动）。
	//
	// 只作排障线索：本包不提供 CDP 服务，端口仅在本机回环上，
	// 且浏览器退出后即失效。
	LastDebugPort int `json:"last_debug_port,omitempty"`
}

// UserDataDir 该档案的浏览器配置目录（绝对路径）。
//
// 目录由 Store 的 root 决定，不在档案里存绝对路径 —— 数据根会变
// （portable / -data-dir / 换机器恢复备份），存绝对路径会让恢复后的档案
// 全部指向不存在的目录。
func (s *Store) UserDataDir(p Profile) string {
	return filepath.Join(s.root, p.ID)
}

// Store 档案表（一个 JSON 文件 + 一目录一档案）。
//
// # 为什么表与目录分开
//
// 表（browser-profiles.json）是**索引**：可读、可手改、可进备份。
// 目录是浏览器自己的一堆二进制状态，不该被网关解析。
// 档案被删时目录一起删 —— 否则账号删了半年，磁盘上还留着 200 MB cookie。
type Store struct {
	root   string // 档案根目录（<dataDir>/browser）
	path   string // 索引文件（<dataDir>/browser-profiles.json）
	mu     sync.Mutex
	loaded bool
	items  map[string]Profile
}

// NewStore 构造。root 是数据根（例如 %APPDATA%\NaiBox）。
//
// 目录规划：索引在 <root>/browser-profiles.json，档案在 <root>/browser/<id>/。
// 放根下而不是 data/ 里，是为了让"哪些是浏览器垃圾"一眼可见（data/ 里是
// state.json 这类运行态，混进几百 MB cookie 会让人不敢删）。
func NewStore(root string) *Store {
	return &Store{root: filepath.Join(root, "browser"), path: filepath.Join(root, "browser-profiles.json")}
}

// load 读索引。文件不存在 = 空表（首次启动是正常状态，不报错）。
func (s *Store) load() error {
	if s.loaded {
		return nil
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.items = map[string]Profile{}
			s.loaded = true
			return nil
		}
		return fmt.Errorf("读取浏览器档案索引失败: %w", err)
	}
	var doc struct {
		Profiles []Profile `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		// 索引坏掉不能把整个网关拖死：备份文件后当空表继续（档案目录还在，
		// 用户仍能手工找回 cookie，只是要重新登记）。
		_ = os.WriteFile(s.path+".corrupt", raw, 0o600)
		s.items = map[string]Profile{}
		s.loaded = true
		return fmt.Errorf("浏览器档案索引损坏（已另存为 %s.corrupt）：%w", s.path, err)
	}
	s.items = make(map[string]Profile, len(doc.Profiles))
	for _, p := range doc.Profiles {
		s.items[p.ID] = p
	}
	s.loaded = true
	return nil
}

// save 原子写索引（tmp + rename），权限 600：里面只有路径与 URL，没有凭据，
// 但 LastURL 可能带 OAuth code —— 那等同于临时口令。
func (s *Store) save() error {
	list := make([]Profile, 0, len(s.items))
	for _, p := range s.items {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	raw, err := json.MarshalIndent(struct {
		Version  int       `json:"version"`
		Profiles []Profile `json:"profiles"`
	}{Version: 1, Profiles: list}, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".browser-profiles-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // rename 成功后删不到东西，幂等兜底
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, s.path)
}

// List 全部档案（按 ID 排序）。
func (s *Store) List() ([]Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil && len(s.items) == 0 {
		return nil, err
	}
	out := make([]Profile, 0, len(s.items))
	for _, p := range s.items {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Get 取一个档案。
func (s *Store) Get(id string) (Profile, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.load()
	p, ok := s.items[id]
	return p, ok
}

// Ensure 取或建一个档案（幂等）。用于"按上游自动建一份"的场景。
func (s *Store) Ensure(id, provider, label string) (Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil && len(s.items) == 0 {
		return Profile{}, err
	}
	if p, ok := s.items[id]; ok {
		return p, nil
	}
	p := Profile{ID: id, Provider: provider, Label: label, CreatedAt: time.Now()}
	s.items[id] = p
	if err := s.save(); err != nil {
		delete(s.items, id)
		return Profile{}, err
	}
	return p, nil
}

// Upsert 新增或整体更新一份档案（保留 CreatedAt）。
func (s *Store) Upsert(p Profile) (Profile, error) {
	if err := validateID(p.ID); err != nil {
		return Profile{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil && len(s.items) == 0 {
		return Profile{}, err
	}
	if old, ok := s.items[p.ID]; ok && !old.CreatedAt.IsZero() {
		p.CreatedAt = old.CreatedAt
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now()
	}
	prev, had := s.items[p.ID]
	s.items[p.ID] = p
	if err := s.save(); err != nil {
		if had {
			s.items[p.ID] = prev
		} else {
			delete(s.items, p.ID)
		}
		return Profile{}, err
	}
	return p, nil
}

// Delete 删档案**并删它的浏览器目录**。
//
// keepDir=true 时只从索引里移除（用户想手工抢救 cookie 时用）。
func (s *Store) Delete(id string, keepDir bool) error {
	if err := validateID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil && len(s.items) == 0 {
		return err
	}
	prev, ok := s.items[id]
	if !ok {
		return os.ErrNotExist
	}
	delete(s.items, id)
	if err := s.save(); err != nil {
		s.items[id] = prev
		return err
	}
	if !keepDir {
		// 目录删失败不算错误：索引已经干净了，残留目录是磁盘卫生问题，
		// 不该让"删档案"这个动作报错（用户会以为没删掉）。
		_ = os.RemoveAll(filepath.Join(s.root, id))
	}
	return nil
}

// Touch 记一次使用（启动成功后调用）。
func (s *Store) Touch(id, rawURL string, debugPort int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil && len(s.items) == 0 {
		return err
	}
	p, ok := s.items[id]
	if !ok {
		return os.ErrNotExist
	}
	p.LastUsedAt = time.Now()
	if rawURL != "" {
		p.LastURL = rawURL
	}
	if debugPort > 0 {
		p.LastDebugPort = debugPort
	}
	s.items[id] = p
	return s.save()
}

// validateID 档案 id 同时是**目录名**，所以必须挡住路径穿越与保留名。
func validateID(id string) error {
	if id == "" {
		return errors.New("档案 id 不能为空")
	}
	if len(id) > 64 {
		return errors.New("档案 id 过长（最多 64）")
	}
	for _, r := range id {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.'
		if !ok {
			return fmt.Errorf("档案 id 只能包含字母/数字/-/_/.（当前含 %q）", r)
		}
	}
	if id == "." || id == ".." {
		return errors.New("档案 id 不能是 . 或 ..")
	}
	return nil
}
