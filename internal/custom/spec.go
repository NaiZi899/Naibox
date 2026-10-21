// Package custom 接入任意 OpenAI 兼容端点：模型目录、流式转发与连通性探测。
//
// # 这个包在整个解耦链里的位置
//
// 它是**用户自定义**的上游实现：每一条配置项注册成一个独立 Provider，
// 模型前缀就是它的 id（`my-openai/gpt-4o-mini`）。核心（gateway/pool/server）
// 对它零感知 —— 与 workbuddy / codearts 走的是同一条 Provider 契约。
//
// # 与内置上游的差别（刻意的）
//
//	凭证      配置里的静态 key，没有 OAuth、没有 refresh token
//	余额      不探测、不参与额度调度（不声明 CapQuotaProbe，不实现 QuotaExt）
//	生命周期  没有签到/成长/旅行，也没有 CredentialRefresher
//	并发      单端点默认一个合成账号；用 shards 分片抬高在途上限
//
// # 协议边界
//
// 只支持 OpenAI 兼容的 `POST <base>/chat/completions` 与 `GET <base>/models`。
// 其它协议（Anthropic /v1/messages 等）不在本包范围内 —— 它们的请求体、
// 鉴权头与流式事件形状都不同，硬塞进一个 Provider 会让两边都变浅。
package custom

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Spec 是 config.json 里的一条 `custom_upstreams` 配置。
//
// 字段全部可选地出现：默认值在 Normalize 里补，配置里只写与默认不同的部分。
type Spec struct {
	// ID 上游标识，同时是模型前缀。必须匹配 ^[a-z][a-z0-9-]*$。
	ID string `json:"id"`
	// Name 给人看的名字（导航分组标题）。空则用 ID。
	Name string `json:"name,omitempty"`
	// Enabled 缺省 true；显式 false 的条目不会被注册、不会入池。
	Enabled *bool `json:"enabled,omitempty"`
	// BaseURL 端点基址，只填到版本段（如 https://api.example.com/v1）。
	BaseURL string `json:"base_url"`
	// APIKey 静态密钥；与 APIKeyEnv 二选一。
	APIKey string `json:"api_key,omitempty"`
	// APIKeyEnv 从环境变量读密钥（部署时把 key 留在环境里，配置可分享）。
	APIKeyEnv string `json:"api_key_env,omitempty"`
	// ClearAPIKey 显式要求清空密钥（管理 API 的语义位，不落盘）。
	//
	// 列表接口不回显明文，所以"两个密钥字段都为空"在 PUT 里默认被理解为
	// "沿用旧密钥"。要让"清空"这个意图能被表达，必须有一个显式位 ——
	// 否则密钥泄露后唯一该做的动作（把 key 从 config.json 里删掉）在管理页上做不到。
	ClearAPIKey bool `json:"clear_api_key,omitempty"`
	// Models 静态模型列表；留空则运行时拉取 models_path。
	Models []string `json:"models,omitempty"`
	// ChatPath 对话路径，默认 /chat/completions。
	ChatPath string `json:"chat_path,omitempty"`
	// ModelsPath 模型目录路径，默认 /models。
	ModelsPath string `json:"models_path,omitempty"`
	// AuthHeader 鉴权头名，默认 Authorization。
	AuthHeader string `json:"auth_header,omitempty"`
	// AuthScheme 鉴权方案前缀。不写 = Bearer；写空串 = 直接把 key 作为头值；
	// 写别的值（如 Token）= "<值> <key>"。用指针是为了区分“没写”与“写了空串”。
	AuthScheme *string `json:"auth_scheme,omitempty"`
	// Headers 附加请求头（不含鉴权头，鉴权头由上面两个字段生成）。
	Headers map[string]string `json:"headers,omitempty"`
	// TimeoutSeconds 建连与响应头超时，默认 120；不约束流式响应体时长。
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// Shards 合成账号数（1–8），默认 1。每个账号受 pool.max_in_flight 限制。
	Shards int `json:"shards,omitempty"`
	// InsecureSkipVerify 跳过 TLS 证书校验（自签端点）。默认 false。
	InsecureSkipVerify bool `json:"insecure_skip_verify,omitempty"`
}

// Endpoint 是规范化之后的运行期形态。
//
// 与 Spec 的差别：默认值已补齐、密钥已解析、URL 已归一、模型列表已去重。
// 它会作为账号池里的不透明 secret 原样保存（`*Endpoint`），Provider 取值时
// 用类型断言读回 —— 与其它上游把自己的凭证结构塞进 secret 是同一手法。
type Endpoint struct {
	ID                 string
	Name               string
	BaseURL            string
	APIKey             string
	ChatPath           string
	ModelsPath         string
	AuthHeader         string
	AuthScheme         string
	Headers            map[string]string
	StaticModels       []string
	Timeout            time.Duration
	Shards             int
	InsecureSkipVerify bool
}

// resolvedKey 返回该端点实际使用的密钥（供 Provider 拼鉴权头）。
func (e Endpoint) resolvedKey() string { return e.APIKey }

// MaskKey 把密钥变成可展示的形态：只保留尾 4 位。
//
// 空串返回空串；短于 8 位的 key 一律显示 ****（尾 4 位不足以区分，
// 反而会暴露更多信息）。
func MaskKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) < 8 {
		return "****"
	}
	return "…" + key[len(key)-4:]
}

// reservedIDs 是内置上游的 ID。自定义条目与它们重名会让注册失败，
// 提前拦下并给一条能读懂的报错，比让 Registry 抛格式错更省时间。
var reservedIDs = map[string]bool{
	"workbuddy": true, "workbuddy-intl": true, "codearts": true,
	"loomy": true, "inkstone": true, "jiyuan": true, "trae": true,
	"cline": true, "raccoon": true, "lobsterai": true,
	"qoder": true, "qodercn": true,
}

// IsReservedID 报告 id 是否与内置上游冲突。
func IsReservedID(id string) bool { return reservedIDs[id] }

var idPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// headerNamePattern 是 RFC 7230 的 token 字符集。
var headerNamePattern = regexp.MustCompile("^[!#$%&'*+\\-.^_`|~0-9A-Za-z]+$")

// forbiddenHeaders 是不允许在 `headers` 里覆盖的请求头。
//
// Host / Content-Length / Transfer-Encoding 由 http.Client 自己管理，
// 手工改写会让请求形状与 Go 的假设不一致；鉴权头单独禁止是为了避免
// “两个地方都能设 key”这种歧义。
var forbiddenHeaders = map[string]bool{
	"host":              true,
	"content-length":    true,
	"transfer-encoding": true,
	"connection":        true,
	"proxy-connection":  true,
	"upgrade":           true,
	"authorization":     true, // 由 auth_header / auth_scheme 生成（名字可改）
	"content-type":      true, // 由 Chat 统一设置
}

// ConfigError 一条配置项的校验失败。Index 是它在数组里的位置（0 起），
// 让人不必在一长串 JSON 里猜是哪一条。
type ConfigError struct {
	Index int
	ID    string
	Field string
	Msg   string
}

func (e *ConfigError) Error() string {
	who := e.ID
	if who == "" {
		who = "(无 id)"
	}
	return fmt.Sprintf("custom_upstreams[%d]（%s）的 %s 非法：%s", e.Index, who, e.Field, e.Msg)
}

// IsEnabled 报告该条配置是否启用（缺省 true）。
func (s Spec) IsEnabled() bool { return s.Enabled == nil || *s.Enabled }

// NormalizeAll 校验并规范化全部配置项。
//
// 只处理**启用**的条目；显式停用的条目不参与校验（它不注册、不入池，
// 形态写错不影响启动 —— 这正是“临时停用一个端点”该有的语义）。
// 重复的启用 id 直接报错：两个端点抢同一个模型前缀时，后来者永远用不上。
func NormalizeAll(specs []Spec, getenv func(string) string) ([]Endpoint, error) {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	out := make([]Endpoint, 0, len(specs))
	// 先按 id 查重（**包含停用条目**）：同一份配置里出现两个同 id 的条目，
	// 启用其中一个之后"另一个是不是同一条"就没有答案了 —— 管理页会显示
	// 两行一样的 id，而其中一行永远不生效。宁可保存时就拒绝。
	seen := make(map[string]int, len(specs))
	for i, s := range specs {
		id := strings.TrimSpace(s.ID)
		if id == "" {
			continue // 空 id 交给启用条目的 Normalize 报错；停用条目允许空
		}
		if prev, dup := seen[id]; dup {
			return nil, &ConfigError{
				Index: i, ID: id, Field: "id",
				Msg: fmt.Sprintf("与 custom_upstreams[%d] 重复（模型前缀必须唯一）", prev),
			}
		}
		seen[id] = i
	}
	for i, s := range specs {
		if !s.IsEnabled() {
			continue
		}
		ep, err := Normalize(s, getenv)
		if err != nil {
			if ce, ok := err.(*ConfigError); ok {
				ce.Index = i
				return nil, ce
			}
			return nil, &ConfigError{Index: i, ID: s.ID, Field: "整体", Msg: err.Error()}
		}
		out = append(out, ep)
	}
	return out, nil
}

// Normalize 把一条配置补全默认值并校验。返回的错误总是 *ConfigError。
func Normalize(s Spec, getenv func(string) string) (Endpoint, error) {
	fail := func(field, msg string) (Endpoint, error) {
		return Endpoint{}, &ConfigError{ID: s.ID, Field: field, Msg: msg}
	}

	id := strings.TrimSpace(s.ID)
	if id == "" {
		return fail("id", "不能为空（它会成为模型前缀）")
	}
	if !idPattern.MatchString(id) {
		return fail("id", "要求 ^[a-z][a-z0-9-]*$（小写字母开头，只含字母、数字与连字符）")
	}
	if IsReservedID(id) {
		return fail("id", "与内置上游重名，换一个前缀")
	}

	name := strings.TrimSpace(s.Name)
	if name == "" {
		name = id
	}

	base, err := normalizeBaseURL(s.BaseURL)
	if err != nil {
		return fail("base_url", err.Error())
	}

	key := strings.TrimSpace(s.APIKey)
	envName := strings.TrimSpace(s.APIKeyEnv)
	if key != "" && envName != "" {
		return fail("api_key", "api_key 与 api_key_env 只能填一个")
	}
	if key == "" && envName == "" {
		return fail("api_key", "api_key 或 api_key_env 必须填一个")
	}
	if key == "" {
		key = strings.TrimSpace(getenv(envName))
		if key == "" {
			return fail("api_key_env", fmt.Sprintf("环境变量 %s 为空或未设置", envName))
		}
	}

	chatPath, err := normalizePath(s.ChatPath, "/chat/completions", "chat_path")
	if err != nil {
		return fail("chat_path", err.Error())
	}
	modelsPath, err := normalizePath(s.ModelsPath, "/models", "models_path")
	if err != nil {
		return fail("models_path", err.Error())
	}

	authHeader := strings.TrimSpace(s.AuthHeader)
	if authHeader == "" {
		authHeader = "Authorization"
	}
	if !headerNamePattern.MatchString(authHeader) {
		return fail("auth_header", "不是合法的 HTTP 头名")
	}
	authScheme := "Bearer"
	if s.AuthScheme != nil {
		authScheme = strings.TrimSpace(*s.AuthScheme)
	}
	if strings.ContainsAny(authScheme, "\r\n") {
		return fail("auth_scheme", "不允许换行")
	}

	headers, err := normalizeHeaders(s.Headers, authHeader)
	if err != nil {
		return fail("headers", err.Error())
	}

	timeoutSec := s.TimeoutSeconds
	if timeoutSec == 0 {
		timeoutSec = 120
	}
	if timeoutSec < 5 || timeoutSec > 600 {
		return fail("timeout_seconds", "取值范围 5–600")
	}

	shards := s.Shards
	if shards == 0 {
		shards = 1
	}
	if shards < 1 || shards > 8 {
		return fail("shards", "取值范围 1–8")
	}

	models := normalizeModels(s.Models)

	return Endpoint{
		ID:                 id,
		Name:               name,
		BaseURL:            base,
		APIKey:             key,
		ChatPath:           chatPath,
		ModelsPath:         modelsPath,
		AuthHeader:         authHeader,
		AuthScheme:         authScheme,
		Headers:            headers,
		StaticModels:       models,
		Timeout:            time.Duration(timeoutSec) * time.Second,
		Shards:             shards,
		InsecureSkipVerify: s.InsecureSkipVerify,
	}, nil
}

// normalizeBaseURL 归一基址：只认 http/https、必须有主机名、去尾斜杠。
//
// 拒绝 userinfo 是刻意的：`https://user:pass@host` 会把凭证写进 URL，
// 而 URL 会进日志。鉴权统一走 Header。
func normalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("不能为空")
	}
	// 尾斜杠先去掉，避免拼出 `//chat/completions`。
	raw = strings.TrimRight(raw, "/")
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("解析失败：%v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("只支持 http/https，当前是 %q", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("缺少主机名")
	}
	if u.User != nil {
		return "", fmt.Errorf("不要在 URL 里放用户名/密码，凭证请用 api_key")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("基址不要带查询串或片段")
	}
	if u.Path != "" && !strings.HasPrefix(u.Path, "/") {
		return "", fmt.Errorf("路径必须以 / 开头")
	}
	return raw, nil
}

// normalizePath 校验并补全端点路径。
func normalizePath(raw, def, field string) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" {
		return def, nil
	}
	if !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("必须以 / 开头")
	}
	if strings.ContainsAny(p, "?#") {
		return "", fmt.Errorf("不要带查询串或片段")
	}
	return p, nil
}

// normalizeHeaders 校验附加请求头。
//
// authHeader 单独传入：附加头里出现同一个名字会让“到底发哪个值”变成
// 实现细节，这里直接拒绝。
func normalizeHeaders(in map[string]string, authHeader string) (map[string]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		name := strings.TrimSpace(k)
		if !headerNamePattern.MatchString(name) {
			return nil, fmt.Errorf("头名 %q 非法", k)
		}
		low := strings.ToLower(name)
		if forbiddenHeaders[low] || low == strings.ToLower(authHeader) {
			return nil, fmt.Errorf("不允许覆盖请求头 %q", name)
		}
		if strings.ContainsAny(v, "\r\n") {
			return nil, fmt.Errorf("头 %q 的值里不允许换行", name)
		}
		out[name] = v
	}
	return out, nil
}

// normalizeModels 去空、去重，保持配置里的顺序。
func normalizeModels(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, m := range in {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}

// ModelsURL / ChatURL 由 Provider 使用；放在这里是为了让 URL 拼接只有一处实现。
func (e Endpoint) ChatURL() string   { return e.BaseURL + e.ChatPath }
func (e Endpoint) ModelsURL() string { return e.BaseURL + e.ModelsPath }

// applyHeaders 给请求装附加头与鉴权头（附加头先，鉴权头后 —— 后者优先）。
func (e Endpoint) applyHeaders(req *http.Request) {
	for k, v := range e.Headers {
		req.Header.Set(k, v)
	}
	val := e.APIKey
	if e.AuthScheme != "" {
		val = e.AuthScheme + " " + e.APIKey
	}
	req.Header.Set(e.AuthHeader, val)
}
