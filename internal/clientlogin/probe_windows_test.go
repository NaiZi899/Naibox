//go:build windows

package clientlogin

import (
	"os/exec"
	"testing"
)

// TestHideWindowSetsCreateNoWindow 钉住回退路径的窗口隐藏。
//
// 这条判据一旦丢，GUI 子系统（-H windowsgui）的桌面壳每次探测都会新建一个
// 控制台 → 用户看到终端窗口一闪。真实报障：任务执行期间每 3 秒一次。
func TestHideWindowSetsCreateNoWindow(t *testing.T) {
	cmd := exec.Command("tasklist")
	hideWindow(cmd)
	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr 为空：子进程会被分配新控制台，也就是弹窗")
	}
	if !cmd.SysProcAttr.HideWindow {
		t.Error("HideWindow 未置位")
	}
	if cmd.SysProcAttr.CreationFlags&0x08000000 == 0 {
		t.Error("CreationFlags 未带 CREATE_NO_WINDOW")
	}
}

// TestProbeNativeWorks 快照是首选路径；它失败才会退回 tasklist 子进程。
//
// 只断言"快照可用"，不断言 found —— 开发机上客户端可能真的开着。
func TestProbeNativeWorks(t *testing.T) {
	found, ok := probeNative()
	t.Logf("probeNative: found=%v ok=%v", found, ok)
	if !ok {
		t.Fatal("Toolhelp32 快照不可用：探测会退回 tasklist 子进程（有窗口风险）")
	}
}

// TestIsClientExe 快照路径的匹配判据：精确到 exe 名，大小写不敏感。
func TestIsClientExe(t *testing.T) {
	for _, name := range []string{"WorkBuddy.exe", "workbuddyai.exe", "CodeBuddy.EXE"} {
		if !isClientExe(name) {
			t.Errorf("%s 应判定为客户端进程", name)
		}
	}
	for _, name := range []string{"chrome.exe", "tasklist.exe", "workbuddy-updater.exe"} {
		if isClientExe(name) {
			t.Errorf("%s 不应判定为客户端进程", name)
		}
	}
}

// TestContainsClientImage 宽松匹配仍覆盖 tasklist CSV 与 pgrep 输出。
func TestContainsClientImage(t *testing.T) {
	if !containsClientImage(`"WorkBuddy.exe","4321","Console","1","120,000 K"`) {
		t.Error("tasklist CSV 行应命中")
	}
	if containsClientImage("INFO: No tasks are running which match the specified criteria.") {
		t.Error("空列表输出不应命中")
	}
}
