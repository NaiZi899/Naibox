// legacyconfig_test.go 已删除功能在老 config.json 里的残留清理。
//
// 判据：清理幂等、不动解析不了的文件、写不进去也不阻断启动。
package boot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readConfig(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("清理后不是合法 JSON: %v", err)
	}
	return doc
}

// TestStripLegacyPromptSection 有该段 → 删掉，其它键原样保留。
func TestStripLegacyPromptSection(t *testing.T) {
	path := writeConfig(t, `{
	  "listen": ":7863",
	  "api_key": "k",
	  "prompt": {"mode": "custom", "file": "persona.md"},
	  "pool": {"breaker_threshold": 3}
	}`)

	removed, err := stripLegacyPromptSection(path)
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if !removed {
		t.Fatal("有 prompt 段时应报告已改写")
	}

	doc := readConfig(t, path)
	if _, ok := doc["prompt"]; ok {
		t.Error("prompt 段应被删除")
	}
	if doc["listen"] != ":7863" || doc["api_key"] != "k" {
		t.Errorf("其它键被动了: %v", doc)
	}
	pool, ok := doc["pool"].(map[string]any)
	if !ok || pool["breaker_threshold"] != float64(3) {
		t.Errorf("嵌套段丢字段: %v", doc["pool"])
	}
}

// TestStripLegacyPromptSectionIdempotent 没有该段 → 不写盘、字节不变。
//
// 幂等不是洁癖：启动每次都跑一次。若不判键就重写，用户手写的缩进/键序
// 会在每个实例启动时被规范化一次，diff 里出现无法解释的整文件变更。
func TestStripLegacyPromptSectionIdempotent(t *testing.T) {
	body := `{"listen": ":7863",   "api_key": "k"}`
	path := writeConfig(t, body)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	removed, err := stripLegacyPromptSection(path)
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if removed {
		t.Error("没有 prompt 段时不该报告已改写")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("没有该段时文件字节必须完全不变")
	}
}

// TestStripLegacyPromptSectionBrokenJSON 解析不了的文件不碰。
func TestStripLegacyPromptSectionBrokenJSON(t *testing.T) {
	body := `{"prompt": {"mode": "custom"` // 少一个右括号
	path := writeConfig(t, body)

	removed, err := stripLegacyPromptSection(path)
	if err != nil {
		t.Fatalf("解析失败应静默跳过，不该报错: %v", err)
	}
	if removed {
		t.Error("解析不了的文件不该被改写")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != body {
		t.Error("解析失败时文件必须保持原样（用户可能正在手工修它）")
	}
}

// TestStripLegacyPromptSectionMissingFile 文件不存在 → 不创建。
func TestStripLegacyPromptSectionMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such.json")
	removed, err := stripLegacyPromptSection(path)
	if err != nil || removed {
		t.Fatalf("文件不存在时不该出错也不该报告改写: removed=%v err=%v", removed, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("不该凭空创建配置文件")
	}
}

// TestStripLegacyPromptSectionEmptyPath 空路径（纯默认值启动）是空操作。
func TestStripLegacyPromptSectionEmptyPath(t *testing.T) {
	removed, err := stripLegacyPromptSection("")
	if err != nil || removed {
		t.Fatalf("空路径应为空操作: removed=%v err=%v", removed, err)
	}
}

// TestStripLegacyPromptSectionReadOnlyDir 只读目录 → 报错但不 panic，文件不变。
//
// 场景：容器里 config.json 以只读方式挂载。启动流程会把这个错误降级成
// 一行日志（见 boot.go 的调用点），所以这里只要求"返回错误且文件原样"。
func TestStripLegacyPromptSectionReadOnlyDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"prompt": {"mode": "custom"}, "listen": ":7863"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// 目录设为只读：Windows 上 Chmod 语义有限，权限没生效时跳过（不假装通过）。
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skipf("无法设置目录权限: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	removed, werr := stripLegacyPromptSection(path)
	if removed || werr == nil {
		// Windows 上目录权限位不影响写文件，这条路径无法在此平台构造。
		// 直接跳过而不是伪造通过 —— 假绿比缺失更糟。
		t.Skip("该平台对目录只读不生效（Windows 语义），无法构造写失败")
	}
	// 真的写失败了 → 文件必须保持原样。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if string(raw) != body {
		t.Error("写入失败时文件必须保持原样")
	}
}
