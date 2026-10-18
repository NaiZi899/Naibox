//go:build windows

package desktop

import (
	"fmt"
	"strings"

	"golang.org/x/sys/windows/registry"
)

const (
	// autostartKeyPath 每用户自启项。用 HKCU 而不是 HKLM：
	// 写 HKLM 需要管理员，且"这台机器的所有用户都自启"不是桌面工具的语义。
	autostartKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`
	// autostartValueName 任务管理器"启动"页显示的项名。
	autostartValueName = "NaiBox"
	// autostartValueNameLegacy 改名前的项名（omni2api → NaiBox）。
	//
	// 必须认它：老用户的自启项还挂在旧名下，只读新名字会被判成"没开"，
	// 于是第一次点「开机自启」变成"再写一个"，任务管理器的启动页里就出现
	// 两条一模一样的东西。关闭时两个都要删。
	autostartValueNameLegacy = "omni2api"
)

// autostartEnabled 查询开机自启是否已开（新旧两个项名都算）。
func autostartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, autostartKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	for _, name := range []string{autostartValueName, autostartValueNameLegacy} {
		if v, _, err := k.GetStringValue(name); err == nil && strings.TrimSpace(v) != "" {
			return true
		}
	}
	return false
}

// setAutostart 增删自启项。
//
// 命令行固定加 -autostart：开机时用户不在电脑前，别把浏览器的
// 控制台标签页弹到他脸上（window 模式下由窗口自己决定是否前置）。
func setAutostart(enabled bool, exePath string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, autostartKeyPath, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		if !enabled {
			return nil // 删一个本来就不存在的项：视作成功
		}
		return fmt.Errorf("打开注册表 HKCU\\%s: %w", autostartKeyPath, err)
	}
	defer k.Close()
	if !enabled {
		for _, name := range []string{autostartValueName, autostartValueNameLegacy} {
			if err := k.DeleteValue(name); err != nil && err != registry.ErrNotExist {
				return fmt.Errorf("删除自启项 %s: %w", name, err)
			}
		}
		return nil
	}
	cmd := fmt.Sprintf("%q -autostart", exePath)
	if err := k.SetStringValue(autostartValueName, cmd); err != nil {
		return fmt.Errorf("写入自启项: %w", err)
	}
	return nil
}
