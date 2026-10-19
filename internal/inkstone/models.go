// models.go 书生·端砚 TokenPlan 的模型目录（逆向实测，2026-10）。
//
// 与 Loomy 同形态：一份静态表。表里的 id 是**上游裸模型名**，
// /v1/models 的前缀（inkstone/…）由核心的 registry 合并时统一添加
// （见 internal/server/provider_router.go 的 prefixed）。
package inkstone

import (
	"context"

	"github.com/NaiZi899/Naibox/internal/gateway"
)

// inkstoneModels 书生·端砚官方模型表（GET /v1/models 实测，10 个）。
//
// # ContextWindow 与 SupportsImages 的来源（2026-10 复核修正）
//
// 两条都直接取自上游 `/v1/models` 的自描述 schema：
//
//	ContextWindow  = input_modalities[].supported_inputs.max_context_length.value
//	SupportsImages = "image" ∈ input_modalities[].type
//
// ⚠ 本次复核发现**旧表的窗口有多处与上游矛盾**，已按上游改正：
//
//	Atria-Dawn-Preview  262144 → 524288
//	glm-5.3             200000 → 1048576   （差 5 倍）
//	intern-s2           200000 → 262144
//	kimi-k2.6          1000000 → 262144    （旧值高报 4 倍，最危险）
//	deepseek-v4-*      1000000 → 1048576   （取整写法统一到上游原值）
//
// 「高报」比「低报」危险得多：调用方按 1000000 组织上下文，上游按 262144
// 拒收，用户看到的是"模型坏了"而不是"声明错了"。
//
// MaxOutputTokens 本次**未改**：上游 schema 的 max_tokens 上限等于窗口值，
// 而表里的 8192 另有来源（手册实测）。两者语义不同（"协议允许"vs"实测可用"），
// 没有新的实测证据前不动它 —— 保持限制方向是保守的（只会夹小不会夹大）。
var inkstoneModels = []gateway.ModelInfo{
	{ID: "deepseek-v4-flash-0731", ContextWindow: 1048576, MaxOutputTokens: 8192},
	{ID: "deepseek-v4-flash-vision", ContextWindow: 1048576, MaxOutputTokens: 8192, SupportsImages: true},
	{ID: "deepseek-v4-pro-0813", ContextWindow: 1048576, MaxOutputTokens: 8192},
	{ID: "Agents-A1", ContextWindow: 262144, MaxOutputTokens: 262144, SupportsImages: true},
	{ID: "Atria-Dawn-Preview", ContextWindow: 524288, MaxOutputTokens: 262144},
	{ID: "intern-s2", ContextWindow: 262144, MaxOutputTokens: 8192, SupportsImages: true},
	{ID: "glm-5.3", ContextWindow: 1048576, MaxOutputTokens: 8192},
	{ID: "kimi-k2.6", ContextWindow: 262144, MaxOutputTokens: 8192, SupportsImages: true},
	{ID: "minimax-m3", ContextWindow: 1048576, MaxOutputTokens: 8192},
	{ID: "qwen3.8-27b", ContextWindow: 262144, MaxOutputTokens: 8192, SupportsImages: true},
}

// Models 实现 gateway.Provider.Models —— 返回本上游的模型目录（裸 ID）。
//
// 静态表恒非空，满足契约对 CapModels 的要求（声明必须真返回非空）。
func (p *Provider) Models(ctx context.Context, cred gateway.Credential) ([]gateway.ModelInfo, error) {
	out := make([]gateway.ModelInfo, len(inkstoneModels))
	copy(out, inkstoneModels)
	return out, nil
}

// knownModelIDs 供测试与诊断：返回全部裸模型 id。
func knownModelIDs() []string {
	out := make([]string, len(inkstoneModels))
	for i, m := range inkstoneModels {
		out[i] = m.ID
	}
	return out
}

// maxOutputFor 返回某裸模型名的输出上限；未知返回 0。
func maxOutputFor(id string) int {
	for _, m := range inkstoneModels {
		if m.ID == id {
			return m.MaxOutputTokens
		}
	}
	return 0
}
