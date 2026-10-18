package desktop

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	// DefaultPort 桌面版首选端口（与 cmd/server 的默认配置一致，用户的书签/
	// 第三方客户端不用改）。
	DefaultPort = 7863
	// PortScanEnd 占用顺延的上限。顺延 10 个端口足够覆盖"上次的僵尸进程还没
	// 退干净"这类常见冲突，又不至于静默漂到让用户找不到的位置。
	PortScanEnd = 7873

	runtimeFileName = "runtime.json"
)

// RuntimeInfo runtime.json 的内容：告诉第二实例与用户"活着的那个实例在哪"。
type RuntimeInfo struct {
	PID       int    `json:"pid"`
	Addr      string `json:"addr"`
	Mode      string `json:"mode"`
	Version   string `json:"version"`
	StartedAt string `json:"started_at"`
}

// RuntimePath runtime.json 路径。
func (l Layout) RuntimePath() string { return filepath.Join(l.DataDir, runtimeFileName) }

// WriteRuntime 原子写 runtime.json。
func (l Layout) WriteRuntime(info RuntimeInfo) error {
	body, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	tmp := l.RuntimePath() + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, l.RuntimePath())
}

// ReadRuntime 读 runtime.json；不存在时返回 os.ErrNotExist 包装错误。
func (l Layout) ReadRuntime() (RuntimeInfo, error) {
	var info RuntimeInfo
	body, err := os.ReadFile(l.RuntimePath())
	if err != nil {
		return info, err
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return info, fmt.Errorf("解析 %s: %w", l.RuntimePath(), err)
	}
	return info, nil
}

// RemoveRuntime 删 runtime.json（退出路径；幂等）。
func (l Layout) RemoveRuntime() error {
	err := os.Remove(l.RuntimePath())
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	return err
}

// PickPort 选监听端口。
//
// # 为什么"占用顺延"要先自己 bind 一次
//
// 装配层只接受一个端口号（boot.Options.Port），bind 由它做。因此这里必须
// 先探测可用性再交给它 —— 直接传一个被占用的端口只会得到一条启动失败。
// 探测与真正 bind 之间存在竞态（探测后、bind 前别人抢走），所以顺延时
// 保留整段 7863..7873，而不是只赌一个。
//
// 顺延段全占时**让系统分配**：先 bind :0 拿到一个当前空闲的端口号再关掉，
// 把这个号码交给装配层。这是本函数里唯一一次真实的 bind 竞态，且退无可退
// （否则只剩"启动失败"一个结局）。
func PickPort(preferred int, logger *log.Logger) (int, error) {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	if preferred > 0 {
		return preferred, nil
	}
	for p := DefaultPort; p <= PortScanEnd; p++ {
		if portFree(p) {
			if p != DefaultPort {
				logger.Printf("端口 %d 被占用，改用 %d", DefaultPort, p)
			}
			return p, nil
		}
		if p == DefaultPort {
			logger.Printf("端口 %d 被占用，尝试顺延 %d..%d", p, p+1, PortScanEnd)
		}
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("端口 %d..%d 全部被占且系统分配也失败: %w", DefaultPort, PortScanEnd, err)
	}
	p := l.Addr().(*net.TCPAddr).Port
	l.Close()
	logger.Printf("端口 %d..%d 全部被占用，改由系统分配：%d", DefaultPort, PortScanEnd, p)
	return p, nil
}

// portFree 探测端口能否被本程序真正占用。
//
// # 为什么要测两种地址形态（Windows 特有的坑，实测踩到）
//
// 装配层绑的是配置里的 listen，默认 `:7863`，也就是**通配地址**。
// 而 Windows 与 Unix 不同：一个进程已经绑了通配地址 `:7863` 之后，
// 另一个进程**仍然可以**绑 `127.0.0.1:7863`（反之亦然）—— 两者在
// Windows 的绑定冲突判定里不算同一元组（除非用了 SO_EXCLUSIVEADDRUSE）。
//
// 只测 127.0.0.1 会得到"端口空闲"的假结论，然后把 7863 交给装配层，
// 装配层绑通配地址时撞上已有实例 → `bind: Only one usage of each socket
// address` → 启动失败。实测就是这样：浏览器模式双开时第二个实例直接报错。
//
// 因此这里两种形态都要求能绑上：通配（当前默认写法）+ 回环（用户把
// listen 改成 127.0.0.1:x 的写法）。任一被占就判为不空闲。
func portFree(p int) bool {
	port := strconv.Itoa(p)
	return canBindPort(net.JoinHostPort("", port)) && canBindPort(net.JoinHostPort("127.0.0.1", port))
}

func canBindPort(addr string) bool {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	l.Close()
	return true
}

// ProbeService 校验 addr 上跑的是**自家**服务。
//
// # 为什么不能只信 runtime.json
//
// runtime.json 是普通文件，可能残留（上次崩溃没删）、可能被别的东西覆盖，
// 端口也可能被另一个程序抢到。只按 port 判断会连到别人家服务上，然后
// 把"打开控制台"变成一次莫名其妙的外部请求。所以第二实例让位之前必须
// 看到 /healthz 的 service 字段等于自己的服务名。
func ProbeService(addr, wantService string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var body struct {
		Service string `json:"service"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body); err != nil {
		return fmt.Errorf("/healthz 响应不是 JSON: %w", err)
	}
	if wantService != "" && body.Service != wantService {
		return fmt.Errorf("/healthz 的 service=%q 与本程序（%q）不一致", body.Service, wantService)
	}
	return nil
}
