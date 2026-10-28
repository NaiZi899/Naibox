package admin

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NaiZi899/Naibox/internal/auth"
	"github.com/NaiZi899/Naibox/internal/gateway"
	"github.com/NaiZi899/Naibox/internal/pool"
)

// newImportTarget 造一个"空的目标机"：有 workbuddy 上游、空池、空的凭证根。
//
// 导入的判据里有一条是"写进哪个目录"（importDirFor），它要求上游已注册，
// 所以这里必须有 Registry —— 否则所有条目都会被判成"上游不存在"，
// 测试会变成在验证一条错误路径。
func newImportTarget(t *testing.T) (*Handler, string) {
	t.Helper()
	dir := t.TempDir()
	reg := gateway.NewRegistry()
	if err := reg.Register(&importProvider{stubProvider: stubProvider{id: "workbuddy", caps: gateway.CapChat}}); err != nil {
		t.Fatal(err)
	}
	h := New(Config{
		Pool:            pool.New(""),
		Registry:        reg,
		DefaultProvider: "workbuddy",
		ReloadProvider:  "workbuddy",
		AuthDir:         dir,
		AuthsBase:       dir,
	})
	return h, dir
}

// exportZip 走真实的导出端点拿 zip 字节。
func exportZip(t *testing.T, h *Handler, withSecrets bool) []byte {
	t.Helper()
	body := `{"format":"zip","include_secrets":false}`
	if withSecrets {
		body = `{"format":"zip","include_secrets":true,"confirm":"export-secrets"}`
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, exportReq(t, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("导出失败: %d %s", rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}

// zipEntries 把 zip 拆成 name → 内容。
func zipEntries(t *testing.T, raw []byte) map[string][]byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("zip 解析: %v", err)
	}
	out := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("打开 %s: %v", f.Name, err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		out[f.Name] = b
	}
	return out
}

// importReq 造一条 import-file 请求（body 是文件本体，参数走 query）。
func importReq(t *testing.T, h *Handler, body []byte, query string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/admin/accounts/import-file"+query, bytes.NewReader(body))
	r.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// importReply 解析回执。
type importReply struct {
	OK          bool                `json:"ok"`
	Created     int                 `json:"created"`
	Replaced    int                 `json:"replaced"`
	Unchanged   int                 `json:"unchanged"`
	Skipped     int                 `json:"skipped"`
	Rejected    int                 `json:"rejected"`
	Results     []importFileResult  `json:"results"`
	Reload      []map[string]string `json:"reload"`
	ManifestUse bool                `json:"manifest_used"`
	DiskWritten bool                `json:"disk_written"`
}

func decodeImport(t *testing.T, rec *httptest.ResponseRecorder) importReply {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("导入失败: %d %s", rec.Code, rec.Body.String())
	}
	var rep importReply
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatalf("回执不是 JSON: %v", err)
	}
	return rep
}

// TestImportFileZipRoundTrip 导出 → 导入 → 重载，账号回到池里，文件逐字节一致。
//
// 这是这条功能的**目标断言**：往返之后不是"看起来成功"，而是
// ① 磁盘上的凭证与原机器逐字节相同 ② 池里真的出现了这两个号。
func TestImportFileZipRoundTrip(t *testing.T) {
	src, _ := newExportFixture(t)
	zipRaw := exportZip(t, src, true)
	entries := zipEntries(t, zipRaw)
	if _, ok := entries["manifest.json"]; !ok {
		t.Fatal("导出的 zip 里没有 manifest.json")
	}

	dst, dstDir := newImportTarget(t)
	rep := decodeImport(t, importReq(t, dst, zipRaw, "?confirm=import-secrets"))
	if rep.Created != 2 || rep.Rejected != 0 {
		t.Fatalf("created=%d rejected=%d results=%+v", rep.Created, rep.Rejected, rep.Results)
	}
	if !rep.ManifestUse {
		t.Fatal("应当用到 manifest")
	}
	// ① 逐字节一致
	for name, want := range entries {
		if name == "manifest.json" {
			continue
		}
		got, err := os.ReadFile(filepath.Join(dstDir, "workbuddy", name))
		if err != nil {
			t.Fatalf("导入后找不到 %s: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s 内容与导出不一致", name)
		}
	}
	// ② 重载之后账号进池（导入本身不 reload，凭据是既有端点）
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/accounts/reload",
		strings.NewReader(`{"provider":"workbuddy"}`))
	req.RemoteAddr = "127.0.0.1:12345"
	dst.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("重载失败: %d %s", rec.Code, rec.Body.String())
	}
	list := dst.cfg.Pool.List()
	if len(list) != 2 {
		t.Fatalf("重载后账号数 = %d，want 2（导入的文件没被扫到）", len(list))
	}
	got := map[string]string{}
	for _, st := range list {
		got[st.UID] = st.Nickname
	}
	if got["u1"] == "" || got["u2"] == "" {
		t.Fatalf("池里的 uid 不对: %+v", got)
	}
}

// TestImportFileRejectsTamperedEntry 清单里的 sha256 对不上 = 这个文件被改过，
// 必须逐条拒绝，而且**其余条目照常导入**（不能因为一条坏掉整包失败）。
func TestImportFileRejectsTamperedEntry(t *testing.T) {
	src, _ := newExportFixture(t)
	good := exportZip(t, src, true)
	entries := zipEntries(t, good)

	// 重建一个 zip：把 u1 的内容改掉，清单里的 sha 保持原值
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, raw := range entries {
		if name == "workbuddy-u1.json" {
			raw = append([]byte{}, raw...)
			raw = bytes.Replace(raw, []byte(`"tok-u1"`), []byte(`"tok-EVIL"`), 1)
		}
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(raw)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	dst, dstDir := newImportTarget(t)
	rep := decodeImport(t, importReq(t, dst, buf.Bytes(), "?confirm=import-secrets"))
	if rep.Rejected != 1 || rep.Created != 1 {
		t.Fatalf("应当 1 rejected + 1 created，实际 rejected=%d created=%d", rep.Rejected, rep.Created)
	}
	var why string
	for _, r := range rep.Results {
		if r.Action == "rejected" {
			why = r.Error
		}
	}
	if !strings.Contains(why, "sha256") {
		t.Fatalf("拒绝原因应当点明 sha256，实际 %q", why)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "workbuddy", "workbuddy-u1.json")); !os.IsNotExist(err) {
		t.Fatal("被篡改的文件不该落盘")
	}
}

// TestImportFileRejectsLiteExport 不含凭据的导出不能当迁移包用。
func TestImportFileRejectsLiteExport(t *testing.T) {
	src, _ := newExportFixture(t)
	lite := exportZip(t, src, false)
	dst, dstDir := newImportTarget(t)
	rep := decodeImport(t, importReq(t, dst, lite, "?confirm=import-secrets"))
	if rep.Rejected != 2 || rep.Created != 0 {
		t.Fatalf("不含凭据的包应当逐条拒绝，实际 rejected=%d created=%d", rep.Rejected, rep.Created)
	}
	if !strings.Contains(rep.Results[0].Error, "secrets_included") {
		t.Fatalf("原因应当指向 secrets_included:false，实际 %q", rep.Results[0].Error)
	}
	if entries, _ := os.ReadDir(filepath.Join(dstDir, "workbuddy")); len(entries) != 0 {
		t.Fatalf("不该往目录里写任何东西，实际 %d 个文件", len(entries))
	}
}

// TestImportFileSingleJSON 单账号导出（json）导入：必须显式给 provider。
func TestImportFileSingleJSON(t *testing.T) {
	src, _ := newExportFixture(t)
	rec := httptest.NewRecorder()
	src.ServeHTTP(rec, exportReq(t, `{"format":"json","include_secrets":true,"confirm":"export-secrets","uids":["u1"]}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("导出单账号失败: %d %s", rec.Code, rec.Body.String())
	}
	single := rec.Body.Bytes()

	dst, _ := newImportTarget(t)
	// 不给 provider：必须失败并说清为什么
	bad := importReq(t, dst, single, "?confirm=import-secrets")
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("缺 provider 应当 400，实际 %d", bad.Code)
	}
	if !strings.Contains(bad.Body.String(), "provider") {
		t.Fatalf("错误信息应当点名 provider: %s", bad.Body.String())
	}
	// 给了 provider：导入成功，文件名用凭证里的 uid
	rep := decodeImport(t, importReq(t, dst, single, "?confirm=import-secrets&provider=workbuddy"))
	if rep.Created != 1 {
		t.Fatalf("created=%d，want 1（%+v）", rep.Created, rep.Results)
	}
	if rep.Results[0].Name != "workbuddy-u1.json" {
		t.Fatalf("落盘名应当由 uid 推出，实际 %q", rep.Results[0].Name)
	}
}

// TestImportFileZipSlip zip 里的文件名是外部输入，必须挡住 ../ 越界。
func TestImportFileZipSlip(t *testing.T) {
	dst, dstDir := newImportTarget(t)
	evil := []byte(`{"auth":{"accessToken":"x"}}`)
	sum := sha256.Sum256(evil)
	man := map[string]any{
		"format_version":   2,
		"secrets_included": true,
		"files": []map[string]any{{
			"name": "../evil.json", "uid": "evil", "provider": "workbuddy",
			"size": len(evil), "sha256": hex.EncodeToString(sum[:]),
		}},
	}
	manRaw, _ := json.Marshal(man)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("../evil.json")
	w.Write(evil)
	w2, _ := zw.Create("manifest.json")
	w2.Write(manRaw)
	zw.Close()

	rep := decodeImport(t, importReq(t, dst, buf.Bytes(), "?confirm=import-secrets"))
	if rep.Rejected != 1 {
		t.Fatalf("越界条目应当被拒，实际 rejected=%d（%+v）", rep.Rejected, rep.Results)
	}
	if !strings.Contains(rep.Results[0].Error, "路径分隔符") {
		t.Fatalf("原因应当说明越界，实际 %q", rep.Results[0].Error)
	}
	// 目标目录的**上级**不该出现 evil.json
	if _, err := os.Stat(filepath.Join(filepath.Dir(dstDir), "evil.json")); !os.IsNotExist(err) {
		t.Fatal("文件被写到了凭证目录之外（zip-slip）")
	}
}

// TestImportFileConflictPolicy skip / replace 必须给出不同结果。
func TestImportFileConflictPolicy(t *testing.T) {
	src, _ := newExportFixture(t)
	zipRaw := exportZip(t, src, true)
	entries := zipEntries(t, zipRaw)

	prep := func(t *testing.T) (*Handler, string, string) {
		h, dir := newImportTarget(t)
		sub := filepath.Join(dir, "workbuddy")
		if err := os.MkdirAll(sub, 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(sub, "workbuddy-u1.json")
		// 先放一份**不同内容**的同名文件
		if err := os.WriteFile(target, []byte(`{"auth":{"accessToken":"old"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		return h, dir, target
	}

	h1, _, target1 := prep(t)
	repSkip := decodeImport(t, importReq(t, h1, zipRaw, "?confirm=import-secrets"))
	if repSkip.Skipped != 1 || repSkip.Created != 1 {
		t.Fatalf("skip 策略下应当 1 skipped + 1 created，实际 %d/%d", repSkip.Skipped, repSkip.Created)
	}
	if got, _ := os.ReadFile(target1); !bytes.Contains(got, []byte("old")) {
		t.Fatal("skip 策略不该覆盖已有文件")
	}

	h2, _, target2 := prep(t)
	repReplace := decodeImport(t, importReq(t, h2, zipRaw, "?confirm=import-secrets&on_conflict=replace"))
	if repReplace.Replaced != 1 || repReplace.Created != 1 {
		t.Fatalf("replace 策略下应当 1 replaced + 1 created，实际 %d/%d", repReplace.Replaced, repReplace.Created)
	}
	got, _ := os.ReadFile(target2)
	if !bytes.Equal(got, entries["workbuddy-u1.json"]) {
		t.Fatal("replace 策略应当写入导出包里的内容")
	}
}

// TestImportFileDryRun 预演不落盘，但结论与真跑一致。
func TestImportFileDryRun(t *testing.T) {
	src, _ := newExportFixture(t)
	zipRaw := exportZip(t, src, true)
	dst, dstDir := newImportTarget(t)
	rep := decodeImport(t, importReq(t, dst, zipRaw, "?confirm=import-secrets&dry_run=1"))
	if rep.Created != 2 || rep.DiskWritten {
		t.Fatalf("预演应当报告 created=2 且 disk_written=false，实际 %d/%v", rep.Created, rep.DiskWritten)
	}
	if entries, _ := os.ReadDir(filepath.Join(dstDir, "workbuddy")); len(entries) != 0 {
		t.Fatalf("预演不该写盘，实际 %d 个文件", len(entries))
	}
}

// TestImportFileRejectsCSVAndMissingConfirm 两条入口纪律。
func TestImportFileRejectsCSVAndMissingConfirm(t *testing.T) {
	dst, _ := newImportTarget(t)
	// csv 是清单格式，不含凭据
	rec := importReq(t, dst, []byte("provider,uid\nworkbuddy,u1\n"), "?confirm=import-secrets")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("csv 应当 400，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "清单") {
		t.Fatalf("应当说明 csv 是清单格式: %s", rec.Body.String())
	}
	// 缺 confirm
	rec2 := importReq(t, dst, []byte("{}"), "")
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("缺 confirm 应当 400，实际 %d", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), "import-secrets") {
		t.Fatalf("应当回显确认串名: %s", rec2.Body.String())
	}
	// 非法 on_conflict
	rec3 := importReq(t, dst, []byte("{}"), "?confirm=import-secrets&on_conflict=merge")
	if rec3.Code != http.StatusBadRequest {
		t.Fatalf("非法 on_conflict 应当 400，实际 %d", rec3.Code)
	}
}

// TestImportFileManifestCarriesProvider v2 清单必须带 provider：
// 没有它，导入就得靠调用方猜目录（或靠文件名前缀猜），混合上游的包根本导不回去。
func TestImportFileManifestCarriesProvider(t *testing.T) {
	src, _ := newExportFixture(t)
	entries := zipEntries(t, exportZip(t, src, true))
	var man importManifest
	if err := json.Unmarshal(entries["manifest.json"], &man); err != nil {
		t.Fatalf("manifest 解析: %v", err)
	}
	if man.FormatVersion < 2 {
		t.Fatalf("format_version = %d，want ≥2", man.FormatVersion)
	}
	for _, f := range man.Files {
		if f.Provider != "workbuddy" {
			t.Fatalf("%s 的 provider = %q，want workbuddy", f.Name, f.Provider)
		}
	}
}

// TestCredentiallessReason 判据本身：缺凭据、空值、非 JSON 三种都要拦。
func TestCredentiallessReason(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string // 期望命中的关键字；空 = 应当判为"有凭据"
	}{
		{"嵌套 accessToken", `{"auth":{"accessToken":"t"}}`, ""},
		{"扁平 apiKey", `{"apiKey":"k"}`, ""},
		{"cookies 字符串", `{"cookies":"a=b"}`, ""},
		{"空 token 不算", `{"auth":{"accessToken":""}}`, "凭据字段"},
		{"显式声明无凭据", `{"secrets_included":false,"auth":{"expiresAt":1}}`, "secrets_included"},
		{"非 JSON", `provider,uid`, "不像凭证"},
		{"空内容", ``, "内容为空"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := credentiallessReason([]byte(c.body))
			if c.want == "" {
				if got != "" {
					t.Fatalf("应当判为有凭据，实际 %q", got)
				}
				return
			}
			if !strings.Contains(got, c.want) {
				t.Fatalf("原因应当含 %q，实际 %q", c.want, got)
			}
		})
	}
}

// TestImportFileAuthParseCompatible 导入落盘后的文件必须能被 auth.Parse 读回 ——
// 否则"导入成功"只是写了个文件，重载时会被静默跳过。
func TestImportFileAuthParseCompatible(t *testing.T) {
	src, _ := newExportFixture(t)
	zipRaw := exportZip(t, src, true)
	entries := zipEntries(t, zipRaw)
	for name, raw := range entries {
		if name == "manifest.json" {
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			t.Fatalf("%s 导出的内容 auth.Parse 读不回来: %v", name, err)
		}
		if a.UID == "" || a.AccessToken == "" {
			t.Fatalf("%s 解析结果缺 uid/token: %+v", name, a)
		}
	}
}

// importProvider 在 stubProvider 之上实现 gateway.CredentialLoader ——
// 重载端点靠它扫描目录（核心不硬编码任何上游的凭证格式）。
//
// 实现取"读这个目录下的 *.json，按 auth.Parse 投影 uid/nickname"：
// 与真实上游同款投影，于是 round-trip 测试里"池里出现了这两个号"
// 是被真实解析支撑的，而不是桩返回恒真。
type importProvider struct {
	stubProvider
}

func (p *importProvider) LoadCredentials(dir string) ([]gateway.Credential, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	out := make([]gateway.Credential, 0, len(paths))
	for _, path := range paths {
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			continue
		}
		a, perr := auth.Parse(raw)
		if perr != nil || a == nil || a.UID == "" {
			continue
		}
		out = append(out, gateway.Credential{
			Provider: p.ID(),
			UID:      a.UID,
			Nickname: a.Nickname,
			FilePath: path,
		})
	}
	return out, nil
}
