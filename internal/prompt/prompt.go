// Package prompt 提供网关自有系统提示词：内置默认 + 文件覆盖 + 降级中性提示词。
//
// # 为什么需要这一层（借鉴 omni2api-panel）
//
// 客户端（Claude Code / Codex 等 CLI）会在 system prompt 里注入若干**固定模板句**。
// 上游内容审核不是语义审核，而是对这些模板句做**逐字精确匹配**，
// 命中就整单拦截（HTTP 400 + 审核文案，如 code 11128）。
//
// 后果是一批**完全合法**的流量被误杀：用户只是在用 Claude Code 而已。
//
// 内层（internal/upstream/sanitize.go）做的是"把已知指纹串改写掉"，
// 但它只能覆盖**已知**的几种形态；模板句一变（客户端升级、换 CLI），
// 就得重新补规则，是一场追不上的赛跑。
//
// 本层换一个思路：**从源头消灭 system / developer 来源的指纹**——
// 出站前用网关自有提示词整体替换客户端注入的 system/developer 消息，
// 于是那些模板句根本不会出现在请求体里。
//
// # 两层的关系（互不替代）
//
//	prompt 层（本包）→ 解决 **system / developer 消息**里的指纹
//	sanitize 层       → 兜底 **user / assistant / tool 消息**里的指纹串
//
// user 消息里用户自己粘贴的日志、assistant 历史、工具调用参数，
// 都不归提示词层管 —— 那些必须靠 sanitize 逐串擦。
// 所以关掉任一层都会留下一个明确的缺口，两层都必须开着。
package prompt

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
)

//go:embed defaultprompt.md
var defaultPrompt string

// Default 返回内置默认系统提示词。
//
// # 为什么要暴露它，而不是只让 Load 用
//
// 出站客户端（internal/upstream）在 custom 模式下必须拿到**一段**提示词。
// 如果装配层忘了注入 PromptText，最"安静"的失败是 Rewrite 因为
// systemPrompt=="" 而原样返回 —— 表现为"功能配了但完全没生效"。
//
// 让客户端能拿到 Default() 之后，那条路径变成"回落内置默认"，
// 即"配置没接上，但功能仍然按默认语义工作"，是可观测且安全的方向。
func Default() string { return defaultPrompt }

// 提示词模式。
const (
	// ModeCustom 出站前用网关自有提示词**替换**客户端 system / developer 消息。
	//
	// 这是默认值：它从源头消灭 system 来源的指纹误报，
	// 代价是丢弃客户端自己的 system（例如 Claude Code 的工程约定）。
	ModeCustom = "custom"
	// ModePassthrough 透传客户端原始 system，不做改写。
	//
	// 保留客户端人格的完整语义，代价是仍可能撞上指纹误报 ——
	// 此时由降级机制（见 gate.go）兜底：撞一次之后自动换中性提示词重试。
	ModePassthrough = "passthrough"
	// ModeAppend 叠加模式：保留客户端的 system / developer，在其后追加一条网关 system。
	//
	// 与 custom 的取舍：custom 从源头删掉客户端 system，代价是丢掉它的工程约定
	// （项目结构、命名习惯、工具用法这类只写在客户端 system 里的信息），模型
	// 因此会变得"不听话"；append 保留那一段，但客户端 system 里的模板句一个都
	// 没消掉，指纹误报的风险与 passthrough 完全相同。
	//
	// 所以它只在降级期之外有意义。降级期的目标是"尽可能不带指纹的文本"，
	// 而 append 的形态与那个目标正好相反 —— 路由见 policy.go 的 Inject。
	ModeAppend = "append"
)

// Degraded 降级提示词：误报处理用，刻意极简中性。
//
// # 触发场景
//
// passthrough 模式下请求被上游内容策略拦截（HTTP 400 + 审核文案）时，
// 判定为 system 来源的指纹误报，**同请求内**换这段最小中性提示词重试一次。
//
// # 为什么单独写一段而不是复用 defaultPrompt
//
// defaultPrompt 是"一份完整的工程助手人格"，约 2KB。降级的目标是
// **尽可能不携带任何可能被匹配的文本**，所以它必须短、平、无特征：
// 越短越不容易撞上逐字匹配表。复用 defaultPrompt 等于把 2KB 的表面积
// 重新暴露一遍，那与"降级"的目的相反。
//
// # 定位说明
//
// 它不是对抗手段：只用于绕开 **system 来源的误报**，
// 不改变用户指令的语义，也不影响任何内容的合法性判断 ——
// 用户内容本身触发审核时，第二次仍会被拦，并如实返回给调用方。
const Degraded = "You are a helpful assistant. Respond in the user's language, " +
	"follow the user's instructions, and be direct and concise."

// Load 按 mode 与 file 加载系统提示词文本。
//
//   - file 非空 → 读文件；不存在/读失败**返回 error**（调用方 fail fast）
//   - file 空   → 返回内置 defaultPrompt
//
// # 为什么 file 非空但不可读时必须报错而不是回落内置默认（fail fast）
//
// 这是一个**静默降级**极易发生的位置：用户配了 `prompt.file` 指向自己的
// 人格文件，路径打错一个字符，如果静默回落内置默认，表现是
// "网关能跑、请求正常、只是我的自定义人格没了" —— 没有任何信号。
//
// 用户会花很久去查"为什么我的人格不生效"，而真因只是一个路径。
// 启动时报错把这次失败暴露在唯一能立刻发现的位置。
//
// # mode 参数为什么不参与判断
//
// custom / passthrough 的路由由调用方决定（见 internal/upstream 的 applyPrompt）。
// Load 只负责一件事：**拿到一段提示词文本**，不关心它怎么被用。
// 把路由语义塞进来会让它既是加载器又是策略器。
func Load(mode, file string) (string, error) {
	_ = mode // 保留参数以便未来按 mode 校验（如 passthrough 下 file 无意义）
	if file == "" {
		return defaultPrompt, nil
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("prompt 文件 %s 读取失败: %w", file, err)
	}
	if len(raw) == 0 {
		// 空文件是一个**几乎必然是配置错误**的状态：用户显然想要一份提示词，
		// 但拿到了空串。当作错误报出来，而不是发一个空 system 给上游。
		return "", fmt.Errorf("prompt 文件 %s 为空", file)
	}
	return string(raw), nil
}

// Rewrite 解析 OpenAI 请求体并替换系统提示词：
//
//   - 删除 messages 中**所有** role 为 system / developer 的消息
//   - 在 messages 头部插入一条 {"role":"system","content":systemPrompt}
//   - 其余字段与 user / assistant / tool 消息逐字不动
//
// # 为什么解析失败时原样返回，而不是报错
//
// Rewrite 位于**出站改写的关键路径**上，它的输入是客户端刚发来的原始 body。
// 任何解析错误（非 JSON、超大、编码异常）都**不应该**让请求在这里失败：
//
//   - 本函数的职责是"优化"，不是"校验"；
//   - 真正的校验属于上游，让它按原始语义处理，客户端才能拿到上游的真实错误；
//   - 在这里造一个错误，等于把一个上游能诊断的问题变成一个我们自己编的问题。
//
// 因此所有失败路径都返回**入参本身**（同一个 []byte）。
//
// # 为什么 developer 也要删
//
// developer 是 OpenAI 新规范里 system 的别名，上游按 system 级指令处理它。
// 只删 system 会让 developer 携带的模板句继续漏网。
// （注：internal/upstream 的 normalizeRoles 还会把 developer 归一为 system，
// 但那是"上游不认识 developer 这个 role"的协议兼容，与本处的"内容替换"正交，
// 两者叠加不冲突 —— 先删掉就不再需要归一。）
//
// # 为什么 systemPrompt 为空时不动
//
// 空提示词插入一条 content 为 "" 的 system 消息，对上游是新形态，
// 可能触发参数校验。没有提示词就什么都不做，是最保守的选择。
func Rewrite(body []byte, systemPrompt string) []byte {
	if len(body) == 0 || systemPrompt == "" {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		// 无 messages 字段或类型不符 → 插入单条 system 后原样保留其余字段。
		obj["messages"] = []any{map[string]any{"role": "system", "content": systemPrompt}}
		if out, err := json.Marshal(obj); err == nil {
			return out
		}
		return body
	}
	// 过滤掉所有 system / developer 消息，保留 user / assistant / tool 及其他角色。
	//
	// 注意这里是**全删**而不是"只保留第一条"：客户端有时会塞多条 system
	// （例如 Claude Code 的 system + 一段 developer 补充）。只删第一条
	// 会让后面那些继续漏网 —— 而那正是本层要消灭的东西。
	kept := make([]any, 0, len(msgs)+1)
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			kept = append(kept, m)
			continue
		}
		role, _ := mm["role"].(string)
		if role == "system" || role == "developer" {
			continue
		}
		kept = append(kept, m)
	}
	// 头部插入单条 system 消息。
	//
	// 用 prepend 而不是 append：system 必须在最前面才是 system 语义，
	// 追加到尾部会让它变成一段"最后的用户输入"，语义完全变了。
	rewritten := append(
		[]any{map[string]any{"role": "system", "content": systemPrompt}},
		kept...,
	)
	obj["messages"] = rewritten
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// systemMessage 是网关插入的那一条消息的固定形态。
//
// 用结构体而不是 map：encoding/json 对 map 按键名排序，会输出
// {"content":...,"role":...}。而这里字段顺序是**可观测量**（测试、日志、
// 人工 review 都在看它），契约要求的就是 {"role":"system","content":...}。
type systemMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// messageHead 是判断一条消息所需的最小视图。
//
// Content 保留原始 JSON 而不解成字符串：它可能是多模态数组，
// 那类块仍算"system 块"，但不参与查重（非字符串不比）。
type messageHead struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// parseMessageHead 解析单条消息的 role 与原始 content。
// 非 JSON 对象（数字、字符串、null）返回 false。
func parseMessageHead(raw json.RawMessage) (messageHead, bool) {
	var head messageHead
	if err := json.Unmarshal(raw, &head); err != nil {
		return messageHead{}, false
	}
	return head, true
}

// isInstructionHead 报告这条消息是否属于指令层（system / developer）。
//
// developer 是 OpenAI 新规范里 system 的别名，上游按 system 级指令处理。
// 插入点必须把它算进开头块，否则网关 system 会插到客户端 developer 之前，
// 语义上变成"被客户端指令覆盖"—— 那正好是本模式要避免的事。
func isInstructionHead(head messageHead) bool {
	return head.Role == "system" || head.Role == "developer"
}

// instructionBlockEnd 返回 messages 开头连续 system / developer 块的结束下标；
// 开头就不是指令层时返回 0，也就是插到 messages[0] 之前。
func instructionBlockEnd(messages []json.RawMessage) int {
	for i, raw := range messages {
		head, ok := parseMessageHead(raw)
		if !ok || !isInstructionHead(head) {
			return i
		}
	}
	return len(messages)
}

// containsSystemPrompt 报告 messages 里是否已存在 content 与提示词**完全相同**的
// system / developer 消息，即"这一条我们插过了"。
func containsSystemPrompt(messages []json.RawMessage, systemPrompt string) bool {
	for _, raw := range messages {
		head, ok := parseMessageHead(raw)
		if !ok || !isInstructionHead(head) {
			continue
		}
		var content string
		if err := json.Unmarshal(head.Content, &content); err != nil {
			continue // 多模态数组 / 字段缺失 / null：不是我们插入的形态，不参与比较
		}
		if content == systemPrompt {
			return true
		}
	}
	return false
}

// marshalWithoutHTMLEscape 序列化 v，但不把 < > & 改写成 \u00xx。
//
// json.Marshal 默认开 HTML 转义，而它对 json.RawMessage 同样生效：
// 一条含 "a&b" 的 tool 结果会被改写成 "a\u0026b"。语义没变，
// 但就不再是"逐字不动"了。本函数只负责加一条消息，
// 其余字节不该由它决定。Encoder.Encode 会补一个换行，这里裁掉。
func marshalWithoutHTMLEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Append 在开头连续的 system / developer 块之后插入一条网关 system 消息。
//
// # 语义
//
//   - 插入点 = 开头连续 system / developer 块的**末尾**；开头不是指令层
//     就插在 messages[0] 之前
//   - 客户端原有的 system / developer 与其余所有消息逐字保留
//   - content 是数组（多模态）的 system 块仍算"块"，插入点要跨过去
//   - messages 缺失或不是数组 → 原样返回
//   - 已存在 content 完全相同的 system / developer → 原样返回（幂等）
//
// # 为什么没有 messages 时不退化成 Rewrite
//
// 本模式的契约是"叠加在客户端 system 之后"。没有 messages 就没有可叠加的位置，
// 此时独占插一条 system 会让实际形态等同于 custom —— 调用方选的是 append，
// 拿到的却是 custom。这种"看起来生效了"的错位比什么都不做更难查，
// 所以宁可原样返回，让上层看到"这条请求没被改写"。
//
// # 为什么不用 map[string]any 重新 marshal
//
// 那样会把数字重写（1.50 → 1.5）、把 \u 转义解开、把键序打乱、
// 把 tool_calls 里的 arguments 重新缩进。本函数只该加一条消息，
// 却让整份历史都换了字节：diff 无从审阅，上游的前缀缓存也会整段失效 ——
// 代价是**每一轮对话**都要重新计费、重新推理。
// 所以外层用 map[string]json.RawMessage、messages 用 []json.RawMessage 承载，
// 只序列化新插入的那一条，其余原样放回。
//
// # 为什么插入前必须查重（幂等）
//
// 同一个 body 可能被走多遍（重试、降级重放、上层再次包装），
// 而每次插入都会让 messages 多一条 system。不去重的话，一次失败重试
// 就让同一条指令出现三次 —— 既白付 token，又让模型看到"被反复强调"的指令
// 而改变行为。所以只要已存在 content 完全相同的 system / developer，
// 就认为插过了。重复调用必须得到完全相同的字节。
//
// # 为什么失败路径都返回入参本身
//
// 与 Rewrite 同一条理由：这里位于出站关键路径，职责是"优化"而不是"校验"。
// 解析失败、messages 形态不认识，都让上游按原始语义去诊断，
// 而不是在改写层编一个客户端看不懂的错误。
func Append(body []byte, systemPrompt string) []byte {
	if len(body) == 0 || systemPrompt == "" {
		return body
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return body
	}
	rawMessages, ok := envelope["messages"]
	if !ok {
		return body
	}
	// messages 必须**真的是**数组。只靠 Unmarshal 判不出来：它对 null 不报错，
	// 会解出 nil 切片，于是 {"messages":null} 会被当成"空数组"，
	// 从而插出一条客户端根本没请求过的 system —— 那是篡改，不是叠加。
	if trimmed := bytes.TrimLeft(rawMessages, " \t\r\n"); len(trimmed) == 0 || trimmed[0] != '[' {
		return body
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(rawMessages, &messages); err != nil {
		return body
	}
	if containsSystemPrompt(messages, systemPrompt) {
		return body
	}
	inserted, err := marshalWithoutHTMLEscape(systemMessage{Role: "system", Content: systemPrompt})
	if err != nil {
		return body
	}
	at := instructionBlockEnd(messages)
	merged := make([]json.RawMessage, 0, len(messages)+1)
	merged = append(merged, messages[:at]...)
	merged = append(merged, inserted)
	merged = append(merged, messages[at:]...)
	rawMerged, err := marshalWithoutHTMLEscape(merged)
	if err != nil {
		return body
	}
	envelope["messages"] = rawMerged
	out, err := marshalWithoutHTMLEscape(envelope)
	if err != nil {
		return body
	}
	return out
}
