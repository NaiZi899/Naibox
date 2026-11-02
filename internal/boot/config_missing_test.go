// config_missing_test.go 「配置文件本身不存在」与「配置存在但内容有问题」必须分得开。
//
// # 这件事为什么值得一条专门的测试
//
// boot.go 在配置读不到时会**回落成纯默认值启动**（这是有意的：空目录也能跑起来）。
// 判据一旦宽到能匹配 fs.ErrNotExist 或任意解析错误，用户改错一个字段就会变成
// "你整份配置被忽略，日志说 config.json 不存在"，他改的每一样东西都静默失效。
//
// 2026-10-20 冒烟实测就是这个形状（当时的原因是 normalize 里读 prompt.file 失败
// 也命中同一个判据；提示词功能已删除，但这条纪律与它的其他触发点仍在）。
package boot

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestConfigNotFoundSentinelForMissingConfig 配置文件本身缺失 → 哨兵命中。
func TestConfigNotFoundSentinelForMissingConfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-config.json")
	_, err := LoadAt(missing, "")
	if err == nil {
		t.Fatal("配置文件不存在时应返回错误")
	}
	if !errors.Is(err, ErrConfigNotFound) {
		t.Fatalf("应命中 ErrConfigNotFound（调用方靠它决定是否回落默认值），实际: %v", err)
	}
}

// TestBrokenConfigIsNotConfigNotFound 配置存在但内容非法 → **不是**配置缺失。
//
// 这条守的是回落逻辑：一旦被判成 ErrConfigNotFound，boot 会弃用整份配置、
// 按纯默认值启动，于是"某个字段写错了"变成"所有设置都没了"，而且日志还在
// 讨论 config.json 不存在。
func TestBrokenConfigIsNotConfigNotFound(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	// 合法 JSON、非法取值：cooldown 段给一个不能解析的时长。
	body := map[string]any{
		"cooldown": map[string]any{"soft_rate": "2hours"},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = LoadAt(cfgPath, "")
	if err == nil {
		t.Fatal("非法取值应启动失败（fail fast），而不是静默回落默认值")
	}
	if errors.Is(err, ErrConfigNotFound) {
		t.Fatalf("不得命中 ErrConfigNotFound —— 那会让 boot 弃用整份配置：%v", err)
	}
}
