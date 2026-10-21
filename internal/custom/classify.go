// classify.go —— 自定义端点的错误分类（gateway.ErrorClassifier 的实现）。
//
// # 为什么要这么保守
//
// 内置上游的错误分类是靠**实测过的错误码表**（workbuddy 的 12153、codearts 的
// InferHub.4291）判的。自定义端点没有这张表，也不可能提前知道用户接的是谁：
// 同一个 HTTP 429 在 A 家是“你买的基础套餐并发是 1”，在 B 家是“今天的免费额度
// 用完了”。在这种信息条件下，任何**从正文里猜关键词**的判据都可能把
// 一个健康账号判成额度耗尽（见 gateway/errorclassifier.go 记的两个危害）。
//
// 所以这里只认**协议层真的有共识**的判据：状态码本身。
//
// # 401/403/429 为什么是“透传”而不是“换号”
//
// 自定义端点用一把静态 key，`shards` 只是同一把 key 的多个并发槽位。
// 这三类错误在每个分片上都是同一个答案：
//
//	401/403  密钥就是错的 —— 换分片再打一次仍然是 401
//	429      上游在限流 —— 换分片只是把同一个 key 再打一次
//
// 交给默认策略（只换号不罚）的后果是：shards=N 时一个客户端请求会向上游
// 发 N 个注定失败的请求，最后客户端拿到 **503「所有账号不可用」**，
// 而上游给出的 429（带 Retry-After）与 401（密钥失效这个事实）都被吞掉。
//
// ErrKindPassthrough 让出口层把状态码与正文原样回给调用方：不换号、不冷却、
// 不喂熔断。这是唯一一个三种处置都不适用的错误类别。
//
// # 404 归 ErrKindClient
//
// 404 通常是 models_path / chat_path 写错（配置问题），不是账号问题。
// 归 ErrKindClient（只换号不罚）能让同一端点的其它分片继续试，
// 同时不会喂熔断计数 —— 熔断是给“上游真的坏了”准备的，配置写错不该触发它。
package custom

import "github.com/NaiZi899/Naibox/internal/gateway"

// Classify 按状态码判定错误类别。
//
// 纯函数：不发网络请求、不读写共享状态、不写日志（尤其**不打正文与密钥** ——
// 它在出站循环的每次非 2xx 上被调用，把正文写进日志等于把用户对话落盘）。
//
// body 参数刻意不使用：本包的判据只到状态码这一层（见文件头）。
func (p *Provider) Classify(status int, body string) gateway.ErrorKind {
	_ = body
	switch {
	case status == 401 || status == 403:
		return gateway.ErrKindPassthrough
	case status == 429:
		return gateway.ErrKindPassthrough
	case status == 404:
		return gateway.ErrKindClient
	case status >= 500:
		return gateway.ErrKindServer
	}
	return gateway.ErrKindNone
}
