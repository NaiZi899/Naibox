// models.go 基元律动的模型目录（gateway.Provider.Models）。
//
// # 为什么是"实时拉 + 静态兜底"而不是像 inkstone 那样纯静态表
//
// 这个上游的目录**会变**：实测 2026-10 是 25 个模型，且它是聚合平台
// （自带 `tr-alpha-channel` 这种灰度通道），上新/下架比自建上游频繁。
// 纯静态表会让"上游上了新模型但网关看不到"变成一个需要改代码发版的问题。
//
// 而纯实时又不行：`Models()` 在**目录刷新**路径上被同步调用，一个卡住的
// 上游会让整个控制台的"刷新目录"按钮跟着转；且契约测试用伪造凭证调它，
// 此时必然拉不到 → 会返回空目录 → 被 `verifyModelsNonEmpty` 判不合格。
//
// 所以：实时优先，失败回落静态表。两条路径都保证**非空**。
//
// # 表里的 id 是上游裸模型名
//
// `/v1/models` 的前缀（jiyuan/…）由核心在 registry 合并目录时统一添加
// （见 internal/server/provider_router.go 的 prefixed），本包不拼前缀。
package jiyuan

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/NaiZi899/omni2api/internal/gateway"
)

// upstreamModel 是 `GET /v1/models` 单条的形状（只取本包要用的字段）。
//
// 上游还回了一堆定价字段（input_price_per_million / effective_pricing /
// pricing / discount_pricing / responses_capabilities…）。本包**不取**它们：
// gateway.ModelInfo 刻意只有"跨上游有共识"的字段，定价是上游特有事实，
// 要塞进去得走各自的 AdminExt 端点 —— 那是另一件事。
//
// ⚠ 例外是 supports_vision：它属于"跨上游共识"（五个在册上游各自都上报，
// 只是字段名不同），所以进 ModelInfo。判据见 gateway.ModelInfo 的注释。
type upstreamModel struct {
	ID               string `json:"id"`
	ContextLength    int    `json:"context_length"`
	MaxCompletionTok int    `json:"max_completion_tokens"`
	SupportsVision   bool   `json:"supports_vision"`
}

// upstreamModelsResp `GET /v1/models` 的响应信封（OpenAI 标准形态）。
type upstreamModelsResp struct {
	Object string          `json:"object"`
	Data   []upstreamModel `json:"data"`
}

// fallbackModels 静态兜底表（2026-10 实测 `GET /v1/models` 的 25 个模型）。
//
// 只在实时拉取失败时使用。它同时是"上游改名前的最小可用目录"——
// 空目录比过期目录更糟：空目录会让界面显示"上游没有模型"，
// 而过期目录至少让已经能用的模型继续可用。
//
// SupportsImages 取自同一份实测回执的 `supports_vision`。注意本上游的
// 取值**与直觉不符的两处**，都是上游如实声明、已按上游照抄：
//
//	glm-5.3-flash   true   而 glm-5.3     false（同族反向）
//	qwen3.8-max     true   而 qwen3.7-max false（上一代反向）
//
// 这两对已被官方文档独立证实（智谱 GLM-5.3-Flash 官方标注支持图像/视频，
// GLM-5.3 官方原文"目前仅支持处理文本模态信息"），所以不是解析错误。
//
// ⚠ tr-alpha-channel 已从上游目录消失（新出现的是 minimax-m3.1-flash-preview），
// 它在上游没有 `supports_vision` 可依，故保持未声明。这一条目留着是为了
// "上游改回时目录不空"，不是因为它还有实测值。
var fallbackModels = []gateway.ModelInfo{
	{ID: "deepseek-flash", ContextWindow: 1000000, MaxOutputTokens: 128000, SupportsImages: true},
	{ID: "deepseek-v4-flash-0731", ContextWindow: 1000000, MaxOutputTokens: 128000},
	{ID: "deepseek-v4-pro-0813", ContextWindow: 1000000, MaxOutputTokens: 128000},
	{ID: "glm-5.1", ContextWindow: 200000, MaxOutputTokens: 128000},
	{ID: "glm-5.2", ContextWindow: 1000000, MaxOutputTokens: 128000},
	{ID: "glm-5.3", ContextWindow: 1048576, MaxOutputTokens: 128000},
	{ID: "glm-5.3-flash", ContextWindow: 1048576, MaxOutputTokens: 128000, SupportsImages: true},
	{ID: "glm-5.3-flashx", ContextWindow: 1048576, MaxOutputTokens: 128000, SupportsImages: true},
	{ID: "kimi-k2.6", ContextWindow: 256000, MaxOutputTokens: 128000, SupportsImages: true},
	{ID: "kimi-k2.7-code", ContextWindow: 256000, MaxOutputTokens: 128000, SupportsImages: true},
	{ID: "kimi-k3", ContextWindow: 1048576, MaxOutputTokens: 128000, SupportsImages: true},
	{ID: "longcat-2.0", ContextWindow: 1000000, MaxOutputTokens: 128000},
	{ID: "mimo-v2.5-pro", ContextWindow: 256000, MaxOutputTokens: 128000},
	{ID: "mimo-v2.6-flash", ContextWindow: 1048576, MaxOutputTokens: 128000, SupportsImages: true},
	{ID: "mimo-v2.6-pro", ContextWindow: 1048576, MaxOutputTokens: 128000, SupportsImages: true},
	{ID: "minimax-m2.7", ContextWindow: 200000, MaxOutputTokens: 192000},
	{ID: "neohorse-1-9b", ContextWindow: 262144, MaxOutputTokens: 128000},
	{ID: "qwen3.7-flash", ContextWindow: 1000000, MaxOutputTokens: 128000, SupportsImages: true},
	{ID: "qwen3.7-max", ContextWindow: 1000000, MaxOutputTokens: 128000},
	{ID: "qwen3.8-27b", ContextWindow: 1000000, MaxOutputTokens: 128000, SupportsImages: true},
	{ID: "qwen3.8-flash", ContextWindow: 1000000, MaxOutputTokens: 128000, SupportsImages: true},
	{ID: "qwen3.8-max", ContextWindow: 1000000, MaxOutputTokens: 128000, SupportsImages: true},
	{ID: "seed-2.1-pro", ContextWindow: 262144, MaxOutputTokens: 128000, SupportsImages: true},
	{ID: "seed-2.1-turbo", ContextWindow: 262144, MaxOutputTokens: 128000, SupportsImages: true},
	{ID: "tr-alpha-channel", ContextWindow: 1000000, MaxOutputTokens: 128000},
}

// fetchModels 实时拉取模型目录（失败返回 error，由调用方决定是否兜底）。
func (p *Provider) fetchModels(ctx context.Context, cred gateway.Credential) ([]gateway.ModelInfo, error) {
	a, err := authOf(cred)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, modelsTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL()+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.APIKey)
	req.Header.Set("User-Agent", "omni2api/jiyuan (+OpenAI-compatible gateway)")

	resp, err := (&http.Client{Timeout: modelsTimeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errors.New("jiyuan: 上游 /models 返回 HTTP " + strconv.Itoa(resp.StatusCode))
	}
	raw, err := readAllLimited(resp.Body, 4<<20)
	if err != nil {
		return nil, err
	}
	var out upstreamModelsResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return normalizeModels(out.Data), nil
}

// normalizeModels 把上游条目投影成 gateway.ModelInfo，并做三件清理：
//
//	去空 id（上游偶发脏条目）
//	去重（同一 id 出现两次会让界面出现两行一样的模型）
//	按 id 排序（目录顺序稳定，界面与快照可 diff）
func normalizeModels(rows []upstreamModel) []gateway.ModelInfo {
	seen := make(map[string]bool, len(rows))
	out := make([]gateway.ModelInfo, 0, len(rows))
	for _, r := range rows {
		id := strings.TrimSpace(r.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, gateway.ModelInfo{
			ID:              id,
			ContextWindow:   r.ContextLength,
			MaxOutputTokens: r.MaxCompletionTok,
			SupportsImages:  r.SupportsVision,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Models 实现 gateway.Provider.Models。
//
// 两条路径：
//
//	Config.LiveModels = false（零值，契约测试与独立测试用）
//	    直接返回静态表。**不发任何网络请求** —— 单元测试必须离线可跑。
//
//	Config.LiveModels = true（装配层，生产）
//	    实时拉 `GET /v1/models`；失败或空则回落静态表。
//
// 两条路径都保证返回**非空**目录：契约对 CapModels 的要求是
// "声明了就必须真的返回非空"（见 gateway/contract.go 的 verifyModelsNonEmpty）。
func (p *Provider) Models(ctx context.Context, cred gateway.Credential) ([]gateway.ModelInfo, error) {
	if p.cfg.LiveModels {
		if list, err := p.fetchModels(ctx, cred); err == nil && len(list) > 0 {
			return list, nil
		}
	}
	out := make([]gateway.ModelInfo, len(fallbackModels))
	copy(out, fallbackModels)
	return out, nil
}

// fallbackModelIDs 供测试与诊断：返回静态表里的全部裸模型 id。
func fallbackModelIDs() []string {
	out := make([]string, len(fallbackModels))
	for i, m := range fallbackModels {
		out[i] = m.ID
	}
	return out
}
