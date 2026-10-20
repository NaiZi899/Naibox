// config_missing_test.go 「配置不存在」与「配置里的某个文件不存在」必须分得开。
//
// # 这两件事为什么值得一条专门的测试
//
// boot.go 在配置读不到时会**回落成纯默认值启动**（这是有意的：空目录也能跑起来）。
// 判据一旦宽到能匹配 fs.ErrNotExist，normalize 里读 `prompt.file` 失败也会命中它 ——
// 于是"提示词路径写错一个字"变成"你整份配置被忽略，日志说 config.json 不存在"，
// 用户改的每一样东西静默失效。
//
// 2026-10-20 冒烟实测就是这个形状，所以这里把两条路径钉死。
package boot

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// TestMissingPromptFileIsNotConfigNotFound 配置存在、但提示词文件缺失 → **不是**配置缺失。
//
// 这条守的是回落逻辑：一旦被判成 ErrConfigNotFound，boot 会弃用整份配置，
// 我们新加的 prompt fail-fast 就等于没写。
func TestMissingPromptFileIsNotConfigNotFound(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	missingPrompt := filepath.Join(dir, "persona-not-here.md")

	body := map[string]any{
		"prompt": map[string]any{"mode": "custom", "file": missingPrompt},
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
		t.Fatal("提示词文件缺失时应启动失败（fail fast），而不是静默回落")
	}
	if errors.Is(err, ErrConfigNotFound) {
		t.Fatalf("不得命中 ErrConfigNotFound —— 那会让 boot 弃用整份配置：%v", err)
	}
	if !strings.Contains(err.Error(), "prompt") {
		t.Errorf("错误信息应指明是提示词文件的问题，实际: %v", err)
	}
}
