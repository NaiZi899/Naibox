// config_drift_test.go —— 守"桌面版内嵌默认配置与仓库根示例配置的漂移"。
//
// # 这个文件为什么存在（一次真实发布事故）
//
// inkstone（书生·端砚）与 jiyuan（基元律动）这两个上游是后加进代码的：
// config.go 里有完整的段定义与 normalize，boot.go 里有 registry.Register，
// internal/<name>/ 下的实现也齐全。
//
// **但两份 example 配置都没同步加段。** 而本项目的配置语义是
// "段缺席 = 不加载"（`c.JiyuanEnabled = c.Jiyuan.Enabled`，缺省 false）。
//
// 后果不是报错，是**静默少两个上游**：
//
//	用户下载 Release 包 → 拿到的 config.example.json 里没有 jiyuan/inkstone
//	→ 双击启动 → 顶栏显示"上游 10"
//	→ 而 README 承诺 12 个 → 用户以为"基元模块被打包丢了"
//
// 与此同时 workbuddy_intl 在 example 里是 enabled:true，于是现象看起来
// 像"基元被 workbuddy 国际版顶替了"——实际上国际版一直都在，只是另外
// 两个从未出现在示例配置里。
//
// 这类缺口**编译不报错、测试不报错、启动不报错**，只在用户拿到成品后
// 以"少了一个功能"的形态暴露。所以必须由测试把"示例配置的完整性"
// 变成可执行断言。
package desktop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// exampleUpstreams 所有**应当出现在示例配置里**的上游段名。
//
// 判据不是"代码里注册了几个"（那样会把这个测试变成 implementation 的镜像），
// 而是"README 承诺给用户的那份清单"。新增上游时这里与 README 同步改 ——
// 漏改的表现就是本文件顶部描述的那次事故。
var exampleUpstreams = []string{
	"codearts", "cline", "raccoon", "lobsterai", "qoder",
	"workbuddy_intl", "trae", "loomy", "inkstone", "jiyuan",
}

func readJSONMap(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", path, err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s 不是合法 JSON: %v", path, err)
	}
	return m
}

func keysOfRaw(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestExampleConfigsListEveryUpstream 两份示例配置都必须列出全部上游段，且默认启用。
//
// 两份都要查：用户拿到的可能是 Release 里的 config.example.json（服务器形态），
// 也可能是桌面版首次启动生成的那份（内嵌 defaultconfig.example.json）。
// 只修一份等于另一条路径仍然缺上游。
func TestExampleConfigsListEveryUpstream(t *testing.T) {
	docs := []struct {
		label string
		path  string
	}{
		{"仓库根 config.example.json", filepath.Join("..", "..", "config.example.json")},
		{"桌面版内嵌 defaultconfig.example.json", "defaultconfig.example.json"},
	}

	for _, d := range docs {
		m := readJSONMap(t, d.path)
		for _, name := range exampleUpstreams {
			raw, ok := m[name]
			if !ok {
				t.Errorf("%s 缺少 %q 段 —— 配置语义是「段缺席=不加载」，"+
					"用户在成品里将看不到这个上游（不会报错，只是少一个）", d.label, name)
				continue
			}
			var seg struct {
				Enabled bool `json:"enabled"`
			}
			if err := json.Unmarshal(raw, &seg); err != nil {
				t.Errorf("%s 的 %q 段不是合法对象: %v", d.label, name, err)
				continue
			}
			if !seg.Enabled {
				t.Errorf("%s 的 %q.enabled=false —— 示例配置的作用就是让用户"+
					"开箱看到全部上游，默认关着等于没写", d.label, name)
			}
		}
	}
}

// TestExampleConfigsStayInSync 两份示例配置的**顶层键集合**必须一致。
//
// 桌面版那份是根示例的副本（`//go:embed` 不能跨目录，所以只能复制）。
// 复制就会漂移：改了根的那份忘了改内嵌的那份，表现为"服务器形态有、
// 桌面形态没有"，或者反过来。
//
// 只比键集合而不比完整内容：`api_key` 的值刻意不同（内嵌那份在运行时
// 会被替换成随机 key），比内容会误报。
func TestExampleConfigsStayInSync(t *testing.T) {
	root := readJSONMap(t, filepath.Join("..", "..", "config.example.json"))
	emb := readJSONMap(t, "defaultconfig.example.json")

	rk, ek := keysOfRaw(root), keysOfRaw(emb)
	if strings.Join(rk, ",") != strings.Join(ek, ",") {
		t.Errorf("两份示例配置的顶层键不一致（复制后漂移了）：\n  根:   %v\n  内嵌: %v\n"+
			"以根那份为准补齐内嵌那份，或者反过来", rk, ek)
	}
}
