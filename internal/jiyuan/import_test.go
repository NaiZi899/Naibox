// import_test.go 批量导入的解析测试。
//
// 这是本上游**唯一**的上号入口，所以它的输入形态（用户实际会粘什么）
// 值得逐条钉住 —— 解析错一个形态，用户看到的是"导入了 0 个"，
// 而原因在自己的粘贴内容里，很难自查。
package jiyuan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestImportBareKeys 主用法：一行一把裸 key。
func TestImportBareKeys(t *testing.T) {
	p := New(Config{})
	got, err := p.ImportCredentials(`
sk_tr_1a2b3c4d5e6f7a8b9c0d
sk_tr_99887766554433221100
sk_tr_fixture0000000000000
`, importKindKey)
	if err != nil {
		t.Fatalf("ImportCredentials: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("应解析出 3 条，得到 %d", len(got))
	}
	for i, c := range got {
		if c.UID == "" || c.FileName == "" || len(c.Raw) == 0 {
			t.Errorf("第 %d 条字段不完整: %+v", i+1, c)
		}
		if !strings.HasPrefix(c.FileName, "jiyuan-") || !strings.HasSuffix(c.FileName, ".json") {
			t.Errorf("第 %d 条文件名不符约定: %q", i+1, c.FileName)
		}
	}
	// 三把不同 key → 三个不同 uid
	seen := map[string]bool{}
	for _, c := range got {
		if seen[c.UID] {
			t.Fatalf("不同 key 派生出相同 uid: %s", c.UID)
		}
		seen[c.UID] = true
	}
}

// TestImportLabelsAndComments 同行标签、逗号分隔、`#` 注释、空行。
//
// 用户从表格/记事本里复制时这些形态都会出现，逐条钉住。
func TestImportLabelsAndComments(t *testing.T) {
	p := New(Config{})
	got, err := p.ImportCredentials(`
# 基元律动 key 清单
sk_tr_1a2b3c4d5e6f7a8b9c0d  账号A

sk_tr_99887766554433221100, 账号B
sk_tr_fixture0000000000000 账号C  # 这是行内备注
`, importKindKey)
	if err != nil {
		t.Fatalf("ImportCredentials: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("应解析出 3 条（注释行/空行跳过），得到 %d", len(got))
	}
	want := []string{"账号A", "账号B", "账号C"}
	for i, w := range want {
		if got[i].Nickname != w {
			t.Errorf("第 %d 条昵称 = %q，期望 %q", i+1, got[i].Nickname, w)
		}
	}
}

// TestImportJSONForms 兼容形态：数组 / 单个对象。
func TestImportJSONForms(t *testing.T) {
	p := New(Config{})

	arr, err := p.ImportCredentials(`[{"key":"sk_tr_1a2b3c4d5e6f7a8b9c0d","name":"A"},
	                                  {"api_key":"sk_tr_99887766554433221100"}]`, importKindKey)
	if err != nil {
		t.Fatalf("数组形态: %v", err)
	}
	if len(arr) != 2 {
		t.Fatalf("数组应解析出 2 条，得到 %d", len(arr))
	}
	if arr[0].Nickname != "A" {
		t.Errorf("name 字段未被当作昵称: %q", arr[0].Nickname)
	}
	if arr[1].Nickname == "" {
		t.Error("没给昵称时应回落 uid 前缀")
	}

	one, err := p.ImportCredentials(`{"key":"sk_tr_fixture0000000000000"}`, importKindKey)
	if err != nil {
		t.Fatalf("单对象形态: %v", err)
	}
	if len(one) != 1 {
		t.Fatalf("单对象应解析出 1 条，得到 %d", len(one))
	}
}

// TestImportJSONAliasFields 各种字段别名都要认（用户从别的工具导出）。
func TestImportJSONAliasFields(t *testing.T) {
	p := New(Config{})
	aliases := []string{"key", "api_key", "apikey", "apiKey", "token"}
	for i, field := range aliases {
		body := `{"` + field + `":"sk_tr_af5e593a2d4692c6d0f` + string(rune('0'+i)) + `1"}`
		got, err := p.ImportCredentials(body, importKindKey)
		if err != nil {
			t.Errorf("字段 %q 未被识别: %v", field, err)
			continue
		}
		if len(got) != 1 {
			t.Errorf("字段 %q 应解析出 1 条，得到 %d", field, len(got))
		}
	}
}

// TestImportRejectsSessionCookie 会话凭据必须被**明确拒绝**并说清怎么办。
//
// 这是本上游最容易踩的坑：用户在浏览器 F12 里能同时看到
// `tr_session`（sess_…）与 API Key（sk_…），抄错那个会导入一个
// 永远 401 的"账号"。所以拒绝信息必须指出"要的是 sk_ 开头的"。
// TestImportRejectsSessionCookie 把登录凭证粘进 **key 栏** 必须被拒绝，
// 并且要**指向正确的那一栏**（用户最容易犯的错：两个值都能在浏览器里拿到）。
func TestImportRejectsSessionCookie(t *testing.T) {
	p := New(Config{})
	_, err := p.ImportCredentials("sess_nm7ZTi9kQ2xLpR4vBnH8cWyD3sF6gJ", importKindKey)
	if err == nil {
		t.Fatal("登录凭证不该被 key 栏收下（会变成每请求 401 的账号）")
	}
	msg := err.Error()
	for _, want := range []string{"登录凭证", "sk_", "登录凭证」那一栏"} {
		if !strings.Contains(msg, want) {
			t.Errorf("拒绝信息里应提到 %q（用户要据此知道该贴到哪），实际: %s", want, msg)
		}
	}
}

// TestImportKindsDeclared 自报的导入路径：形状与顺序。
//
// ⚠ 第三条（session-create，代开一把）是**写上游**的形态，必须单独存在：
// 上游只下发脱敏 key，「贴 sess_ 拉全部 key」这条路在上游能力上不存在，
// 要拿到可用凭证只能由用户自己去复制、或显式授权网关代开。
// 让只读的 session 框静默升级成写操作，是用户不该承受的意外。
func TestImportKindsDeclared(t *testing.T) {
	p := New(Config{})
	kinds := p.ImportCredentialKinds()
	if len(kinds) != 3 {
		t.Fatalf("应有三条导入路径（key / session / session-create），得到 %d", len(kinds))
	}
	// key 必须在**前**：只有一把 key 的用户是绝大多数，默认落在第一个框最省事。
	if kinds[0].ID != importKindKey {
		t.Errorf("第一条应为 key（默认框），得到 %q", kinds[0].ID)
	}
	if kinds[1].ID != importKindSession {
		t.Errorf("第二条应为 session（只读），得到 %q", kinds[1].ID)
	}
	if kinds[2].ID != importKindSessionCreate {
		t.Errorf("第三条应为 session-create（代开），得到 %q", kinds[2].ID)
	}
	// 两个登录凭证栏都要标成 Secret：前端据此用密码框渲染并加警告。
	if !kinds[1].Secret {
		t.Error("登录凭证（只读）必须标记为 Secret —— 它能读账号信息与余额，泄漏后果更重")
	}
	if !kinds[2].Secret {
		t.Error("登录凭证（代开）必须标记为 Secret")
	}
	if kinds[0].Secret {
		t.Error("API Key 不该标成 Secret（它是调用凭据，不是登录态）")
	}
	// 写操作必须在界面上**明确告知后果**：会真实新增凭证、占用配额。
	if !strings.Contains(kinds[2].Hint, "真实新建") {
		t.Errorf("代开栏必须说明会在上游真实新建 key，实际 Hint: %s", kinds[2].Hint)
	}
	// 只读栏必须说明它**不导入 key**（否则用户会一直以为是自己操作错了）。
	if !strings.Contains(kinds[1].Hint, "不会") {
		t.Errorf("只读栏必须说明不会导入 key，实际 Hint: %s", kinds[1].Hint)
	}
	for _, k := range kinds {
		if k.Label == "" || k.Hint == "" || k.Placeholder == "" {
			t.Errorf("形态 %q 的展示字段不完整: %+v —— 前端会渲染出空框", k.ID, k)
		}
	}
}

// TestImportSessionRejectsKeyInSessionSlot 反向：把 key 粘进**凭证栏**也要拒。
func TestImportSessionRejectsKeyInSessionSlot(t *testing.T) {
	p := New(Config{})
	_, err := p.ImportCredentials("sk_tr_zG2kCsmroO2ITJUIVAjZPENEmsYs5ha3BUOscFYuKYY", importKindSession)
	if err == nil {
		t.Fatal("key 被凭证栏收下了 —— 那会拿一把 key 去打 /api/*（必然 401）")
	}
	if !strings.Contains(err.Error(), "sess_") {
		t.Errorf("错误信息应说明凭证要以 sess_ 开头，实际: %s", err)
	}
	// 要指回 key 栏，否则用户不知道该去哪。
	if !strings.Contains(err.Error(), "API Key") {
		t.Errorf("错误信息应指回「API Key」栏，实际: %s", err)
	}
}

// TestImportBlankFormatDefaultsToKey 老客户端不带 format → 按 key 形态解释。
//
// 这条保证"加了一个参数"没有改变既有调用方的行为（向后兼容）。
func TestImportBlankFormatDefaultsToKey(t *testing.T) {
	p := New(Config{})
	for _, format := range []string{"", "  ", "unknown-kind"} {
		got, err := p.ImportCredentials("sk_tr_1a2b3c4d5e6f7a8b9c0d", format)
		if err != nil {
			t.Errorf("format=%q 时应回落 key 形态，得到错误: %v", format, err)
			continue
		}
		if len(got) != 1 {
			t.Errorf("format=%q 时导入条数 = %d，期望 1", format, len(got))
		}
		if got[0].Format != importKindKey {
			t.Errorf("format=%q 时回执形态 = %q，期望 %q", format, got[0].Format, importKindKey)
		}
	}
}

// TestImportRejectsJunk 明显不是 key 的行要指名报错（带行号）。
func TestImportRejectsJunk(t *testing.T) {
	p := New(Config{})
	cases := []struct{ name, in, wantSub string }{
		{"太短", "sk_tr_abc", "太短"},
		{"含引号", `"sk_tr_1a2b3c4d5e6f7a8b9c0d"`, "引号"},
	}
	for _, c := range cases {
		_, err := p.ImportCredentials(c.in, importKindKey)
		if err == nil {
			t.Errorf("%s: 应当报错", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.wantSub) {
			t.Errorf("%s: 错误信息应含 %q，实际: %s", c.name, c.wantSub, err)
		}
	}

	// 多行里第 2 行是垃圾 → 错误必须带行号（否则用户不知道改哪一行）
	_, err := p.ImportCredentials("sk_tr_1a2b3c4d5e6f7a8b9c0d\n!!!\nsk_tr_99887766554433221100", importKindKey)
	if err == nil {
		t.Fatal("含垃圾行应当报错")
	}
	if !strings.Contains(err.Error(), "第 2 行") {
		t.Errorf("错误应指出是第 2 行，实际: %s", err)
	}
}

// TestImportDedupes 同一段里重复贴同一把 key 只收一次。
func TestImportDedupes(t *testing.T) {
	p := New(Config{})
	got, err := p.ImportCredentials(
		"sk_tr_1a2b3c4d5e6f7a8b9c0d\nsk_tr_1a2b3c4d5e6f7a8b9c0d\nsk_tr_99887766554433221100", importKindKey)
	if err != nil {
		t.Fatalf("ImportCredentials: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("重复的 key 应去重，得到 %d 条（会造成回执虚报）", len(got))
	}
}

// TestImportEmpty 空输入要报错而不是静默返回 0 条。
func TestImportEmpty(t *testing.T) {
	p := New(Config{})
	for _, in := range []string{"", "   ", "\n\n", "# 只有注释\n"} {
		if _, err := p.ImportCredentials(in, importKindKey); err == nil {
			t.Errorf("输入 %q 应当报错（静默 0 条会让用户以为粘错了）", in)
		}
	}
}

// TestImportRoundTripIntoDir 导入产物能被 LoadDir 原样读回。
//
// 这条覆盖的是"导入写进 A 目录、重载只扫 B 目录"那类缺陷：
// 文件名与内容都由本包给出，核心只负责写盘。
func TestImportRoundTripIntoDir(t *testing.T) {
	dir := t.TempDir()
	p := New(Config{AuthDir: dir})

	creds, err := p.ImportCredentials("sk_tr_1a2b3c4d5e6f7a8b9c0d 账号A\nsk_tr_99887766554433221100", importKindKey)
	if err != nil {
		t.Fatalf("ImportCredentials: %v", err)
	}
	for _, c := range creds {
		if err := os.WriteFile(filepath.Join(dir, c.FileName), c.Raw, 0o600); err != nil {
			t.Fatalf("写盘: %v", err)
		}
	}

	back, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(back) != 2 {
		t.Fatalf("应读回 2 份凭证，得到 %d", len(back))
	}
	// 读回顺序由派生的 uid（文件名）决定，与导入顺序无关 —— 因此按昵称查找，
	// 而不是假设"带昵称的那条一定排第一"。
	foundLabel := false
	for _, a := range back {
		if a.Nickname == "账号A" {
			foundLabel = true
		}
	}
	if !foundLabel {
		t.Errorf("昵称丢失: back=%+v", back)
	}
	for _, a := range back {
		if a.FilePath == "" {
			t.Errorf("FilePath 未填（删除账号要靠它连文件移除）: %+v", a)
		}
	}

	// 通过 CredentialLoader 投影出来的 uid 必须与导入时的 uid 一致 ——
	// 否则导入成功但池子里对不上号。
	projected, err := p.LoadCredentials(dir)
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	if len(projected) != 2 {
		t.Fatalf("投影应得 2 条，得到 %d", len(projected))
	}
	got := map[string]bool{}
	for _, c := range projected {
		if c.Provider != ProviderID {
			t.Errorf("provider 归属错误: %q", c.Provider)
		}
		got[c.UID] = true
	}
	for _, c := range creds {
		if !got[c.UID] {
			t.Errorf("导入的 uid %s 在投影结果里找不到", c.UID)
		}
	}

	// Secret 必须是同一份 *Auth（否则 Chat 取不到 key）
	withSecrets, err := p.LoadCredentialsWithSecrets(dir)
	if err != nil {
		t.Fatalf("LoadCredentialsWithSecrets: %v", err)
	}
	for _, cs := range withSecrets {
		a, ok := cs.Secret.(*Auth)
		if !ok || a == nil || a.APIKey == "" {
			t.Fatalf("Secret 不是可用的 *Auth: %+v", cs.Secret)
		}
	}
}
