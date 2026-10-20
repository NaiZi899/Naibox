// settings.go 管理台「设置」页的数据契约。
//
// # Task 3c 之后的形状（这一层是解耦的关键）
//
// 改造前本文件是**一张写死的字段表**：`TravelAutoClaim`、`GrowthAutoAccept`、
// `GrowthAutoOpen` …… 九个字段，每个都是某个上游专属的开关名。
// 于是核心的管理台必须知道"workbuddy 有成长中心、有六个自动动作"，
// 而这正是判据 1 要禁止的：加第二个上游就要往同一个 struct 里继续塞字段。
//
// 现在拆成两段：
//
//	通用段（本文件硬编码）  签到/保活时点、日志保留天数、上游超时、账号池限流…
//	                        —— 每个上游都有的东西，改这些不需要动上游
//	上游专属段（透传）     由上游自己声明一张 SettingsSection，核心只负责
//	                        读快照、写补丁、原样序列化。**核心不认识任何字段名。**
//
// # 前端契约有没有变
//
// 没有。上游的 SettingField 用的是同一个 JSON key（如 "growth_auto_open"），
// 所以拼出来的 /admin/settings 响应体与改造前**逐字节同形**。
// 变的只是这些 key 从"admin 里的硬编码字段"变成"workbuddy 声明的数据"。
package admin

import (
	"encoding/json"
	"log"
	"net/http"
)

// Settings 设置页可读写的全部字段。
//
// 分两类：
//   - 运行时生效（改完立即影响行为，无需重启）
//   - 需重启（已写入 config.json，但当前进程不重新读取）
//
// 哪一类由宿主的 Apply 决定并回传，UI 据此提示用户。
//
// # Upstream 字段
//
// 上面那些是**通用**设置（任何上游都有）。上游专属的开关不走这里，
// 而是由上游的 SettingsProvider 提供（见 SettingsExt），在 handler 层
// 与通用段合并成一个 JSON 对象下发 —— 前端看不出两者来自不同的地方。
type Settings struct {
	// ---- 调度（运行时生效）----
	CheckinEnabled   bool  `json:"checkin_enabled"`
	KeepaliveEnabled bool  `json:"keepalive_enabled"`
	CheckinHours     []int `json:"checkin_hours"`
	KeepaliveHours   []int `json:"keepalive_hours"`

	// ---- 历史与日志（运行时生效）----
	CheckinLogKeepDays int `json:"checkin_log_keep_days"`
	RequestLogKeepDays int `json:"request_log_keep_days"`

	// ---- 以下需重启（保存进 config.json，当前进程不重读）----
	SoftRate              string `json:"cooldown_soft_rate"`
	UpstreamTimeoutSec    int    `json:"upstream_timeout_seconds"`
	UpstreamHeaderTimeout int    `json:"upstream_header_timeout_seconds"`
	UpstreamIdleTimeout   int    `json:"upstream_idle_timeout_seconds"`
	// UpstreamStreamMax 聊天**流式**请求的总时长上限（秒）；0 = 走宿主默认（15 分钟）。
	//
	// 与上面三项同一条链路（Snapshot / currentSettings / applyToMemory /
	// persist / validate 五处都要登记，见 cmd/server/settings_store.go）。
	UpstreamStreamMax int `json:"upstream_stream_max_seconds"`
	// UpstreamFirstByte 聊天流式"建连后多久必须见到首个内容帧"（秒）；
	// 0 = 走宿主默认（120s）。与 HeaderTimeout 管的阶段不同，见 config.go 的注释。
	UpstreamFirstByte    int    `json:"upstream_first_byte_seconds"`
	PoolMaxInFlight      int    `json:"pool_max_in_flight"`
	BreakerThreshold     int    `json:"pool_breaker_threshold"`
	BreakerCooldown      string `json:"pool_breaker_cooldown"`
	BreakerCooldownMax   string `json:"pool_breaker_cooldown_max"`
	SanitizeFingerprints bool   `json:"sanitize_blacklist_fingerprints"`
	UpstashURL           string `json:"upstash_url"`
	SessionStickyEnabled bool   `json:"session_sticky_enabled"`
	SessionStickyTTL     string `json:"session_sticky_ttl"`
	// PromptMode / PromptFile 系统提示词的**配置意图**（config.json 的 prompt 段）。
	//
	// 与上面一批"需重启"的字段同一个理由：正文在启动时由 cfg.normalize()
	// 一次性读进 cfg.PromptText，出站客户端拿的是那份快照。运行中改这里
	// 只改文件与回显，不会让在途进程换上新的词 —— 所以回报 need_restart，
	// 而不是假装已生效（那会变成"界面说生效了、实际没生效"）。
	PromptMode string `json:"prompt_mode"`
	PromptFile string `json:"prompt_file"`

	// ---- 控制台展示（运行时生效，纯展示语义）----

	// HiddenProviders 账号池与左侧导航里不显示的上游 id。
	//
	// 与 enabled 的区别是硬边界：隐藏只影响界面，账号仍在池里参与调度。
	// 之所以做成配置项而不是浏览器本地存储，见 config.go 里 UI 段的说明。
	HiddenProviders []string `json:"hidden_providers"`

	// ---- 只读展示 ----
	Listen     string `json:"listen"`
	AuthDir    string `json:"auth_dir"`
	ConfigPath string `json:"config_path"`
	RedisMode  string `json:"redis_mode"`
	APIKeySet  bool   `json:"api_key_set"`
}

// PromptStatus 当前**生效**的系统提示词状态（只读，PUT 补丁里不接受它）。
//
// # 为什么与 settings 里的 prompt_mode / prompt_file 并存
//
// 那两个是"下次启动会用哪个"（config.json 里的意图），这里是"此刻正在用哪个"
// （启动时算出来的结果）。保存之后到下次重启之前，两者必然不一致 ——
// 这正是需要同时展示的原因：只显示意图，用户会以为保存即生效；
// 只显示实况，用户不知道自己刚才那次保存到底写没写进文件。
type PromptStatus struct {
	// Mode 此刻生效的模式。
	//
	// ⚠ 取自宿主的 PromptMode（启动时归一 + 环境变量覆盖之后的那个），
	// **不是** Config.Prompt.Mode：后者只是 config.json 里的原始值，
	// 保存后到下次启动前都不代表实况。两者混淆的后果是界面显示"已切到 append"
	// 而出站仍在按 custom 走 —— 一个只能靠读日志才能发现的分歧。
	Mode string `json:"mode"`
	// Source 生效正文的来源："builtin"（内置默认）或 "file:<路径>"。
	Source string `json:"source"`
	// Bytes 生效提示词正文的字节数。
	Bytes int `json:"bytes"`
	// DegradedUntil 降级期结束时刻（RFC3339）；未降级为空串。
	DegradedUntil string `json:"degraded_until"`
	// Scope 这份提示词的作用范围，固定 "workbuddy"。
	Scope string `json:"scope"`
}

// PromptStatusSource 可选的只读能力：把"此刻生效的提示词状态"报给设置页。
//
// # 为什么不塞进 SettingsStore
//
// SettingsStore 描述的是**可读写的字段表**（Snapshot/Apply 都是字段），
// 而状态块的来源是宿主的内存态（已加载的正文、降级状态机），
// 两者的装配时机与生命周期都不同。做成可选接口，没实现的宿主照样能跑 ——
// 设置页少一个只读块，而不是整个设置页不可用。
type PromptStatusSource interface {
	PromptStatus() PromptStatus
}

// SettingsStore 由宿主（cmd/server）实现：提供当前生效设置、校验并持久化补丁。
//
// # 为什么 Apply 收的是原始 JSON
//
// 补丁里既有通用字段、也有上游专属字段。如果 Apply 收 *Settings，
// 上游字段就会在 admin 这一层被丢掉（它们不在 Settings 里）——
// 表现为"保存了但没生效，也没有报错"。收原始 JSON 则完整保留，
// 由宿主按 key 分发给通用段与各上游的 SettingsExtension。
type SettingsStore interface {
	// Snapshot 返回当前进程生效的**通用**设置。
	Snapshot() Settings
	// Apply 校验并持久化整份补丁；返回运行时已生效的字段名、需要重启才生效的字段名。
	// 校验失败必须返回 error，且不得写入磁盘。
	Apply(patch json.RawMessage) (applied []string, needRestart []string, err error)
}

// SettingField 上游声明的一个设置项（值 + 分类）。
//
// 为什么值用 any：上游的开关可能是 bool、可能是数字（守卫轮间隔秒数）、
// 也可能是字符串。核心不解释它，只负责原样序列化与回写 —— 于是
// "上游有几种类型的设置"不会渗透进核心的类型系统。
type SettingField struct {
	// Key 是**对外的 JSON 字段名**（如 "growth_auto_open"）。
	// 它同时是前端读写的键、也是持久化到 config.json 的键，必须稳定。
	Key string
	// Value 当前值。
	Value any
	// RestartRequired 该字段是否"改了要重启才生效"。
	// 由上游回答，因为只有它知道自己的开关是在哪个时机被装配的。
	RestartRequired bool
}

// SettingsExt 上游自己的设置项（可选实现）。
//
// 核心只做三件事：把 Fields 合并进 /admin/settings 的响应、
// 把 PUT 补丁里属于自己 Key 的部分交给 Apply、把读写结果拼成 applied/need_restart。
// **核心不认识任何具体 Key。**
//
// 与 AdminExt / JobExt / LoginFlow 同一模式：可选实现 + 类型断言发现。
// 一个没有任何专属设置的纯 API Key 上游可以不实现它。
type SettingsExt interface {
	// Fields 当前生效的上游专属设置项。顺序稳定（前端按序渲染）。
	Fields() []SettingField
	// ApplySettings 应用补丁中属于本上游的键。patch 是**完整补丁**，
	// 上游自己挑出它认得的键（不认得的忽略），返回实际生效的键名。
	//
	// 为什么给整份补丁而不是预先切好的一份：切分需要核心知道 Key 的归属，
	// 那就等于核心认识上游字段了 —— 与这个接口存在的理由相反。
	ApplySettings(patch json.RawMessage) (applied []string, err error)
}

func (h *Handler) settings(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Settings == nil {
		writeError(w, http.StatusNotImplemented, "设置存储未接线")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"settings":       h.settingsView(),
		"runtime_fields": h.runtimeFields(),
		"restart_fields": h.restartFields(),
		"request_log":    h.requestLogStats(),
	})
}

func (h *Handler) settingsUpdate(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Settings == nil {
		writeError(w, http.StatusNotImplemented, "设置存储未接线")
		return
	}
	var patch json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeError(w, http.StatusBadRequest, "设置解析失败: "+err.Error())
		return
	}
	applied, needRestart, err := h.cfg.Settings.Apply(patch)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// 上游专属字段：各自从完整补丁里挑出自己认得的键。
	for _, ext := range h.settingsExts() {
		upApplied, uerr := ext.ApplySettings(patch)
		if uerr != nil {
			// 上游拒绝时整次保存**已经落盘**（SettingsStore.Apply 已返回）。
			// 这里如实报告而不是回滚：回滚需要跨上游事务，代价远大于收益，
			// 而错误信息足够让人工修正配置。
			log.Printf("admin: 上游设置应用失败: %v", uerr)
			writeError(w, http.StatusBadRequest, uerr.Error())
			return
		}
		applied = append(applied, upApplied...)
	}
	log.Printf("admin: 设置已保存，运行时生效=%v 需重启=%v", applied, needRestart)
	writeJSON(w, http.StatusOK, map[string]any{
		"settings":     h.settingsView(),
		"applied":      applied,
		"need_restart": needRestart,
		"request_log":  h.requestLogStats(),
	})
}

// settingsView 把通用设置与各上游的设置项合并成一个 JSON 对象。
//
// 合并的实现在这里而不是让前端拼接：前端只认一张扁平键表，
// 它不需要知道哪些键来自上游 —— 那正是"能力由后端下发"的延续。
func (h *Handler) settingsView() map[string]any {
	out := map[string]any{}
	raw, err := json.Marshal(h.cfg.Settings.Snapshot())
	if err != nil {
		return out
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out
	}
	for _, ext := range h.settingsExts() {
		for _, f := range ext.Fields() {
			if f.Key == "" {
				continue
			}
			out[f.Key] = f.Value
		}
	}
	// 只读提示词状态块（JSON 键固定 prompt_status）。
	//
	// 只出现在下发方向：PUT 补丁里即使带上它也会被当成未知键忽略 ——
	// admin.Settings 里没有这个字段，encoding/json 静默跳过，不会被误写进 config.json。
	if ps, ok := h.cfg.Settings.(PromptStatusSource); ok {
		out["prompt_status"] = ps.PromptStatus()
	}
	return out
}

// runtimeFields 运行时生效的字段名清单（通用段）。
//
// 上游专属字段由各自在 SettingField.RestartRequired 里回答，
// 所以这里不再出现 "growth_auto_open" 这类名字。
var runtimeFields = []string{
	"checkin_enabled",
	"keepalive_enabled",
	"checkin_hours",
	"keepalive_hours",
	"checkin_log_keep_days",
	"request_log_keep_days",
}

// restartRequiredFields 需重启的字段名清单（通用段）。
var restartRequiredFields = []string{
	"cooldown_soft_rate",
	"upstream_timeout_seconds",
	"upstream_header_timeout_seconds",
	"upstream_idle_timeout_seconds",
	"upstream_stream_max_seconds",
	"upstream_first_byte_seconds",
	"pool_max_in_flight",
	"pool_breaker_threshold",
	"pool_breaker_cooldown",
	"pool_breaker_cooldown_max",
	"sanitize_blacklist_fingerprints",
	"upstash_url",
	"session_sticky_enabled",
	"session_sticky_ttl",
	// 提示词：模式与文件都要重启才生效（正文在启动时加载，见 PromptMode 的注释）。
	"prompt_mode",
	"prompt_file",
}

// runtimeFields 返回通用段 + 上游段里"运行时生效"的键。
//
// 返回 []string 而不是直接暴露包级切片：调用方（handler）会把结果序列化出去，
// 让它拿到一个可变的包级切片迟早会被某个调用方 append 坏。
func (h *Handler) runtimeFields() []string {
	out := make([]string, 0, len(runtimeFields))
	out = append(out, runtimeFields...)
	for _, ext := range h.settingsExts() {
		for _, f := range ext.Fields() {
			if !f.RestartRequired && f.Key != "" {
				out = append(out, f.Key)
			}
		}
	}
	return out
}

// restartFields 返回通用段 + 上游段里"需重启"的键。
func (h *Handler) restartFields() []string {
	out := make([]string, 0, len(restartRequiredFields))
	out = append(out, restartRequiredFields...)
	for _, ext := range h.settingsExts() {
		for _, f := range ext.Fields() {
			if f.RestartRequired && f.Key != "" {
				out = append(out, f.Key)
			}
		}
	}
	return out
}

// settingsExts 返回已注入的上游设置扩展。
//
// 它们由 cmd/server 传进来，而不是从 Registry 里类型断言出来 ——
// 理由见 Config.SettingsExts 的注释（上游的 SettingField 是各自声明的类型，
// 方法集精确匹配导致断言必然静默失败）。
func (h *Handler) settingsExts() []SettingsExt {
	return h.cfg.SettingsExts
}

// requestLogStats 落盘日志概况（文件路径、条数、体积、保留天数）。
func (h *Handler) requestLogStats() map[string]any {
	if h.cfg.Ring == nil {
		return map[string]any{"enabled": false}
	}
	if s := h.cfg.Ring.Sink(); s != nil {
		return s.Stats()
	}
	return map[string]any{"enabled": false}
}
