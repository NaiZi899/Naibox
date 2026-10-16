// chat.go 基元律动的对话转发（gateway.Provider.Chat）。
//
// 基元律动本身就是 OpenAI 兼容端点，Chat 做的工作极少：
//
//  1. 凭证类型断言（不匹配返回明确错误，不 panic）
//  2. ctx 已取消则立刻返回（契约要求）
//  3. 出站前做两处 body 整形（见下）
//  4. 用 API Key 直连，body 原样转发
//
// status 非 2xx 仍返回流（把上游错误体带回去给调用方继续分类），
// 只有网络层失败才返回 error —— 与 inkstone/loomy 同一条判据。
package jiyuan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/NaiZi899/omni2api/internal/gateway"
)

// authOf 从 gateway.Credential 取出本上游凭证（类型断言）。
func authOf(cred gateway.Credential) (*Auth, error) {
	if cred.Secret == nil {
		return nil, errors.New("jiyuan: 凭证缺少 secret（账号没进池或没有凭证）")
	}
	a, ok := cred.Secret.(*Auth)
	if !ok || a == nil {
		return nil, errors.New("jiyuan: 凭证类型不符（应为本上游凭证）")
	}
	if strings.TrimSpace(a.APIKey) == "" {
		return nil, errors.New("jiyuan: 凭证缺少 api_key")
	}
	return a, nil
}

// baseURL 返回推理端点基址（去尾斜杠）。
func (p *Provider) baseURL() string {
	base := strings.TrimSpace(p.cfg.BaseURL)
	if base == "" {
		base = DefaultBaseURL
	}
	return strings.TrimRight(base, "/")
}

// stripEmptyTools 移除请求体里的 `tools: []`。
//
// # 为什么需要它（与 inkstone 同一个坑的预防性处理）
//
// agent 框架（DSH / Codex 类）常带一个空的 `tools` 字段。部分
// OpenAI 兼容实现把它当成"要调用工具但没给定义"而回 400
// （inkstone 的书生端砚是实测确认会拒的：`"tools" must not be an
// empty array`）。
//
// 基元律动实测**没有**这个问题，但剥掉空 tools 对它是纯收益：
// 语义完全等价（没有工具就是没有工具），而一旦上游哪天收紧校验，
// 这里不会突然开始 400。
//
// 只动"存在且为空数组"这一种形态：非空 tools 原样保留，
// 缺字段不做任何事（不凭空造字段）。
func stripEmptyTools(body []byte) []byte {
	if len(body) == 0 || !bytes.Contains(body, []byte("tools")) {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body // 不是合法 JSON，原样转发（上游会回更明确的错误）
	}
	v, exists := obj["tools"]
	tools, isArr := v.([]any)
	if exists && isArr && len(tools) == 0 {
		delete(obj, "tools")
		if out, err := json.Marshal(obj); err == nil {
			return out
		}
	}
	return body
}

// forceStream 把请求体里的 stream 置为 true。
//
// # 为什么必须强制（这是 omni2api 的接线事实，不是上游的怪癖）
//
// omni2api 对上游响应用**统一的 SSE 解析器**（`wire.Aggregate`，
// 按 `data:` 帧解析），且对客户端 `stream:false` 的请求，会在拿到
// 上游 SSE 之后自己聚合回 JSON 返回。
//
// 也就是说：**上游必须回 SSE**，否则解析器拿不到任何 `data:` 帧，
// 报 `upstream stream contained no valid data events`。
// 而大部分 OpenAI 兼容实现对 `stream:false` 返回纯 JSON 对象 ——
// 正好踩这个坑（inkstone 第一版就是死在这条上）。
//
// 强制 `stream:true` 后上游必回 SSE，omni2api 再按客户端喜好转回 JSON，
// 对外行为完全一致。
func forceStream(body []byte) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if v, ok := obj["stream"]; !ok || v != true {
		obj["stream"] = true
		if out, err := json.Marshal(obj); err == nil {
			return out
		}
	}
	return body
}

// Chat 转发一次对话请求（gateway.Provider）。
func (p *Provider) Chat(ctx context.Context, cred gateway.Credential, body []byte) (gateway.ChatStream, error) {
	a, err := authOf(cred)
	if err != nil {
		return gateway.ChatStream{}, err
	}
	// 契约要求：ctx 已取消必须立刻返回，不发请求。
	if err := ctx.Err(); err != nil {
		return gateway.ChatStream{}, err
	}

	body = stripEmptyTools(body)
	body = forceStream(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.baseURL()+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return gateway.ChatStream{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+a.APIKey)
	// 上游（Cloudflare 前置）对缺失 UA 的请求偶发拦截；给一个明确的来源标识。
	req.Header.Set("User-Agent", "omni2api/jiyuan (+OpenAI-compatible gateway)")

	cli := &http.Client{Timeout: httpTimeout}
	resp, err := cli.Do(req)
	if err != nil {
		return gateway.ChatStream{}, err
	}

	// 非 2xx：不返回 error，把状态码 + 错误体作为流带回，让调用方分类
	// （分类后决定换号还是冷却 —— 这条链在 handler 的 applyErrorPolicy）。
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		eb, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if len(eb) > 0 {
			return gateway.ChatStream{Status: resp.StatusCode, Body: io.NopCloser(bytes.NewReader(eb))}, nil
		}
		return gateway.ChatStream{Status: resp.StatusCode, Body: io.NopCloser(strings.NewReader(""))}, nil
	}

	// 2xx：把流透传给调用方（调用方负责 Close）。
	return gateway.ChatStream{Status: resp.StatusCode, Body: resp.Body}, nil
}
