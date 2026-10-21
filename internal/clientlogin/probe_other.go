//go:build !windows

package clientlogin

import (
	"context"
	"os/exec"
	"time"
)

// defaultProbe 非 Windows 平台：pgrep -f 匹配进程名。
func defaultProbe() bool { return probeUnix() }

// probeUnix 在非 Windows 上用 pgrep -f 匹配进程名。
// pgrep 不存在时返回 false，同样只是失去保护而非报错。
func probeUnix() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "pgrep", "-f", "-i", "workbuddy").Output()
	if err != nil {
		return false
	}
	return containsClientImage(string(out))
}
