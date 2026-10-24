package server

import (
	"strings"
	"testing"
)

// TestBrowserProfilesPanelForbidsBlindConfig 「不确定就别设」这块警示是产品决策，
// 不是装饰文案：指纹与档案配错**不会报错**，只会以"账号被风控"结账。
//
// # 为什么期望值硬编码在这里
//
// 警示被悄悄删掉时界面一切正常（它只是几行字），没有任何运行期证据会变红。
// 把关键词逐条钉在测试里，删文案就必须同时改断言 —— 那正是该有的摩擦。
func TestBrowserProfilesPanelForbidsBlindConfig(t *testing.T) {
	page := string(webuiHTML)
	for _, want := range []string{
		"不确定这一页是干什么的，就不要设置",
		"全部留空",         // 指纹的安全退路
		"一次性无痕模式就够",    // "只想加个号"的退路
		"Chrome 不处理认证", // 带认证代理为什么不生效
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("浏览器档案面板的警示缺少 %q —— 盲配导致风控的责任提示被删了", want)
		}
	}
	// 四件"不要做"要成组出现（缺一条就说明有人删了清单里的某项）
	if got := strings.Count(page, "<br>\n        <b>请不要在这里做这几件事：</b>"); got != 1 {
		t.Fatalf("「请不要在这里做这几件事」清单出现 %d 次，应当恰好 1 次", got)
	}
}
