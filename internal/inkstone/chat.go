// chat.go 书生·端砚的对话转发（gateway.Provider.Chat）。
//
// 书生端砚本身就是 OpenAI 兼容端点，Chat 做的工作极少：
//
//  1. 凭证类型断言（不匹配返回明确错误，不 panic）
//  2. ctx 已取消则立刻返回（契约要求）
//  3. 用 API Key 直连推理 API，body 原样转发（model 已是裸名，
//     前缀由核心在出站时剥掉了）
//
// status 非 2xx 仍返回流（把上游错误体带回去给调用方继续分类），
// 只有网络层失败才返回 error —— 与 loomy 同一条判据。
package inkstone

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/NaiZi899/Naibox/internal/gateway"
)

// authOf 从 gateway.Credential 取出书生端砚凭证（类型断言）。
func authOf(cred gateway.Credential) (*Auth, error) {
	if cred.Secret == nil {
		return nil, errors.New("inkstone: 凭证缺少 secret（账号没进池或没有凭证）")
	}
	a, ok := cred.Secret.(*Auth)
	if !ok || a == nil {
		return nil, errors.New("inkstone: 凭证类型不符（应为本上游凭证）")
	}
	if strings.TrimSpace(a.APIKey) == "" {
		return nil, errors.New("inkstone: 凭证缺少 api_key")
	}
	return a, nil
}

// inferURL 返回推理端点（去尾斜杠）。
func (p *Provider) inferURL() string {
	base := p.cfg.InferBase
	if strings.TrimSpace(base) == "" {
		base = DefaultInferBase
	}
	return strings.TrimRight(base, "/")
}

// stripEmptyTools 移除请求体里的 `tools: []`。
//
// 书生端砚严格要求 tools 非空数组，空数组会返回 400
// `upstream_request_rejected: "tools" must not be an empty array`。
// 而 DSH 等 agent 框架常带空的 tools 字段。这里在出站前剥掉，
// 保证对书生端砚的请求不带空 tools。
func stripEmptyTools(body []byte) []byte {
	if len(body) == 0 || !bytes.Contains(body, []byte("tools")) {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body // 不是合法 JSON，原样转发（构造已失败上游会报更明确错误）
	}
	v, ok := obj["tools"]
	tools, isArr := v.([]any)
	if ok && isArr && len(tools) == 0 {
		delete(obj, "tools")
		if out, err := json.Marshal(obj); err == nil {
			return out
		}
	}
	return body
}

// forceStream 把请求体里的 stream 置为 true。
//
// omni2api 对上游响应用统一的 SSE 解析器（wire.Aggregate）读 `data:` 帧，
// 且对客户端 stream:false 的请求会在拿到上游 SSE 后自己聚合回 JSON。
// 所以这里强制上游走 SSE：书生端砚对非 stream 请求返回纯 JSON（无 data: 帧），
// 会让 omni2api 报 `upstream stream contained no valid data events`。
// 强制 stream:true 后书生端砚必回 SSE，omni2api 按客户端喜好转。
func forceStream(body []byte) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if _, ok := obj["stream"]; !ok || obj["stream"] != true {
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
	if err := ctx.Err(); err != nil {
		return gateway.ChatStream{}, err
	}
	// 书生端砚不接受空 tools；且必须走 SSE（omni2api 出站统一解析 SSE）。
	body = stripEmptyTools(body)
	body = forceStream(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.inferURL()+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return gateway.ChatStream{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.APIKey)

	cli := &http.Client{Timeout: httpTimeout}
	resp, err := cli.Do(req)
	if err != nil {
		return gateway.ChatStream{}, err
	}

	// 非 2xx：不返回 error，把状态码 + 错误体作为流带回，让调用方分类。
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		eb, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if len(eb) > 0 {
			return gateway.ChatStream{Status: resp.StatusCode, Body: io.NopCloser(bytes.NewReader(eb))}, nil
		}
		return gateway.ChatStream{Status: resp.StatusCode, Body: io.NopCloser(strings.NewReader(""))}, nil
	}

	// 2xx：把 body 透传给调用方（调用方负责 Close）。
	return gateway.ChatStream{Status: resp.StatusCode, Body: resp.Body}, nil
}
