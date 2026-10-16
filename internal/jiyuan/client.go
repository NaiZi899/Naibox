// client.go 基元律动的端点常量与共享 HTTP 客户端参数。
package jiyuan

import (
	"io"
	"time"
)

// DefaultBaseURL 推理端点基址（OpenAI 兼容面）。
//
// 实测（2026-10）：`GET /v1/models` 与 `POST /v1/chat/completions`
// 都在这个面下，Bearer 一把 `sk_…` key 即可鉴权。
//
// ⚠ 管理面是 `/api/*`（另一个前缀，只认浏览器 session cookie）——
// 本包不碰它，理由见包注释。
const DefaultBaseURL = "https://tokenrhythm.studio/v1"

// httpTimeout 单次上游请求总时限。
//
// 流式对话的超时由调用方 ctx 控制，这里给的是"握手 + 短请求"的上限；
// 与 inkstone/loomy 取同一个值（120s）便于跨上游横向比对日志。
const httpTimeout = 120 * time.Second

// modelsTimeout Models() 的上限。
//
// 比 httpTimeout 短得多：目录拉取是**同步**发生在目录刷新路径上的，
// 一个卡住的上游不该让整个控制台的"刷新目录"按钮转两分钟。
const modelsTimeout = 20 * time.Second

// readAllLimited 读一个流到上限为止（超出部分丢弃，不报错）。
//
// # 为什么要有上限
//
// 上游回执的**长度不受我们控制**。一个异常大的响应（或恶意的中间人）
// 会把内存吃光 —— 而网关是常驻进程，一次 OOM 掉的是所有人的服务。
// 4 MiB 对本站的所有接口都远超需要（最大的 Key 列表也就几十 KB），
// 同时又足够宽松，不会误伤正常响应。
//
// 超出上限**不返回错误**：部分数据仍可解析（JSON 解析失败时会明确报错），
// 而"因为回执太大就整个失败"会让用户看到一个无法理解的错误。
func readAllLimited(r io.Reader, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, limit))
}
