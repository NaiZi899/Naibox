// notice_test.go 说明性结论 vs 解析失败的分野。
//
// # 这条分野为什么值得单独一个文件
//
// 基元律动的"只读"栏要告诉用户一个**内容为真**的结论：
// 「登录凭证有效，账号 demo-user，余额 99.75 —— 但上游只下发脱敏 key，
// 所以这一栏不导入」。这不是错误，是一条有用的说明。
//
// 而 admin 层对 `ImportCredentials` 的 error 一律套「导入内容无法解析：」，
// 于是同一句话变成自相矛盾：
//
//	导入内容无法解析：登录凭证有效。账号：demo-user 余额：99.75 …
//
// 这是本项目反复出现的那类缺陷 —— **声明与事实不一致**（消息说失败，
// 内容说成功）。修复引入 gateway.ImportNotice 把两类分开；
// 本文件钉住这个分野，两个方向都要测：
//
//	说明性结论 → 不套前缀（否则自相矛盾）
//	真解析失败 → **必须**套前缀（否则用户不知道"哪一步"错了）
package jiyuan

import (
	"strings"
	"testing"

	"github.com/NaiZi899/Naibox/internal/gateway"
)

// TestReadOnlySessionReturnsNoticeNotParseError 只读栏的结论必须是 Notice。
//
// 判据落在 gateway.IsImportNotice 上 —— admin 层就是按它决定要不要套
// 「导入内容无法解析：」。若这里变红，线上会重新出现那句自相矛盾的话。
func TestReadOnlySessionReturnsNoticeNotParseError(t *testing.T) {
	f := newFakeUpstream()
	f.serve(t)

	p := New(Config{})
	_, err := p.ImportCredentials("sess_fake_session_value", importKindSession)
	if err == nil {
		t.Fatal("只读栏不该导入账号，必须返回一条说明")
	}
	if !gateway.IsImportNotice(err) {
		t.Fatalf("只读栏的结论应标记为 ImportNotice（否则界面会说成\"无法解析\"），实际: %T %v", err, err)
	}
}

// TestNoticeMessageStandsAloneWithoutPrefix 消息本身要能脱离前缀独立成立。
//
// admin 层原样展示它 —— 所以它不能依赖"前面会有人加一句"才读得通。
func TestNoticeMessageStandsAloneWithoutPrefix(t *testing.T) {
	f := newFakeUpstream()
	f.serve(t)

	p := New(Config{})
	_, err := p.ImportCredentials("sess_fake_session_value", importKindSession)
	if err == nil {
		t.Fatal("应返回说明")
	}
	msg := err.Error()
	// 不能自称"解析失败"（那是前缀的语义，前缀已经不在了）。
	if strings.Contains(msg, "无法解析") {
		t.Errorf("说明性结论文本里不该出现\"无法解析\"（会与\"凭证有效\"矛盾）:\n%s", msg)
	}
	// 必须包含真实事实 —— 这些是用户唯一能看到的信息来源。
	for _, want := range []string{"登录凭证有效", "demo-user", "99.75"} {
		if !strings.Contains(msg, want) {
			t.Errorf("说明里应包含 %q:\n%s", want, msg)
		}
	}
}

// TestParseFailuresStayOrdinaryErrors 反向：真正的解析失败**不能**是 Notice。
//
// 否则 admin 层会原样展示它们，用户就看不到"这一步是解析"这层信息，
// 而"粘错了格式"与"凭证有效但没东西可导"是两件完全不同的事。
func TestParseFailuresStayOrdinaryErrors(t *testing.T) {
	p := New(Config{})

	cases := []struct {
		name   string
		pasted string
		format string
	}{
		{"空输入", "", importKindSession},
		{"key 粘进凭证栏", "sk_tr_1a2b3c4d5e6f7a8b9c0d", importKindSession},
		{"凭证栏但没有 sess_ 前缀", "hello-world-not-a-session", importKindSession},
		{"key 形态但内容为空", "", importKindKey},
		{"key 形态但都是注释", "# 只有注释\n# 再来一行", importKindKey},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := p.ImportCredentials(c.pasted, c.format)
			if err == nil {
				t.Fatalf("%q 应返回错误", c.pasted)
			}
			if gateway.IsImportNotice(err) {
				t.Errorf("这是**解析失败**，不该标成 Notice（界面会不套前缀）: %v", err)
			}
		})
	}
}
