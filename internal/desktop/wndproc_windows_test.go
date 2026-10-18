//go:build windows

package desktop

import "testing"

// TestQueryEndSessionAlwaysAllowsShutdown 守本功能最容易造成数据丢失的一条：
// 关机询问必须无条件放行。
//
// 如果 WM_QUERYENDSESSION 返回 FALSE，系统会认为"这个程序在阻止关机"：
// 用户要么多等十几秒，要么直接点强制关机 —— 那时进程被 TerminateProcess
// 干掉，state.json / request-log 来不及落盘，账号池状态就丢了。
//
// 所以"保住数据"的做法恰恰是**同意关机**，然后在 WM_ENDSESSION 里抢时间落盘。
// 这里把返回值钉死成 TRUE(1)，任何人想"顺手拦一下关机"都会先红在这里。
func TestQueryEndSessionAlwaysAllowsShutdown(t *testing.T) {
	d := decideSystemShutdown(wmQueryEndSession, 0)
	if !d.handled {
		t.Fatal("WM_QUERYENDSESSION 必须由我们处理并返回 TRUE，不能交给默认过程")
	}
	if d.result != 1 {
		t.Fatalf("WM_QUERYENDSESSION 返回 %#x，必须是 TRUE(1)——否则关机被拦、进程会被强杀", d.result)
	}
	if d.quitAsync {
		t.Error("WM_QUERYENDSESSION 阶段不该开始收尾：关机仍可能被别的程序取消")
	}
}

// TestEndSessionFlushesOnlyWhenShutdownIsCertain 关机已成定局才落盘。
//
// WM_ENDSESSION 的 wParam 是"真的结束了(TRUE)"还是"取消结束(FALSE)"。
// 在 FALSE 上提前收尾，会把"用户只是想关别的程序"变成"我们的网关没了"。
func TestEndSessionFlushesOnlyWhenShutdownIsCertain(t *testing.T) {
	d := decideSystemShutdown(wmEndSession, 1) // TRUE：正在关机
	if !d.handled || !d.quitAsync {
		t.Fatalf("WM_ENDSESSION(TRUE) 应触发异步落盘，实际 %+v", d)
	}

	d = decideSystemShutdown(wmEndSession, 0) // FALSE：关机被取消
	if !d.handled {
		t.Fatal("WM_ENDSESSION 应由我们处理（返回 0）")
	}
	if d.quitAsync {
		t.Error("关机被取消时不能收尾：服务不该因为别人拦了关机而消失")
	}
}

// TestUnrelatedMessagesGoToOriginalProc 其它消息必须走 CallWindowProc。
//
// 这一条是"子类化之后窗口还能拖动/缩放/响应 DPI 变更"的前提：
// 我们只处理 3 条消息，其余全部交回原过程。
func TestUnrelatedMessagesGoToOriginalProc(t *testing.T) {
	for _, msg := range []uint32{0x0000, 0x0005, 0x0014, 0x0024, 0x0083, 0x0200} {
		if d := decideSystemShutdown(msg, 0); d.handled {
			t.Errorf("消息 %#x 被当成关机消息处理了（会吞掉窗口的基础行为）", msg)
		}
	}
}
