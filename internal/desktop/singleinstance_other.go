//go:build !windows

package desktop

import (
	"log"
	"time"
)

// instanceGuard 非 Windows 的空实现（单实例只对桌面形态有意义）。
type instanceGuard struct{}

// acquireInstance 非 Windows 恒为"主实例"。
func acquireInstance(l Layout, logPath, expectService string, waitForLock time.Duration, logger *log.Logger, onActivate func()) (*instanceGuard, bool, error) {
	return &instanceGuard{}, true, nil
}

// Close 空实现。
func (g *instanceGuard) Close() {}
