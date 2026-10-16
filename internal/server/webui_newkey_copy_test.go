// webui_newkey_copy_test.go 新建/轮换 key 后**必须能拿到完整 key**。
//
// # 这个 bug 的形态（用户报"创建 key 之后无法通过浏览器复制"）
//
// 原实现把完整 key 只放进一个 5 秒后自动消失的 toast，外加一次性的自动复制：
//
//	toast(`已创建（仅此一次显示完整 key）：\n${r.full}\n请立即复制保存`, false, 15000);
//	copyText(r.full, 'API Key');
//
// 而 `r.full` **只在创建回执里出现过一次** —— 列表接口只下发掩码
// （apikey.MaskID），错过那一刻这把 key 就**永久查不回来**，只能删了重建。
//
// 两处叠加的真实原因：
//
//  1. 给 toast 传了第三个参数想延长到 15 秒，但 `toast(msg, bad)` 的签名
//     只有两个参数 —— 那个 15000 被**静默丢弃**，实际仍是 5 秒。
//  2. copyText 依赖 clipboard API（非安全上下文不可用）或 execCommand
//     （可能被浏览器策略拒绝）。复制一失败，用户既没有剪贴板内容，
//     也没有可重新复制的入口。
//
// 修法：改成**持久面板**（只有点「知道了」才关）+ 可反复点的复制按钮 +
// readonly 文本框（兜底 Ctrl+C）。三条退路任一可用即可拿到 key。
//
// # 本文件钉住的判据
//
// 照仓库既有的前端守卫风格（按源码文本断言，先剥注释再判）：
//
//	① 完整 key 不得只出现在 toast 里 —— 必须有持久落点
//	② 面板里必须有复制按钮（且绑定的是"重新复制当前 key"）
//	③ 关闭时才清 DOM（key 不留在页面上）
//	④ 创建与轮换**两条**路径都要走这个面板（轮换漏了更严重：
//	   旧钥匙已作废，没抄到就等于把自己锁在门外）
package server

import (
	"strings"
	"testing"
)

// newKeyGuard 前端守卫的共用判据。
type newKeyGuard struct {
	src string // 已剥注释的源码
}

func newNewKeyGuard(t *testing.T) newKeyGuard {
	t.Helper()
	raw, err := readWebUIHTML()
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}
	return newKeyGuard{src: stripJSCommentsDeep(raw)}
}

// count 统计子串出现次数。
func (g newKeyGuard) count(sub string) int { return strings.Count(g.src, sub) }

// has 是否存在。
func (g newKeyGuard) has(sub string) bool { return strings.Contains(g.src, sub) }

// fnBody 取出某函数体（从 `function 名` 到下一个顶层 `function`）。
//
// 比"整文件 contains"更严：能钉住"这件事发生在**这个函数里**"，
// 而不是"文件里某处有这行"。
func (g newKeyGuard) fnBody(name string) string {
	marker := "function " + name
	i := strings.Index(g.src, marker)
	if i < 0 {
		return ""
	}
	rest := g.src[i:]
	if j := strings.Index(rest[len(marker):], "\n  function "); j >= 0 {
		return rest[:len(marker)+j]
	}
	return rest
}

// TestNewKeyHasPersistentPanel 完整 key 必须有不会自动消失的落点。
func TestNewKeyHasPersistentPanel(t *testing.T) {
	g := newNewKeyGuard(t)

	if !g.has("function showNewKey(") {
		t.Fatal("★ 没有 showNewKey —— 完整 key 只能靠会消失的 toast 展示，" +
			"用户错过那几秒就永久拿不到这把 key（用户报的就是这个）")
	}

	// ① 创建路径必须走持久面板。
	create := g.fnBody("apiKeyCreate")
	if create == "" {
		t.Fatal("取不到 apiKeyCreate 函数体")
	}
	if !strings.Contains(create, "showNewKey(") {
		t.Error("★ 创建 key 后没有调用 showNewKey —— 完整 key 仍然只出现在 toast 里")
	}
	if strings.Contains(create, "r.full") && strings.Contains(create, "toast(") &&
		strings.Contains(create, "已创建") {
		t.Error("创建路径仍然把完整 key 塞进 toast（toast 会自动消失，不能作为唯一出口）")
	}

	// ② 轮换路径同样必须走 —— 这条更严重：旧管理钥匙已作废，
	//    新钥匙没抄到就等于把自己锁在门外。
	rotate := g.fnBody("apiKeyRotateAdmin")
	if rotate == "" {
		t.Fatal("取不到 apiKeyRotateAdmin 函数体")
	}
	if !strings.Contains(rotate, "showNewKey(") {
		t.Error("★ 轮换管理钥匙后没有调用 showNewKey —— 旧钥匙已作废，" +
			"新钥匙只闪 5 秒的话用户会被锁在门外")
	}
}

// TestNewKeyPanelHasCopyButton 面板里必须有可反复点的复制按钮。
func TestNewKeyPanelHasCopyButton(t *testing.T) {
	g := newNewKeyGuard(t)
	body := g.fnBody("showNewKey")
	if body == "" {
		t.Fatal("取不到 showNewKey 函数体")
	}

	// 复制按钮的 DOM id 与绑定。
	if !strings.Contains(body, `id="newKeyCopy"`) {
		t.Error("★ 面板里没有复制按钮 —— 自动复制失败（非安全上下文 / 浏览器拒绝）后" +
			"用户没有重试入口，这正是「无法复制」的另一半")
	}
	if !strings.Contains(body, "newKeyCopy") || !strings.Contains(body, "copyText(") {
		t.Error("复制按钮没有绑定 copyText")
	}

	// readonly 文本框：第三条退路（Ctrl+C）。
	if !strings.Contains(body, `id="newKeyText"`) || !strings.Contains(body, "readonly") {
		t.Error("★ 没有 readonly 文本框 —— 复制按钮也被拒时，用户连手动选中都做不到")
	}
	if !strings.Contains(body, "select()") {
		t.Error("文本框应当自动全选，省掉用户拖选")
	}

	// 面板必须是"点了才关"：不能自己定时隐藏。
	if strings.Contains(body, "setTimeout") {
		t.Error("showNewKey 里出现了 setTimeout —— 面板不该自动消失" +
			"（完整 key 只在创建时出现一次，消失即永久丢失）")
	}
	if !strings.Contains(body, "newKeyClose") {
		t.Error("没有关闭按钮 —— 面板要有明确的「知道了」，而不是被动等它消失")
	}
}

// TestNewKeyPanelClearsOnClose 关闭时把 key 从 DOM 里清掉。
func TestNewKeyPanelClearsOnClose(t *testing.T) {
	g := newNewKeyGuard(t)
	body := g.fnBody("showNewKey")
	if body == "" {
		t.Fatal("取不到 showNewKey 函数体")
	}
	// 关闭时既隐藏又清内容 —— 明文 key 不该留在页面上（截图/共屏会泄漏）。
	if !strings.Contains(body, "el.hidden = true") {
		t.Error("关闭按钮没有隐藏面板")
	}
	if !strings.Contains(body, "el.innerHTML = ''") {
		t.Error("★ 关闭时没有清空 innerHTML —— 明文 key 会留在 DOM 里" +
			"（截图、共享屏幕、DOM 检查都会泄漏）")
	}
}

// TestToastSignatureHasNoIgnoredThirdArg toast 不能收第三参。
//
// 这是原 bug 的一个具体形态：调用方写了 `toast(msg, false, 15000)` 想延长，
// 而签名只有 (msg, bad)，多余参数被静默丢弃 —— 于是"以为延长到 15 秒"
// 与"实际 5 秒"不符，而没有任何报错。
//
// 本用例钉两件事：签名就是两个参数；且**没有任何调用点**再传第三个参数。
func TestToastSignatureHasNoIgnoredThirdArg(t *testing.T) {
	g := newNewKeyGuard(t)

	body := g.fnBody("toast")
	if body == "" {
		t.Fatal("取不到 toast 函数体")
	}
	if !strings.Contains(body, "function toast(msg, bad)") {
		t.Errorf("toast 签名变了（现在是两参 msg/bad）。若确实要支持第三个参数，"+
			"请同步更新本条断言 —— 但别再让调用方传一个被忽略的参数。\n实际: %s",
			strings.SplitN(body, "\n", 2)[0])
	}

	// 扫全部调用点：不允许出现 `toast(x, y, z)` 这种三参形态。
	// 用宽松的启发式（字符扫描）而不是正则 —— 文案里含逗号与括号很常见，
	// 正则容易误伤；这里只要发现"第三个顶层逗号参数"就报。
	for i := 0; ; {
		k := strings.Index(g.src[i:], "toast(")
		if k < 0 {
			break
		}
		start := i + k + len("toast(")
		depth, commas := 1, 0
		j := start
		for ; j < len(g.src); j++ {
			switch g.src[j] {
			case '(':
				depth++
			case ')':
				depth--
			case ',':
				if depth == 1 {
					commas++
				}
			}
			if depth == 0 {
				break
			}
		}
		if commas >= 2 {
			snippet := g.src[start:min(j, start+60)]
			t.Errorf("★ 有 toast 调用传了第三个参数（会被静默忽略）：%s…\n"+
				"这正是原 bug 的形态 —— 调用方以为延长了显示时长，实际没有。", snippet)
		}
		i = j
	}
}
