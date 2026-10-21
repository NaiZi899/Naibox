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

// TestNoteUpstreamRestrictedDisablesAfterThreshold 连续达到阈值才禁用。
//
// 一次 403 可能是上游误判，也可能是带风控措辞的软限流；连续被拒才是真状态。
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
				t.Fatalf("第 %d 次就禁用了 —— 单次风控可能是误判", i)
			}
		}
	}
	if got := p.RestrictedStreak("u1"); got != restrictedThreshold {
		t.Errorf("RestrictedStreak=%d want %d", got, restrictedThreshold)
	}
}

// TestNoteUpstreamRestrictedClearedBySuccess 一次成功清零。
//
// 不清的话，跨天的无关抖动会叠加到阈值上，把一个已经恢复的号禁掉。
func TestNoteUpstreamRestrictedClearedBySuccess(t *testing.T) {
	p := newRestrictedPool(t)
	p.NoteUpstreamRestricted("u1")
	p.NoteUpstreamRestricted("u1")
	p.NoteSuccess("u1")
	if got := p.RestrictedStreak("u1"); got != 0 {
		t.Fatalf("成功后计数应为 0，实际 %d", got)
	}
	for i := 0; i < restrictedThreshold-1; i++ {
		p.NoteUpstreamRestricted("u1")
	}
	if st, ok := p.Status("u1"); ok && st.Disabled {
		t.Fatalf("清零后重新累积未达阈值就禁用了")
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
