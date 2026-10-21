// probe.go —— “这台机器现在能不能连上这个端点”。
//
// # 为什么探针必须说人话
//
// 配置自定义上游时最常见的三种失败，用一句 “connection failed” 完全区分不开：
//
//	① 地址写错 / 端点没起来      → 连接被拒
//	② key 写错 / 没权限          → 401 / 403
//	③ 基址写成了域名前缀，少了版本段 → 404（端点可达，但没有模型目录）
//
// 三种情况用户要做的动作完全不同（改地址 / 改密钥 / 改路径），所以错误文案
// 必须把这三者分开，并且**带上真正被打的 URL 与底层错误**。
//
// # 为什么探测也缓存 15 秒
//
// 面板上的“探测”是个按钮，用户会连点。每次都真打一遍上游既是浪费，
// 也可能把一个有速率限制的端点打成 429 —— 于是“配置没错”被显示成“配置错了”。
package custom

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// probeBodyLimit 探测时读取的响应体上限：只为取模型条数与一段可展示的原因，
// 不需要把整个目录读进内存。
const probeBodyLimit = 1 << 20

// probeErrorBodyChars 失败文案里保留的正文长度（字符数，不是字节数）。
//
// 200 是“够看出上游在抱怨什么”与“不把控制台刷屏”之间的取舍；
// 更长的正文里通常只有重复的堆栈。
const probeErrorBodyChars = 200

// ProbeResult 一次探测的结果（管理台直接展示）。
//
// 字段名与 internal/admin 的 CustomProbeView 同形但**刻意分开定义**：
// admin 不 import 具体上游包（架构判据），由宿主投影。
type ProbeResult struct {
	OK bool `json:"ok"`
	// Status 上游返回的 HTTP 状态码；根本没连上时为 0。
	Status int `json:"status"`
	// LatencyMS 从发请求到拿到响应头的毫秒数（不含读完响应体）。
	LatencyMS int64 `json:"latency_ms"`
	// ModelsCount 目录里的模型条数。非 2xx 或响应形状不符时为 0。
	ModelsCount int `json:"models_count"`
	// Error 失败原因；OK 时通常为空。
	//
	// ⚠ 已脱敏：正文里的完整密钥会被换成 MaskKey 的形态（见 redactKey）。
	Error string `json:"error,omitempty"`
	// At 这次探测的时刻（Z 表示之前的时刻是缓存值）。
	At time.Time `json:"at"`
}

// Probe 探测一次，15 秒内复用上一次结果。
//
// 缓存判定在锁内做，但**网络请求在锁外发**：否则一个卡住的探测会让
// 并发进来的所有探测一起排队等 5 秒挂断。
func (p *Provider) Probe(ctx context.Context) ProbeResult {
	p.probeMu.Lock()
	if !p.probeCache.at.IsZero() && time.Since(p.probeCache.at) < probeTTL {
		res := p.probeCache.res
		p.probeMu.Unlock()
		return res
	}
	p.probeMu.Unlock()

	// 缓存过期时**不**在这里写缓存：交给 ProbeNow 统一写，
	// 避免“缓存过期”与“结果写回”两处各自维护时间戳。
	return p.ProbeNow(ctx)
}

// ProbeNow 立刻探测一次并刷新缓存（管理台的强制刷新走这里）。
//
// 多 key 时逐把探测，任意一把通就算端点可用；只有全部失败才返回第一把的
// 错误。总时长单独限 8 秒，避免 8 把坏 key × 5 秒把面板拖死。
func (p *Provider) ProbeNow(ctx context.Context) ProbeResult {
	keys := p.ep.resolvedKeys()
	if len(keys) <= 1 {
		res := probeEndpoint(ctx, p.client, p.ep)
		p.storeProbe(res)
		return res
	}

	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var first ProbeResult
	for i, key := range keys {
		ep := p.ep
		ep.APIKey = key
		ep.Keys = nil
		res := probeEndpoint(ctx, p.client, ep)
		if res.OK {
			p.storeProbe(res)
			return res
		}
		if i == 0 {
			first = res
		}
	}
	first.Error = fmt.Sprintf("%s（%d 把 key 均未通过）", first.Error, len(keys))
	p.storeProbe(first)
	return first
}

func (p *Provider) storeProbe(res ProbeResult) {
	p.probeMu.Lock()
	p.probeCache = probeCache{res: res, at: time.Now()}
	p.probeMu.Unlock()
}

// ProbeSpec 探测一条**尚未保存**的配置（表单里的“测试连通性”）。
//
// 为什么必须走 Normalize 而不是直接拼 URL：默认值（路径、鉴权头、超时）
// 与密钥解析（api_key_env）都只在 Normalize 里实现一份。绕过它去探测，
// 用户会看到“测试通过、保存后却不同”的假象 —— 两份判据迟早漂移。
//
// 它不需要账号池、不需要凭证：临时 Provider 用构造时的 ep 就能探。
func ProbeSpec(ctx context.Context, spec Spec, getenv func(string) string) ProbeResult {
	ep, err := Normalize(spec, getenv)
	if err != nil {
		// 前缀是给面板用的：这类失败与“端点连不上”是两码事，不该显示成网络问题。
		return ProbeResult{Error: "配置非法：" + err.Error(), At: time.Now()}
	}
	return New(ep, nil).ProbeNow(ctx)
}

// probeEndpoint 探测的主体（纯函数式：只依赖入参，不碰 Provider 状态）。
//
// 单独抽出来是为了让 ProbeSpec（临时 Provider）与 ProbeNow（常驻 Provider）
// 共用同一份判据 —— 面板上“测试草稿”与“测活着的端点”给的结果必须一致。
func probeEndpoint(ctx context.Context, client *http.Client, ep Endpoint) ProbeResult {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	res := ProbeResult{At: time.Now()}
	target := ep.ModelsURL()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		res.Error = fmt.Sprintf("探测 %s 失败：%v", target, err)
		res.Error = redactKey(res.Error, ep.resolvedKey())
		return res
	}
	ep.applyHeaders(req)

	start := time.Now()
	resp, err := client.Do(req)
	res.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		// 连不上 / 超时 / 跨主机重定向被拒：URL 与底层错误都要在文案里，
		// 否则用户拿到的只是“dial tcp: connectex ...”这种没有主语的句子。
		res.Error = fmt.Sprintf("无法连接 %s：%v", target, err)
		res.Error = redactKey(res.Error, ep.resolvedKey())
		return res
	}
	defer func() { _ = resp.Body.Close() }()

	res.Status = resp.StatusCode
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, probeBodyLimit))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		// 2xx ⇒ 端点可达且鉴权通过，这就是 OK 的全部含义。
		res.OK = true
		if list, perr := parseModelsJSON(raw); perr == nil {
			res.ModelsCount = len(list)
		} else {
			// 能连上但目录形状不认识：不算失败（很多端点确实没有 /models），
			// 但要把原因写在附注里，否则“0 个模型”会让人以为目录是空的。
			res.Error = redactKey(fmt.Sprintf("端点可达，但响应不是模型目录：%v", perr), ep.resolvedKey())
		}
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		res.Error = fmt.Sprintf("密钥无效（HTTP %d）", resp.StatusCode)
	case resp.StatusCode == 404:
		res.Error = "端点可达，但没有模型目录（HTTP 404）"
	default:
		// 先脱敏再截断：反过来的话，跨过截断边界的 key 只匹配到一半，
		// 剩下的前缀会被原样显示（MaskKey 只保留尾 4 位，前缀信息量更大）。
		snippet := truncateChars(redactKey(string(raw), ep.resolvedKey()), probeErrorBodyChars)
		if snippet == "" {
			res.Error = fmt.Sprintf("上游返回 HTTP %d", resp.StatusCode)
		} else {
			res.Error = fmt.Sprintf("上游返回 HTTP %d：%s", resp.StatusCode, snippet)
		}
	}
	return res
}

// redactKey 把文案里出现的完整密钥换成 MaskKey 形态。
//
// 为什么这一条不可省：探测的错误文案会**原样进控制台并被复制**，
// 而它的一部分内容来自上游响应体。密钥进界面的代价远大于损失的诊断信息。
func redactKey(msg, key string) string {
	if msg == "" || key == "" {
		return msg
	}
	return strings.ReplaceAll(msg, key, MaskKey(key))
}

// truncateChars 按**字符**（不是字节）截断，避免把一个多字节字符切成乱码。
//
// 逐字节截断在多字节正文上会产出无效 UTF-8，JSON 编码时会被替换成一串
// U+FFFD —— 那比截断本身更让人困惑。
func truncateChars(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
