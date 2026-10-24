package browserprofile

// launch.go 用一份持久档案启动浏览器。
//
// # 与 browseropen.Plan 的三处关键差异
//
//	browseropen  无痕参数（--incognito / --inprivate）+ 临时目录，48h 清理
//	本包          无无痕参数 + **持久**目录 + 调试端口 + 可选代理
//
// 无痕与持久是互斥的：给了 --incognito 还指望登录态留存，是自欺欺人
// （无痕窗口的 cookie 只活到窗口关闭）。所以这里**刻意不传**无痕参数，
// 也不复用 Plan 的参数向量 —— 只复用它的浏览器发现（browseropen.Resolve）。
import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/NaiZi899/Naibox/internal/browseropen"
)

// Options 一次启动的参数。
type Options struct {
	// URL 要打开的地址（空 = 只开一个空白窗口）。
	URL string
	// ProxyURL 该档案的出口（来自线路表的 proxy）。
	//
	// ⚠ 带用户名的代理**不支持**：Chrome 的 --proxy-server 不处理认证，
	// 带 userinfo 的 URL 会被它静默忽略（表现为"设了代理但没生效"）。
	// 所以这里宁可报错，也不让用户以为自己配上了。
	ProxyURL string
	// ExtraArgs 追加参数（排障用：--headless=new / --window-size 等）。
	ExtraArgs []string
	// Explicit 显式指定的浏览器可执行文件（config.login.browser）。
	Explicit string
	// Lookup 浏览器探测钩子（测试注入）。
	Lookup func(string) (string, bool)
	// Now 时间源（测试注入）。
	Now time.Time
}

// Cmd 一次启动的完整描述（规划结果，不含执行）。
type Cmd struct {
	// Browser 展示名。
	Browser string
	// Path 启动用的可执行文件（darwin 上是 /usr/bin/open）。
	Path string
	// Args 参数向量（URL 在末位，由 Argv 追加）。
	Args []string
	// URL 要打开的地址。
	URL string
	// UserDataDir 持久档案目录。
	UserDataDir string
	// Private 恒为 false —— 保留字段是为了让"这个档案无痕不无痕"成为
	// 可断言的数据，而不是注释里的承诺。
	Private bool
}

// Argv 最终参数向量（URL 在末位）。
//
// ⚠ URL 必须最后：放中间时浏览器会把它当成某个开关的值吃掉，
// 表现成"窗口开了但停在首页"。
func (c Cmd) Argv() []string {
	out := make([]string, 0, len(c.Args)+1)
	out = append(out, c.Args...)
	if c.URL != "" {
		out = append(out, c.URL)
	}
	return out
}

// Plan 规划一次启动。**纯决策**（唯一副作用是确保档案目录存在 ——
// Chrome 在 --user-data-dir 指向不存在的路径时行为依版本而异，
// 有的会静默改用默认 profile，那样"独立档案"就静默失效了）。
func Plan(goos string, p Profile, userDataDir string, opts Options) (Cmd, error) {
	// 归属优先：档案上钉死的 BrowserPath（由调用方塞进 opts.Explicit）→
	// 全局 login.browser → 自动探测。指纹档案与普通档案因此可以共存。
	launcher, err := browseropen.Resolve(goos, opts.Explicit, opts.Lookup)
	if err != nil {
		return Cmd{}, err
	}
	if strings.TrimSpace(userDataDir) == "" {
		return Cmd{}, errors.New("档案目录为空")
	}
	if err := os.MkdirAll(userDataDir, 0o755); err != nil {
		return Cmd{}, fmt.Errorf("创建档案目录失败: %w", err)
	}

	args := []string{
		"--user-data-dir=" + userDataDir,
		"--no-first-run",
		"--no-default-browser-check",
		// 0 = 由浏览器选一个空闲端口，启动后读 DevToolsActivePort。
		// 固定端口在"同时开两个档案"时会撞车，而撞车的表现是
		// "第二个窗口起不来"，很难查。
		"--remote-debugging-port=0",
	}
	if opts.ProxyURL != "" {
		if err := checkProxySettable(opts.ProxyURL); err != nil {
			return Cmd{}, err
		}
		args = append(args, "--proxy-server="+opts.ProxyURL)
	}
	// 指纹参数来自档案（固化），排在用户附加参数之前 —— 附加参数里若真写了
	// 同名开关，后写的生效，用户仍有最终决定权（Plan 只挡 --user-data-dir
	// 与 --proxy-server 这两个会破坏"档案/线路"语义的）。
	if p.Fingerprint != nil {
		args = append(args, p.Fingerprint.Args()...)
	}
	args = append(args, opts.ExtraArgs...)

	cmd := Cmd{
		Browser:     launcher.Name,
		Path:        launcher.Path,
		Args:        args,
		URL:         opts.URL,
		UserDataDir: userDataDir,
	}
	if launcher.Darwin {
		// open -n -a <App> --args … ：`--args` 是分界符，没有它浏览器
		// 什么都收不到（open 会把未知 --xxx 当成自己的开关）。
		cmd.Args = append([]string{"-n", "-a", launcher.Name, "--args"}, args...)
	}
	return cmd, nil
}

// checkProxySettable 只放行 Chrome 能自己处理的代理形态。
func checkProxySettable(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("代理 URL 解析失败: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return fmt.Errorf("浏览器不支持 %q 代理（只支持 http / https / socks5 / socks5h）", u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf("代理带认证信息（%s://user:pass@…）：Chrome 的 --proxy-server 不处理认证，"+
			"设了也不会生效。请换一条免认证的出口，或用本地转发口把它变成免认证的", u.Scheme)
	}
	return nil
}

// starter 启动一个规划好的命令（测试注入用）。
var starter = func(path string, args []string) error {
	c := exec.Command(path, args...)
	if err := c.Start(); err != nil {
		return err
	}
	// 不接管道、不 Wait：本包只负责把浏览器放飞。
	_ = c.Process.Release()
	return nil
}

// Open = Plan + 启动 + 记账（不等待浏览器退出）。
//
// 启动成功后立刻 Touch（LastUsedAt/LastURL），调试端口在读回后补写 ——
// 读 DevToolsActivePort 是异步的（浏览器要先把窗口建起来）。
func Open(s *Store, p Profile, opts Options) (Cmd, error) {
	dir := s.UserDataDir(p)
	cmd, err := Plan(runtime.GOOS, p, dir, opts)
	if err != nil {
		return Cmd{}, err
	}
	// 指纹参数只对"变体"有效；原版会静默忽略。宁可在启动前拒绝，
	// 也不开一个"看起来配好了、指纹其实没变"的窗口。
	if p.Fingerprint != nil && !p.Fingerprint.Empty() {
		ok, serr := SupportsFingerprint(cmd.Path)
		if serr != nil {
			return cmd, fmt.Errorf("无法确认 %s 是否支持指纹参数: %w", cmd.Browser, serr)
		}
		if !ok {
			return cmd, fmt.Errorf("%s 不是支持 --fingerprint-* 的浏览器变体：这些参数会被静默忽略，"+
				"等于指纹没变。请在档案里把「浏览器」指向 fingerprint-chromium 那类变体，或清空指纹设置", cmd.Browser)
		}
	}
	if err := starter(cmd.Path, cmd.Argv()); err != nil {
		return cmd, fmt.Errorf("启动 %s 失败: %w", cmd.Browser, err)
	}
	_ = s.Touch(p.ID, opts.URL, 0)
	go func() {
		if port, ok := readDevToolsPort(dir, 5*time.Second); ok {
			_ = s.Touch(p.ID, opts.URL, port)
		}
	}()
	return cmd, nil
}

// WaitDebugPort 等浏览器把调试端口写进 DevToolsActivePort，最多等 timeout。
//
// 导出给管理台用：启动回执里要显示端口，而端口是浏览器自己选的（:0），
// 只能靠这个文件拿。等不到不算失败（浏览器可能还在起），调用方按 0 处理。
func WaitDebugPort(userDataDir string, timeout time.Duration) (int, bool) {
	return readDevToolsPort(userDataDir, timeout)
}

// readDevToolsPort 读 `<user-data-dir>/DevToolsActivePort` 的第一行。
//
// # 为什么用这个文件而不是日志/进程参数
//
// `--remote-debugging-port=0` 时端口是浏览器选的，它会把结果写进这个文件
// （第一行端口、第二行 ws 路径）。这是 Chrome/Edge/Brave/Chromium 共有的
// 行为，而解析命令行拿不到（0 就是 0）。轮询它比 sleep 之后猜端口可靠。
func readDevToolsPort(dir string, timeout time.Duration) (int, bool) {
	path := filepath.Join(dir, "DevToolsActivePort")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil {
			line := strings.SplitN(string(raw), "\n", 2)[0]
			if n, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && n > 0 {
				return n, true
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return 0, false
}
