// profiles.go 管理台「浏览器档案」：列表 / 新建编辑 / 启动 / 删除 / 打开目录。
//
// # 为什么它属于核心
//
// 档案是"设备画像"（cookie + 出口 + 浏览器版本），与账号池、出口线路一样是
// 部署级事实；它不表达任何上游身份。因此不走上游的 AdminExt。
//
// # 与 mockup 的关系
//
// `dist/mockups/browser-profiles-ui.html` 是这块界面的设计稿，本文件是它的
// 后端一半。界面上的每个按钮都对应一条端点，签名以本文件为准确契约。
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// 档案端点的两条超时：普通操作（列/存/删/开目录）用短超时；
// 启动要真起一个浏览器进程，给更宽但仍有界的时间。
const (
	profileOpTimeout     = 15 * time.Second
	profileLaunchTimeout = 60 * time.Second
)

// BrowserProfileView 一条档案的展示形态。
type BrowserProfileView struct {
	ID string `json:"id"`
	// UID 绑定的账号（空 = 不绑定）。
	UID string `json:"uid,omitempty"`
	// AccountName 该 uid 当前在池里的展示名（账号改名后这里跟着变，不用手改档案）。
	AccountName string   `json:"account_name,omitempty"`
	Provider    string   `json:"provider,omitempty"`
	Label       string   `json:"label,omitempty"`
	EgressID    string   `json:"egress_id,omitempty"`
	StartURL    string   `json:"start_url,omitempty"`
	ExtraArgs   []string `json:"extra_args,omitempty"`
	CreatedAt   string   `json:"created_at,omitempty"`
	LastUsedAt  string   `json:"last_used_at,omitempty"`
	LastURL     string   `json:"last_url,omitempty"`
	DebugPort   int      `json:"last_debug_port,omitempty"`
	// DiskBytes 档案目录占用（0 = 还没启动过，目录不存在）。
	DiskBytes int64 `json:"disk_bytes"`
	// Dir 档案目录（只读展示；不提供"任意路径打开"，见 open-dir 端点）。
	Dir string `json:"dir,omitempty"`
}

// BrowserProbeView 本机浏览器探测结果（编辑器的"留空 = 自动探测"要有据可依）。
type BrowserProbeView struct {
	OK   bool   `json:"ok"`
	Name string `json:"name,omitempty"`
	Path string `json:"path,omitempty"`
	Err  string `json:"error,omitempty"`
}

// LaunchResult 一次启动的回执。
type LaunchResult struct {
	OK        bool   `json:"ok"`
	Browser   string `json:"browser,omitempty"`
	DebugPort int    `json:"debug_port,omitempty"`
	Command   string `json:"command,omitempty"`
	Error     string `json:"error,omitempty"`
}

// ProfilesStore 宿主持久化契约（由 boot 实现并注入；nil = 未接线，端点不注册）。
type ProfilesStore interface {
	// List 档案 + 本机浏览器探测结果 + 可绑定账号投影。
	List(ctx context.Context) ([]BrowserProfileView, BrowserProbeView, []EgressAccountView)
	// Upsert 新建或更新（保留 created_at；uid 绑定会同时记下账号名）。
	Upsert(ctx context.Context, patch json.RawMessage) (BrowserProfileView, error)
	// Delete 删档案（keepDir=true 只清索引）。
	Delete(ctx context.Context, id string, keepDir bool) error
	// Launch 启动档案里的浏览器。
	Launch(ctx context.Context, id, url string) (LaunchResult, error)
	// OpenDir 打开档案目录（服务端形态返回 ErrNotSupported）。
	OpenDir(ctx context.Context, id string) error
}

// ErrProfilesUnsupported 当前形态不支持该动作（例如服务端形态没有"打开目录"）。
var ErrProfilesUnsupported = errors.New("当前运行形态不支持该动作")

// browserProfiles GET /admin/browser-profiles
func (h *Handler) browserProfiles(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Profiles == nil {
		writeError(w, http.StatusNotImplemented, "浏览器档案未接线")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), profileOpTimeout)
	defer cancel()
	list, browser, accounts := h.cfg.Profiles.List(ctx)
	if list == nil {
		list = []BrowserProfileView{}
	}
	if accounts == nil {
		accounts = []EgressAccountView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"profiles": list, "browser": browser, "accounts": accounts,
	})
}

// browserProfileUpsert POST /admin/browser-profiles
func (h *Handler) browserProfileUpsert(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Profiles == nil {
		writeError(w, http.StatusNotImplemented, "浏览器档案未接线")
		return
	}
	var patch json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&patch); err != nil {
		writeError(w, http.StatusBadRequest, "档案解析失败: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), profileOpTimeout)
	defer cancel()
	view, err := h.cfg.Profiles.Upsert(ctx, patch)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "profile": view})
}

// browserProfileDelete DELETE /admin/browser-profiles/{id}?keep_dir=1
func (h *Handler) browserProfileDelete(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Profiles == nil {
		writeError(w, http.StatusNotImplemented, "浏览器档案未接线")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "缺少 id")
		return
	}
	keep := r.URL.Query().Get("keep_dir")
	keepDir := keep == "1" || strings.EqualFold(keep, "true")
	ctx, cancel := context.WithTimeout(r.Context(), profileOpTimeout)
	defer cancel()
	if err := h.cfg.Profiles.Delete(ctx, id, keepDir); err != nil {
		if errors.Is(err, ErrProfilesUnsupported) {
			writeError(w, http.StatusNotImplemented, err.Error())
			return
		}
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "keep_dir": keepDir})
}

// browserProfileLaunch POST /admin/browser-profiles/{id}/launch {"url":""}
//
// 启动是**有副作用**的动作（真起一个浏览器进程），所以给足超时而不是继承
// 请求的 ctx：用户点了就该看到结果（成功=浏览器名+端口，失败=原因）。
func (h *Handler) browserProfileLaunch(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Profiles == nil {
		writeError(w, http.StatusNotImplemented, "浏览器档案未接线")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "缺少 id")
		return
	}
	var body struct {
		URL string `json:"url"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
			return
		}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), profileLaunchTimeout)
	defer cancel()
	res, err := h.cfg.Profiles.Launch(ctx, id, strings.TrimSpace(body.URL))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// browserProfileOpenDir POST /admin/browser-profiles/{id}/open-dir
func (h *Handler) browserProfileOpenDir(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Profiles == nil {
		writeError(w, http.StatusNotImplemented, "浏览器档案未接线")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "缺少 id")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), profileOpTimeout)
	defer cancel()
	if err := h.cfg.Profiles.OpenDir(ctx, id); err != nil {
		if errors.Is(err, ErrProfilesUnsupported) {
			writeError(w, http.StatusNotImplemented, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
