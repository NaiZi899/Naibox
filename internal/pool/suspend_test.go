// suspend_test.go 池级暂停（上游风控命中后的"整池停手"）。
//
// # 为什么这一档必须存在
//
// 风控标记打在账号上，但上游的认定看的是账号背后的设备令牌、machineId
// 以及（当前部署形态下）同一条出口 IP。账号级冷却拦不住"换号继续打"——
// 换号是轮转循环的本能。社区复盘（workbuddy2api-hub CHANGELOG）的实测是
// 一次任务把 7 个号全封，红线是「看到 11140 立即停手」。
package pool

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/NaiZi899/Naibox/internal/auth"
)

func newSuspendPool(t *testing.T) *Pool {
	t.Helper()
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at"})
	p.SetCredits("u1", 1000)
	return p
}

// TestSuspendBlocksPickAndServability 暂停期间选不出号，且 /healthz 口径同变。
func TestSuspendBlocksPickAndServability(t *testing.T) {
	p := newSuspendPool(t)
	if p.PickFor("", "", nil) == nil {
		t.Fatal("前置条件失败：未暂停时应当能选出账号")
	}

	until := p.Suspend("上游风控（403 账号级限制）", time.Minute)
	if until.IsZero() {
		t.Fatal("Suspend 必须返回生效后的截止时刻")
	}

	// 关键断言 ①：暂停期间**不选号**，且不走"全冷却兜底"。
	if got := p.PickFor("", "", nil); got != nil {
		t.Errorf("暂停期间不得选出任何账号，实际选中 %s —— "+
			"漏了 pickFor 的闸门，暂停期间请求会照常打到上游", got.UID)
	}
	// 关键断言 ②：/healthz 的口径（"能不能受理对话"）必须同步为不可服务。
	if p.ServableNow() {
		t.Error("暂停期间 ServableNow 必须为 false（否则探活说健康、chat 却 503）")
	}

	gotUntil, reason := p.SuspendedUntil()
	if gotUntil.IsZero() || reason == "" {
		t.Fatalf("SuspendedUntil 应给出截止时刻与原因，实际 until=%v reason=%q", gotUntil, reason)
	}
}

// TestSuspendPersistsAcrossRestart 暂停必须活过重启。
//
// 这是 workbuddy2api-hub 复盘里的一条独立条目：冷却只存内存，
// 重启 / 刷新 token / 一键测试都会把它清掉，于是重启后所有号重新上膛。
func TestSuspendPersistsAcrossRestart(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at"})
	p.SetCredits("u1", 1000)
	p.Suspend("上游风控（403 账号级限制）", 30*time.Minute)

	// 重启：同一份 state.json 重新加载。
	p2 := New(fp)
	if got := p2.PickFor("", "", nil); got != nil {
		t.Fatalf("重启后仍应处于暂停，实际选出 %s —— 暂停没落盘", got.UID)
	}
	if until, reason := p2.SuspendedUntil(); until.IsZero() || reason == "" {
		t.Errorf("重启后暂停状态丢了：until=%v reason=%q", until, reason)
	}
}

// TestSuspendBacksOffAndCaps 连续命中时退避，且封顶 maxSuspendTTL。
func TestSuspendBacksOffAndCaps(t *testing.T) {
	p := New("")

	first := p.Suspend("x", 10*time.Minute)
	second := p.Suspend("x", 10*time.Minute)
	if !second.After(first) {
		t.Errorf("已在暂停中再次命中应从原截止时刻往后翻倍：first=%v second=%v", first, second)
	}

	capped := p.Suspend("x", 100*time.Hour)
	if d := time.Until(capped); d > maxSuspendTTL+time.Second {
		t.Errorf("暂停时长必须封顶 %v，实际 %v", maxSuspendTTL, d)
	}
}

// TestSuspendExpiresAndPicksResume 到期后自动恢复（不需要人工清）。
func TestSuspendExpiresAndPicksResume(t *testing.T) {
	p := newSuspendPool(t)
	p.Suspend("x", 20*time.Millisecond)
	time.Sleep(60 * time.Millisecond)

	if got := p.PickFor("", "", nil); got == nil {
		t.Error("暂停到期后必须恢复选号")
	}
	if !p.ServableNow() {
		t.Error("暂停到期后 ServableNow 必须恢复 true")
	}
	if until, reason := p.SuspendedUntil(); !until.IsZero() || reason != "" {
		t.Errorf("已到期时 SuspendedUntil 应返回零值，实际 until=%v reason=%q", until, reason)
	}
}
