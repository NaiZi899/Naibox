package desktop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestNormalizeCloseBehavior 守"window.json 被手改坏之后仍能启动"。
//
// 这里刻意把大小写、空格、空串、拼错的值都列进来：close_behavior 是用户
// 打开 JSON 就能改的字段，任何一版"只认精确小写"的实现都会让一个拼写
// 把默认行为变成意外 —— 最坏情况是误判成 quit（后台服务被关掉）。
func TestNormalizeCloseBehavior(t *testing.T) {
	cases := []struct {
		in   string
		want CloseBehavior
	}{
		{"ask", CloseAsk},
		{"minimize", CloseMinimize},
		{"quit", CloseQuit},
		{"  MINIMIZE  ", CloseMinimize},
		{"Quit", CloseQuit},
		{"", CloseAsk},
		{"min", CloseAsk},
		{"minimise", CloseAsk},
		{"直接退出", CloseAsk},
	}
	for _, c := range cases {
		if got := normalizeCloseBehavior(c.in); got != c.want {
			t.Errorf("normalizeCloseBehavior(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// TestCloseActionFor 守"设置 → 动作"的映射。
func TestCloseActionFor(t *testing.T) {
	cases := []struct {
		in   CloseBehavior
		want closeAction
	}{
		{CloseAsk, closeAsk},
		{CloseMinimize, closeHide},
		{CloseQuit, closeDefer},
		{"", closeAsk},         // 空 = 默认询问
		{"nonsense", closeAsk}, // 未知 = 默认询问，不是 quit
	}
	for _, c := range cases {
		if got := closeActionFor(c.in); got != c.want {
			t.Errorf("closeActionFor(%q) = %d，期望 %d", c.in, got, c.want)
		}
	}
}

// TestResolveAsk 询问框的两个答案各自落到哪个动作。
func TestResolveAsk(t *testing.T) {
	if got := resolveAsk(true); got != closeHide {
		t.Errorf("用户选「是」(最小化到托盘) → %d，期望 closeHide(%d)", got, closeHide)
	}
	if got := resolveAsk(false); got != closeDefer {
		t.Errorf("用户选「否」(退出) → %d，期望 closeDefer(%d)", got, closeDefer)
	}
}

// TestWindowStoreKeepsGeometryAndPrefsApart 守 window.json 的两个字段互不覆盖。
//
// 这是本功能最容易出的回归：窗口线程每 5 秒写一次几何，托盘每改一次写一次
// 偏好。两边谁按"读全量 → 改自己那块 → 写全量"来，都会把对方的字段冲掉。
// 更隐蔽的是 SaveWindowState 对 width<480 **静默放弃落盘**，所以只带偏好的
// 写盘根本不会落盘 —— 用户点了菜单、勾也变了，重启后设置蒸发。
func TestWindowStoreKeepsGeometryAndPrefsApart(t *testing.T) {
	l := Layout{DataDir: t.TempDir()}
	s := newWindowStore(l, nil)

	if got := s.Get().CloseBehavior; got != CloseAsk {
		t.Fatalf("首次加载的默认关窗行为 = %q，期望 %q", got, CloseAsk)
	}

	// 1) 窗口线程先写几何
	if !s.SetGeometry(120, 80, 1100, 760) {
		t.Fatal("SetGeometry 没报告变化")
	}
	// 2) 托盘再改偏好
	if err := s.SetCloseBehavior(CloseMinimize); err != nil {
		t.Fatalf("SetCloseBehavior: %v", err)
	}
	// 3) 窗口线程继续轮询（同样的几何 → 不该写盘，但也不能冲掉偏好）
	if s.SetGeometry(120, 80, 1100, 760) {
		t.Error("几何没变却报告了变化")
	}

	// 落盘结果：几何与偏好都在
	var disk WindowState
	body, err := os.ReadFile(l.windowStatePath())
	if err != nil {
		t.Fatalf("读 window.json: %v", err)
	}
	if err := json.Unmarshal(body, &disk); err != nil {
		t.Fatalf("解析 window.json: %v", err)
	}
	if disk.Width != 1100 || disk.Height != 760 || disk.X != 120 || disk.Y != 80 {
		t.Errorf("几何没落盘：%+v", disk)
	}
	if disk.CloseBehavior != CloseMinimize {
		t.Errorf("关窗行为没落盘：%q", disk.CloseBehavior)
	}

	// 4) 重启一次：从磁盘读回来还是 minimize
	s2 := newWindowStore(l, nil)
	if got := s2.Get().CloseBehavior; got != CloseMinimize {
		t.Errorf("重启后关窗行为 = %q，期望 %q", got, CloseMinimize)
	}
	if got := s2.Get(); got.Width != 1100 || !got.HasPos {
		t.Errorf("重启后几何 = %+v", got)
	}
}

// TestWindowStoreBadCloseBehaviorFallsBackToAsk 手改成垃圾值时回落默认。
func TestWindowStoreBadCloseBehaviorFallsBackToAsk(t *testing.T) {
	l := Layout{DataDir: t.TempDir()}
	body := []byte(`{"width":1000,"height":700,"x":10,"y":20,"has_pos":true,"close_behavior":"最小化"}`)
	if err := os.WriteFile(filepath.Join(l.DataDir, windowStateFileName), body, 0o644); err != nil {
		t.Fatal(err)
	}
	s := newWindowStore(l, nil)
	if got := s.Get().CloseBehavior; got != CloseAsk {
		t.Errorf("非法 close_behavior 应回落 ask，实际 %q", got)
	}
}

// TestWindowStoreTrayHintWrittenOnce 一次性气泡标记只写一次、且可跨进程读回。
func TestWindowStoreTrayHintWrittenOnce(t *testing.T) {
	l := Layout{DataDir: t.TempDir()}
	s := newWindowStore(l, nil)
	if s.Get().TrayHintShown {
		t.Fatal("首次启动不该已经标记为已提示")
	}
	if err := s.MarkTrayHintShown(); err != nil {
		t.Fatalf("MarkTrayHintShown: %v", err)
	}
	// 第二次调用是空操作（不重复写盘、不报错）
	if err := s.MarkTrayHintShown(); err != nil {
		t.Fatalf("重复 MarkTrayHintShown: %v", err)
	}
	if got := newWindowStore(l, nil).Get().TrayHintShown; !got {
		t.Error("标记没落盘：重启后会再弹一次")
	}
}

// TestWindowStateDefaultsIncludeAsk 默认值必须是"询问"，不是"退出"。
func TestWindowStateDefaultsIncludeAsk(t *testing.T) {
	if got := DefaultWindowState().CloseBehavior; got != CloseAsk {
		t.Errorf("DefaultWindowState().CloseBehavior = %q，期望 %q", got, CloseAsk)
	}
}
