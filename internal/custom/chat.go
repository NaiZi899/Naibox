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
