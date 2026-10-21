// running.go 探测 WorkBuddy 桌面客户端进程是否在运行。
//
// 为什么需要这个判断（实测教训）：
//
// 客户端是常驻 Electron 应用，登录态主要活在它自己的内存里。我们在磁盘上把
// workbuddy-desktop.info 换成别的账号后，客户端**不会察觉**，它下一次刷新 token
// 时仍按内存里的旧会话写回磁盘 —— 现场表现就是「切了但过一会儿自己变回去了」。
//
// 实测证据：08:30 切到目标账号 A 后，09:40 客户端自己把 token 刷了一遍，
// 写回的 sessionState 与它 09-10 就持有的那份完全相同，说明磁盘写入被原地覆盖。
//
// 所以切换前必须先确认客户端没在跑；在跑就明确拒绝并让用户先退出，
// 而不是写完让用户以为成功、过一会儿又被还原。
//
// # 平台实现（见 probe_windows.go / probe_other.go）
//
// Windows 上走 Toolhelp32 快照，**不起子进程**；快照不可用才回退 tasklist，
// 且回退路径必须隐藏窗口。原因：桌面壳是 -H windowsgui 子系统，父进程没有
// 控制台可继承，起任何控制台程序都会被分配一个新控制台 —— 用户看到的就是
// 一个终端窗口一闪。而轮询会把这条路径跑到每几秒一次（真实报障）。
package clientlogin

import (
	"strings"
	"sync"
	"time"
)

// procCacheTTL 是进程探测结果的缓存时长。
//
// 管理台的本机登录状态是按需查询的（成长面板可见时 30s 一轮、手动刷新、
// 切换前后各一次），没必要每次都重新探测；但也不能缓存太久，否则用户
// 刚退出客户端仍会被拒。
const procCacheTTL = 3 * time.Second

// clientExeNames 是 Windows 上会看到的**精确**进程名（小写比较）。
// 主进程国内版是 WorkBuddy.exe，海外版（WorkBuddy AI）是 WorkBuddyAI.exe；
// 不同版本/安装方式下可能还有别的名字，一并覆盖。
//
// 快照路径用精确匹配：tasklist 那条宽松匹配会把 workbuddy-updater.exe
// 之类也算进来，在"只要能看见进程名"的场景无所谓，但没必要扩大误判面。
var clientExeNames = []string{
	"workbuddy.exe",
	"workbuddyai.exe", // 海外版主进程 WorkBuddyAI.exe
	"codebuddy.exe",
}

// clientImageNames 用于**整段文本**的宽松匹配（pgrep 输出、tasklist CSV）。
// 多出来的 "workbuddy" / "workbuddy-ai" 是为了兜住命令行里不带 .exe 的形态。
var clientImageNames = []string{
	"workbuddy.exe",
	"workbuddyai.exe",
	"workbuddy",
	"workbuddy-ai",
	"codebuddy.exe",
}

// processChecker 缓存一次进程探测结果，避免高频轮询反复探测。
type processChecker struct {
	mu    sync.Mutex
	at    time.Time
	alive bool
	// probe 是实际探测函数；nil 表示走平台默认实现。测试用它注入假结果。
	probe func() bool
}

// running 报告客户端是否在运行（带短缓存）。
func (p *processChecker) running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.at.IsZero() && time.Since(p.at) < procCacheTTL {
		return p.alive
	}
	probe := p.probe
	if probe == nil {
		probe = defaultProbe
	}
	p.alive = probe()
	p.at = time.Now()
	return p.alive
}

// invalidate 让下一次 running 重新探测。
// 切换/回滚成功后调用：刚改完盘，用户可能马上就退出了客户端。
func (p *processChecker) invalidate() {
	p.mu.Lock()
	p.at = time.Time{}
	p.mu.Unlock()
}

// isClientExe 判断一个**单个**进程名是不是客户端主进程（精确匹配，大小写不敏感）。
func isClientExe(name string) bool {
	low := strings.ToLower(name)
	for _, exe := range clientExeNames {
		if low == exe {
			return true
		}
	}
	return false
}

// containsClientImage 判断进程列表**文本**里是否出现客户端进程名。
//
// 调用方是 tasklist 回退路径（整段 CSV）与 pgrep 输出（命令行）。快照路径
// 拿到的是单个 exe 名，用 isClientExe 精确匹配 —— 两者判据不同，别互换。
func containsClientImage(out string) bool {
	low := strings.ToLower(out)
	for _, name := range clientImageNames {
		if strings.Contains(low, name) {
			return true
		}
	}
	return false
}
