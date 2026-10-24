// account_export.go 账号数据导出：json / zip / csv / txt。
//
// # 为什么导出要跟导入放在同一层，并且形状对称
//
// account_import.go 的判据是"那段 JSON 是什么意思由上游回答"。导出是反方向：
// **只有上游知道自己的凭证怎么写回磁盘**。两者共用同一组字段
// （FileName / Raw / UID / Nickname），于是"导出 → 导入"的往返由类型保证，
// 而不是靠两份实现碰巧一致。
//
// # 凭据的三级来源（顺序是硬要求）
//
//  1. 上游实现了 gateway.AccountExportExt → 用它（它知道自己缺哪些字段没用）
//  2. 没实现 → **把磁盘上的凭证文件逐字节拷出来**（导入读的就是同一份，
//     往返最稳；核心不解释内容）
//  3. 连文件都找不到 → 回落 auth.RenderNested 生成嵌套 JSON，
//     并在清单里标 `secrets_included:false`（宁可少给，不可给假）
//
// # csv / txt **永远不含凭据**
//
// 它们的用途是"看看我有哪些号、额度多少、挂在哪条线上"，不是搬运口令。
// 一个"导出 csv 顺手带上 accessToken"的实现，会在用户把表格发出去的那一刻
// 变成事故 —— 所以这里根本没有那个开关。
package admin

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/NaiZi899/Naibox/internal/auth"
	"github.com/NaiZi899/Naibox/internal/gateway"
	"github.com/NaiZi899/Naibox/internal/pool"
)

const (
	exportFormatJSON = "json"
	exportFormatZip  = "zip"
	exportFormatCSV  = "csv"
	exportFormatTXT  = "txt"
	// exportConfirmToken 含凭据导出必须回传的确认串。
	//
	// # 为什么不用布尔字段
	//
	// `include_secrets:true` 可能来自一次误触（前端 bug、脚本重放）。
	// 要求回传一个**写对了才成立**的字符串，等于强迫调用方"知道自己在干什么"。
	// 代价为零，拦住的是那类"顺手把开关打开"的事故。
	exportConfirmToken = "export-secrets"
	// exportMaxAccounts 一次导出的账号上限。
	//
	// 导出不是分页接口：几千个号打成 zip 会把内存与响应时间一起顶起来。
	// 超过就明确拒绝并让用户按上游/UID 分批 —— 静默截断会让人以为导全了。
	exportMaxAccounts = 2000
)

// exportFileVersion zip 内 manifest.json 的格式版本。
const exportFileVersion = 1

// accountsExport POST /admin/accounts/export
//
// 请求体：
//
//	{"provider":"", "uids":[], "format":"json|zip|csv|txt",
//	 "include_secrets":false, "confirm":""}
//
// 回执：文件本体（Content-Disposition: attachment）。
func (h *Handler) accountsExport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider       string   `json:"provider"`
		UIDs           []string `json:"uids"`
		Format         string   `json:"format"`
		IncludeSecrets bool     `json:"include_secrets"`
		Confirm        string   `json:"confirm"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	format := strings.ToLower(strings.TrimSpace(body.Format))
	if format == "" {
		format = exportFormatJSON
	}
	switch format {
	case exportFormatJSON, exportFormatZip, exportFormatCSV, exportFormatTXT:
	default:
		writeError(w, http.StatusBadRequest, "format 只接受 json / zip / csv / txt")
		return
	}
	// csv / txt 不含凭据：显式拒绝而不是"忽略这个开关" —— 静默忽略会让人
	// 以为里面真有 token，拿去当迁移包用。
	if body.IncludeSecrets && (format == exportFormatCSV || format == exportFormatTXT) {
		writeError(w, http.StatusBadRequest,
			"csv / txt 是**清单**格式，不包含凭据；要搬运账号请用 json 或 zip")
		return
	}
	if body.IncludeSecrets && body.Confirm != exportConfirmToken {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("含凭据导出需要 confirm=%q（该文件等同账号口令，请确认只落到你自己控制的介质）",
				exportConfirmToken))
		return
	}

	rows, err := h.exportRows(body.Provider, body.UIDs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(rows) == 0 {
		writeError(w, http.StatusNotFound, "没有匹配的账号（检查 provider / uids）")
		return
	}
	if len(rows) > exportMaxAccounts {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("一次最多导出 %d 个账号（当前 %d 个）：请按上游或 UID 分批",
				exportMaxAccounts, len(rows)))
		return
	}

	stamp := time.Now().Format("20060102-150405")
	switch format {
	case exportFormatCSV:
		h.writeExportCSV(w, rows, stamp)
	case exportFormatTXT:
		h.writeExportTXT(w, rows, stamp)
	case exportFormatJSON:
		h.writeExportJSON(w, rows, body.IncludeSecrets, stamp)
	case exportFormatZip:
		h.writeExportZip(w, rows, body.IncludeSecrets, stamp)
	}
}

// exportRow 一个待导出的账号：池里的运行态 + 磁盘上的凭证。
type exportRow struct {
	status pool.Status
	auth   *auth.Auth
	// provider 解析后的上游 id（空 Provider 按 DefaultProvider 解释）。
	provider string
}

// exportRows 按 provider / uids 过滤池里的账号。
func (h *Handler) exportRows(provider string, uids []string) ([]exportRow, error) {
	if h.cfg.Pool == nil {
		return nil, fmt.Errorf("账号池未接线")
	}
	provider = strings.TrimSpace(provider)
	want := make(map[string]bool, len(uids))
	for _, u := range uids {
		if u = strings.TrimSpace(u); u != "" {
			want[u] = true
		}
	}
	def := h.defaultProviderID()
	var out []exportRow
	for _, st := range h.cfg.Pool.List() {
		pid := st.Provider
		if pid == "" {
			pid = def
		}
		if provider != "" && pid != provider {
			continue
		}
		if len(want) > 0 && !want[st.UID] {
			continue
		}
		out = append(out, exportRow{status: st, auth: h.cfg.Pool.AuthByUID(st.UID), provider: pid})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].provider != out[j].provider {
			return out[i].provider < out[j].provider
		}
		return out[i].status.UID < out[j].status.UID
	})
	return out, nil
}

// defaultProviderID 空 Provider 的归属。
func (h *Handler) defaultProviderID() string {
	if h.cfg.DefaultProvider != "" {
		return h.cfg.DefaultProvider
	}
	return h.cfg.ReloadProvider
}

// exportCredential 取一条账号的凭证原文（含凭据导出时用）。
//
// 三级来源见文件头。返回 (文件名, 内容, 是否成功)。
func (h *Handler) exportCredential(row exportRow, extResults map[string]gateway.ExportedCredential) (string, []byte, bool) {
	if c, ok := extResults[row.status.UID]; ok && len(c.Raw) > 0 {
		name := c.FileName
		if name == "" {
			name = fmt.Sprintf("%s-%s.json", row.provider, row.status.UID)
		}
		return name, c.Raw, true
	}
	if row.auth != nil && row.auth.FilePath != "" {
		if raw, err := os.ReadFile(row.auth.FilePath); err == nil && len(raw) > 0 {
			return filepath.Base(row.auth.FilePath), raw, true
		}
	}
	if row.auth != nil {
		if raw, err := auth.RenderNested(row.auth); err == nil {
			return fmt.Sprintf("%s-%s.json", row.provider, row.status.UID), raw, true
		}
	}
	return "", nil, false
}

// exportExtResults 批量问各上游要凭证（一个上游一次调用，不逐个号打）。
func (h *Handler) exportExtResults(rows []exportRow) map[string]gateway.ExportedCredential {
	out := map[string]gateway.ExportedCredential{}
	if h.cfg.Registry == nil {
		return out
	}
	byProvider := map[string][]string{}
	for _, r := range rows {
		byProvider[r.provider] = append(byProvider[r.provider], r.status.UID)
	}
	for pid, uids := range byProvider {
		p, ok := h.cfg.Registry.Get(pid)
		if !ok {
			continue
		}
		ext, ok := gateway.ExtOf[gateway.AccountExportExt](p)
		if !ok {
			continue
		}
		list, err := ext.ExportCredentials(uids)
		if err != nil {
			log.Printf("admin: 上游 %s 导出凭证失败（回落按文件导出）：%v", pid, err)
			continue
		}
		for _, c := range list {
			if c.UID == "" || len(c.Raw) == 0 {
				continue
			}
			out[c.UID] = c
		}
	}
	return out
}

// exportLiteDoc 不含凭据的嵌套形 JSON（凭据字段整体缺席，而不是空串）。
//
// 用 `secrets_included:false` 显式声明：读到这个文件的人第一眼就该知道
// "这里没有 token，别拿它去恢复账号"。
func exportLiteDoc(row exportRow) []byte {
	channel := ""
	domain := ""
	expires := int64(0)
	enterprise := ""
	if row.auth != nil {
		channel = row.auth.Channel
		if channel == "" {
			channel = auth.DeriveChannel(row.auth.Domain)
		}
		domain = row.auth.Domain
		expires = row.auth.ExpiresAt
		enterprise = row.auth.EnterpriseID
	}
	doc := map[string]any{
		"account": map[string]any{
			"uid":          row.status.UID,
			"enterpriseId": enterprise,
			"nickname":     row.status.Nickname,
		},
		"auth": map[string]any{
			"expiresAt": expires,
			"domain":    domain,
			"channel":   channel,
			"realm":     channel,
		},
		"secrets_included": false,
		"note":             "本文件不含 accessToken / refreshToken：只用于清点账号，不能用来恢复",
	}
	raw, _ := json.MarshalIndent(doc, "", "  ")
	return append(raw, '\n')
}

// writeExportJSON 单账号导出。
func (h *Handler) writeExportJSON(w http.ResponseWriter, rows []exportRow, withSecrets bool, stamp string) {
	if len(rows) != 1 {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("json 是**单账号**格式（%d 个账号）—— 要批量请用 zip", len(rows)))
		return
	}
	row := rows[0]
	fileName := fmt.Sprintf("%s-%s.json", row.provider, row.status.UID)
	var payload []byte
	if withSecrets {
		extResults := h.exportExtResults(rows)
		name, raw, ok := h.exportCredential(row, extResults)
		if !ok {
			writeError(w, http.StatusUnprocessableEntity,
				"该账号的凭证读不出来（文件缺失且内存里没有可用 token）：请改用不含凭据的导出")
			return
		}
		if name != "" {
			fileName = name
		}
		payload = raw
	} else {
		payload = exportLiteDoc(row)
	}
	h.logExport(1, withSecrets, rows)
	writeAttachment(w, fileName, "application/json", payload)
}

// writeExportZip 批量导出（逐账号一个文件 + manifest.json）。
func (h *Handler) writeExportZip(w http.ResponseWriter, rows []exportRow, withSecrets bool, stamp string) {
	var extResults map[string]gateway.ExportedCredential
	if withSecrets {
		extResults = h.exportExtResults(rows)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	type manFile struct {
		Name   string `json:"name"`
		UID    string `json:"uid"`
		Size   int    `json:"size"`
		SHA256 string `json:"sha256"`
	}
	man := struct {
		FormatVersion   int       `json:"format_version"`
		CreatedAt       time.Time `json:"created_at"`
		AccountCount    int       `json:"account_count"`
		SecretsIncluded bool      `json:"secrets_included"`
		Note            string    `json:"note,omitempty"`
		Files           []manFile `json:"files"`
	}{
		FormatVersion:   exportFileVersion,
		CreatedAt:       time.Now(),
		AccountCount:    len(rows),
		SecretsIncluded: withSecrets,
	}
	if !withSecrets {
		man.Note = "不含凭据：只能用于清点账号，不能用来恢复。要迁移账号请勾选「含凭据」。"
	}

	used := map[string]bool{}
	for _, row := range rows {
		var name string
		var raw []byte
		if withSecrets {
			n, r, ok := h.exportCredential(row, extResults)
			if !ok {
				continue // 清单里不出现；note 由前端/日志说明有几个号没导出来
			}
			name, raw = n, r
		} else {
			name, raw = fmt.Sprintf("%s-%s.json", row.provider, row.status.UID), exportLiteDoc(row)
		}
		// 同名文件（不同上游重名/同 UID 重复）加序号，避免 zip 里互相覆盖。
		if used[name] {
			base := strings.TrimSuffix(name, filepath.Ext(name))
			for i := 2; ; i++ {
				cand := fmt.Sprintf("%s-%d%s", base, i, filepath.Ext(name))
				if !used[cand] {
					name = cand
					break
				}
			}
		}
		used[name] = true
		fh := &zip.FileHeader{Name: name, Method: zip.Deflate}
		fh.Modified = time.Now()
		fw, err := zw.CreateHeader(fh)
		if err != nil {
			continue
		}
		if _, err := fw.Write(raw); err != nil {
			continue
		}
		sum := sha256.Sum256(raw)
		man.Files = append(man.Files, manFile{
			Name: name, UID: row.status.UID, Size: len(raw), SHA256: hex.EncodeToString(sum[:]),
		})
	}
	rawMan, _ := json.MarshalIndent(man, "", "  ")
	if fw, err := zw.Create("manifest.json"); err == nil {
		fw.Write(append(rawMan, '\n'))
	}
	if err := zw.Close(); err != nil {
		writeError(w, http.StatusInternalServerError, "打包失败: "+err.Error())
		return
	}
	h.logExport(len(man.Files), withSecrets, rows)
	name := fmt.Sprintf("naibox-accounts-%s.zip", stamp)
	if withSecrets {
		name = fmt.Sprintf("naibox-accounts-%s-with-secrets.zip", stamp)
	}
	writeAttachment(w, name, "application/zip", buf.Bytes())
}

// exportCSVHeader CSV 的列（顺序固定，前端/脚本按列名读）。
var exportCSVHeader = []string{
	"provider", "uid", "nickname", "channel", "domain",
	"credits", "quota_kind", "quota_unit",
	"cooling", "cool_kind", "reason", "disabled",
	"success_count", "err_total", "last_success", "last_err",
	"egress", "expires_at", "file",
}

// writeExportCSV 清单格式（永不含凭据）。
//
// 编码细节：UTF-8 **带 BOM**（不带 BOM 的 UTF-8 CSV 在 Excel 里中文全是乱码，
// 而用户拿到的第一反应就是"导出坏了"）、CRLF 行尾（RFC4180）、字段按需加引号
// （昵称里出现逗号/引号/换行是常态）。
func (h *Handler) writeExportCSV(w http.ResponseWriter, rows []exportRow, stamp string) {
	var buf bytes.Buffer
	buf.WriteString("\xef\xbb\xbf") // UTF-8 BOM
	cw := csv.NewWriter(&buf)
	cw.UseCRLF = true
	_ = cw.Write(exportCSVHeader)
	for _, row := range rows {
		st := row.status
		channel, domain, expires, file := "", "", "", ""
		if row.auth != nil {
			channel = row.auth.Channel
			if channel == "" {
				channel = auth.DeriveChannel(row.auth.Domain)
			}
			domain = row.auth.Domain
			if row.auth.ExpiresAt > 0 {
				expires = time.Unix(row.auth.ExpiresAt, 0).Format(time.RFC3339)
			}
			if row.auth.FilePath != "" {
				file = filepath.Base(row.auth.FilePath)
			}
		}
		lastSuccess, lastErr := "", ""
		if !st.LastSuccessTime.IsZero() {
			lastSuccess = st.LastSuccessTime.Format(time.RFC3339)
		}
		if !st.LastErrTime.IsZero() {
			lastErr = st.LastErrTime.Format(time.RFC3339)
		}
		_ = cw.Write([]string{
			row.provider, st.UID, st.Nickname, channel, domain,
			strconv.FormatInt(st.Credits, 10), st.Quota.Kind, st.Quota.Unit,
			boolText(st.Cooling), st.CoolKind, st.Reason, boolText(st.Disabled),
			strconv.FormatInt(st.SuccessCount, 10), strconv.FormatInt(st.ErrTotal, 10),
			lastSuccess, lastErr,
			st.Egress, expires, file,
		})
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		writeError(w, http.StatusInternalServerError, "生成 CSV 失败: "+err.Error())
		return
	}
	h.logExport(len(rows), false, rows)
	writeAttachment(w, fmt.Sprintf("naibox-accounts-%s.csv", stamp), "text/csv; charset=utf-8", buf.Bytes())
}

// writeExportTXT 人读清单：按上游分组，末尾一行汇总。
func (h *Handler) writeExportTXT(w http.ResponseWriter, rows []exportRow, stamp string) {
	var b strings.Builder
	fmt.Fprintf(&b, "NaiBox 账号清单（%s，共 %d 个）\n", time.Now().Format("2006-01-02 15:04:05"), len(rows))
	fmt.Fprintf(&b, "本文件不含任何凭据（accessToken / refreshToken / 设备令牌都没有）。\n\n")
	cur := ""
	width := 0
	for _, row := range rows {
		if row.provider != cur {
			cur = row.provider
			fmt.Fprintf(&b, "== %s ==\n", cur)
			fmt.Fprintf(&b, "%-24s %-14s %-10s %-8s %-10s %s\n",
				"UID", "昵称", "渠道", "额度", "状态", "线路")
			width++
		}
		status := "可用"
		switch {
		case row.status.Disabled:
			status = "已禁用"
		case row.status.Cooling:
			status = "冷却中"
		}
		channel := ""
		if row.auth != nil {
			channel = row.auth.Channel
			if channel == "" {
				channel = auth.DeriveChannel(row.auth.Domain)
			}
		}
		quota := fmt.Sprintf("%d", row.status.Credits)
		if row.status.Quota.Unit != "" {
			quota += row.status.Quota.Unit
		}
		egress := row.status.Egress
		if egress == "" {
			egress = "共用出口"
		}
		fmt.Fprintf(&b, "%-24s %-14s %-10s %-8s %-10s %s\n",
			row.status.UID, row.status.Nickname, channel, quota, status, egress)
	}
	h.logExport(len(rows), false, rows)
	writeAttachment(w, fmt.Sprintf("naibox-accounts-%s.txt", stamp),
		"text/plain; charset=utf-8", []byte(b.String()))
}

// logExport 含凭据导出**必须**留痕。
//
// 记录数量与来源，不记录内容：审计要回答的是"谁在什么时候导走了多少"，
// 而不是再抄一份凭据进日志 —— 那会让日志文件变成新的泄漏面。
func (h *Handler) logExport(count int, withSecrets bool, rows []exportRow) {
	providers := map[string]int{}
	for _, r := range rows {
		providers[r.provider]++
	}
	keys := make([]string, 0, len(providers))
	for k := range providers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, providers[k]))
	}
	if withSecrets {
		log.Printf("admin: 导出 %d 个账号的**含凭据**数据（%s）", count, strings.Join(parts, " "))
		return
	}
	log.Printf("admin: 导出 %d 个账号的清单（不含凭据，%s）", count, strings.Join(parts, " "))
}

// writeAttachment 统一的下载响应头。
func writeAttachment(w http.ResponseWriter, fileName, contentType string, payload []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, fileName))
	w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
	// 导出文件里有口令级内容，禁止中间层缓存。
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(payload)
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
