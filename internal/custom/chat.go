// chat.go —— 一次流式对话的转发。
//
// # 这个文件刻意很短
//
// 自定义上游的全部特殊性都收在 Endpoint 里（URL 拼接、鉴权头拼法、
// 附加头、超时、TLS 策略），归一之后剩下的就是“POST 一份 body、把流交回去”。
// 任何在这里出现的业务判断（改请求体、裁剪 max_tokens、识别错误码）
// 都会变成“对所有 OpenAI 兼容端点都成立”的假设 —— 而它们不成立。
//
// # 为什么不在这里读响应体
//
// Body 的所有权归调用方（见 gateway.ChatStream 的注释）。在这里读一次做
// 错误判断，等于把流式响应整包读完才返回 —— 长回复会变成“等十几分钟才吐出
// 第一个字”。非 2xx 的错误体同理：出口层要自己读完并分类（见 classify.go），
// 这里只负责把状态码与 Body 原样交出去。
package custom

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/NaiZi899/Naibox/internal/gateway"
)

// Chat 转发一次对话请求。
//
// 返回值语义与其它上游一致：
//
//	err != nil      传输层失败（连不上、TLS 失败、ctx 取消、跨主机重定向被拒）
//	err == nil      拿到了上游响应，**非 2xx 也是正常返回**，
//	                状态码与响应体交给出口层分类（429 冷却 / 401 换号 …）
func (p *Provider) Chat(ctx context.Context, cred gateway.Credential, body []byte) (gateway.ChatStream, error) {
	ep := p.endpointOf(cred)
	target := ep.ChatURL()

	// 出口层的增量解析只认 SSE，所以对上游一律要流式；非流式的聚合由出口层做
	//（与其它内置上游同一形态，它们的 Chat 也固定要 SSE）。
	//
	// 必须改请求体里的 stream，不能只发 Accept 头：上游是按**请求体的 stream**
	// 决定返回形态的，Accept 不参与判定。只发头会让 stream:false 的请求拿到整包
	// JSON，而出口层按 SSE 解析 —— 一个 data: 帧都读不到，直接
	// `upstream stream contained no valid data events`（502 upstream_parse）。
	//
	// 改写必须在这里：下一行就把 body 包成 Reader 了，之后改切片不生效。
	if forced, ferr := forceStream(body); ferr == nil {
		body = forced
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		// 走到这里说明配置层面的 URL 不可用（Normalize 之后基本不可能），
		// 仍然带上是哪个端点，否则日志里只有一个孤零零的 net/url 错误。
		return gateway.ChatStream{}, fmt.Errorf("custom %s：构造对话请求失败（%s）：%w", ep.ID, target, err)
	}

	// 顺序与 spec.go 的 applyHeaders 一致：附加头先、鉴权头后（后者优先）。
	ep.applyHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	// 显式要 SSE：部分网关在没有 Accept 时会回落成“整包 JSON”，
	// 而出口层的增量解析只认 data: 帧 —— 那会让整段回答被吞掉。
	req.Header.Set("Accept", "text/event-stream")

	resp, err := p.client.Do(req)
	if err != nil {
		// 网络层失败才返回 error。**不打印 body，也不打印任何头** ——
		// 请求体是用户对话、头里是密钥，两者都不能进日志。
		// 错误文案里带 URL 与端点 id：排查时需要知道“打的是谁”。
		return gateway.ChatStream{}, fmt.Errorf("custom %s：请求 %s 失败：%w", ep.ID, target, err)
	}

	// 不读、不 Close：Caller owns Close（见 gateway.ChatStream.Body 的注释）。
	return gateway.ChatStream{Status: resp.StatusCode, Body: resp.Body}, nil
}


// forceStream 把请求体里的 stream 强制置为 true，其余字段原样保留。
//
// # 为什么不是字符串替换
//
// `"stream": false` / `"stream":false`、字段顺序都可能不同，正则改写会在某种写法
// 上悄悄失效 —— 而失效的表现又是"非流式坏了"，排查成本远高于这里一次 JSON 往返。
// 请求体本身是 JSON（OpenAI 协议），解析再序列化零信息损失。
//
// # 已是 true 时不重新序列化
//
// 保持原始字节：部分上游对请求体做指纹/签名，重新序列化会改变字段顺序与空白，
// 可能让本来能通过的请求失败。
//
// 解析失败时原样返回 body（不改写）：那种情况上游本来也会报错，把原文透出去比
// 在这里造一个"体已合法"的假象更好。
func forceStream(body []byte) ([]byte, error) {
	if len(body) == 0 {
		return body, fmt.Errorf("empty body")
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body, err
	}
	if v, ok := m["stream"].(bool); ok && v {
		return body, nil
	}
	m["stream"] = true
	out, err := json.Marshal(m)
	if err != nil {
		return body, err
	}
	return out, nil
}
