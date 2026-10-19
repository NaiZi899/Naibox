package desktop

import (
	"encoding/json"
	"os"
	"path/filepath"
)

const windowStateFileName = "window.json"

// WindowState 窗口位置与尺寸（存 <DataDir>\window.json）。
//
// # 为什么单独存一个文件而不是塞进 config.json
//
// config.json 是用户手改的文件（还带注释性质的分节）。窗口尺寸属于
// "程序自己维护的界面状态"，混进去会让用户在一次误编辑里连带改掉窗口
// 行为，也会让"恢复默认配置"变成"顺便忘了窗口在哪"。分开存、分开坏。
type WindowState struct {
	Width  int `json:"width"`
	Height int `json:"height"`
	X      int `json:"x"`
	Y      int `json:"y"`
	// HasPos 用户摆过窗口（否则不套用 X/Y，交给系统居中）。
	HasPos bool `json:"has_pos"`
	// Maximized 上次退出时窗口是最大化状态。
	//
	// # 为什么必须单独记（实测事故，2026-10-19）
	//
	// 最大化窗口的 GetWindowRect 返回的是「铺满屏幕 + 边框」的尺寸，
	// 实测 2560×1600 的屏幕上记成 `2582×1622 @ (-11,-11)`。只存几何而不存
	// 「当时是最大化的」的话，下次启动会按**普通窗口**去建这么一个大窗口：
	// 比屏幕还大、位置又是负坐标，用户看到的是"窗口跑到屏幕外面/左上角
	// 只剩一条"。
	//
	// 恢复时若该位为真，先按记录的几何还原（那是还原态的正常尺寸），
	// 再 ShowWindow(SW_MAXIMIZE) —— 与用户在资源管理器里双击标题栏的行为一致。
	Maximized bool `json:"maximized,omitempty"`
	// CloseBehavior 点窗口 X 时怎么办：ask | minimize | quit。
	//
	// 与几何放在同一个文件是刻意的：这两个字段都是"程序自己维护的界面状态"，
	// 而且都只在桌面会话里有意义。代价是一个坑 —— 谁想改偏好就得连几何一起
	// 写，否则 SaveWindowState 会因为 width=0 直接放弃落盘（见 windowStore）。
	CloseBehavior CloseBehavior `json:"close_behavior"`
	// TrayHintShown "已最小化到托盘"的托盘气泡是否弹过。
	//
	// 只弹一次的判据放在文件里而不是内存里：进程重启后用户同样需要被提醒一次，
	// 但不需要每次都被提醒。
	TrayHintShown bool `json:"tray_hint_shown"`
}

// DefaultWindowState 首次启动的窗口尺寸与默认关窗行为。
func DefaultWindowState() WindowState {
	return WindowState{Width: 1180, Height: 820, CloseBehavior: CloseAsk}
}

// Path window.json 的路径。
func (l Layout) windowStatePath() string { return filepath.Join(l.DataDir, windowStateFileName) }

// LoadWindowState 读窗口状态；文件缺失/损坏都回落到默认值（不报错：
// 一个记不住位置的窗口不该阻止程序启动）。
func (l Layout) LoadWindowState() WindowState {
	st := DefaultWindowState()
	body, err := os.ReadFile(l.windowStatePath())
	if err != nil {
		return st
	}
	var got WindowState
	if err := json.Unmarshal(body, &got); err != nil {
		return st
	}
	// 兜底：被手改坏的尺寸会让窗口小到看不见。
	if got.Width < 480 || got.Height < 360 {
		return st
	}
	if got.Width > 12000 || got.Height > 12000 {
		return st
	}
	got.HasPos = got.HasPos && got.X > -20000 && got.Y > -20000
	// 手改成 "min" / "Minimize" / 空串都收敛到默认，不因为一个拼写就不启动。
	got.CloseBehavior = normalizeCloseBehavior(string(got.CloseBehavior))
	return got
}

// SaveWindowState 写窗口状态（退出时调用；写失败只影响下次位置，不报错）。
func (l Layout) SaveWindowState(st WindowState) error {
	if st.Width < 480 || st.Height < 360 {
		return nil
	}
	body, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return os.WriteFile(l.windowStatePath(), body, 0o644)
}
