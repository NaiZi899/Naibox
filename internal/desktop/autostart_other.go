//go:build !windows

package desktop

import "errors"

// autostartEnabled 非 Windows 没有开机自启项。
func autostartEnabled() bool { return false }

// setAutostart 非 Windows 空实现。
func setAutostart(enabled bool, exePath string) error {
	if !enabled {
		return nil
	}
	return errors.New("当前平台不支持开机自启")
}
