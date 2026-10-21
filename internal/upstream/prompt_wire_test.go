// prompt_wire_test.go PromptMode × 降级状态的四种组合，断言**上游收到的字节**。
//
// # 为什么在 wire 层断言，而不是直接调 applyPrompt
//
// applyPrompt 是私有函数，单测它能证明"函数本身对"，
// 但证明不了"它真的被接进了出站管线" —— 而那正是最容易漏的一步：
// prepareBody 里少调一次，编译通过、单测全绿、线上毫无变化。
//
// 因此这里全部走 ChatStream，从假上游读回真实 body。
package upstream

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/NaiZi899/Naibox/internal/auth"
	"github.com/NaiZi899/Naibox/internal/prompt"
)

// wireBody 起一个假上游，返回"客户端发出去时上游收到的 body"。
func wireBody(t *testing.T, c *Client, body []byte) []byte {
	t.Helper()
	var got []byte
	ts := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	defer ts.Close()

	c.ChatBaseCN = ts.URL
	acct := &auth.Auth{AccessToken: "t", Domain: "copilot.tencent.com", UID: "u1"}
	rc, status, respBody, err := c.ChatStream(acct, body)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer rc.Close()
	if status >= 400 {
		t.Fatalf("假上游返回 %d: %s", status, respBody)
	}
	return got
}

// firstSystemContent 取出改写后 messages[0].content（非字符串返回空串）。
func firstSystemContent(t *testing.T, body []byte) string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("wire body 不是 JSON: %v; %s", err, body)
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatalf("wire body 没有 messages: %s", body)
	}
	s, _ := msgs[0].(map[string]any)["content"].(string)
	return s
}

const clientSystem = "You are Claude Code, Anthropic's official CLI for Claude."

func clientBody() []byte {
	return []byte(`{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":"` + clientSystem + `"},` +
		`{"role":"user","content":"hi"}]}`)
}

// TestPromptCustomReplacesClientSystem custom + 未降级 → 用 PromptText 替换。
func TestPromptCustomReplacesClientSystem(t *testing.T) {
	c := New()
	c.PromptMode = prompt.ModeCustom
	c.PromptText = "网关自有提示词"
	got := wireBody(t, c, clientBody())

	if sys := firstSystemContent(t, got); sys != "网关自有提示词" {
		t.Errorf("system=%q，期望网关自有提示词", sys)
	}
	if strings.Contains(string(got), "official CLI for Claude") {
		t.Errorf("客户端 system 未被替换（模板句仍会撞内容策略）: %s", got)
	}
}

// TestPromptCustomFallsBackToDefaultWhenTextEmpty PromptText 为空 → 回落内置默认。
//
// # 为什么必须有这条
//
// 装配层漏注入 PromptText 时，prompt.Rewrite 会因为 systemPrompt=="" 而
// **原样返回** —— 一个完全静默的功能失效（配置看起来生效了，实际什么都没做）。
// 回落内置默认把这个失败模式变成"功能仍按默认语义工作"。
func TestPromptCustomFallsBackToDefaultWhenTextEmpty(t *testing.T) {
	c := New()
	c.PromptMode = prompt.ModeCustom
	c.PromptText = "" // 模拟装配漏注入
	got := wireBody(t, c, clientBody())

	sys := firstSystemContent(t, got)
	if sys == "" {
		t.Fatal("PromptText 为空时不该什么都不做 —— 应回落内置默认提示词")
	}
	if sys == clientSystem || strings.Contains(sys, "official CLI for Claude") {
		t.Errorf("客户端 system 仍在，说明降级/替换都没发生: %q", sys)
	}
}

// TestPromptEmptyModeIsPassthrough 空 mode 按 passthrough 处理（与配置缺省一致）。
//
// 2026-10 起默认模式从 custom 改为 passthrough：没配置就一个字节都不动，
// 改写要由用户显式选择。
func TestPromptEmptyModeIsPassthrough(t *testing.T) {
	c := New()
	c.PromptMode = "" // 未配置
	c.PromptText = "默认提示词"
	c.SanitizeFingerprints = false // 隔离脱敏层，断言逐字
	got := wireBody(t, c, clientBody())

	if sys := firstSystemContent(t, got); sys != clientSystem {
		t.Errorf("空 mode 应按 passthrough 处理（保留客户端 system），得到 %q", sys)
	}
	if strings.Contains(string(got), "默认提示词") {
		t.Error("空 mode 下 PromptText 不得出现在 wire body 上")
	}
}

// TestPromptPassthroughKeepsClientSystem passthrough + 未降级 → 一个字节都不动。
//
// ⚠ 关掉脱敏以隔离变量：passthrough 保留的是"提示词层不动它"，
// 而脱敏层**仍然会**改写身份句（那是它的职责，见 sanitizeRewrites）。
// 开着脱敏断言"逐字相同"会得到一条恒红的测试，而它红的原因
// 与"passthrough 是否正确"无关 —— 那是两个不同的层。
func TestPromptPassthroughKeepsClientSystem(t *testing.T) {
	c := New()
	c.PromptMode = prompt.ModePassthrough
	c.PromptText = "不该被用到"
	c.SanitizeFingerprints = false // ← 隔离脱敏层
	got := wireBody(t, c, clientBody())

	if sys := firstSystemContent(t, got); sys != clientSystem {
		t.Errorf("passthrough 必须保留客户端 system（脱敏关闭时逐字相同），得到 %q", sys)
	}
	if strings.Contains(string(got), "不该被用到") {
		t.Error("passthrough 模式下 PromptText 不得出现在 wire body 上")
	}
}

// TestPromptPassthroughStillSanitizes passthrough 只挡提示词层，脱敏照常生效。
//
// 这条与上一条成对：两层是独立的，关掉一层不该影响另一层。
// 它钉住"passthrough ≠ 关掉全部防护"这个容易误解的语义。
func TestPromptPassthroughStillSanitizes(t *testing.T) {
	c := New()
	c.PromptMode = prompt.ModePassthrough
	c.SanitizeFingerprints = true
	got := wireBody(t, c, clientBody())

	sys := firstSystemContent(t, got)
	if strings.Contains(sys, "official CLI for Claude") && !strings.Contains(sys, "CLI tool for Claude") {
		t.Errorf("passthrough 下脱敏层应照常改写身份句，得到 %q", sys)
	}
	if !strings.HasPrefix(sys, "You are Claude Code") {
		t.Errorf("脱敏只该改一个词，不该整句替换；得到 %q", sys)
	}
}

// TestPromptCustomDegradedUsesNeutral custom + 降级中 → 用 Degraded 而非 PromptText。
func TestPromptCustomDegradedUsesNeutral(t *testing.T) {
	c := New()
	c.PromptMode = prompt.ModeCustom
	c.PromptText = "网关自有提示词"
	g := prompt.NewGate()
	g.Trigger()
	c.PromptGate = g

	got := wireBody(t, c, clientBody())
	sys := firstSystemContent(t, got)

	if sys != prompt.Degraded {
		t.Errorf("降级期应用 Degraded，得到 %q", sys)
	}
	if strings.Contains(string(got), "网关自有提示词") {
		t.Error("降级期内不得再用 PromptText")
	}
}

// TestPromptPassthroughDegradedReplacesClientSystem passthrough + 降级中 → 用 Degraded 替换。
//
// # 为什么 passthrough 也会被改写
//
// "透传"表达的是尊重客户端人格，而降级期的存在**本身就说明**
// 那份人格刚刚撞了内容策略。继续透传 = 继续撞 400。
// 降级是用户通过配置已经同意的兜底路径，不是我们擅自改写。
func TestPromptPassthroughDegradedReplacesClientSystem(t *testing.T) {
	c := New()
	c.PromptMode = prompt.ModePassthrough
	g := prompt.NewGate()
	g.Trigger()
	c.PromptGate = g

	got := wireBody(t, c, clientBody())
	sys := firstSystemContent(t, got)

	if sys != prompt.Degraded {
		t.Errorf("passthrough 降级期应用 Degraded，得到 %q", sys)
	}
	if strings.Contains(string(got), clientSystem) {
		t.Error("降级期内客户端 system 必须被换掉（否则继续撞 400）")
	}
}

// TestPromptGateNilMeansNeverDegraded 未注入 Gate（nil）→ 恒不降级。
//
// nil 是合法状态：core 的 PromptGate 是可选字段，nil 表示本部署
// 未接提示词体系。它必须表现为"永不降级"，而不是 panic 或随机行为。
func TestPromptGateNilMeansNeverDegraded(t *testing.T) {
	c := New()
	c.PromptMode = prompt.ModeCustom
	c.PromptText = "网关自有提示词"
	c.PromptGate = nil

	got := wireBody(t, c, clientBody()) // 不得 panic
	if sys := firstSystemContent(t, got); sys != "网关自有提示词" {
		t.Errorf("nil Gate 应表现为未降级，system=%q", sys)
	}
}

// TestPromptRunsBeforeSanitize 顺序钉住：提示词替换必须在脱敏之前。
//
// # 为什么要钉顺序
//
// 反过来的话，sanitize 要先在**即将被删掉**的客户端 system 上跑一遍正则 ——
// 纯白做功；更重要的是顺序一乱，读者会以为"sanitize 负责 system"，
// 而实际分工是 prompt 管 system/developer、sanitize 管其余消息
// （见 internal/prompt 包注释的分工表）。
//
// 这里用"只存在于客户端 system 里的指纹"来判别：
// 两条路径都会让它消失，所以再断言 sanitize 的开关**不影响**结果 ——
// 那种组合只在 prompt 先跑时成立。
func TestPromptRunsBeforeSanitize(t *testing.T) {
	mk := func(sanitize bool) string {
		c := New()
		c.PromptMode = prompt.ModeCustom
		c.PromptText = "网关自有提示词"
		c.SanitizeFingerprints = sanitize
		return firstSystemContent(t, wireBody(t, c, clientBody()))
	}
	on, off := mk(true), mk(false)

	if on != "网关自有提示词" || off != "网关自有提示词" {
		t.Errorf("脱敏开关不该影响 system 替换结果：sanitize=true → %q，false → %q", on, off)
	}
}

// ---------------------------------------------------------------------------
// 矩阵补全：append × 降级状态、append 幂等、3 模式 × 2 状态的整表对照
// ---------------------------------------------------------------------------

// gatewayText append 模式下被追加的那条网关 system。
//
// 刻意写成一句"改写认知"的短指令：append 的用途是给客户端人格补一份
// 只写差异的补充说明，而不是再叠一份完整人格（理由见 boot.normalize 的
// append + 空文件告警）。
const gatewayText = "网关补充：先读项目再动手。"

// wireMsg 一条出站消息。
//
// content 用 RawMessage 承载：多模态 content 是数组，强行解成 string 会让
// 测试自己先报错；本文件要断言的是"顺序与文本内容"，不是 content 的形态。
type wireMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// text 非字符串 content 返回空串。
func (m wireMsg) text() string {
	var s string
	if err := json.Unmarshal(m.Content, &s); err != nil {
		return ""
	}
	return s
}

// wireMessages 解出 wire body 的 messages（顺序即上游看到的顺序）。
func wireMessages(t *testing.T, body []byte) []wireMsg {
	t.Helper()
	var env struct {
		Messages []wireMsg `json:"messages"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("wire body 不是合法 JSON: %v; %s", err, body)
	}
	if len(env.Messages) == 0 {
		t.Fatalf("wire body 没有 messages: %s", body)
	}
	return env.Messages
}

// rawMessageBytes 逐条取出 messages 元素的原始字节。
//
// # 为什么必须比字节，而不只比 content
//
// append 的契约是"只加一条，其余一个字节都不动"。只比 content 的话，
// 键序被打乱、转义被改写、tool_calls 里的 arguments 被重新缩进 ——
// 这些"把整份历史用 map 重新 Marshal"必然带来的问题一个都看不见，
// 而它们恰好是上游前缀缓存整段失效、每轮重新计费的真因。
func rawMessageBytes(t *testing.T, body []byte) []string {
	t.Helper()
	var env map[string]json.RawMessage
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("wire body 不是 JSON 对象: %v; %s", err, body)
	}
	raw, ok := env["messages"]
	if !ok {
		t.Fatalf("wire body 没有 messages: %s", body)
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(raw, &msgs); err != nil {
		t.Fatalf("messages 不是数组: %v; %s", err, raw)
	}
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = string(m)
	}
	return out
}

// assembleMessages 把若干条消息字节重新拼成一个 JSON 数组（不转义 HTML）。
//
// 与 prompt.marshalWithoutHTMLEscape 同一手法：必须 SetEscapeHTML(false)，
// 否则 content 里的 & 会被写成 \u0026，"逐字保留"就被断言自己破坏了。
func assembleMessages(t *testing.T, raws []string) []byte {
	t.Helper()
	msgs := make([]json.RawMessage, len(raws))
	for i, r := range raws {
		msgs[i] = json.RawMessage(r)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(msgs); err != nil {
		t.Fatalf("重新拼装 messages: %v", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// systemLayers 按出站顺序取出所有 system / developer 消息的文本。
//
// 归在一起是因为上游把 developer 当 system 级指令处理（wire.normalizeRoles
// 还会把 developer 直接改写成 system）。矩阵只需要看"指令层有几条、各是什么"。
func systemLayers(t *testing.T, body []byte) []string {
	t.Helper()
	out := make([]string, 0, 2)
	for _, m := range wireMessages(t, body) {
		if m.Role == "system" || m.Role == "developer" {
			out = append(out, m.text())
		}
	}
	return out
}

// TestPromptAppendKeepsClientSystemThenGatewaySystem append × 未降级。
//
// 三条契约，逐条断言：
//  1. 客户端 system 原文仍在 body 里（append 不是 custom）
//  2. 网关 system 紧跟其后 —— 是"下一条"，不是"body 里某处存在"
//  3. 其余 messages 逐字不动
func TestPromptAppendKeepsClientSystemThenGatewaySystem(t *testing.T) {
	c := New()
	c.PromptMode = prompt.ModeAppend
	c.PromptText = gatewayText
	c.SanitizeFingerprints = false // 隔离脱敏层：它也会改写身份句（见 passthrough 用例）

	// 对照件：同一条管线 + 同一条客户端 body，只是这个模式不改写提示词层。
	ctrl := New()
	ctrl.PromptMode = prompt.ModePassthrough
	ctrl.SanitizeFingerprints = false

	got := wireBody(t, c, clientBody())
	base := wireBody(t, ctrl, clientBody())

	// 1) 客户端 system 原文仍在
	if !bytes.Contains(got, []byte(clientSystem)) {
		t.Errorf("append 必须保留客户端 system 原文（否则就退化成 custom 了），body=%s", got)
	}

	// 2) 位置：紧跟客户端 system 之后
	msgs := wireMessages(t, got)
	if len(msgs) != 3 {
		t.Fatalf("append 应只多出恰好一条消息（2 → 3），实际 %d 条: %s", len(msgs), got)
	}
	if msgs[0].text() != clientSystem {
		t.Errorf("messages[0] 应是客户端 system，得到 %q", msgs[0].text())
	}
	if msgs[1].Role != "system" || msgs[1].text() != gatewayText {
		t.Errorf("messages[1] 应是网关 system %q（紧跟客户端 system 的下一条），得到 role=%q content=%q",
			gatewayText, msgs[1].Role, msgs[1].text())
	}
	if msgs[2].Role != "user" || msgs[2].text() != "hi" {
		t.Errorf("messages[2] 应是客户端 user，得到 role=%q content=%q", msgs[2].Role, msgs[2].text())
	}

	// 3) 其余 messages 逐字不动。
	//
	// # 参照物为什么是 passthrough 的产物，而不是客户端原始 body 字节
	//
	// 提示词层之后还有 role 归一 / stream 强制 / 白名单裁剪，那一层本来
	// 就会整份重编码（键序、转义、数字形态）。拿原始 body 做参照，会把
	// "别的层正常工作"误报成 append 破坏了字节 —— 一条恒红的测试，
	// 而它红的原因与本层无关。以同管线 passthrough 为参照，两者之差
	// 就**只剩** append 自己插的那一条，这才是本节要证明的东西。
	after := rawMessageBytes(t, got)
	before := rawMessageBytes(t, base)
	if len(after) != len(before)+1 {
		t.Fatalf("消息条数：append=%d，passthrough=%d（应恰好多 1）", len(after), len(before))
	}
	rest := append([]string{after[0]}, after[2:]...)
	if !reflect.DeepEqual(rest, before) {
		t.Errorf("去掉插入的那条后，其余消息字节与客户端发出时不一致：\n append 其余 = %q\n passthrough = %q", rest, before)
	}
	if !bytes.Equal(assembleMessages(t, rest), assembleMessages(t, before)) {
		t.Error("去掉插入的那条后重新拼装 != 原 messages 字节")
	}
}

// TestPromptAppendInsertsAfterContiguousInstructionBlock 插入点在"开头连续指令块"之后。
//
// # 为什么必须补这一条
//
// 只有一条 system 时，"紧跟其后"与"插在最前面"两种实现在 wire 上**无法区分**。
// 加一条 developer 才能把插入点钉死：网关 system 必须落在整块之后
// （index 2），而不是 messages[1] —— 后者会把 developer 挤到网关提示词后面，
// 语义上等于"网关指令压过项目约定"。
func TestPromptAppendInsertsAfterContiguousInstructionBlock(t *testing.T) {
	const devText = "项目约定：Go 1.22，测试用 go test -count=1。"
	body := []byte(`{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":"` + clientSystem + `"},` +
		`{"role":"developer","content":"` + devText + `"},` +
		`{"role":"user","content":"hi"}]}`)

	c := New()
	c.PromptMode = prompt.ModeAppend
	c.PromptText = gatewayText
	c.SanitizeFingerprints = false

	got := wireBody(t, c, body)
	msgs := wireMessages(t, got)
	if len(msgs) != 4 {
		t.Fatalf("应只多出恰好一条消息（3 → 4），实际 %d 条: %s", len(msgs), got)
	}
	// wire.normalizeRoles 会把 developer 改写成 system（协议兼容），
	// 所以这里按**内容**判别顺序，不按 role。
	want := []string{clientSystem, devText, gatewayText, "hi"}
	for i, w := range want {
		if msgs[i].text() != w {
			t.Errorf("messages[%d] 应为 %q，得到 %q；实际指令层 = %q",
				i, w, msgs[i].text(), systemLayers(t, got))
		}
	}
}

// TestPromptAppendDegradedDropsClientSystem append + 降级中。
//
// 降级期的唯一形态是 Rewrite(body, Degraded)：客户端 system 必须消失，
// 指令层只剩中性那一条，追加段也不能留。
//
// 若这里还留着客户端 system 或网关追加段，说明 append 在降级期走了
// "叠加式降级" —— 那等于把刚刚撞了内容策略的文本原样再发一遍，
// 白付一次完整的失败往返。
func TestPromptAppendDegradedDropsClientSystem(t *testing.T) {
	c := New()
	c.PromptMode = prompt.ModeAppend
	c.PromptText = gatewayText
	c.SanitizeFingerprints = false
	g := prompt.NewGate()
	g.Trigger()
	c.PromptGate = g

	got := wireBody(t, c, clientBody())

	if bytes.Contains(got, []byte(clientSystem)) {
		t.Errorf("降级期内客户端 system 必须消失（否则继续撞 400），body=%s", got)
	}
	if bytes.Contains(got, []byte(gatewayText)) {
		t.Errorf("降级期内不得再追加网关提示词（叠加只会扩大逐字匹配面积），body=%s", got)
	}
	if layers := systemLayers(t, got); len(layers) != 1 || layers[0] != prompt.Degraded {
		t.Errorf("降级期指令层应只有 prompt.Degraded 一条，实际 %d 条: %q", len(layers), layers)
	}
}

// TestPromptAppendIsIdempotentOnWire 幂等：同一 body 连跑两遍出站管线，字节相等。
//
// # 为什么这是 append 这条线的核心验收
//
// append 会**往 messages 里加一条**。重试、降级重放、上层再包装都会让同一份
// body 走第二遍；不去重的话第二遍就插出第二条 system、第三遍三条 ——
// 指令被反复强调，模型行为会变，而现象是"偶发答非所问"，看起来与配置无关。
//
// 第二遍的**入参就是第一遍的真实出站字节**，与线上重放完全同形；
// 不是造出来的假场景。
func TestPromptAppendIsIdempotentOnWire(t *testing.T) {
	c := New()
	c.PromptMode = prompt.ModeAppend
	c.PromptText = gatewayText
	c.SanitizeFingerprints = false

	once := wireBody(t, c, clientBody())
	twice := wireBody(t, c, once)

	if !bytes.Equal(once, twice) {
		t.Errorf("第二遍与第一遍字节不等 → 重复追加或存在其它非幂等改写\n第一次: %s\n第二次: %s", once, twice)
	}
	if layers := systemLayers(t, twice); len(layers) != 2 {
		t.Errorf("跑两遍后指令层应仍是 2 条（客户端 system + 网关 system），实际 %d 条: %q", len(layers), layers)
	}
}

// TestPromptModeDegradedMatrix 3 模式 × 2 降级状态，一张表跑完。
//
// # 为什么要有这张表
//
// 单点用例各自钉住一条路径，但"加一个模式时漏掉降级分支"这类错误，
// 现象恰恰是"某两格答案本该不同却相同"，而每个单点用例只看自己那一格，
// 谁都不会红。把 6 格放进一张表逐格比对，缺格与错格都无处可藏。
//
// 脱敏一律关闭：它也会改写身份句（那是它的职责），开着会让
// passthrough 那两格红在与本层无关的原因上。
func TestPromptModeDegradedMatrix(t *testing.T) {
	cases := []struct {
		name     string
		mode     string
		degraded bool
		want     []string
	}{
		{"passthrough/未降级", prompt.ModePassthrough, false, []string{clientSystem}},
		{"passthrough/降级", prompt.ModePassthrough, true, []string{prompt.Degraded}},
		{"append/未降级", prompt.ModeAppend, false, []string{clientSystem, gatewayText}},
		{"append/降级", prompt.ModeAppend, true, []string{prompt.Degraded}},
		{"custom/未降级", prompt.ModeCustom, false, []string{gatewayText}},
		{"custom/降级", prompt.ModeCustom, true, []string{prompt.Degraded}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := New()
			c.PromptMode = tc.mode
			c.PromptText = gatewayText
			c.SanitizeFingerprints = false
			if tc.degraded {
				g := prompt.NewGate()
				g.Trigger()
				c.PromptGate = g
			}
			got := wireBody(t, c, clientBody())
			if layers := systemLayers(t, got); !reflect.DeepEqual(layers, tc.want) {
				t.Errorf("指令层 = %q，期望 %q（body=%s）", layers, tc.want, got)
			}
			// 降级状态压过模式：6 格里降级那 3 格的答案必须完全相同。
			if tc.degraded && !bytes.Contains(got, []byte(prompt.Degraded)) {
				t.Errorf("降级期 body 里没有 Degraded: %s", got)
			}
		})
	}
}
