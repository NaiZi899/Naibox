// webui_stats_tokens_test.go 统计卡 token 口径的源码守卫。
//
// # 守的是什么
//
// 面板上「消耗 token」曾经有**两个方向相反**的错误，而它们都只在 JS 里，
// Go 侧的单测看不见：
//
//	① 只算输出侧 —— 输入侧根本没进聚合，实测真实量是显示的 60 倍
//	② 写成 tokens + think_tokens —— reasoning 是 completion 的子集，高报 58%
//
// 修完之后，任何一次"顺手把两个数加一下"的改动都会让 ①② 之一复发，而且
// 页面上永远是个"看起来合理的数字"，不会有报错。所以判据必须是源码级的：
//
//	statsCardsEntries() 只读后端给的 prompt_tokens / tokens / total_tokens，
//	不得出现 think_tokens（它就是 ② 的指纹）。
//
// 这比浏览器实测更靠前：tests/frontend/ 那套不在 `go test ./...` 里跑，
// 下一个改卡片的人不会立刻看到红。与 webui_stats_range_test.go 同一理由。
package server

import (
	"strings"
	"testing"
)

// TestWebUIStatsCardsUseBackendTokenFields 卡片只许用后端的三口径字段。
func TestWebUIStatsCardsUseBackendTokenFields(t *testing.T) {
	src, err := readWebUIHTML()
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}
	code := stripJSCommentsDeep(src)

	body, ok := extractJSFunction(code, "function statsCardsEntries(")
	if !ok {
		t.Fatal("找不到 statsCardsEntries() —— 守卫失效（fail-open）")
	}

	// fail-closed 自检：三个字段都必须真的被读到。
	for _, want := range []string{
		"prompt_tokens", // 输入侧
		"s.tokens",      // 输出侧
		"total_tokens",  // 后端口径合计
	} {
		if !strings.Contains(body, want) {
			t.Errorf("statsCardsEntries() 未读取 %q —— 口径字段被绕过了", want)
		}
	}

	// 判据本体：think_tokens 是 reasoning 的键，出现在这里就意味着又把它
	// 当"额外量"加了一遍（reasoning 已含在 completion 内）。
	if strings.Contains(body, "think_tokens") {
		t.Error("statsCardsEntries() 里出现了 think_tokens —— reasoning 是 completion 的子集，\n" +
			"再加一遍会把「消耗 token」高报（实测 +58%）。总量请用后端 total_tokens。")
	}
}

// TestWebUIStatsCoverageHintVisible 输入侧覆盖率必须能被看见。
//
// 历史行没有 prompt_tokens（反序列化得 0），若界面对此一字不提，用户会在
// 改完之后看到"输入 token 只有新数据那么多"，再判一次"统计不准"。
func TestWebUIStatsCoverageHintVisible(t *testing.T) {
	src, err := readWebUIHTML()
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}
	code := stripJSCommentsDeep(src)

	// 本仓库的口径文案写在 renderStats 里（没有单独抽成
	// renderStatsWindowText，见 webui.html 的 renderStats 顶部）。
	body, ok := extractJSFunction(code, "function renderStats(")
	if !ok {
		t.Fatal("找不到 renderStats() —— 守卫失效（fail-open）")
	}
	for _, want := range []string{"prompt_covered", "prompt_derived", "输入侧覆盖"} {
		if !strings.Contains(body, want) {
			t.Errorf("口径文案里缺 %q —— 覆盖不全时界面不会解释，用户会再判一次「不准」", want)
		}
	}
}
