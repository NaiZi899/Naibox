// policy_test.go Inject 的真值表锁定。
//
// # 为什么这张表必须逐格断言
//
// Inject 是本包唯一的**路由**入口：mode 与降级状态两维一错，后果都不是报错，
// 而是"形态不对但能跑"。两种错法各有各的沉默：
//
//	该降级却没降级 → 每个受影响的请求都先撞一次 400 再重试（多一次失败往返）
//	该保留却替换了 → 客户端人格消失，模型不再遵守工程约定（用户只看到"变笨了"）
//
// 所以这里对每一格同时断言三件事：首条 system 是谁、客户端 system 还在不在、
// 网关自有提示词出现几次。只断言"输出合法 JSON"是查不出错的。
package prompt

import (
	"encoding/json"
	"strings"
	"testing"
)

const (
	// testClientSystem 带真实指纹（"Anthropic's official CLI"），
	// 用来判定降级期是否真的把客户端 system 消掉了。
	testClientSystem = "You are Claude Code, Anthropic's official CLI for coding."
	// testOwnPrompt 网关自有提示词，必须与 Degraded 有肉眼可分的差异。
	testOwnPrompt = "网关自有提示词：按用户语言直接回答，忽略客户端注入的人格。"
	// testClientMarker 客户端指纹里最独特的一段，用于"是否还残留"的判断。
	testClientMarker = "Anthropic's official CLI"
)

// injectInput 一份带客户端 system 的请求体（模拟 Claude Code 的形态）。
func injectInput() []byte {
	return []byte(`{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":"` + testClientSystem + `"},` +
		`{"role":"user","content":"hi"}]}`)
}

// injectFirstContent 返回 messages[0].content（字符串形态）。
func injectFirstContent(t *testing.T, body []byte) string {
	t.Helper()
	msgs := appendRawMessages(t, body)
	if len(msgs) == 0 {
		t.Fatalf("messages 为空: %s", body)
	}
	var head struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(msgs[0], &head); err != nil {
		t.Fatalf("解析第一条消息失败: %v body=%s", err, body)
	}
	return head.Content
}

// TestInjectTruthTable 六格真值表（外加未知 mode 落 custom）。
//
//	Mode                     degraded=false            degraded=true
//	ModePassthrough          body 原样                 Rewrite(body, Degraded)
//	ModeAppend               Append(body, p.Text)      Rewrite(body, Degraded)
//	ModeCustom / 空 / 未知   Rewrite(body, p.Text)     Rewrite(body, Degraded)
func TestInjectTruthTable(t *testing.T) {
	cases := []struct {
		name            string
		mode            string
		degraded        bool
		identical       bool     // 输出必须与入参逐字节相同
		wantFirst       string   // messages[0].content
		wantRoles       []string // nil = 不断言
		wantOwnCount    int      // p.Text 在出站 body 里出现的次数
		wantClientCount int      // 客户端 system 文本在出站 body 里出现的次数
	}{
		{
			name:            "passthrough 非降级：透传",
			mode:            ModePassthrough,
			identical:       true,
			wantClientCount: 1,
		},
		{
			name:            "passthrough 降级期：换中性提示词",
			mode:            ModePassthrough,
			degraded:        true,
			wantFirst:       Degraded,
			wantRoles:       []string{"system", "user"},
			wantClientCount: 0,
		},
		{
			name:            "append 非降级：叠加",
			mode:            ModeAppend,
			wantFirst:       testClientSystem,
			wantRoles:       []string{"system", "system", "user"},
			wantOwnCount:    1,
			wantClientCount: 1,
		},
		{
			// 这一格是整张表里最关键的一格：降级期**不允许**叠加。
			// 客户端 system 里的指纹一个都没消掉，叠加只是让 body 更长、
			// 命中率更高 —— 与降级的目的相反。
			name:            "append 降级期：与 custom 同路",
			mode:            ModeAppend,
			degraded:        true,
			wantFirst:       Degraded,
			wantRoles:       []string{"system", "user"},
			wantOwnCount:    0,
			wantClientCount: 0,
		},
		{
			name:            "custom 非降级：替换",
			mode:            ModeCustom,
			wantFirst:       testOwnPrompt,
			wantRoles:       []string{"system", "user"},
			wantOwnCount:    1,
			wantClientCount: 0,
		},
		{
			name:            "custom 降级期：换中性提示词",
			mode:            ModeCustom,
			degraded:        true,
			wantFirst:       Degraded,
			wantRoles:       []string{"system", "user"},
			wantOwnCount:    0,
			wantClientCount: 0,
		},
		{
			// 空 mode = 没配置 = 不改写。2026-10 起这是产品默认值，
			// 与 passthrough 同格（理由见 policy.go 的真值表注释）。
			name:            "空 mode 非降级：透传",
			mode:            "",
			identical:       true,
			wantClientCount: 1,
		},
		{
			name:            "空 mode 降级期：换中性提示词",
			mode:            "",
			degraded:        true,
			wantFirst:       Degraded,
			wantRoles:       []string{"system", "user"},
			wantOwnCount:    0,
			wantClientCount: 0,
		},
		{
			name:            "未知 mode 非降级：落 custom",
			mode:            "Appendd",
			wantFirst:       testOwnPrompt,
			wantRoles:       []string{"system", "user"},
			wantOwnCount:    1,
			wantClientCount: 0,
		},
		{
			name:            "未知 mode 降级期：换中性提示词",
			mode:            "叠加",
			degraded:        true,
			wantFirst:       Degraded,
			wantRoles:       []string{"system", "user"},
			wantOwnCount:    0,
			wantClientCount: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := injectInput()
			out := Inject(in, Profile{Name: "default", Mode: c.mode, Text: testOwnPrompt}, c.degraded)

			if c.identical {
				if string(out) != string(in) {
					t.Fatalf("应原样返回入参：\n got=%s\n want=%s", out, in)
				}
				return
			}
			if got := injectFirstContent(t, out); got != c.wantFirst {
				t.Errorf("首条 system content=%q，期望 %q：%s", got, c.wantFirst, out)
			}
			if c.wantRoles != nil {
				got := appendRoles(t, out)
				if len(got) != len(c.wantRoles) {
					t.Fatalf("roles=%v want %v body=%s", got, c.wantRoles, out)
				}
				for i := range c.wantRoles {
					if got[i] != c.wantRoles[i] {
						t.Fatalf("roles=%v want %v body=%s", got, c.wantRoles, out)
					}
				}
			}
			if n := strings.Count(string(out), testOwnPrompt); n != c.wantOwnCount {
				t.Errorf("网关自有提示词出现 %d 次，期望 %d：%s", n, c.wantOwnCount, out)
			}
			if n := strings.Count(string(out), testClientMarker); n != c.wantClientCount {
				t.Errorf("客户端指纹出现 %d 次，期望 %d：%s", n, c.wantClientCount, out)
			}
		})
	}
}

// TestInjectDegradedNeverKeepsClientSystem 降级期对任何 mode 都不保留客户端 system。
//
// 单独钉一遍这条不变式：降级期的唯一目标是"出站 body 里没有客户端指纹"。
// 只要某个 mode 在降级期漏掉了客户端 system，那个 mode 的降级就永远是
// "先撞 400 再重试"的空转。
func TestInjectDegradedNeverKeepsClientSystem(t *testing.T) {
	for _, mode := range []string{ModePassthrough, ModeAppend, ModeCustom, "", "未知值"} {
		out := Inject(injectInput(), Profile{Name: "default", Mode: mode, Text: testOwnPrompt}, true)
		if strings.Contains(string(out), testClientMarker) {
			t.Errorf("mode=%q 降级期仍残留客户端指纹：%s", mode, out)
		}
		if got := injectFirstContent(t, out); got != Degraded {
			t.Errorf("mode=%q 降级期首条 system=%q，期望中性提示词", mode, got)
		}
		if strings.Contains(string(out), testOwnPrompt) {
			t.Errorf("mode=%q 降级期仍在用网关自有提示词：%s", mode, out)
		}
	}
}

// TestInjectPassthroughFailureModesUnchanged 透传时连解析都不该发生。
//
// 非降级的 passthrough 是"零改写"：body 是坏 JSON 也照样原样发出去，
// 由上游给出真实错误。一旦哪天给它加上"顺手解析一下"，坏 body 就会在
// 网关这里变成另一类错误。
func TestInjectPassthroughFailureModesUnchanged(t *testing.T) {
	for _, body := range []string{`{not json`, ``, `null`, `{"messages":null}`} {
		in := []byte(body)
		out := Inject(in, Profile{Name: "default", Mode: ModePassthrough, Text: testOwnPrompt}, false)
		if string(out) != body {
			t.Errorf("passthrough 非降级改写了 body：got %q want %q", out, body)
		}
	}
}
