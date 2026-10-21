// Package openurl 用系统默认浏览器打开一个 http(s) 链接。
//
// 控制台跑在 WebView2 里，直接 target=_blank 可能把页面本身导航走，
// 所以「关于软件」里的项目地址与推荐项目统一交给后端启动系统浏览器。
// 只接受 http/https，file:// 与 javascript: 在参数校验阶段就被拒绝。
package openurl

import (
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
)

const maxURLLength = 2048

// Open 启动系统浏览器打开 raw，不等待浏览器退出。
func Open(raw string) error {
	target, err := validate(raw)
	if err != nil {
		return err
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		// rundll32 对 URL 里的 & 等字符按单独参数处理，不经过 shell。
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	case "darwin":
		cmd = exec.Command("open", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动浏览器失败: %w", err)
	}
	// 回收子进程，避免僵尸；浏览器是否真的打开不由这里判断。
	go func() { _ = cmd.Wait() }()
	return nil
}

// validate 只放行 http/https，返回规范化后的地址。
func validate(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("链接为空")
	}
	if len(s) > maxURLLength {
		return "", fmt.Errorf("链接过长（最多 %d 字节）", maxURLLength)
	}
	if strings.ContainsAny(s, "\r\n\x00") {
		return "", fmt.Errorf("链接包含控制字符")
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("链接不合法: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("只支持 http/https 链接")
	}
	if u.Host == "" {
		return "", fmt.Errorf("链接缺少主机名")
	}
	return u.String(), nil
}
