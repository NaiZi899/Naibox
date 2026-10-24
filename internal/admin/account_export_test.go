package admin

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NaiZi899/Naibox/internal/auth"
	"github.com/NaiZi899/Naibox/internal/pool"
)

// newExportFixture 造一个带两个账号的 handler。
//
// 账号凭证写到磁盘上（嵌套形），因为"含凭据导出"的实现第一条就是
// **逐字节拷文件** —— 只有真文件才能验证"导出能被导入吃回去"。
func newExportFixture(t *testing.T) (*Handler, string) {
	t.Helper()
	dir := t.TempDir()
	mk := func(name, uid, nickname, token, channel string) *auth.Auth {
		path := filepath.Join(dir, name)
		doc := map[string]any{
			"auth": map[string]any{
				"accessToken": token, "refreshToken": token + "-refresh",
				"expiresAt": 1800000000, "domain": "copilot.tencent.com", "channel": channel,
			},
			"account": map[string]any{
				"uid": uid, "enterpriseId": "ent-" + uid, "nickname": nickname,
			},
		}
		raw, _ := json.MarshalIndent(doc, "", "  ")
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatalf("写凭证: %v", err)
		}
		a, err := auth.Parse(raw)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		a.FilePath = path
		return a
	}
	// 昵称里带逗号与引号：CSV 转义的真实场景（上游昵称就是用户自己起的）
	a1 := mk("workbuddy-u1.json", "u1", `阿"猫", 一号`, "tok-u1", "cn")
	a2 := mk("workbuddy-u2.json", "u2", "二号", "tok-u2", "intl")

	p := pool.New("")
	p.AddFor("workbuddy", a1, nil)
	p.AddFor("workbuddy", a2, nil)

	h := New(Config{Pool: p, DefaultProvider: "workbuddy", ReloadProvider: "workbuddy"})
	return h, dir
}

func exportReq(t *testing.T, body string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/admin/accounts/export", strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:12345"
	return r
}

// TestExportCSV清单格式与转义 CSV 是给人看的清单：带 BOM、CRLF、正确转义、
// **绝不含凭据**。
func TestExportCSV清单格式与转义(t *testing.T) {
	h, _ := newExportFixture(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, exportReq(t, `{"format":"csv"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	raw := rec.Body.Bytes()
	if !bytes.HasPrefix(raw, []byte("\xef\xbb\xbf")) {
		t.Fatal("CSV 必须带 UTF-8 BOM（否则 Excel 打开中文全是乱码）")
	}
	if !bytes.Contains(raw, []byte("\r\n")) {
		t.Fatal("CSV 必须是 CRLF 行尾（RFC4180）")
	}
	for _, secret := range []string{"tok-u1", "tok-u2", "refresh"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("CSV 里出现了凭据 %q —— 清单格式永远不含凭据", secret)
		}
	}

	// 用 encoding/csv 反解，验证转义正确（昵称含逗号与引号）
	rd := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))))
	rows, err := rd.ReadAll()
	if err != nil {
		t.Fatalf("CSV 解析失败（转义写坏了）: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("行数 = %d，want 表头+2 行", len(rows))
	}
	if got := rows[0]; len(got) != len(exportCSVHeader) {
		t.Fatalf("表头列数 = %d，want %d", len(got), len(exportCSVHeader))
	}
	var nick string
	for _, row := range rows[1:] {
		for i, col := range rows[0] {
			if col == "nickname" && row[1] == "u1" {
				nick = row[i]
			}
		}
	}
	if nick != `阿"猫", 一号` {
		t.Fatalf("昵称往返失败：%q", nick)
	}
}

// TestExportJSON含凭据可往返 导出的东西必须能被 auth.Parse 原样读回。
//
// 这是整个功能的价值所在：不是"能下载一个文件"，而是"能拿它恢复账号"。
func TestExportJSON含凭据可往返(t *testing.T) {
	h, _ := newExportFixture(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, exportReq(t,
		`{"format":"json","uids":["u1"],"include_secrets":true,"confirm":"export-secrets"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	got, err := auth.Parse(rec.Body.Bytes())
	if err != nil {
		t.Fatalf("导出的 JSON 无法被 auth.Parse 读回: %v\n%s", err, rec.Body.String())
	}
	if got.AccessToken != "tok-u1" || got.RefreshToken != "tok-u1-refresh" {
		t.Fatalf("token 往返失败: %+v", got)
	}
	if got.UID != "u1" || got.EnterpriseID != "ent-u1" {
		t.Fatalf("账号字段往返失败: %+v", got)
	}
	if got.Channel != "cn" {
		t.Fatalf("渠道往返失败: %q", got.Channel)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "workbuddy-u1.json") {
		t.Fatalf("文件名应当沿用磁盘上的凭证名（可直接丢回 auths/）: %q", cd)
	}
}

// TestExportJSON不带凭据明确标注 不含凭据的文件必须自己说清楚，而不是静默缺字段。
func TestExportJSON不带凭据明确标注(t *testing.T) {
	h, _ := newExportFixture(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, exportReq(t, `{"format":"json","uids":["u1"]}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var doc struct {
		Account map[string]any `json:"account"`
		Auth    map[string]any `json:"auth"`
		Secrets bool           `json:"secrets_included"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("不是 JSON: %v", err)
	}
	if doc.Secrets {
		t.Fatal("默认导出不该含凭据")
	}
	if _, ok := doc.Auth["accessToken"]; ok {
		t.Fatal("不含凭据的导出里不该出现 accessToken 键")
	}
	if doc.Account["uid"] != "u1" {
		t.Fatalf("清单至少要带上 uid: %v", doc.Account)
	}
}

// TestExportSecretsGuards 三道闸门：csv/txt 不接受凭据、json/zip 必须确认串、
// json 只允许单账号。
func TestExportSecretsGuards(t *testing.T) {
	h, _ := newExportFixture(t)
	cases := []struct {
		body string
		want int
		why  string
	}{
		{`{"format":"csv","include_secrets":true,"confirm":"export-secrets"}`,
			http.StatusBadRequest, "csv 是清单格式，不该接受凭据"},
		{`{"format":"txt","include_secrets":true,"confirm":"export-secrets"}`,
			http.StatusBadRequest, "txt 是清单格式，不该接受凭据"},
		{`{"format":"json","uids":["u1"],"include_secrets":true}`,
			http.StatusBadRequest, "含凭据必须回传 confirm"},
		{`{"format":"json","uids":["u1"],"include_secrets":true,"confirm":"随便"}`,
			http.StatusBadRequest, "confirm 写错必须拒绝"},
		{`{"format":"json"}`,
			http.StatusBadRequest, "json 是单账号格式，多账号应当提示用 zip"},
		{`{"format":"zip","uids":["nope"]}`,
			http.StatusNotFound, "没有匹配账号要明确说，而不是给个空包"},
		{`{"format":"xml"}`,
			http.StatusBadRequest, "未知 format"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, exportReq(t, c.body))
		if rec.Code != c.want {
			t.Errorf("%s: status=%d want=%d body=%s", c.why, rec.Code, c.want, rec.Body.String())
		}
	}
}

// TestExportZipHasManifestAndFiles zip 里逐账号一个文件 + manifest（含 sha256）。
func TestExportZipHasManifestAndFiles(t *testing.T) {
	h, _ := newExportFixture(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, exportReq(t,
		`{"format":"zip","include_secrets":true,"confirm":"export-secrets"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("不是合法 zip: %v", err)
	}
	names := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("打开 %s: %v", f.Name, err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		names[f.Name] = b
	}
	if len(names) != 3 {
		t.Fatalf("应当有 2 个账号文件 + manifest，实际 %v", keysOfBytes(names))
	}
	if _, ok := names["manifest.json"]; !ok {
		t.Fatal("缺少 manifest.json")
	}
	var man struct {
		FormatVersion   int  `json:"format_version"`
		AccountCount    int  `json:"account_count"`
		SecretsIncluded bool `json:"secrets_included"`
		Files           []struct {
			Name string `json:"name"`
			UID  string `json:"uid"`
		} `json:"files"`
	}
	if err := json.Unmarshal(names["manifest.json"], &man); err != nil {
		t.Fatalf("manifest 不是 JSON: %v", err)
	}
	if man.AccountCount != 2 || len(man.Files) != 2 || !man.SecretsIncluded {
		t.Fatalf("manifest 内容不符: %+v", man)
	}
	// 每个账号文件都要能被解析回凭证
	for _, f := range man.Files {
		if _, err := auth.Parse(names[f.Name]); err != nil {
			t.Fatalf("%s 无法解析回凭证: %v", f.Name, err)
		}
	}
}

// TestExportProviderFilter provider 过滤必须生效（否则用户以为只导了一个上游）。
func TestExportProviderFilter(t *testing.T) {
	h, dir := newExportFixture(t)
	// 再加一个别的上游的账号
	path := filepath.Join(dir, "codearts-u9.json")
	raw := []byte(`{"account":{"uid":"u9","nickname":"九号"},"auth":{"accessToken":"tok-u9","domain":"codearts.example"}}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := auth.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	a.FilePath = path
	h.cfg.Pool.AddFor("codearts", a, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, exportReq(t, `{"format":"csv","provider":"codearts"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "u9") || strings.Contains(body, "u1") {
		t.Fatalf("provider 过滤没生效：\n%s", body)
	}
}

func keysOfBytes(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
