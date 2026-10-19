package desktop

import (
	"log"
	"os"
	"path/filepath"
	"testing"
)

// newStoreAt 造一个指向临时目录的 windowStore。
func newStoreAt(t *testing.T) (*windowStore, Layout) {
	t.Helper()
	dir := t.TempDir()
	return newWindowStore(Layout{DataDir: dir}, log.New(os.Stderr, "", 0)), Layout{DataDir: dir}
}

// TestGeometryRoundTripsMaximized 最大化标志必须与几何一起存取。
//
// # 守的是什么（实测事故，2026-10-19）
//
// 最大化窗口的 GetWindowRect 返回「铺满屏幕 + 边框」的尺寸：实测在逻辑
// 2560×1600 的屏幕上记成 `2582×1622 @ (-11,-11)`。只存几何不存「当时是
// 最大化的」，下次启动就会按**普通窗口**建这么一个大窗口 —— 比屏幕还大、
// 位置又是负坐标，用户看到窗口跑到屏幕外/只剩左上角一条。
//
// 用户机器上的真实 window.json 就是这份形态，所以这条用例的夹具直接取它。
func TestGeometryRoundTripsMaximized(t *testing.T) {
	store, l := newStoreAt(t)

	store.SetGeometryMax(-11, -11, 2582, 1622, true)

	// 模拟重启：从同一目录重新加载。
	reloaded := newWindowStore(l, log.New(os.Stderr, "", 0)).Get()
	if !reloaded.Maximized {
		t.Error("重启后 Maximized 丢了 —— 会按普通窗口建一个比屏幕还大的窗口")
	}
	if reloaded.Width != 2582 || reloaded.Height != 1622 {
		t.Errorf("几何丢失: %dx%d", reloaded.Width, reloaded.Height)
	}
	if !reloaded.HasPos || reloaded.X != -11 || reloaded.Y != -11 {
		t.Errorf("位置丢失: has_pos=%v (%d,%d)", reloaded.HasPos, reloaded.X, reloaded.Y)
	}
}

// TestUnmaximizedGeometryClearsFlag 取消最大化后必须把标志位落成 false。
//
// 否则用户把窗口从最大化恢复成普通大小，程序仍然记着"上次是最大化的"，
// 下次启动又给他弹一个最大化窗口 —— 他自己调过的尺寸等于白调。
func TestUnmaximizedGeometryClearsFlag(t *testing.T) {
	store, l := newStoreAt(t)

	store.SetGeometryMax(-11, -11, 2582, 1622, true)
	store.SetGeometryMax(120, 90, 1100, 760, false) // 用户还原了窗口

	got := newWindowStore(l, log.New(os.Stderr, "", 0)).Get()
	if got.Maximized {
		t.Error("还原成普通窗口后 Maximized 仍为 true —— 下次启动会再次最大化，用户调过的尺寸白调")
	}
	if got.Width != 1100 || got.Height != 760 {
		t.Errorf("还原后的几何没存对: %dx%d", got.Width, got.Height)
	}
}

// TestGeometryStillRejectsTinySizes 最小尺寸保护不受这次改动影响。
//
// 缩到看不见的窗口（<480×360）不该被记住 —— 那条保护是给"读到垃圾几何"
// 准备的。新增 max 参数时很容易顺手把它绕过去。
func TestGeometryStillRejectsTinySizes(t *testing.T) {
	store, l := newStoreAt(t)

	if changed := store.SetGeometryMax(0, 0, 135, 37, false); changed {
		t.Error("135×37 不该被记住（那是窗口被压到最小跟踪尺寸的形态，记下来会自我延续）")
	}
	got := newWindowStore(l, log.New(os.Stderr, "", 0)).Get()
	if got.Width != 1180 || got.Height != 820 {
		t.Errorf("被拒绝的几何污染了状态: %dx%d（应为默认 1180×820）", got.Width, got.Height)
	}
}

// TestSetGeometryWithoutMaxFlagDefaultsFalse SetGeometry 保持"非最大化"语义。
//
// 保留这个单参版本是为了老调用方语义不变：它表达的是"普通窗口的一次几何
// 更新"，不该继承之前可能存在的 true。
func TestSetGeometryWithoutMaxFlagDefaultsFalse(t *testing.T) {
	store, _ := newStoreAt(t)

	store.SetGeometryMax(0, 0, 1200, 800, true)
	store.SetGeometry(50, 60, 1000, 700)

	if got := store.Get(); got.Maximized {
		t.Error("SetGeometry 应把 Maximized 归 false，实际仍是 true")
	}
}

// TestWindowStateJSONKeepsOldFilesReadable 老 window.json（没有 maximized 键）仍可读。
//
// 该字段带 omitempty 且零值就是 false —— 所以历史文件的"没有这个键"与
// "上次不是最大化"语义一致，正是我们想要的回落方向。
func TestWindowStateJSONKeepsOldFilesReadable(t *testing.T) {
	_, l := newStoreAt(t)
	old := `{"width":1180,"height":820,"x":100,"y":80,"has_pos":true,"close_behavior":"ask","tray_hint_shown":true}`
	if err := os.WriteFile(filepath.Join(l.DataDir, windowStateFileName), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	got := newWindowStore(l, log.New(os.Stderr, "", 0)).Get()
	if got.Maximized {
		t.Error("老文件没有 maximized 键，必须读成 false")
	}
	if got.Width != 1180 || got.X != 100 {
		t.Errorf("老文件几何读坏: %dx%d @ (%d,%d)", got.Width, got.Height, got.X, got.Y)
	}
}
