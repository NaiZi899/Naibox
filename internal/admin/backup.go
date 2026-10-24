// backup.go 管理台「备份」页：通道管理 / 立即备份 / 连通性测试 / 下载。
//
// # 为什么备份属核心而不是某个上游
//
// 它备的是**整个数据根**（config.json + 全部上游的 auths/ + data/），
// 不表达任何单个上游的身份。放在上游的 AdminExt 里等于让某个上游
// "拥有"别的上游的账号目录。
//
// # 与 desktop 托盘那条路径的关系
//
// 托盘「备份数据」与这里的「立即备份」走**同一个** Manager（见
// boot 的 backupStore）。改造前托盘自己调 Layout.Backup()，于是
// "托盘备份"和"管理台备份"会各自长出一套保留策略 —— 这种双份实现
// 在本项目已经吃过一次亏（见 pool 的注释）。
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// BackupChannelView 一条通道的展示形态。
//
// ⚠ 三个凭据字段都是**掩码**（只留尾 4 位）：页面没有任何理由持有明文，
// 要改就重填。PUT 时空串表示"沿用旧值"，所以掩码值不会被当成新密钥写回。
type BackupChannelView struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Enabled   bool   `json:"enabled"`
	Dir       string `json:"dir,omitempty"`
	Endpoint  string `json:"endpoint,omitempty"`
	Bucket    string `json:"bucket,omitempty"`
	Region    string `json:"region,omitempty"`
	Prefix    string `json:"prefix,omitempty"`
	RemoteDir string `json:"remote_dir,omitempty"`
	AccessKey string `json:"access_key,omitempty"`
	SecretKey string `json:"secret_key,omitempty"`
	Token     string `json:"token,omitempty"`
}

// BackupObjectView 通道里的一个包。
type BackupObjectView struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	ModTime string `json:"mod_time,omitempty"`
}

// BackupResultView 一次执行里单通道的结果。
type BackupResultView struct {
	Channel  string `json:"channel"`
	Kind     string `json:"kind"`
	Name     string `json:"name,omitempty"`
	Location string `json:"location,omitempty"`
	Bytes    int64  `json:"bytes,omitempty"`
	Error    string `json:"error,omitempty"`
	At       string `json:"at,omitempty"`
	OK       bool   `json:"ok"`
}

// BackupProbeView 「测试」按钮的回执（与 CustomProbeView 同形）。
type BackupProbeView struct {
	OK        bool   `json:"ok"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
	At        string `json:"at,omitempty"`
}

// BackupView 设置页一次读到的全部内容。
type BackupView struct {
	Enabled  bool                `json:"enabled"`
	Hours    []int               `json:"hours"`
	Keep     int                 `json:"keep"`
	Channels []BackupChannelView `json:"channels"`
	Last     []BackupResultView  `json:"last,omitempty"`
}

// BackupStore 宿主持久化契约（由 boot 实现并注入；nil = 未接线）。
//
// 与 SettingsStore / CustomUpstreamStore 同一模式：admin 只定义形状，
// 不碰 config.json，也不知道通道有哪几种。
type BackupStore interface {
	// Snapshot 读当前配置回声 + 上一次执行结果（**不得**打网络）。
	Snapshot(ctx context.Context) BackupView
	// Run 立即备份。ids 为空 = 全部已启用通道。
	Run(ctx context.Context, ids []string) ([]BackupResultView, error)
	// TestChannel 单通道连通性自检。
	TestChannel(ctx context.Context, id string) (BackupProbeView, error)
	// Objects 列某通道里的包（这一步会打网络）。
	Objects(ctx context.Context, id string) ([]BackupObjectView, error)
	// Open 取回一个包。
	Open(ctx context.Context, id, name string) (io.ReadCloser, int64, error)
	// Save 保存 backup 段并热生效。
	Save(patch json.RawMessage) (applied, needRestart []string, err error)
}

// backupProbeTimeout 连通性测试的上限。
//
// 15 分钟的上传超时不该套在「测试」按钮上：用户点下去是在等一个答案，
// 而不是在跑一次真实备份。
const backupProbeTimeout = 20 * time.Second

// backup GET /admin/backup
func (h *Handler) backup(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Backup == nil {
		writeError(w, http.StatusNotImplemented, "备份管理未接线")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), backupProbeTimeout)
	defer cancel()
	writeJSON(w, http.StatusOK, map[string]any{"backup": h.cfg.Backup.Snapshot(ctx)})
}

// backupUpdate PUT /admin/backup —— 整段保存 backup 配置。
func (h *Handler) backupUpdate(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Backup == nil {
		writeError(w, http.StatusNotImplemented, "备份管理未接线")
		return
	}
	var patch json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&patch); err != nil {
		writeError(w, http.StatusBadRequest, "备份配置解析失败: "+err.Error())
		return
	}
	applied, needRestart, err := h.cfg.Backup.Save(patch)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if applied == nil {
		applied = []string{}
	}
	if needRestart == nil {
		needRestart = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "applied": applied, "need_restart": needRestart,
	})
}

// backupRun POST /admin/backup/run {"channels":["local"]}
func (h *Handler) backupRun(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Backup == nil {
		writeError(w, http.StatusNotImplemented, "备份管理未接线")
		return
	}
	var body struct {
		Channels []string `json:"channels"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
			return
		}
	}
	// 备份本身可能跑很久（大包 + 慢通道），这里给一个宽松上限而不是继承
	// 请求的 ctx：用户在页面上点了「立即备份」就该等它做完，中途取消
	// 只会留下半包与一个看不懂的错误。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Minute)
	defer cancel()
	res, err := h.cfg.Backup.Run(ctx, body.Channels)
	if err != nil {
		if errors.Is(err, ErrBackupRunning) {
			writeError(w, http.StatusConflict, "已有一次备份正在进行")
			return
		}
		writeError(w, http.StatusInternalServerError, "备份失败: "+err.Error())
		return
	}
	if res == nil {
		res = []BackupResultView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": res})
}

// ErrBackupRunning "忙"的哨兵值：宿主的 Run 在撞到自己那把锁时返回它。
//
// # 为什么由 admin 声明而不是复用 backup.ErrRunning
//
// admin 不该 import internal/backup（它只需要知道"忙"是一个可映射成 409
// 的独立情形）。宿主的 Store 实现负责把 backup.ErrRunning 翻译成这个值。
var ErrBackupRunning = errors.New("backup: 已有一次备份正在进行")

// backupTestChannel POST /admin/backup/channels/test {"id":"s3"}
func (h *Handler) backupTestChannel(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Backup == nil {
		writeError(w, http.StatusNotImplemented, "备份管理未接线")
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	id := strings.TrimSpace(body.ID)
	if id == "" {
		writeError(w, http.StatusBadRequest, "缺少 id（要测哪条通道）")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), backupProbeTimeout)
	defer cancel()
	view, err := h.cfg.Backup.TestChannel(ctx, id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// backupObjects GET /admin/backup/objects?channel=local
func (h *Handler) backupObjects(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Backup == nil {
		writeError(w, http.StatusNotImplemented, "备份管理未接线")
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("channel"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "缺少 channel")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), backupProbeTimeout)
	defer cancel()
	list, err := h.cfg.Backup.Objects(ctx, id)
	if err != nil {
		writeError(w, http.StatusBadGateway, "列举备份包失败: "+err.Error())
		return
	}
	if list == nil {
		list = []BackupObjectView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"objects": list})
}

// backupDownload GET /admin/backup/download?channel=local&name=…
//
// 只做"取回"，**不做**自动恢复：热恢复会覆盖正在运行的 config.json 与
// auths/，那是不可回滚的破坏性动作。页面把包下载下来，停机替换由用户决定。
func (h *Handler) backupDownload(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Backup == nil {
		writeError(w, http.StatusNotImplemented, "备份管理未接线")
		return
	}
	q := r.URL.Query()
	id := strings.TrimSpace(q.Get("channel"))
	name := strings.TrimSpace(q.Get("name"))
	if id == "" || name == "" {
		writeError(w, http.StatusBadRequest, "缺少 channel 或 name")
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Minute)
	defer cancel()
	rc, size, err := h.cfg.Backup.Open(ctx, id, name)
	if err != nil {
		writeError(w, http.StatusBadGateway, "取回备份包失败: "+err.Error())
		return
	}
	defer rc.Close()

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`,
		url.PathEscape(filepathBase(name))))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if size > 0 {
		w.Header().Set("Content-Length", fmt.Sprint(size))
	}
	if _, err := io.Copy(w, rc); err != nil {
		// 头已经发出去了，只能断开 —— 客户端会看到截断而不是一个 200 的空文件。
		return
	}
}

// filepathBase 取文件名（避免为一个 base 名去 import path/filepath）。
func filepathBase(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
