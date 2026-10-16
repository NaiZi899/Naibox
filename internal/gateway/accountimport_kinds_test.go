// accountimport_kinds_test.go 导入形态清单的**线上契约**：下发的 JSON 字段名。
//
// # 为什么值得单独一个文件
//
// `ImportCredentialKind` 是**直接下发到前端**的结构体（manifest 的
// `import_kinds`）。没有 json tag 时 Go 会用字段名原样序列化
// （"ID"/"Label"/"Secret"），前端按 `k.id` 读就是 undefined ——
//
//	界面上：弹窗里标签空白、输入框没有占位符、密码框退化成明文框
//	接口上：一切正常（两条形态确实下发了，字段都在）
//
// 这种"接口对、界面错"的漂移最难查，因为两边各自看都没问题。
// 实测踩过一次（上线后看 manifest 才发现），所以这里把字段名钉死。
package gateway

import (
	"encoding/json"
	"testing"
)

// TestImportCredentialKindJSONFieldNames 钉住 snake/lowercase 字段名。
func TestImportCredentialKindJSONFieldNames(t *testing.T) {
	raw, err := json.Marshal(ImportCredentialKind{
		ID:          "session",
		Label:       "登录凭证",
		Hint:        "提示",
		Placeholder: "sess_…",
		Secret:      true,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// 前端读的就是这几个名字（webui.html 的 openImportKindsDialog）。
	for _, want := range []string{"id", "label", "hint", "placeholder", "secret"} {
		if _, ok := m[want]; !ok {
			t.Errorf("下发 JSON 缺字段 %q —— 前端会读到 undefined。实际字段: %v", want, keysOfAny(m))
		}
	}
	// 反向：Go 字段名不该出现（那就是漏了 tag）。
	for _, bad := range []string{"ID", "Label", "Hint", "Placeholder", "Secret"} {
		if _, ok := m[bad]; ok {
			t.Errorf("下发 JSON 里出现了 Go 字段名 %q —— 说明漏了 json tag", bad)
		}
	}

	// 值必须原样带出（不能只对字段名）。
	if m["id"] != "session" || m["label"] != "登录凭证" ||
		m["placeholder"] != "sess_…" || m["secret"] != true {
		t.Errorf("字段值不对: %v", m)
	}
}

// TestImportCredentialKindSecretAlwaysPresent secret 不用 omitempty。
//
// 理由：`false` 是**有意义的取值**（"这一栏不是敏感输入"）。
// 省掉它会让前端分不清"明确说不是"与"上游没提供这个字段"——
// 而两者的渲染决策相反（前者用普通输入框，后者该按未知保守处理）。
func TestImportCredentialKindSecretAlwaysPresent(t *testing.T) {
	raw, _ := json.Marshal(ImportCredentialKind{ID: "key", Label: "API Key"})
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	v, ok := m["secret"]
	if !ok {
		t.Fatal("secret=false 时字段被省略了 —— 前端无法区分「明确不是」与「没提供」")
	}
	if v != false {
		t.Errorf("secret 应为 false，得到 %v", v)
	}
}

// TestImportCredentialKindOptionalFieldsOmitted 可选字段（hint/placeholder）可以省。
//
// 与 secret 相反：这两个是纯装饰，缺省时前端回落到"什么都不显示"，
// 不存在"错误决策"。省掉它们能让 manifest 更短。
func TestImportCredentialKindOptionalFieldsOmitted(t *testing.T) {
	raw, _ := json.Marshal(ImportCredentialKind{ID: "x", Label: "X"})
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	for _, k := range []string{"hint", "placeholder"} {
		if _, ok := m[k]; ok {
			t.Errorf("空的可选字段 %q 不该下发（manifest 会变长）", k)
		}
	}
}

func keysOfAny(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
