// settings_store.go 管理台设置页的宿主持久化实现。
//
// 两条设计约束：
//  1. 落盘用「读原 JSON → 改嵌套键 → 写回」，而不是把 Config 结构体整体序列化。
//     否则用户手工在 config.json 里留的注释键、未知键、字段顺序都会在一次保存后消失。
//  2. 只有明确能在运行时生效的字段才立刻应用；其余照常写盘，但如实回报
//     「需重启」，绝不假装已生效。
package boot

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NaiZi899/Naibox/internal/admin"
	"github.com/NaiZi899/Naibox/internal/checkinlog"
	"github.com/NaiZi899/Naibox/internal/prompt"
	"github.com/NaiZi899/Naibox/internal/scheduler"
	"github.com/NaiZi899/Naibox/internal/wire"
	"github.com/NaiZi899/Naibox/internal/workbuddy"
)

// settingsStore 实现 admin.SettingsStore。
// settingsStore 实现 admin.SettingsStore。
//
// # Task 3c 之后的形状
//
// 补丁是**原始 JSON**，因为里面混着两类键：
//
//	通用段   由本文件解释（签到/保活时点、上游超时、账号池限流…）
//	上游段   由上游自己解释（workbuddy 的成长/旅行开关，见 internal/workbuddy/settings.go）
//
// 核心不再认识 "growth_auto_open" 这类名字 —— 那 19 处上游概念
// （9 个字段 + 两张写死的清单）已随设置项一起搬进上游包。
//
// 上游段的落盘也在这里做（而不是让上游写 config.json）：
// 上游包不得碰配置文件格式，那是宿主的职责。
type settingsStore struct {
	path string
	cfg  *Config
	sch  *scheduler.Scheduler
	// up 上游的设置扩展（workbuddy 的成长/旅行开关）。
	// 为 nil 时上游段全为空 —— 与"该上游没有专属设置"一致。
	up  UpstreamSettings
	log *checkinlog.Log
	// setLogKeepDays 调整落盘请求日志的保留天数（由 main 注入，避免这里依赖 logbuf）。
	setLogKeepDays func(int)
	// gate 提示词降级状态机（只读展示用：prompt_status.degraded_until）。
	//
	// nil = 未注入：状态块照常下发，只是 degraded_until 恒为空串 ——
	// 与 prompt.Gate 的"调用方可能根本没被注入"约定一致（nil 的 Until() 返回零值）。
	gate *prompt.Gate
}

// 编译期断言：设置页的只读状态块走的是**可选接口**（类型断言），
// 方法名/签名一旦对不上，表现是"页面上少了一个块、没有任何报错"。
// 把这条放在编译期，改坏了当场红。
var _ admin.PromptStatusSource = (*settingsStore)(nil)

// UpstreamSettings 上游专属设置在**宿主**这一侧需要的形状。
//
// 只有一个方法：给出要落盘的键值。
//
// # 为什么没有 Apply
//
// 写入点只有一个 —— admin 的 handler 调 `SettingsExt.ApplySettings`。
// 宿主在这里再写一遍不仅冗余，还会让 applied 里出现重复键
// （前端会把它读成一件说不清的事）。
// 宿主在这一侧只做两件事：落盘（读当前值）与回显（Snapshot 读 config）。
type UpstreamSettings interface {
	// Values 本上游全部设置项的当前值（用于持久化与回显）。
	//
	// 必须是**全量**而不是补丁里那几个键：补丁是稀疏的，
	// 只写补丁里的键会把其它开关从 config.json 里抹掉。
	Values() map[string]any
}

func (s *settingsStore) Snapshot() admin.Settings {
	c := s.cfg
	checkinH := s.sch.SlotHours(workbuddy.SlotCheckin)
	keepaliveH := s.sch.SlotHours(workbuddy.SlotKeepalive)
	return admin.Settings{
		CheckinEnabled:   s.sch.SlotEnabled(workbuddy.SlotCheckin),
		KeepaliveEnabled: s.sch.SlotEnabled(workbuddy.SlotKeepalive),
		CheckinHours:     checkinH,
		KeepaliveHours:   keepaliveH,

		CheckinLogKeepDays: s.log.KeepDays(),
		RequestLogKeepDays: c.RequestLogKeepDays,

		SoftRate:              c.Cooldown.SoftRate,
		UpstreamTimeoutSec:    c.Upstream.TimeoutSeconds,
		UpstreamHeaderTimeout: c.Upstream.HeaderTimeoutSeconds,
		UpstreamIdleTimeout:   c.Upstream.IdleTimeoutSeconds,
		UpstreamStreamMax:     c.Upstream.StreamMaxSeconds,
		UpstreamFirstByte:     c.Upstream.StreamFirstByteSeconds,
		PoolMaxInFlight:       c.Pool.MaxInFlight,
		BreakerThreshold:      c.Pool.BreakerThreshold,
		BreakerCooldown:       c.Pool.BreakerCooldown,
		BreakerCooldownMax:    c.Pool.BreakerCooldownMax,
		SanitizeFingerprints:  c.Features.SanitizeBlacklistFingerprints,
		UpstashURL:            c.Upstash.URL,
		SessionStickyEnabled:  c.SessionSticky.Enabled,
		SessionStickyTTL:      c.SessionSticky.TTL,
		// 提示词段：回显 config.json 里的**意图**（下次启动生效的那个），
		// 与此刻实际生效的 PromptMode / PromptText 刻意分开 —— 见 promptModeSetting。
		PromptMode: s.promptModeSetting(),
		PromptFile: c.Prompt.File,
		// 拷贝一份：Snapshot 的返回值会被前端与测试持有，直接给切片等于把
		// 内部状态的别名交出去（调用方 append 会改到配置）。
		HiddenProviders: append([]string(nil), c.UI.HiddenProviders...),

		Listen:     c.Listen,
		AuthDir:    c.AuthDir,
		ConfigPath: s.path,
		APIKeySet:  c.APIKey != "",
	}
}

// promptModeSetting 回显与回灌用的提示词模式值。
//
// 取值顺序：config.json 的原始值（cfg.Prompt.Mode）→ 此刻生效的 cfg.PromptMode
// → 内置默认 custom。
//
// # 为什么不直接返回 cfg.Prompt.Mode
//
// 因为"空串"会立刻反噬：前端 collectSettings 把整份 settings 原样回传
// （Object.assign({}, savedSettings)），所以下发什么就会被 PUT 回来什么。
// 下发空串 → 回来空串 → 撞上保存校验的第一条（模式必须三选一）→ 用户改个
// 日志保留天数都存不下来，而错误信息说的是 prompt.mode，看上去毫不相干。
//
// 回落到"此刻生效的模式"而不是硬写 custom：环境变量覆盖过（NAIBOX_PROMPT_MODE）
// 或配置文件没写这一段时，用户在该行看到的就是真正在跑的那个值。
func (s *settingsStore) promptModeSetting() string {
	if m := strings.TrimSpace(s.cfg.Prompt.Mode); m != "" {
		return m
	}
	if m := strings.TrimSpace(s.cfg.PromptMode); m != "" {
		return m
	}
	return prompt.ModeCustom
}

// PromptStatus 实现 admin.PromptStatusSource：报**此刻生效**的提示词状态。
//
// 与 Snapshot 里的 prompt_mode / prompt_file 的区别见 admin.PromptStatus 的注释：
// 那两个是配置意图，这里读的是启动时加载的结果（cfg.PromptMode / cfg.PromptText）。
func (s *settingsStore) PromptStatus() admin.PromptStatus {
	c := s.cfg
	st := admin.PromptStatus{
		// 生效模式（启动归一 + 环境变量覆盖之后的那个），不是配置里的原始值。
		Mode:  c.PromptMode,
		Scope: "workbuddy",
		// 装载进内存的正文长度。passthrough 下这段正文不会出站使用，
		// 但它确实是"网关手里那份"，界面据此判断文件读进来了没有。
		Bytes: len(c.PromptText),
	}
	// 来源按装配时实际读的位置说：file 非空就是文件，否则内置默认。
	// 用配置值而不是比对正文内容：正文可能与内置默认**逐字相同**（用户把它存成文件了），
	// 那种情况下"看起来一样"不等于"同一个来源"。
	if f := strings.TrimSpace(c.Prompt.File); f != "" {
		st.Source = "file:" + f
	} else {
		st.Source = "builtin"
	}
	// 降级状态：Gate 未注入时留空串（nil 的 Until() 返回零值）。
	//
	// 额外判一次 Active()：Until() 在降级期走完后仍返回那个**过去的**时刻，
	// 直接用会让界面长期写着"降级到昨天 00:00"。字段语义是"未降级 = 空串"，
	// 所以以 Active() 为准，时间只用来展示。
	if g := s.gate; g != nil {
		if until := g.Until(); !until.IsZero() && g.Active() {
			st.DegradedUntil = until.Format(time.RFC3339)
		}
	}
	return st
}

// Apply 校验 → 写盘 → 应用运行时字段。
//
// 校验失败在写盘之前返回，磁盘保持原样。
//
// # 补丁的分发顺序
//
//  1. 解析出通用段（不认识的键被 encoding/json 静默忽略 —— 正是我们要的）；
//  2. 校验通用段 —— 失败即返回，**不写盘、不改内存**；
//  3. 落盘（通用段 + 上游段的当前值）；
//  4. 应用通用段的运行时值。
//
// 顺序的关键在第 2 步：校验必须在任何副作用之前。
//
// # 上游段的运行时不在这里应用
//
// 上游的设置值由 admin 的 handler 调 `SettingsExt.ApplySettings` 应用
// （那是唯一的写入点）。这里**只负责落盘**，因为它要读上游应用**之后**的当前值。
// 两边都写一次不仅冗余，还会让 applied 里出现重复键 ——
// 而前端会把重复的"已生效"读成一件说不清的事。
func (s *settingsStore) Apply(patch json.RawMessage) (applied, needRestart []string, err error) {
	p, err := s.parse(patch)
	if err != nil {
		return nil, nil, err
	}
	if err := validateSettings(p); err != nil {
		return nil, nil, err
	}

	// 落盘：通用段（本文件负责）+ 上游段的当前值。
	if err := s.persist(p); err != nil {
		return nil, nil, fmt.Errorf("写入 %s 失败: %w", s.path, err)
	}

	// 运行时生效（通用段）
	//
	// 槽位名在这里出现是**装配处**的职责：本文件是唯一同时认识"设置页的
	// 签到/保活字段"与"核心的槽位"的地方。核心只认识槽位，设置页只认识中文，
	// 翻译就发生在这一层。
	s.sch.SetSlotEnabled(workbuddy.SlotCheckin, p.CheckinEnabled)
	s.sch.SetSlotEnabled(workbuddy.SlotKeepalive, p.KeepaliveEnabled)
	if err := s.sch.SetSlotHours(workbuddy.SlotCheckin, p.CheckinHours); err != nil {
		return nil, nil, errors.New("签到时点超出范围（0-23）")
	}
	if err := s.sch.SetSlotHours(workbuddy.SlotKeepalive, p.KeepaliveHours); err != nil {
		return nil, nil, errors.New("保活时点超出范围（0-23）")
	}
	if p.CheckinLogKeepDays > 0 {
		s.log.SetKeepDays(p.CheckinLogKeepDays)
	}
	if p.RequestLogKeepDays > 0 && s.setLogKeepDays != nil {
		s.setLogKeepDays(p.RequestLogKeepDays)
	}
	applied = []string{
		"checkin_enabled", "keepalive_enabled", "checkin_hours", "keepalive_hours",
		"checkin_log_keep_days", "request_log_keep_days",
	}

	// 只写盘、需重启才生效（上游 client 的超时、账号池限流器、Redis 连接
	// 都是在启动时一次性装配的，运行中改不了；守卫轮间隔同理，
	// 由上游在 SettingField.RestartRequired 里声明）。
	needRestart = []string{
		"cooldown_soft_rate",
		"upstream_timeout_seconds", "upstream_header_timeout_seconds", "upstream_idle_timeout_seconds",
		"upstream_stream_max_seconds", "upstream_first_byte_seconds",
		"pool_max_in_flight", "pool_breaker_threshold", "pool_breaker_cooldown", "pool_breaker_cooldown_max",
		"sanitize_blacklist_fingerprints", "upstash_url",
		"session_sticky_enabled", "session_sticky_ttl",
		// 提示词：正文在启动时加载（cfg.normalize），运行中改不了。
		"prompt_mode", "prompt_file",
	}
	// 把新值同步进内存 cfg，让 Snapshot 显示的就是刚保存的值。
	s.applyToMemory(p)
	return applied, needRestart, nil
}

// parse 从原始补丁里取出**通用段**。
//
// 不认识的键被 encoding/json 静默忽略 —— 那正是"核心不认识上游字段"的实现方式：
// 它不必拒绝它们，只是不解释它们。
//
// # ⚠ 关键：先播当前值，再让补丁覆盖
//
// 早先的实现是 `var p admin.Settings; json.Unmarshal(patch, &p)` ——
// 补丁里**没出现的键**保持零值，而调用方随后把这个 p 整个写回 config.json。
// 于是前端只改一个开关（`{"checkin_enabled":false}`）就会把
// upstream.timeout_seconds / pool.max_in_flight / cooldown.soft_rate /
// session_sticky 等**未提交的配置全部清零**。
//
// 这是阶段 0 评审复现的 F1（真实存在，改造前就有）：
//
//	PUT {"checkin_enabled":false}  →
//	  upstream.timeout_seconds: 120 → 0
//	  pool.max_in_flight:         8 → 0
//	  cooldown.soft_rate:     "60s" → ""
//	  session_sticky.enabled:  true → false
//
// 修法：把**当前内存里的值**作为基底先填进 p，再让补丁覆盖它。
// 这样"补丁里没有的键"保持原值，语义从"整体替换"变成真正的"局部更新"。
//
// 为什么用"先播值再 Unmarshal"而不是 map[string]json.RawMessage 探测键存在性：
// 后者需要为每个字段写一遍存在性判断（20+ 个字段，且加字段必忘），
// 而"先播值"对**所有**字段自动正确，加字段零成本。
func (s *settingsStore) parse(patch json.RawMessage) (admin.Settings, error) {
	// 基底 = 当前生效值（含启动时从 config.json 读到的、以及上次保存的）
	p := s.currentSettings()
	if len(patch) == 0 {
		return p, nil
	}
	if err := json.Unmarshal(patch, &p); err != nil {
		return p, fmt.Errorf("设置解析失败: %w", err)
	}
	return p, nil
}

// currentSettings 把当前生效的通用段汇成一个 admin.Settings。
//
// 用途：作为 parse 的基底，保证"补丁里没提到的键"不被动过。
// 必须与 applyToMemory 的字段清单**一一对应**（一个是读、一个是写），
// 缺字段会导致该字段每次保存都被清零 —— 有测试守着这个对称性。
func (s *settingsStore) currentSettings() admin.Settings {
	c := s.cfg
	// log 在部分装配路径（测试、或未启用签到日志的部署）可能为 nil。
	// 不判空会 panic —— 这是我在写这条修复时实测踩到的。
	var checkinLogKeepDays int
	if s.log != nil {
		checkinLogKeepDays = s.log.KeepDays()
	}
	return admin.Settings{
		SoftRate:              c.Cooldown.SoftRate,
		UpstreamTimeoutSec:    c.Upstream.TimeoutSeconds,
		UpstreamHeaderTimeout: c.Upstream.HeaderTimeoutSeconds,
		UpstreamIdleTimeout:   c.Upstream.IdleTimeoutSeconds,
		UpstreamStreamMax:     c.Upstream.StreamMaxSeconds,
		UpstreamFirstByte:     c.Upstream.StreamFirstByteSeconds,
		PoolMaxInFlight:       c.Pool.MaxInFlight,
		BreakerThreshold:      c.Pool.BreakerThreshold,
		BreakerCooldown:       c.Pool.BreakerCooldown,
		BreakerCooldownMax:    c.Pool.BreakerCooldownMax,
		SanitizeFingerprints:  c.Features.SanitizeBlacklistFingerprints,
		UpstashURL:            c.Upstash.URL,
		SessionStickyEnabled:  c.SessionSticky.Enabled,
		SessionStickyTTL:      c.SessionSticky.TTL,
		PromptMode:            s.promptModeSetting(),
		PromptFile:            c.Prompt.File,
		HiddenProviders:       append([]string(nil), c.UI.HiddenProviders...),
		CheckinEnabled:        c.Schedule.CheckinEnabled,
		KeepaliveEnabled:      c.Schedule.KeepaliveEnabled,
		CheckinHours:          c.Schedule.CheckinHours,
		KeepaliveHours:        c.Schedule.KeepaliveHours,
		CheckinLogKeepDays:    checkinLogKeepDays,
		RequestLogKeepDays:    c.RequestLogKeepDays,
	}
}

// applyToMemory 把刚保存的通用段同步进内存 cfg，让 Snapshot 显示的就是新值。
//
// 上游段的对应字段由 upstreamSettingsAdapter.applyToMemory 负责 ——
// 那份清单属于上游，核心不认识它们（这正是 Task 3c 解耦掉的东西）。
func (s *settingsStore) applyToMemory(p admin.Settings) {
	c := s.cfg
	c.Cooldown.SoftRate = p.SoftRate
	c.Upstream.TimeoutSeconds = p.UpstreamTimeoutSec
	c.Upstream.HeaderTimeoutSeconds = p.UpstreamHeaderTimeout
	c.Upstream.IdleTimeoutSeconds = p.UpstreamIdleTimeout
	c.Upstream.StreamMaxSeconds = p.UpstreamStreamMax
	c.Upstream.StreamFirstByteSeconds = p.UpstreamFirstByte
	c.Pool.MaxInFlight = p.PoolMaxInFlight
	c.Pool.BreakerThreshold = p.BreakerThreshold
	c.Pool.BreakerCooldown = p.BreakerCooldown
	c.Pool.BreakerCooldownMax = p.BreakerCooldownMax
	c.Features.SanitizeBlacklistFingerprints = p.SanitizeFingerprints
	c.Upstash.URL = p.UpstashURL
	c.SessionSticky.Enabled = p.SessionStickyEnabled
	c.SessionSticky.TTL = p.SessionStickyTTL
	// 提示词段：**只改配置意图，不重载生效正文**。
	//
	// 语义是"保存后重启生效"，所以这里刻意**不**动 cfg.PromptMode / cfg.PromptText ——
	// 那两格是出站客户端此刻真正读的东西（启动时一次性装好）。
	//
	// # 为什么顺手重载是更差的选择
	//
	// 重载看着"更周到"，但它会让界面变成"已生效"，而出站是否真的换了词取决于
	// 还有没有第二处缓存（出站客户端、降级期的 Gate、在途请求）。一旦真有出入，
	// 排查时看到的两份事实互相矛盾 —— 比明确要求重启难查得多。
	// 保持"文件变了、实况没变"这个单一事实，用户重启一次就对齐了。
	c.Prompt.Mode = p.PromptMode
	c.Prompt.File = p.PromptFile
	// 展示过滤：同样拷贝，避免调用方后续修改切片时改到配置。
	c.UI.HiddenProviders = append([]string(nil), p.HiddenProviders...)
	c.Schedule.CheckinEnabled = p.CheckinEnabled
	c.Schedule.KeepaliveEnabled = p.KeepaliveEnabled
	c.Schedule.CheckinHours = p.CheckinHours
	c.Schedule.KeepaliveHours = p.KeepaliveHours
	c.Admin.CheckinLogKeepDays = p.CheckinLogKeepDays
	c.RequestLogKeepDays = p.RequestLogKeepDays
	// 上游段：由适配器按自己的键表同步（见 upstreamSettingsAdapter）。
	if s.up != nil {
		if a, ok := s.up.(interface{ applyToMemory(map[string]any) }); ok {
			a.applyToMemory(s.up.Values())
		}
	}
}

func validateSettings(p admin.Settings) error {
	checkHour := func(name string, hours []int) error {
		for _, h := range hours {
			if h < 0 || h > 23 {
				return fmt.Errorf("%s: %d 不是合法小时（0-23）", name, h)
			}
		}
		return nil
	}
	if err := checkHour("签到时点", p.CheckinHours); err != nil {
		return err
	}
	if err := checkHour("保活时点", p.KeepaliveHours); err != nil {
		return err
	}
	if p.SoftRate != "" {
		if _, err := time.ParseDuration(p.SoftRate); err != nil {
			return fmt.Errorf("429 冷却时长: %w", err)
		}
	}
	for name, v := range map[string]string{
		"熔断基础时长":   p.BreakerCooldown,
		"熔断退避封顶":   p.BreakerCooldownMax,
		"会话粘性 TTL": p.SessionStickyTTL,
	} {
		if v == "" {
			continue
		}
		if _, err := time.ParseDuration(v); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if p.UpstreamTimeoutSec < 0 || p.UpstreamHeaderTimeout < 0 || p.UpstreamIdleTimeout < 0 {
		return fmt.Errorf("上游超时不能为负")
	}
	// 流式两道闸门同样不许为负。0 是**合法**的"未设置态"（回落宿主默认），
	// 所以这里不能像别处那样把 0 也当非法值拒绝。
	if p.UpstreamStreamMax < 0 || p.UpstreamFirstByte < 0 {
		return fmt.Errorf("上游流式超时不能为负")
	}
	if p.PoolMaxInFlight < 0 || p.BreakerThreshold < 0 {
		return fmt.Errorf("账号池参数不能为负")
	}
	if p.CheckinLogKeepDays < 0 || p.RequestLogKeepDays < 0 {
		return fmt.Errorf("日志保留天数不能为负")
	}
	// 提示词段的三条保存时校验（1→2→3，任何一条失败都不会走到 persist）。
	return validatePromptSettings(p)
}

// validatePromptSettings 提示词段的保存时校验。
//
// # 为什么在保存时拦，而不是等下次重启
//
// 这三条对应的都是"重启才会炸"的错误：非法模式让进程起不来、文件不可读让
// 进程起不来、正文含指纹串让进程起不来。等重启再报，用户看到的是"服务挂了"，
// 而真因藏在一次几小时前的保存里 —— 中间这段正常运行的窗口会让人以为是别的东西。
// 在这里拦住，错误直接显示在点保存的那只手上，config.json 一个字节都没动。
//
// # 校验顺序
//
//  1. 模式三选一（空值也拒：空串漂进 config.json 会被启动归一当成 custom，
//     于是"我明明选了 append"变成一句无从对证的话）；
//  2. 文件可读且非空；
//  3. 最终正文不含脱敏指纹串（`passthrough` 除外，见下方注释）。
//
// 只在模式合法之后才去读文件：模式都不对的时候，"文件读不到"是个噪音错误。
func validatePromptSettings(p admin.Settings) error {
	switch p.PromptMode {
	case prompt.ModeCustom, prompt.ModePassthrough, prompt.ModeAppend:
		// 合法
	default:
		return fmt.Errorf("提示词模式 %q 非法：只接受 %q / %q / %q —— 已拒绝保存，config.json 未改动",
			p.PromptMode, prompt.ModeCustom, prompt.ModePassthrough, prompt.ModeAppend)
	}

	// 生效正文：file 非空读文件，否则内置默认。
	//
	// 直接复用 prompt.Load，而不是在这里再写一遍"读文件 + 判空"：
	// 保存期的判据必须与启动期**完全一致**，两份实现迟早漂移 ——
	// 而漂移的表现是"保存时通过了、重启起不来"，正是本条校验要消灭的东西。
	body, err := prompt.Load(p.PromptMode, p.PromptFile)
	if err != nil {
		return fmt.Errorf("提示词文件不可读或为空（%v）—— 已拒绝保存，config.json 未改动", err)
	}
	// 指纹这一条与**启动期同一口径**：passthrough 下不阻断。
	//
	// passthrough 根本不会把这段文本发出去（applyPrompt 原样返回，文件只被校验、
	// 不被使用），所以"被 sanitize 改写"这个后果在它身上不成立；更要紧的是它正是
	// NAIBOX_PROMPT_MODE 回滚通道的落点 —— 现场拿着一份含指纹的人格文件切过去止血，
	// 若保存也被拒，人就被卡在半路上。启动期同样只 warning（见 config.go 的 normalize），
	// 两处口径必须一致，否则"能跑但存不下来"会变成一个新的谜题。
	if p.PromptMode != prompt.ModePassthrough && wire.HasFingerprint(body) {
		return fmt.Errorf("提示词正文含脱敏指纹串，会在发出前被改写（界面看到的与实际发的不一致）—— 已拒绝保存")
	}
	return nil
}

// persist 把设置合并进 config.json，保留文件里已有的其它键。
func (s *settingsStore) persist(p admin.Settings) error {
	doc := map[string]any{}
	if raw, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(raw, &doc) // 解析失败就用空文档重写
	}

	set := func(section string, kv map[string]any) {
		sub, _ := doc[section].(map[string]any)
		if sub == nil {
			sub = map[string]any{}
		}
		for k, v := range kv {
			sub[k] = v
		}
		doc[section] = sub
	}
	// unset 从某个段里删掉一个键。用于把历史遗留的别名键收敛掉：
	// 只写新键而不删旧键的话，文件里会同时存在两个意思相同、值可能相反的键，
	// 下次读哪个就说不清了。
	unset := func(section, key string) {
		if sub, ok := doc[section].(map[string]any); ok {
			delete(sub, key)
		}
	}

	set("schedule", map[string]any{
		"checkin_enabled":   p.CheckinEnabled,
		"keepalive_enabled": p.KeepaliveEnabled,
		"checkin_hours":     p.CheckinHours,
		"keepalive_hours":   p.KeepaliveHours,
	})
	set("cooldown", map[string]any{"soft_rate": p.SoftRate})
	// 提示词段：键名与 config.json 的 prompt 段结构一致（prompt: {mode, file}）。
	set("prompt", map[string]any{
		"mode": p.PromptMode,
		"file": p.PromptFile,
	})
	set("upstream", map[string]any{
		"timeout_seconds":           p.UpstreamTimeoutSec,
		"header_timeout_seconds":    p.UpstreamHeaderTimeout,
		"idle_timeout_seconds":      p.UpstreamIdleTimeout,
		"stream_max_seconds":        p.UpstreamStreamMax,
		"stream_first_byte_seconds": p.UpstreamFirstByte,
	})
	set("pool", map[string]any{
		"max_in_flight":        p.PoolMaxInFlight,
		"breaker_threshold":    p.BreakerThreshold,
		"breaker_cooldown":     p.BreakerCooldown,
		"breaker_cooldown_max": p.BreakerCooldownMax,
	})
	set("features", map[string]any{"sanitize_blacklist_fingerprints": p.SanitizeFingerprints})
	set("upstash", map[string]any{"url": p.UpstashURL})
	set("session_sticky", map[string]any{
		"enabled": p.SessionStickyEnabled,
		"ttl":     p.SessionStickyTTL,
	})
	// 展示过滤（ui 段）。空列表写 [] 而不是省略键：省略会让"用户主动清空"
	// 与"从没设置过"在文件里长得一样，回溯问题少一条线索。
	set("ui", map[string]any{"hidden_providers": p.HiddenProviders})
	set("admin", map[string]any{
		"checkin_log_keep_days": p.CheckinLogKeepDays,
	})
	// 上游段：由适配器给出它自己的键值（核心不认识这些键名，也不再列它们）。
	// 这里用 set 合并而不是整体替换 —— 与上面各段同一语义，
	// 保留用户手工写在该段里的其它键。
	if s.up != nil {
		set("admin", s.up.Values())
	}
	// 别名键收敛：写入规范键 growth_auto_claim 后，删掉曾用过（且曾被静默忽略）的
	// growth_auto_claim_tasks，避免两个键并存给出互相矛盾的信号。
	unset("admin", "growth_auto_claim_tasks")
	doc["request_log_keep_days"] = p.RequestLogKeepDays

	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if dir := filepath.Dir(s.path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	return os.Rename(tmp, s.path)
}

// settingsStoreOption newSettingsStore 的可选装配参数。
//
// 用可变参数而不是加形参：调用点（boot.go）不必因为一个只读展示项而改签名，
// 也就不会出现"为了一个 UI 字段改装配入口"的连锁改动。
type settingsStoreOption func(*settingsStore)

// withPromptGate 注入提示词降级状态机（只读展示用：prompt_status.degraded_until）。
//
// 不传也能跑：未注入时状态块照常下发，degraded_until 恒为空串 ——
// 与 prompt.Gate 的 nil 语义一致，不假装"没降级"以外的任何事实。
func withPromptGate(g *prompt.Gate) settingsStoreOption {
	return func(s *settingsStore) { s.gate = g }
}

// newSettingsStore 组装宿主实现。
//
// up 是上游的设置扩展适配器（可为 nil —— 该上游没有专属设置）。
func newSettingsStore(path string, cfg *Config, sch *scheduler.Scheduler, up UpstreamSettings,
	log *checkinlog.Log, setLogKeepDays func(int), opts ...settingsStoreOption) admin.SettingsStore {
	s := &settingsStore{
		path:           path,
		cfg:            cfg,
		sch:            sch,
		up:             up,
		log:            log,
		setLogKeepDays: setLogKeepDays,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}
