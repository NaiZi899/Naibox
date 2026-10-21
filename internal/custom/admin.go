// admin.go —— 本端点自己的两条管理路由。
//
// # 为什么要挂在 Provider 上，而不是写在核心的管理台里
//
// 控制台的核心部分不认识任何具体上游（见 gateway/extension.go 的判据）。
// 自定义端点需要两块面板信息：
//
//	GET /admin/custom/<id>          端点信息（地址、密钥尾号、探测结果）
//	GET /admin/custom/<id>/models   模型目录（下拉列表）
//
// 这两条都是**端点自己的事实**（地址、密钥、目录都只有它知道），
// 所以由它自报；核心只负责遍历 AdminRoutes 挂上去 —— 加第二个自定义端点时
// 前端与管理台零改动。
//
// # 这两条端点绝不回显完整密钥
//
// 密钥的回显没有任何使用场景（要改就重填），而面板 HTML/JSON 会被截图、
// 会被粘进 issue。所以只给尾 4 位（MaskKey），且**不包含**任何请求头快照。
package custom

import (
	"encoding/json"
	"net/http"

	"github.com/NaiZi899/Naibox/internal/gateway"
)

// 两条路由的固定片段。抽出来是为了让“路径只用一处拼”这件事可验证：
// 面板要按 id 拼同一个前缀，任何一处手写错了都只是 404。
const adminPathPrefix = "/admin/custom/"

// AdminRoutes 返回本端点的管理路由。
//
// Capability 都挂在 CapModels 上：这两条端点展示的就是“模型目录能不能用”，
// 声明一个不存在的能力位只会让前端多一个点不动的入口。
func (p *Provider) AdminRoutes() []gateway.AdminRoute {
	infoPath := adminPathPrefix + p.ep.ID
	return []gateway.AdminRoute{
		{
			Method:     http.MethodGet,
			Path:       infoPath,
			Handler:    p.adminInfo,
			Capability: gateway.CapModels,
			Title:      "端点信息",
			Hidden:     false,
		},
		{
			Method:     http.MethodGet,
			Path:       infoPath + "/models",
			Handler:    p.adminModels,
			Capability: gateway.CapModels,
			Title:      "模型目录",
			// Hidden：它是「端点信息」页里那个下拉框的数据源，
			// 不是一个人会主动访问的页面。挂一个标签页只会占位置。
			Hidden: true,
		},
	}
}

// adminInfo 端点的静态信息 + 一次探测结果。
//
// 探测走 p.Probe（缓存 15 秒）：面板刷新时会连点，每次都真打上游
// 既浪费也可能把一个有速率限制的端点打成 429（于是“配置没问题”被显示成“有问题”）。
func (p *Provider) adminInfo(w http.ResponseWriter, r *http.Request) {
	source, count := p.modelsSource()

	// 探测结果只用于展示；探测失败**不**让这条端点报错 ——
	// 面板要能显示“端点现在是断的”，而不是整页 500。
	probe := p.Probe(r.Context())
	if count == 0 {
		// 目录是现场拉的（models_source=live）时，唯一已知的条数就是探测读到的。
		count = probe.ModelsCount
	}

	writeJSON(w, http.StatusOK, struct {
		ID           string      `json:"id"`
		Name         string      `json:"name"`
		BaseURL      string      `json:"base_url"`
		ModelsSource string      `json:"models_source"`
		ModelsCount  int         `json:"models_count"`
		Shards       int         `json:"shards"`
		KeyTail      string      `json:"key_tail"`
		Probe        ProbeResult `json:"probe"`
	}{
		ID:           p.ep.ID,
		Name:         p.ep.Name,
		BaseURL:      p.ep.BaseURL,
		ModelsSource: source,
		ModelsCount:  count,
		Shards:       p.shardCount(),
		// 只给尾 4 位（见文件头）。
		KeyTail: MaskKey(p.ep.resolvedKey()),
		Probe:   probe,
	})
}

// adminModels 模型目录（下拉框数据源）。
//
// 失败时仍然回 200 + 空 items + error 文案：面板要展示“目录为空，原因是 X”，
// 用一个 500 让前端只能显示“加载失败”是不可接受的 —— 那会把
// “上游没有 /models” 与 “网关挂了” 混成一件事。
func (p *Provider) adminModels(w http.ResponseWriter, r *http.Request) {
	type item struct {
		// Title 带端点前缀："<id>/<model>"。面板展示的是**全局模型名**，
		// 前缀正是调用时要写的那个名字，去掉它用户就得自己拼。
		Title string `json:"title"`
		Model string `json:"model"`
	}

	models, err := p.Models(r.Context(), gateway.Credential{})
	if err != nil {
		writeJSON(w, http.StatusOK, struct {
			Items []item `json:"items"`
			Error string `json:"error,omitempty"`
		}{Items: []item{}, Error: err.Error()})
		return
	}

	items := make([]item, 0, len(models))
	for _, m := range models {
		items = append(items, item{Title: p.ep.ID + "/" + m.ID, Model: m.ID})
	}
	writeJSON(w, http.StatusOK, struct {
		Items []item `json:"items"`
	}{Items: items})
}

// modelsSource 报告目录从哪来：config / cache / live。
//
// 三者对用户的意义完全不同：
//
//	config 配置里写死了，不会再变（改了要重启）
//	cache  上一次拉到的（可能是几小时前的）
//	live   还没拉过，面板上的条数来自探测
//
// 只报“有几个模型”而不报来源，会让人以为“配置改了没生效”是 bug。
func (p *Provider) modelsSource() (string, int) {
	if len(p.ep.StaticModels) > 0 {
		return "config", len(p.ep.StaticModels)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.cache.list) > 0 {
		return "cache", len(p.cache.list)
	}
	return "live", 0
}

// writeJSON 写一个 JSON 响应。
//
// Content-Type 固定带 charset：面板用 fetch 读 JSON，中文名字在缺 charset 时
// 依赖浏览器的猜测 —— 猜错就是一堆乱码，而它看起来像“数据坏了”。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
