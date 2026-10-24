// restricted_test.go 上游风控拒绝的门控（NoteUpstreamRestricted）。
//
// # 背景
//
// 被上游风控标记的号（403 + code 11140 "request illegal"）改造前落到
// ErrKindClient，走"只换号不罚"：既不冷却也不禁用，永远以健康身份留在
// 池里被反复选中。实测一个号 24 小时被选中 164 次、0 次成功，而仪表盘
// 始终显示它健康 —— 用户只能感知到"时好时坏"，无法自查是哪几个号。
package pool

import (
	"testing"

	"github.com/NaiZi899/Naibox/internal/auth"
)

func newRestrictedPool(t *testing.T) *Pool {
	t.Helper()
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at"})
	p.SetCredits("u1", 1000)
	return p
}

// TestNoteUpstreamRestrictedDisablesAfterThreshold 达到阈值即禁用。
//
// 阈值现在是 1（见 restrictedThreshold 的注释）：判据已经在文案上把
// 11140 的另一半语义（带重置时刻的软限流）排除掉了，还能走到这里的
// 都是上游明确的账号级标记 —— 再"确认两次"的代价是用另外两个号
// 去撞同一条出口/同一台设备。
func TestNoteUpstreamRestrictedDisablesAfterThreshold(t *testing.T) {
	p := newRestrictedPool(t)
	for i := 1; i <= restrictedThreshold; i++ {
		disabled := p.NoteUpstreamRestricted("u1")
		want := i >= restrictedThreshold
		if disabled != want {
			t.Fatalf("第 %d 次：disabled=%v want %v", i, disabled, want)
		}
		if !want {
			if st, ok := p.Status("u1"); ok && st.Disabled {
				t.Fatalf("第 %d 次就禁用了 —— 未达阈值", i)
			}
		}
	}
	if got := p.RestrictedStreak("u1"); got != restrictedThreshold {
		t.Errorf("RestrictedStreak=%d want %d", got, restrictedThreshold)
	}
}

// TestNoteUpstreamRestrictedStreakClearedBySuccess 一次成功清零计数。
//
// 不清的话，跨天的无关抖动会叠加到阈值上。阈值为 1 时首次命中就已经禁用，
// 所以"清零"真正服务的是**调大阈值的部署**（回滚通道 / 现场调参）：
// 那里计数必须能从 0 重新算，否则一个历史抖动会把号提前禁掉。
func TestNoteUpstreamRestrictedStreakClearedBySuccess(t *testing.T) {
	p := newRestrictedPool(t)
	p.NoteUpstreamRestricted("u1")
	p.NoteSuccess("u1")
	if got := p.RestrictedStreak("u1"); got != 0 {
		t.Fatalf("成功后计数应为 0，实际 %d", got)
	}
	// 清零后从零重新累积：下一次命中是否禁用，由阈值本身决定。
	disabled := p.NoteUpstreamRestricted("u1")
	if want := restrictedThreshold <= 1; disabled != want {
		t.Errorf("清零后首次命中：disabled=%v want %v（阈值 %d）",
			disabled, want, restrictedThreshold)
	}
}

// TestNoteUpstreamRestrictedUnknownUIDIsNoop 未知账号是空操作（不 panic）。
func TestNoteUpstreamRestrictedUnknownUIDIsNoop(t *testing.T) {
	p := newRestrictedPool(t)
	if p.NoteUpstreamRestricted("nobody") {
		t.Error("未知账号不该返回 disabled=true")
	}
	if got := p.RestrictedStreak("nobody"); got != 0 {
		t.Errorf("未知账号 RestrictedStreak=%d want 0", got)
	}
}
