package desktop

import (
	"log"
	"sync"
)

// windowStore window.json 的唯一写者：把几何与界面偏好**当作一份**来读写。
//
// # 为什么需要它（这一版最容易踩的坑）
//
// window.json 现在同时装两样东西：窗口几何（窗口线程每 5 秒采样一次）和
// 关窗行为/气泡标记（用户在托盘里改）。如果两边各自「读出全量 → 改自己那
// 一块 → 写回全量」，后写的会把先写的覆盖掉 —— 表现是"改了关窗行为，5 秒后
// 自己变回询问"。
//
// 更隐蔽的是另一头：SaveWindowState 见到 width<480 会**直接放弃落盘**
// （那是给"读到垃圾几何"准备的保护）。所以只带偏好、不带几何的那种写盘
// 根本不会落盘，而且不报错 —— 用户点了菜单，看着勾变了，重启后全白。
//
// 所以：几何与偏好的写操作都从这里过，一把锁 + 一份内存中的全量状态。
type windowStore struct {
	mu  sync.Mutex
	l   Layout
	st  WindowState
	log *log.Logger
}

func newWindowStore(l Layout, logger *log.Logger) *windowStore {
	return &windowStore{l: l, st: l.LoadWindowState(), log: logger}
}

// Get 当前状态的快照（可并发调用）。
func (s *windowStore) Get() WindowState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st
}

// SetGeometry 更新窗口几何并落盘。返回是否发生了实际变化
// （窗口线程 5 秒一次轮询，没必要每次都写盘）。
func (s *windowStore) SetGeometry(x, y, width, height int) bool {
	return s.SetGeometryMax(x, y, width, height, false)
}

// SetGeometryMax 同 SetGeometry，但一并记录"当前是否最大化"。
//
// # 为什么要 max 参数而不是让调用方分两步写
//
// 分两步就是两次落盘，中间那一瞬间的状态是"几何已更新、最大化位还是旧值"。
// 恰好在这个窗口里进程被强杀，下次启动就会用一个不匹配的组合（典型后果
// 见 WindowState.Maximized 的注释）。一次写完没有中间态。
func (s *windowStore) SetGeometryMax(x, y, width, height int, maximized bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 与 SaveWindowState 同一口径的最小尺寸：缩到看不见的窗口不该被记住。
	if width < 480 || height < 360 {
		return false
	}
	next := s.st
	next.X, next.Y = x, y
	next.Width, next.Height = width, height
	next.HasPos = true
	next.Maximized = maximized
	if next == s.st {
		return false
	}
	s.st = next
	s.saveLocked()
	return true
}

// SetCloseBehavior 保存关窗行为。
func (s *windowStore) SetCloseBehavior(b CloseBehavior) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.st
	next.CloseBehavior = normalizeCloseBehavior(string(b))
	if next == s.st {
		return nil
	}
	s.st = next
	return s.saveLocked()
}

// MarkTrayHintShown 记下"托盘气泡已经弹过"。已经置位时不重复写盘。
func (s *windowStore) MarkTrayHintShown() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.TrayHintShown {
		return nil
	}
	s.st.TrayHintShown = true
	return s.saveLocked()
}

// saveLocked 落盘（调用方必须已持锁）。
//
// 写失败不回滚内存状态：内存里那份才是用户此刻看到的界面行为，回滚会让
// "菜单勾选"和"实际动作"当场不一致。落盘失败只影响下次启动，记日志就够。
func (s *windowStore) saveLocked() error {
	err := s.l.SaveWindowState(s.st)
	if err != nil && s.log != nil {
		s.log.Printf("保存窗口状态失败（%v）：本次会话内有效，重启后会丢", err)
	}
	return err
}
