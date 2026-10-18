//go:build !windows

package desktop

import "errors"

// startTray 非 Windows 没有托盘（本壳只面向 Windows 桌面）。
//
// 这里必须给出同名同签名的实现而不是让文件缺失：`GOOS=linux go build ./...`
// 是"build tag 隔离有效"的验收项，缺文件就等于把整个仓库的跨平台编译
// 打死在桌面壳上。
func startTray(spec traySpec, icon []byte, onAction func(trayAction)) (trayHandle, error) {
	return nil, errors.New("当前平台无托盘支持（仅 Windows 桌面提供）")
}
