// egress.go 管理台「出口线路」：看线路、探活、增删改、给账号绑线。
//
// # 为什么它属于核心
//
// 一条线路是"上游眼里的一个身份"（IP + ASN + 地区），所有上游共用同一套
// 线路表；它不表达任何上游身份，因此不该由某个上游的 AdminExt 提供。
// 与 /admin/backup、/admin/apikeys 同一取舍。
//
// # 为什么保存后**需要重启**（而备份配置不需要）
//
// 线路在装配期被编译成 `upstream.Client.Egress` 里的 Transport（一个 id
// 对应两个 http.Client，连接池也跟着建）。运行期改这张表要替换正在被
// 并发请求读取的 map —— 那是数据竞争，不是"热更新"。备份配置不一样：
// Manager 本身就是一把锁 + 一张可替换的表，天然可热换。
//
// 所以 Save 会返回 need_restart=["egress"]，并把话说明白：**探活不需要重启**
// （探活用的是现搭的 Transport，不依赖装配结果），所以用户能先验证再重启。
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// EgressProbeView 一条线路最近一次探活。
type EgressProbeView struct {
	OK        bool   `json:"ok"`
	IP        string `json:"ip,omitempty"`
	Country   string `json:"country,omitempty"`
	LatencyMS int64  `json:"latency_ms"`
	At        string `json:"at,omitempty"`
	Error     string `json:"error,omitempty"`
}

// EgressView 线路的展示形态。
//
// ⚠ proxy 是**脱敏后**的（凭据换成 ***）：管理台没有理由持有代理口令，
// 改它就重填。这与自定义上游只显示 key_tail 是同一条纪律。
type EgressView struct {
	ID          string           `json:"id"`
	Proxy       string           `json:"proxy"`
	Realm       string           `json:"realm,omitempty"`
	Region      string           `json:"region,omitempty"`
	MaxAccounts int              `json:"max_accounts"`
	ProbeURL    string           `json:"probe_url,omitempty"`
	Bound       int              `json:"bound"`
	Probe       *EgressProbeView `json:"probe,omitempty"`
}

// EgressAccountView 绑定面板用的账号投影（只下发展示字段）。
type EgressAccountView struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	Provider string `json:"provider,omitempty"`
	Channel  string `json:"channel,omitempty"`
	Egress   string `json:"egress,omitempty"`
}

// EgressStore 宿主持久化契约（由 boot 实现并注入；nil = 未接线）。
type EgressStore interface {
	// List 线路表 + 账号投影。**不打网络**（探活结果取缓存）。
	List(ctx context.Context) ([]EgressView, []EgressAccountView)
	// Probe 探活指定线路（ids 为空 = 全部）。force=true 忽略缓存。
	Probe(ctx context.Context, ids []string, force bool) ([]EgressView, error)
	// Save 保存 egress 整表（校验后写回 config.json，重启生效）。
	Save(patch json.RawMessage) (applied, needRestart []string, err error)
	// Bind 把一个账号绑到某条线路（空串 = 解绑）。
	Bind(uid, egressID string) error
}

// egressProbeTimeout 单条线路探活的上限。
const egressProbeTimeout = 15 * time.Second

// egress GET /admin/egress
func (h *Handler) egress(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Egress == nil {
		writeError(w, http.StatusNotImplemented, "出口线路管理未接线")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), egressProbeTimeout)
	defer cancel()
	lines, accounts := h.cfg.Egress.List(ctx)
	if lines == nil {
		lines = []EgressView{}
	}
	if accounts == nil {
		accounts = []EgressAccountView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines, "accounts": accounts})
}

// egressProbe POST /admin/egress/probe {"ids":["a"],"force":true}
func (h *Handler) egressProbe(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Egress == nil {
		writeError(w, http.StatusNotImplemented, "出口线路管理未接线")
		return
	}
	var body struct {
		IDs   []string `json:"ids"`
		Force bool     `json:"force"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
			return
		}
	}
	// 探活可能串行打几条线（每条最多 15s），给足预算而不是继承请求的 ctx：
	// 用户点了「全部探活」就该看到结果，中途取消只会留下一半线路的空状态。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()),
		egressProbeTimeout*time.Duration(max(1, len(body.IDs))))
	defer cancel()
	lines, err := h.cfg.Egress.Probe(ctx, body.IDs, body.Force)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if lines == nil {
		lines = []EgressView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
}

// egressUpdate PUT /admin/egress —— 整表保存。
func (h *Handler) egressUpdate(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Egress == nil {
		writeError(w, http.StatusNotImplemented, "出口线路管理未接线")
		return
	}
	var patch json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&patch); err != nil {
		writeError(w, http.StatusBadRequest, "线路配置解析失败: "+err.Error())
		return
	}
	applied, needRestart, err := h.cfg.Egress.Save(patch)
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

// accountEgress POST /admin/accounts/{uid}/egress {"egress_id":"cn-1"}
func (h *Handler) accountEgress(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Egress == nil {
		writeError(w, http.StatusNotImplemented, "出口线路管理未接线")
		return
	}
	uid := strings.TrimSpace(r.PathValue("uid"))
	if uid == "" {
		writeError(w, http.StatusBadRequest, "缺少 uid")
		return
	}
	var body struct {
		EgressID string `json:"egress_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	egressID := strings.TrimSpace(body.EgressID)
	if err := h.cfg.Egress.Bind(uid, egressID); err != nil {
		switch {
		case errors.Is(err, ErrEgressFull):
			writeError(w, http.StatusConflict, err.Error())
		case errors.Is(err, ErrEgressUnknown), errors.Is(err, ErrEgressRealmMismatch):
			writeError(w, http.StatusBadRequest, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "egress": egressID})
}

// 三类绑定失败的哨兵（**导出**：宿主的 Store 实现直接返回它们）。
//
// # 为什么由 admin 声明、boot 使用（而不是反过来）
//
// HTTP 状态码的映射发生在这一层（上限 → 409，不存在/地区不符 → 400）。
// 若哨兵定义在 boot，admin 就要 import boot —— 而 boot 已经 import admin
// （它实现这里的接口），立刻成环。所以哨兵住在这里，boot 负责在
// 绑定失败时把 boot 的判据错误翻成这三个之一（见 boot/egressstore.go）。
var (
	ErrEgressFull          = errors.New("线路上的账号数已达上限")
	ErrEgressUnknown       = errors.New("线路不存在")
	ErrEgressRealmMismatch = errors.New("渠道与线路地区不一致")
)
