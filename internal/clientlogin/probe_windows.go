//go:build windows

package clientlogin

import (
	"context"
	"os/exec"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// defaultProbe Windows 实现：优先原生快照，快照不可用才回退 tasklist。
func defaultProbe() bool {
	if found, ok := probeNative(); ok {
		return found
	}
	return probeTasklist()
}

// probeNative 用 Toolhelp32 快照枚举进程名。
//
// # 为什么不用 tasklist 作为首选（真实报障）
//
// 桌面壳是 `-H windowsgui` 子系统，进程启动时没有控制台。Windows 上起一个
// 控制台程序（tasklist.exe 就是），父进程又没有控制台可继承时，系统会给它
// 分配一个**新的控制台** —— Win11 上由 Windows Terminal 呈现，用户看到的是
// 一个每几秒闪一次的终端窗口。
//
// 快照没有任何子进程，既没有窗口，也把一次探测从 ~400ms 降到 ~1ms
// （后者是"任务执行期间页面每 2 秒刷新"时真正被放大的成本）。
//
// 返回值 ok=false 表示快照不可用（API 失败/权限异常），调用方回退 tasklist。
func probeNative() (found, ok bool) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false, false
	}
	defer windows.CloseHandle(snap)

	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	if err := windows.Process32First(snap, &e); err != nil {
		return false, false
	}
	for {
		if isClientExe(windows.UTF16ToString(e.ExeFile[:])) {
			return true, true
		}
		if err := windows.Process32Next(snap, &e); err != nil {
			break
		}
	}
	return false, true
}

// probeTasklist 是快照失败时的兜底。
//
// 参数说明：/FI 做服务端过滤（比拉全量列表再自己找快得多），
// /NH 去掉表头，/FO CSV 让输出好解析。
// 没有任何匹配时 tasklist 输出的是 "INFO: No tasks are running..."，
// 因此不能只看退出码，必须看输出里有没有进程名。
// 国内版（WorkBuddy.exe）与海外版（WorkBuddyAI.exe）都要查。
//
// ⚠ 每次 spawn 都必须过 hideWindow：漏掉它就会回到"闪终端窗口"的老毛病。
func probeTasklist() bool {
	// tasklist 在 System32 下；万一 PATH 被裁剪，用绝对路径兜底。
	candidates := []string{"tasklist", `C:\Windows\System32\tasklist.exe`}
	for _, bin := range candidates {
		// 任一 image 查询失败 = 这个 bin 不可用（命令缺失/超时），换下一个 bin；
		// 全部查询成功且无匹配则 bin 可用、结论为未运行，直接返回。
		usable := true
		for _, image := range []string{"WorkBuddy.exe", "WorkBuddyAI.exe"} {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			cmd := exec.CommandContext(ctx, bin,
				"/FI", "IMAGENAME eq "+image, "/NH", "/FO", "CSV")
			hideWindow(cmd)
			out, err := cmd.Output()
			cancel()
			if err != nil {
				// 换个候选路径再试；全都失败就只能返回 false（不阻断功能，
				// 只是失去这层保护，由 UI 上的说明兜住）。
				usable = false
				break
			}
			if containsClientImage(string(out)) {
				return true
			}
		}
		if usable {
			return false
		}
	}
	return false
}

// hideWindow 让 spawn 出来的子进程不弹控制台窗口。
//
// 与 qoder/runtimeinfo_windows.go 同一套写法（HideWindow + CREATE_NO_WINDOW）：
// 只设 HideWindow 时控制台仍会被分配（只是隐藏），CREATE_NO_WINDOW 则连分配
// 都省掉。两者一起用，避免任何一版运行时把隐藏语义解释得不一样。
func hideWindow(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
}
