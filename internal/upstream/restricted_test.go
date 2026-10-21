package upstream

import "testing"

// TestClassifyAccountRestricted 风控判据：同一个 code 11140 的两种语义必须分开。
//
// # 为什么要单独钉这条
//
// 上游用 11140 表达两件相反的事：
//
//	"请求过于频繁，将在 … 重置"          → 软限流，等一会儿就好
//	"request illegal / 账号存在异常行为"  → 账号被风控，换号才有救
//
// 判据写宽了（只看 code）会把限流号累计到禁用；判据漏了则风控落到
// ErrClient，坏号既不冷却也不计数，永远以"健康"身份留在池里被反复选中。
// 后者的实测后果：一个号 24 小时被选中 164 次、0 次成功。
func TestClassifyAccountRestricted(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   ErrKind
	}{
		{
			name:   "真实风控响应",
			status: 403,
			body: `{"code":11140,"msg":"request illegal","requestId":"x",` +
				`"displayMsg":{"en":"Your account has been restricted due to unusual activity",` +
				`"zh":"账号存在异常行为，请联系客服"}}`,
			want: ErrAccountRestricted,
		},
		{
			name:   "只有中文文案也认得",
			status: 403,
			body:   `{"code":11140,"displayMsg":{"zh":"账号存在异常行为"}}`,
			want:   ErrAccountRestricted,
		},
		{
			name:   "11140 的软限流语义不得被判成风控",
			status: 403,
			body:   `{"code":11140,"msg":"请求过于频繁，将在 2026-09-15 09:30:00 重置"}`,
			want:   ErrClient,
		},
		{
			name:   "空 body 的 403 仍是 ErrClient",
			status: 403,
			body:   ``,
			want:   ErrClient,
		},
		{
			name:   "未授权通道仍归内容拦截（不得被风控判据抢走）",
			status: 400,
			body:   `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`,
			want:   ErrContentBlocked,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.status, c.body); got != c.want {
				t.Fatalf("Classify(%d, %q) = %v（%s），want %v（%s）",
					c.status, c.body, got, got, c.want, c.want)
			}
		})
	}
}
