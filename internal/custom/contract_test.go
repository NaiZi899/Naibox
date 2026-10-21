// contract_test.go —— 一行接上 gateway.RunProviderContract。
//
// # 为什么工厂里要塞 StaticModels
//
// 契约要求“声明 CapModels ⇒ Models() 必须返回非空目录”。没有静态目录时
// Models 会真去拉网络，而契约的基址是一个关闭端口 —— 那会让检查变成
// “这台机器有没有外网”的测试，而不是“实现合不合格”的测试。
// 静态目录让这条判据只依赖本包自己的代码。
//
// # 为什么 Chat 不需要网络也能过
//
// 契约对 Chat 的要求是“不 panic / 流能 Close / 尊重 ctx 取消 / 不返回
// 不可判断的空响应”。指向 127.0.0.1:1 时 Chat 拿到传输错误并返回 error，
// 上面四条都自然成立 —— 契约自己不要求请求送达（见 contract.go 的“已知局限”）。
package custom

import (
	"testing"
	"time"

	"github.com/NaiZi899/Naibox/internal/gateway"
)

// contractEndpoint 契约用的端点：无外网、可确定。
func contractEndpoint() Endpoint {
	return Endpoint{
		ID:         "contract",
		Name:       "contract",
		BaseURL:    "http://127.0.0.1:1",
		APIKey:     "x",
		ChatPath:   "/chat/completions",
		ModelsPath: "/models",
		AuthHeader: "Authorization",
		AuthScheme: "Bearer",
		StaticModels: []string{
			"m1",
		},
		Timeout: 2 * time.Second,
		Shards:  1,
	}
}

// TestProviderContract 契约检查（加新上游的合格判据）。
func TestProviderContract(t *testing.T) {
	gateway.RunProviderContract(t, func() gateway.Provider {
		ep := contractEndpoint()
		return New(ep, nil)
	})
}
