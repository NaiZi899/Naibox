//go:build !windows

package desktop

import "log"

// newView 非 Windows 恒返回 ErrNoGUI，由组装层回落浏览器模式。
//
// 这里刻意**不**实现"用系统浏览器打开"：打开浏览器是 openURL 的事，
// 组装层已经有一条统一的 browser 分支。让这里也做一遍会出现
// "窗口没开成 → 又开一次浏览器"的双开。
func newView(l Layout, url string, store *windowStore, logger *log.Logger) (WindowView, error) {
	return nil, ErrNoGUI
}
