// models.go —— 模型目录：静态优先，其次拉 /models，结果缓存 10 分钟。
//
// # 为什么“静态优先”不是优化，而是产品判断
//
// 有些端点（自建 vLLM、代理转发、只开放少数模型的网关）根本没有可用的
// /models，或者返回一份几百个“你不能用”的目录。配置里写死 `models`
// 的三条好处：
//
//  1. 端点不可达时 /v1/models 仍然可用（客户端不会以为整个上游没了）
//  2. 启动期不发网络请求（离线部署也能起来）
//  3. 契约测试不需要网络 —— 那是“加了新上游核心零改动”这条判据的验证前提
//
// # 为什么缓存要拿锁串行化
//
// /v1/models 会被出口层在所有端点间合并调用，客户端重连时可能是并发的。
// 不串行化的话 N 个并发请求会同时打上游 N 次 —— 对一个默认只有 1 个账号的
// 端点来说，那本身就是一次限流事故。代价是拉取期间其它调用等锁（最多 10 秒），
// 换来的是“稳态下一次请求”。
package custom

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/NaiZi899/Naibox/internal/gateway"
)

// modelsBodyLimit /models 响应体的上限。目录是几百条 id 级别的小 JSON，
// 4 MB 足够；设上限是为了不因为一个畸形响应把内存打满。
const modelsBodyLimit = 4 << 20

// Models 返回模型目录。
func (p *Provider) Models(ctx context.Context, cred gateway.Credential) ([]gateway.ModelInfo, error) {
	ep := p.endpointOf(cred)

	// 配置即事实：一次网络都不发（见文件头）。
	if len(ep.StaticModels) > 0 {
		return modelsFromIDs(ep.StaticModels), nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// 只把**非空**列表算作有效缓存：上游返回 {"data":[]} 时缓存一份空目录
	// 会让 /v1/models 空 10 分钟，而它可能只是那一刻在重启 ——
	// 空目录不值得缓存，多打一次远比“模型消失”轻。
	if len(p.cache.list) > 0 && time.Since(p.cache.at) < modelsTTL {
		return cloneModels(p.cache.list), nil
	}

	list, err := p.fetchModels(ctx, ep)
	if err != nil {
		// 有旧目录就先用旧目录：目录是“能选哪些模型”的唯一来源，
		// 一次网络抖动不该让它整段消失。日志只记一次（见 modelsCache.errLogged）。
		if len(p.cache.list) > 0 {
			if !p.cache.errLogged {
				p.cache.errLogged = true
				p.logf("custom: %s 拉取模型目录失败，沿用 %s 的缓存（%d 条）：%v",
					ep.ID, p.cache.at.Format(time.RFC3339), len(p.cache.list), err)
			}
			return cloneModels(p.cache.list), nil
		}
		return nil, err
	}

	p.cache.list = list
	p.cache.at = time.Now()
	p.cache.errLogged = false
	return cloneModels(list), nil
}

// fetchModels 真的去拉一次 /models。
func (p *Provider) fetchModels(ctx context.Context, ep Endpoint) ([]gateway.ModelInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, modelsTimeout)
	defer cancel()

	target := ep.ModelsURL()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("custom %s：构造模型目录请求失败（%s）：%w", ep.ID, target, err)
	}
	ep.applyHeaders(req)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("custom %s：请求 %s 失败：%w", ep.ID, target, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 带一小段正文回来便于定位（例如“model list not supported”）。
		// ⚠ 这段正文是**上游可控**的，实测有的网关会在错误体里回显请求头里的
		// key。必须先 redactKey 再截断：先截断会让跨过边界的 key 只匹配到一半，
		// MaskKey 之后仍能读出前缀。
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, modelsBodyLimit))
		safe := truncateChars(redactKey(string(snippet), ep.resolvedKey()), probeErrorBodyChars)
		return nil, fmt.Errorf("custom %s：%s 返回 HTTP %d：%s",
			ep.ID, target, resp.StatusCode, safe)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, modelsBodyLimit))
	if err != nil {
		return nil, fmt.Errorf("custom %s：读取 %s 的响应失败：%w", ep.ID, target, err)
	}
	list, err := parseModelsJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("custom %s：%s 的响应不是模型目录（%v）", ep.ID, target, err)
	}
	return list, nil
}

// modelEntry 上游 /models 里的一个条目。
//
// 只读 id：owned_by / created / object 这些字段在跨上游口径里没有共识，
// 读进来只会诱使下游依赖不可靠的数据（同 gateway.ModelInfo 的最小化判据）。
type modelEntry struct {
	ID string `json:"id"`
}

// modelsResponse OpenAI 形状的目录 {"object":"list","data":[…]}
type modelsResponse struct {
	Data []modelEntry `json:"data"`
}

// parseModelsJSON 解析模型目录，兼容两种形状：
//
//	{"data":[{"id":"…"}]}   OpenAI 标准
//	[{"id":"…"}]            一些自建服务（vLLM、one-api 的部分版本）直接回数组
//
// 顺序很重要：先按对象解，失败再按数组解 —— 反过来时一个对象会被
// “数组解析失败”掩盖成难以理解的错误。
func parseModelsJSON(raw []byte) ([]gateway.ModelInfo, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("响应体为空")
	}

	var wrap modelsResponse
	if err := json.Unmarshal(trimmed, &wrap); err == nil {
		return modelsFromEntries(wrap.Data), nil
	}

	var arr []modelEntry
	if err := json.Unmarshal(trimmed, &arr); err != nil {
		// 两种都不是：只回一个简短原因。**不回显正文**（正文可能就是 200 的
		// HTML 错误页，几十 KB，进日志没有价值）。
		return nil, fmt.Errorf("既不是 {\"data\":[…]} 也不是数组：%w", err)
	}
	return modelsFromEntries(arr), nil
}

// modelsFromEntries 把一个条目列表投影成 gateway.ModelInfo。
//
// 跳过空 id：没有 id 的模型无法被调用（模型名是唯一入口），
// 让它进目录只会在客户端里变成一个必然失败的下拉选项。
func modelsFromEntries(entries []modelEntry) []gateway.ModelInfo {
	out := make([]gateway.ModelInfo, 0, len(entries))
	for _, e := range entries {
		if e.ID == "" {
			continue
		}
		// 只填 ID：ContextWindow 未知就是 0（协议层的 omitempty 会隐藏它），
		// 不去猜“这个名字大概是多少窗口”——猜错会让客户端把长对话截断。
		out = append(out, gateway.ModelInfo{ID: e.ID})
	}
	return out
}

// modelsFromIDs 把配置里的静态模型名投影成目录，保持配置顺序。
func modelsFromIDs(ids []string) []gateway.ModelInfo {
	entries := make([]modelEntry, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, modelEntry{ID: id})
	}
	return modelsFromEntries(entries)
}

// cloneModels 复制一份目录。
//
// 缓存里的切片不能直接交给调用方：出口层会就地做前缀标注，
// 那会把缓存改成 "custom-id/model" 形态，第二次请求就全变形了。
func cloneModels(in []gateway.ModelInfo) []gateway.ModelInfo {
	if in == nil {
		return nil
	}
	return append([]gateway.ModelInfo(nil), in...)
}
