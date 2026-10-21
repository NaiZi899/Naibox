package custom

import (
	"encoding/json"
	"strings"
	"testing"
)

// forceStream 存在的唯一理由：出口层只认 SSE，而上游按请求体的 stream 决定返回形态。
// 两者的判据必须一致 —— 不一致的后果是"经自定义上游的非流式请求全部 502"，
// 而那个错误在下游只表现为 `upstream stream contained no valid data events`，
// 跟"上游没返回内容"长得一样，极难定位。
func TestForceStreamSetsTrue(t *testing.T) {
	in := []byte(`{"model":"m","messages":[],"stream":false}`)
	out, err := forceStream(in)
	if err != nil {
		t.Fatalf("forceStream 返回错误: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("输出不是合法 JSON: %v（out=%s）", err, out)
	}
	if v, ok := got["stream"].(bool); !ok || !v {
		t.Errorf("stream 应被置为 true，实际 %v", got["stream"])
	}
	// 其余字段必须原样保留：漏掉 model/messages 会让上游直接 400。
	if got["model"] != "m" {
		t.Errorf("model 被改动: %v", got["model"])
	}
	if _, ok := got["messages"]; !ok {
		t.Error("messages 丢失")
	}
}

// 缺 stream 字段（等价于 false）时也要补上。
func TestForceStreamMissingField(t *testing.T) {
	out, err := forceStream([]byte(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("forceStream 返回错误: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	if v, ok := got["stream"].(bool); !ok || !v {
		t.Errorf("缺失的 stream 应被补成 true，实际 %v", got["stream"])
	}
}

// 已是 true 时**逐字节不变**。
//
// 这条是硬要求：部分上游对请求体做指纹/签名，重新序列化会改变字段顺序与空白，
// 可能让本来能通过的请求失败。
func TestForceStreamAlreadyTrueKeepsBytes(t *testing.T) {
	// 故意用非规范写法（多余空格、非常规字段顺序）。
	in := []byte(`{ "messages" : [], "stream":true,   "model":"m" }`)
	out, err := forceStream(in)
	if err != nil {
		t.Fatalf("forceStream 返回错误: %v", err)
	}
	if string(out) != string(in) {
		t.Errorf("stream 已是 true 时不应改动字节\n in=%s\nout=%s", in, out)
	}
}

// 非 JSON 输入：原样返回 + 错误，不试图"修好"它。
//
// 否则会把一个明确的上游协议错误，变成"看起来格式正确但语义缺失"的请求。
func TestForceStreamInvalidJSONPassesThrough(t *testing.T) {
	in := []byte(`not json at all`)
	out, err := forceStream(in)
	if err == nil {
		t.Error("非法 JSON 应返回错误")
	}
	if string(out) != string(in) {
		t.Errorf("非法 JSON 应原样返回\n in=%s\nout=%s", in, out)
	}
}

// 空 body：返回错误，不做任何事。
func TestForceStreamEmpty(t *testing.T) {
	out, err := forceStream(nil)
	if err == nil {
		t.Error("空 body 应返回错误")
	}
	if len(out) != 0 {
		t.Errorf("空 body 应原样返回，实际 %q", out)
	}
}

// 流式请求不该被改写 —— 钉住 Chat 里调用它不会破坏流式这条契约。
func TestForceStreamLeavesStreamingRequestIntact(t *testing.T) {
	in := []byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	out, err := forceStream(in)
	if err != nil {
		t.Fatalf("forceStream 返回错误: %v", err)
	}
	if string(out) != string(in) {
		t.Errorf("流式请求体被改动\n in=%s\nout=%s", in, out)
	}
	if !strings.Contains(string(out), `"stream":true`) {
		t.Error("流式请求体的 stream 应为 true")
	}
}
