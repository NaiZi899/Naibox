// append_test.go Append（叠加模式）的判据锁定。
//
// # 这一层的失败模式
//
// 全是静默的：插入点错了 → 网关 system 被客户端指令覆盖，模型不遵守网关人格，
// 但没有任何报错；幂等坏了 → 重试一次就多一条 system，token 翻倍、
// 模型行为漂移；重新 marshal → 历史消息的数字、转义、键序全被改写，
// 上游前缀缓存整段失效（每轮都重新计费），而请求本身看起来一切正常。
//
// 所以每条边界都要有断言，且"逐字不动"必须按字节断言，不能只比 JSON 语义。
package prompt

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// appendRawMessages 取出站 body 的 messages 原始值。
//
// 逐条保留 RawMessage 而不是解成结构体切片：写入层允许出现非对象的消息
// （那条"首条不是对象"的用例就是），用结构体切片解会在测试辅助函数里先炸，
// 反而测不到被测代码对畸形输入的处理。
func appendRawMessages(t *testing.T, body []byte) []json.RawMessage {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("解析 body 失败: %v body=%s", err, body)
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(obj["messages"], &msgs); err != nil {
		t.Fatalf("解析 messages 失败: %v body=%s", err, body)
	}
	return msgs
}

// appendRoles 取出站 body 的角色序列，用于锁定插入点。非对象消息记为空串。
func appendRoles(t *testing.T, body []byte) []string {
	t.Helper()
	msgs := appendRawMessages(t, body)
	out := make([]string, len(msgs))
	for i, m := range msgs {
		var head struct {
			Role string `json:"role"`
		}
		if err := json.Unmarshal(m, &head); err == nil {
			out[i] = head.Role
		}
	}
	return out
}

// appendContents 取出站 body 的 content 序列；非字符串 content 记为空串
// （多模态数组的内容由调用方直接看原始字节）。
func appendContents(t *testing.T, body []byte) []string {
	t.Helper()
	msgs := appendRawMessages(t, body)
	out := make([]string, len(msgs))
	for i, m := range msgs {
		var head struct {
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(m, &head); err != nil {
			continue
		}
		var s string
		if err := json.Unmarshal(head.Content, &s); err == nil {
			out[i] = s
		}
	}
	return out
}

// TestAppendInsertsAfterLeadingInstructionBlock 插入点 = 开头连续 system/developer
// 块的末尾；开头不是指令层就插到最前。
//
// 钉住的是"块"这个语义而不是"第一条"：客户端常见形态是 system 后面再跟一条
// developer 补充，若只跨过第一条，网关 system 会被夹在客户端的两段指令中间，
// 上游按"后出现的覆盖先出现的"处理时，我们的指令就被压掉了。
func TestAppendInsertsAfterLeadingInstructionBlock(t *testing.T) {
	const sys = "网关叠加提示词"
	cases := []struct {
		name     string
		in       string
		wantRole []string
		wantAt   int
	}{
		{
			name:     "开头无 system",
			in:       `{"messages":[{"role":"user","content":"hi"}]}`,
			wantRole: []string{"system", "user"},
			wantAt:   0,
		},
		{
			name:     "单条 system",
			in:       `{"messages":[{"role":"system","content":"客户端人格"},{"role":"user","content":"hi"}]}`,
			wantRole: []string{"system", "system", "user"},
			wantAt:   1,
		},
		{
			name:     "system+developer+system 连续块",
			in:       `{"messages":[{"role":"system","content":"A"},{"role":"developer","content":"B"},{"role":"system","content":"C"},{"role":"user","content":"hi"}]}`,
			wantRole: []string{"system", "developer", "system", "system", "user"},
			wantAt:   3,
		},
		{
			name:     "system 在中间不算开头块",
			in:       `{"messages":[{"role":"user","content":"hi"},{"role":"system","content":"中途指令"}]}`,
			wantRole: []string{"system", "user", "system"},
			wantAt:   0,
		},
		{
			name:     "messages 为空数组",
			in:       `{"messages":[]}`,
			wantRole: []string{"system"},
			wantAt:   0,
		},
		{
			name:     "全是 system（无 user）",
			in:       `{"messages":[{"role":"system","content":"a"}]}`,
			wantRole: []string{"system", "system"},
			wantAt:   1,
		},
		{
			name:     "首条不是对象：不算指令块",
			in:       `{"messages":[123,{"role":"system","content":"a"}]}`,
			wantRole: []string{"system", "", "system"},
			wantAt:   0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := Append([]byte(c.in), sys)
			got := appendRoles(t, out)
			if len(got) != len(c.wantRole) {
				t.Fatalf("roles=%v want %v body=%s", got, c.wantRole, out)
			}
			for i := range c.wantRole {
				if got[i] != c.wantRole[i] {
					t.Fatalf("roles=%v want %v body=%s", got, c.wantRole, out)
				}
			}
			contents := appendContents(t, out)
			if contents[c.wantAt] != sys {
				t.Errorf("第 %d 条 content=%q，期望网关提示词 %q", c.wantAt, contents[c.wantAt], sys)
			}
			if n := strings.Count(string(out), sys); n != 1 {
				t.Errorf("网关提示词出现 %d 次，期望 1 次：%s", n, out)
			}
		})
	}
}

// TestAppendKeepsDeveloperBlockVerbatim developer 块要跨过去，且一个字节都不能动。
//
// developer 是 system 的别名，客户端用它承载"补充指令"。它里面常带模板句，
// 所以这里只能叠加、不能重写 —— 重写就把 append 退化成 custom 了。
func TestAppendKeepsDeveloperBlockVerbatim(t *testing.T) {
	in := []byte(`{"messages":[{"role":"developer","content":"开发者补充指令"},{"role":"user","content":"hi"}]}`)
	out := Append(in, "网关叠加提示词")

	got := appendRoles(t, out)
	want := []string{"developer", "system", "user"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("roles=%v want %v body=%s", got, want, out)
	}
	if !strings.Contains(string(out), `{"role":"developer","content":"开发者补充指令"}`) {
		t.Errorf("developer 消息被改动：%s", out)
	}
}

// TestAppendIsIdempotent 连调多次，字节必须完全不变，且只插一条。
//
// 出站链路会重复改写同一个 body（重试、降级重放、上层再次包装）。
// 每次插入都多一条 system 的话，一次失败重试就能让同一条指令出现三次：
// token 白付，模型还会因为"被反复强调"而改变行为。
func TestAppendIsIdempotent(t *testing.T) {
	const sys = "网关叠加提示词"
	in := []byte(`{"model":"glm-5.2","messages":[{"role":"system","content":"客户端人格"},{"role":"user","content":"hi"}]}`)
	snapshot := append([]byte(nil), in...)

	once := Append(in, sys)
	twice := Append(once, sys)
	thrice := Append(twice, sys)

	if !bytes.Equal(once, twice) {
		t.Fatalf("第二次调用改变了字节：\n once=%s\n twice=%s", once, twice)
	}
	if !bytes.Equal(twice, thrice) {
		t.Fatalf("第三次调用改变了字节：\n twice=%s\n thrice=%s", twice, thrice)
	}
	if n := strings.Count(string(once), sys); n != 1 {
		t.Errorf("网关提示词出现 %d 次，期望 1 次：%s", n, once)
	}
	if got := appendRoles(t, once); len(got) != 3 {
		t.Errorf("messages 长度=%d，期望 3（客户端 system + 网关 system + user）：%s", len(got), once)
	}
	// 改写不得就地修改入参：调用方（出站客户端）可能复用同一份 buffer 重试。
	if !bytes.Equal(in, snapshot) {
		t.Errorf("Append 就地修改了入参：\n before=%s\n after=%s", snapshot, in)
	}
}

// TestAppendAlreadyPresentDeepInHistory 历史里已有同文 system 时也要原样返回。
//
// 查重扫的是整个 messages，不只开头块：会话被多轮回放后，我们那条 system
// 可能已经不在开头（客户端把历史整体搬到了别处），只查开头块就会重复插入。
func TestAppendAlreadyPresentDeepInHistory(t *testing.T) {
	const sys = "网关叠加提示词"
	in := []byte(`{"messages":[{"role":"system","content":"客户端人格"},{"role":"user","content":"hi"},{"role":"system","content":"网关叠加提示词"}]}`)
	out := Append(in, sys)
	if !bytes.Equal(out, in) {
		t.Fatalf("已存在同文 system 时应原样返回，实际改写了：\n in=%s\n out=%s", in, out)
	}
}

// TestAppendFailuresReturnInputVerbatim 所有失败路径都返回**入参本身**。
//
// 与 Rewrite 同一条设计：本函数位于出站关键路径，职责是优化而不是校验。
// 解析不了就让上游按原始语义诊断，别在改写层编一个客户端看不懂的错误。
func TestAppendFailuresReturnInputVerbatim(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		sys  string
	}{
		{"非法 JSON", []byte(`{not valid json`), "SYS"},
		{"空 body", []byte{}, "SYS"},
		{"空提示词", []byte(`{"messages":[{"role":"user","content":"hi"}]}`), ""},
		{"顶层是 null", []byte(`null`), "SYS"},
		{"顶层是数组", []byte(`[{"role":"user","content":"hi"}]`), "SYS"},
		{"messages 缺失", []byte(`{"model":"glm-5.2"}`), "SYS"},
		{"messages 是 null", []byte(`{"messages":null}`), "SYS"},
		{"messages 是字符串", []byte(`{"messages":"nope"}`), "SYS"},
		{"messages 是对象", []byte(`{"messages":{"role":"user"}}`), "SYS"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := Append(c.body, c.sys)
			if !bytes.Equal(out, c.body) {
				t.Errorf("应原样返回入参：got %s want %s", out, c.body)
			}
			// 更强的一条：必须是**同一份底层数组**（没有偷偷重编码）。
			if len(c.body) > 0 && len(out) > 0 && &out[0] != &c.body[0] {
				t.Errorf("返回了新的切片而不是入参本身（重编码会重排键序、丢空白）")
			}
		})
	}
}

// TestAppendMultimodalSystemContentWidensInsertPoint 多模态 system 块仍算块，
// 但不参与查重。
//
// 它不可能"是我们插入的那一条"（我们只插字符串），所以查重要跳过它；
// 但它在插入点计算上必须被跨过，否则网关 system 会插到客户端指令之前。
func TestAppendMultimodalSystemContentWidensInsertPoint(t *testing.T) {
	const sys = "网关叠加提示词"
	in := []byte(`{"messages":[{"role":"system","content":[{"type":"text","text":"客户端多模态指令"}]},{"role":"user","content":"hi"}]}`)
	out := Append(in, sys)

	got := appendRoles(t, out)
	want := []string{"system", "system", "user"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("roles=%v want %v body=%s", got, want, out)
	}
	if !strings.Contains(string(out), `{"type":"text","text":"客户端多模态指令"}`) {
		t.Errorf("多模态 system content 被改动：%s", out)
	}
	if again := Append(out, sys); !bytes.Equal(again, out) {
		t.Errorf("多模态 system 之后的二次调用不幂等：\n once=%s\n twice=%s", out, again)
	}
}

// TestAppendPreservesUntouchedMessagesByteForByte 未插入的消息逐字保留。
//
// 逐字断言三个具体的东西：
//
//	1.50   数字不被规范化成 1.5
//	\u4f60  转义不被解开
//	& <tag> HTML 转义没有把 & 改写成 \u0026（json.Marshal 默认会）
//
// 这三条一起失效的后果是"每轮请求都换字节"：上游的前缀缓存整段失效，
// 用户为此多付全部历史 token，而请求本身看起来完全正常。
func TestAppendPreservesUntouchedMessagesByteForByte(t *testing.T) {
	in := []byte(`{"model":"glm-5.2","stream":true,"messages":[
{"role":"user","content":"温度 1.50 \u4f60 & <tag>"},
{"role":"assistant","content":"ok","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"n\":1.5}"}}]},
{"role":"tool","tool_call_id":"c1","content":"结果 & 完成"}
]}`)
	out := Append(in, "网关叠加提示词")

	if !strings.Contains(string(out), `{"role":"user","content":"温度 1.50 \u4f60 & <tag>"}`) {
		t.Errorf("user 消息未逐字保留：%s", out)
	}
	if !strings.Contains(string(out), `{"role":"tool","tool_call_id":"c1","content":"结果 & 完成"}`) {
		t.Errorf("tool 消息未逐字保留：%s", out)
	}
	if n := strings.Count(string(out), "1.50"); n != 1 {
		t.Errorf("数字 1.50 出现 %d 次，期望 1 次（被规范化成 1.5 了？）：%s", n, out)
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("输出不是合法 JSON: %v body=%s", err, out)
	}
	// 顶层其它字段（含 stream 这种布尔、model 这种字符串）不得丢失或改写。
	if string(obj["model"]) != `"glm-5.2"` || string(obj["stream"]) != `true` {
		t.Errorf("顶层字段被改动：model=%s stream=%s", obj["model"], obj["stream"])
	}
	// tool_calls 必须整体保留：丢了它，tool 结果消息就成了孤儿，
	// 上游会因为"找不到对应的 tool_call"报参数错误。
	if !strings.Contains(string(out), `"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"n\":1.5}"}}]`) {
		t.Errorf("assistant.tool_calls 被改动：%s", out)
	}
}

// TestAppendInsertedMessageShape 插入的形态固定为 {"role":"system","content":...}。
//
// 键序是可观测量（日志、测试、人工 review 都在看它）；用 map 承载会被
// encoding/json 排成 content 在前，与契约不符。
func TestAppendInsertedMessageShape(t *testing.T) {
	out := Append([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), "SYS-固定形态")
	if !strings.Contains(string(out), `{"role":"system","content":"SYS-固定形态"}`) {
		t.Errorf("插入形态不符：%s", out)
	}
}
