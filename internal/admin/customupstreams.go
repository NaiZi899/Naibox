// customupstreams.go 控制台的「自定义上游」管理端点。
//
// # 为什么这三条端点属于核心，而不是某个 Provider 的 AdminExt
//
// 它们操作的是 **config.json 里的一段配置**（新增/删除端点、探测一个还没保存的
// 端点），作用域是"所有自定义端点"，不表达任何单个上游的身份。挂在某个
// Provider 上会让"没有端点时连管理页都进不去"——先有鸡还是先有蛋。
//
// 实际的读写与校验由宿主（internal/boot）实现并通过 CustomUpstreamStore 注入：
// 配置文件的格式、原子写、与启动期共用同一份 Normalize，都是宿主的事实。
package admin

import (
	"encoding/json"
	"io"
	"net/http"
)

// CustomProbeView 一次连通性探测的结果（展示用）。
//
// 字段与 custom.ProbeResult 同形但**刻意分开定义**：admin 不 import 具体
// 上游包（架构判据），宿主负责投影。这样加一个新字段时两边各自决定要不要下发。
type CustomProbeView struct {
	OK          bool   `json:"ok"`
	Status      int    `json:"status"`
	LatencyMS   int64  `json:"latency_ms"`
	ModelsCount int    `json:"models_count"`
	Error       string `json:"error,omitempty"`
	At          string `json:"at,omitempty"`
}

// CustomUpstreamView 列表里的一条端点。
//
// ⚠ 不含 api_key：控制台只展示尾 4 位。密钥的回显没有任何使用场景
// （要改就重填），下发它只会扩大泄漏面。
//
// 其余字段是**可编辑形态**的全量回显：管理页的行内编辑与「停用/删除」都要
// 提交完整 items（PUT 是整表覆盖），列表少给一个字段就会被一次保存悄悄抹掉。
// list 视图与编辑 spec 共用同一份形状，是这个页面最容易出错的地方。
type CustomUpstreamView struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	BaseURL     string          `json:"base_url"`
	Enabled     bool            `json:"enabled"`
	ModelsCount int             `json:"models_count"`
	Shards      int             `json:"shards"`
	KeyTail     string          `json:"key_tail"`
	Probe       CustomProbeView `json:"probe"`

	APIKeyEnv  string   `json:"api_key_env,omitempty"`
	Models     []string `json:"models,omitempty"`
	ChatPath   string   `json:"chat_path,omitempty"`
	ModelsPath string   `json:"models_path,omitempty"`
	AuthHeader string   `json:"auth_header,omitempty"`
	// AuthScheme 用指针保留三态：nil = 没写（默认 Bearer）；"" = 显式裸 key；
	// 其它 = 自定义方案。管理页保存时会原样回传，三种状态都不会被改掉。
	AuthScheme         *string           `json:"auth_scheme,omitempty"`
	Headers            map[string]string `json:"headers,omitempty"`
	TimeoutSeconds     int               `json:"timeout_seconds,omitempty"`
	InsecureSkipVerify bool              `json:"insecure_skip_verify,omitempty"`
}

// CustomUpstreamStore 自定义上游配置的读写契约（由 internal/boot 实现）。
//
// items / spec 都是**原始 JSON**：校验与默认值只在 custom.Normalize 一处实现，
// admin 不复制一份字段表 —— 两份判据迟早漂移（本项目已经为这类分叉付过代价）。
type CustomUpstreamStore interface {
	// ListCustomUpstreams 当前配置里的全部端点（含 disabled 的）。
	ListCustomUpstreams() ([]CustomUpstreamView, error)

	// ReplaceCustomUpstreams 用 items（JSON 数组）整体替换配置段。
	//
	// 校验失败时**不写盘**并返回错误；成功时返回需要重启才生效的字段名。
	ReplaceCustomUpstreams(items json.RawMessage) (needRestart []string, err error)

	// TestCustomUpstream 探测一条尚未保存的配置（不落盘）。
	TestCustomUpstream(spec json.RawMessage) (CustomProbeView, error)
}

// customUpstreams 列表。
func (h *Handler) customUpstreams(w http.ResponseWriter, r *http.Request) {
	if h.cfg.CustomUpstreams == nil {
		writeError(w, http.StatusNotImplemented, "自定义上游管理未接线（config.custom_upstreams 未启用）")
		return
	}
	list, err := h.cfg.CustomUpstreams.ListCustomUpstreams()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取自定义上游失败: "+err.Error())
		return
	}
	if list == nil {
		list = []CustomUpstreamView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list})
}

// customUpstreamsUpdate 整体替换配置段。
func (h *Handler) customUpstreamsUpdate(w http.ResponseWriter, r *http.Request) {
	if h.cfg.CustomUpstreams == nil {
		writeError(w, http.StatusNotImplemented, "自定义上游管理未接线（config.custom_upstreams 未启用）")
		return
	}
	var body struct {
		Items json.RawMessage `json:"items"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求解析失败: "+err.Error())
		return
	}
	if len(body.Items) == 0 || string(body.Items) == "null" {
		writeError(w, http.StatusBadRequest, "缺少 items 数组（要清空请传 []）")
		return
	}
	needRestart, err := h.cfg.CustomUpstreams.ReplaceCustomUpstreams(body.Items)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	list, _ := h.cfg.CustomUpstreams.ListCustomUpstreams()
	if list == nil {
		list = []CustomUpstreamView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"saved":        true,
		"need_restart": needRestart,
		"items":        list,
	})
}

// customUpstreamsTest 探测一条草稿配置（表单里的「测试连通性」）。
func (h *Handler) customUpstreamsTest(w http.ResponseWriter, r *http.Request) {
	if h.cfg.CustomUpstreams == nil {
		writeError(w, http.StatusNotImplemented, "自定义上游管理未接线（config.custom_upstreams 未启用）")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, "请求读取失败: "+err.Error())
		return
	}
	if len(raw) == 0 {
		writeError(w, http.StatusBadRequest, "请求体为空")
		return
	}
	probe, err := h.cfg.CustomUpstreams.TestCustomUpstream(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, probe)
}
