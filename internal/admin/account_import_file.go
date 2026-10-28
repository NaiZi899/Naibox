// account_import_file.go 「导入导出文件」端点：把导出的 zip / json **原样写回**凭证目录。
//
// # 为什么它和 accountsImport 是两条端点，而不是合在一条
//
// accountsImport 处理的是"用户粘的一段文本"，那段文本**是什么意思由上游回答**
// （gateway.AccountImportExt：可能是 key，可能是登录凭证，可能还能换出新 key）。
// 它产出的文件名与内容是上游拼的。
//
// 这条端点处理的是**我们自己导出的文件**（account_export.go）。导出时已经把
// 每条凭证的字节原封不动拷进了 zip（见 exportCredential 的三级来源），所以
// 导入的正确语义是"写回去"，而不是"再让上游解析一遍"：
//
//	让上游重新解析 → 需要上游认得自己导出的形状，而"按文件导出"那条分支
//	导的是磁盘原文，上游未必肯吃回去；两边一旦不同形，往返就断了。
//
// 于是这里只做四件事：认格式、验完整性（sha256）、防越界（zip-slip）、落盘。
// 凭证内容的解释权仍在上游 —— 落盘之后由既有的「重载 auths」按上游的解析器入池。
//
// # 为什么不在导入里顺手 reload
//
// "重载"的判据（哪个目录、用哪个域、按谁剔除）已经有一份实现（accountsReload），
// 而且那份实现踩过两个真实 bug（默认上游带跑偏、codearts 那行扫错目录）。
// 在这里再写一份，就是本项目反复吃过的亏：**判据抄多份，只有一份被改对**。
// 所以导入只返回"哪些上游需要重载"，由调用方（前端按钮 / 脚本）去调既有端点。
package admin

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const (
	// importFileMaxBytes 上传上限。2000 个账号的 zip 在几十 MB 量级
	//（导出那边 exportMaxAccounts 也是 2000），留一倍余量。
	importFileMaxBytes = 64 << 20
	// importConfirmToken 含凭据导入必须回传的确认串。
	//
	// 与 exportConfirmToken 对称：这个动作会把**别人的凭证**写进本机，
	// 写错一次就是"用别人的号发请求"。要求一个写对了才成立的字符串，
	// 等于强迫调用方知道自己在干什么。
	importConfirmToken = "import-secrets"
)

// importManifest 与导出时的 manifest.json 同形（format_version 1 / 2）。
//
// v1 的 files[] 没有 provider 字段，那时单个 zip 只装一个上游；
// v2 起每条带上 provider，导入才能不看调用方参数就知道该写进哪个目录。
type importManifest struct {
	FormatVersion int    `json:"format_version"`
	SecretsIncl   bool   `json:"secrets_included"`
	Note          string `json:"note"`
	Files         []struct {
		Name     string `json:"name"`
		UID      string `json:"uid"`
		Provider string `json:"provider"`
		Size     int    `json:"size"`
		SHA256   string `json:"sha256"`
	} `json:"files"`
}

// importFileResult 单条结果（前端按 action 上色）。
type importFileResult struct {
	Name     string `json:"name"`
	Provider string `json:"provider,omitempty"`
	UID      string `json:"uid,omitempty"`
	Action   string `json:"action"` // created / replaced / unchanged / skipped / rejected
	Error    string `json:"error,omitempty"`
}

// accountsImportFile POST /admin/accounts/import-file?provider=&on_conflict=&dry_run=&confirm=
//
// 请求体：导出产物的原始字节（zip 或单账号 json）。
//
// 回执：{ok, created, replaced, unchanged, skipped, rejected, results:[…], reload:[{provider,endpoint}]}
func (h *Handler) accountsImportFile(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if strings.TrimSpace(q.Get("confirm")) != importConfirmToken {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"导入会把文件里的凭据写进本机凭证目录，需要 confirm=%q", importConfirmToken))
		return
	}
	onConflict := strings.ToLower(strings.TrimSpace(q.Get("on_conflict")))
	if onConflict == "" {
		onConflict = "skip"
	}
	if onConflict != "skip" && onConflict != "replace" {
		writeError(w, http.StatusBadRequest, "on_conflict 只接受 skip / replace")
		return
	}
	dryRun := q.Get("dry_run") == "1" || strings.EqualFold(q.Get("dry_run"), "true")
	providerHint := strings.TrimSpace(q.Get("provider"))

	body, err := io.ReadAll(io.LimitReader(r.Body, importFileMaxBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "读取上传内容失败: "+err.Error())
		return
	}
	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "上传内容为空")
		return
	}
	if len(body) > importFileMaxBytes {
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("上传内容超过 %d MB：请按上游分批导出", importFileMaxBytes>>20))
		return
	}

	// 格式判定看魔术字节，不看 Content-Type —— 浏览器的 multipart 会把
	// 类型写成 application/octet-stream，凭它判断等于没判断。
	switch {
	case bytes.HasPrefix(body, []byte("PK\x03\x04")):
		h.importZip(w, body, providerHint, onConflict, dryRun)
	case bytes.HasPrefix(bytes.TrimSpace(body), []byte("{")):
		h.importSingleJSON(w, body, providerHint, onConflict, dryRun)
	default:
		writeError(w, http.StatusBadRequest,
			"认不出这个文件的格式：只接受导出产生的 zip 或单账号 json。"+
				"csv / txt 是**清单**格式（不含凭据），不能用来恢复账号")
	}
}

// importZip 逐条处理 zip 里的凭证文件。
func (h *Handler) importZip(w http.ResponseWriter, body []byte, providerHint, onConflict string, dryRun bool) {
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		writeError(w, http.StatusBadRequest, "zip 解析失败（文件可能不完整）: "+err.Error())
		return
	}

	// 先收集 manifest：它是"完整性 + 归属"的唯一来源。
	var man importManifest
	hasManifest := false
	for _, f := range zr.File {
		if !strings.EqualFold(f.Name, "manifest.json") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			writeError(w, http.StatusBadRequest, "manifest.json 读不出来: "+err.Error())
			return
		}
		raw, err := io.ReadAll(io.LimitReader(rc, 4<<20))
		rc.Close()
		if err != nil {
			writeError(w, http.StatusBadRequest, "manifest.json 读不出来: "+err.Error())
			return
		}
		if err := json.Unmarshal(raw, &man); err != nil {
			writeError(w, http.StatusBadRequest, "manifest.json 不是合法 JSON: "+err.Error())
			return
		}
		hasManifest = true
	}
	if !hasManifest {
		// 没有清单也能导（用户自己打的 zip），但就没有 sha 校验与归属，
		// 全部依赖 provider 参数 —— 这一点在回执里说清楚。
	}

	sums := map[string]string{}
	providers := map[string]string{}
	uids := map[string]string{}
	for _, f := range man.Files {
		if f.Name == "" {
			continue
		}
		sums[f.Name] = strings.ToLower(strings.TrimSpace(f.SHA256))
		if f.Provider != "" {
			providers[f.Name] = f.Provider
		}
		if f.UID != "" {
			uids[f.Name] = f.UID
		}
	}

	var results []importFileResult
	counts := map[string]int{}
	touched := map[string]bool{}

	for _, f := range zr.File {
		if f.FileInfo().IsDir() || strings.EqualFold(f.Name, "manifest.json") {
			continue
		}
		res := importFileResult{Name: f.Name}

		// ① 越界检查：zip 里的名字是**外部输入**，绝不能直接拼进目标目录。
		if badZipName(f.Name) {
			res.Action, res.Error = "rejected", "文件名含路径分隔符或 ..（拒绝写入 —— 防越出凭证目录）"
			results = append(results, res)
			counts["rejected"]++
			continue
		}
		// ② 归属：清单优先 → 调用方参数 → 文件名前缀（`<provider>-<uid>.json`）
		pid := providers[f.Name]
		if pid == "" {
			pid = providerHint
		}
		if pid == "" {
			pid = h.inferProviderFromName(f.Name)
		}
		if pid == "" {
			res.Action, res.Error = "rejected",
				"不知道该写进哪个上游的目录：清单里没有 provider，调用方也没传 provider"
			results = append(results, res)
			counts["rejected"]++
			continue
		}
		if _, ok := h.providerByID(pid); !ok {
			res.Action, res.Error = "rejected", "上游不存在: "+pid
			results = append(results, res)
			counts["rejected"]++
			continue
		}
		res.Provider = pid
		res.UID = uids[f.Name]

		rc, err := f.Open()
		if err != nil {
			res.Action, res.Error = "rejected", "读不出来: "+err.Error()
			results = append(results, res)
			counts["rejected"]++
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(rc, 4<<20))
		rc.Close()
		if err != nil {
			res.Action, res.Error = "rejected", "读不出来: "+err.Error()
			results = append(results, res)
			counts["rejected"]++
			continue
		}
		// ③ 完整性：清单里有 sha 就必须对得上。对不上说明文件被改过，
		// 写进去等于把坏凭证当成好凭证用。
		if want := sums[f.Name]; want != "" {
			got := sha256.Sum256(raw)
			if hex.EncodeToString(got[:]) != want {
				res.Action, res.Error = "rejected", "sha256 与清单不一致（文件被改动或不完整）"
				results = append(results, res)
				counts["rejected"]++
				continue
			}
		}
		// ④ 内容像不像凭证：清单声明不含凭据的包，逐条说明为什么不能导。
		if reason := credentiallessReason(raw); reason != "" {
			res.Action, res.Error = "rejected", reason
			results = append(results, res)
			counts["rejected"]++
			continue
		}

		action, werr := h.writeImportedFile(pid, f.Name, raw, onConflict, dryRun)
		res.Action = action
		if werr != nil {
			res.Error = werr.Error()
		}
		if action == "created" || action == "replaced" {
			touched[pid] = true
		}
		counts[action]++
		results = append(results, res)
	}

	h.respondImport(w, results, counts, touched, dryRun, hasManifest)
}

// importSingleJSON 单账号导出（`<provider>-<uid>.json`）的导入。
func (h *Handler) importSingleJSON(w http.ResponseWriter, body []byte, providerHint, onConflict string, dryRun bool) {
	if providerHint == "" {
		writeError(w, http.StatusBadRequest,
			"单账号 json 不带上游信息：请在 provider 参数里指明（多账号请用 zip，清单里自带 provider）")
		return
	}
	if _, ok := h.providerByID(providerHint); !ok {
		writeError(w, http.StatusNotFound, "上游不存在: "+providerHint)
		return
	}
	if reason := credentiallessReason(body); reason != "" {
		writeError(w, http.StatusBadRequest, reason)
		return
	}
	name := providerHint + "-" + "imported" + ".json"
	// 文件名优先用凭证里的 uid（与导出时的命名一致），取不到再退回固定名。
	if uid := uidFromCredential(body); uid != "" {
		name = providerHint + "-" + uid + ".json"
	}
	action, err := h.writeImportedFile(providerHint, name, body, onConflict, dryRun)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	counts := map[string]int{action: 1}
	touched := map[string]bool{}
	if action == "created" || action == "replaced" {
		touched[providerHint] = true
	}
	h.respondImport(w, []importFileResult{{
		Name: name, Provider: providerHint, UID: uidFromCredential(body), Action: action,
	}}, counts, touched, dryRun, false)
}

// respondImport 统一回执。
func (h *Handler) respondImport(w http.ResponseWriter, results []importFileResult, counts map[string]int, touched map[string]bool, dryRun, hasManifest bool) {
	if results == nil {
		results = []importFileResult{}
	}
	reload := make([]map[string]string, 0, len(touched))
	for pid := range touched {
		reload = append(reload, map[string]string{
			"provider": pid,
			"endpoint": "POST /admin/accounts/reload",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        counts["rejected"] == 0 && counts["skipped"] == 0,
		"dry_run":   dryRun,
		"created":   counts["created"],
		"replaced":  counts["replaced"],
		"unchanged": counts["unchanged"],
		"skipped":   counts["skipped"],
		"rejected":  counts["rejected"],
		"results":   results,
		// 导入不自己 reload（判据只有一份，见文件头）。调用方按这个列表
		// 逐个调既有的重载端点，账号才会进池。
		"reload":        reload,
		"manifest_used": hasManifest,
		"disk_written":  !dryRun,
	})
}

// writeImportedFile 落盘一条凭证，处理冲突。
//
// 三种冲突结果要分开：内容一样（unchanged）、跳过（skipped）、覆盖（replaced）。
// 只回 "ok" 的话，批量导入 200 个号时用户没法知道到底改了几个。
func (h *Handler) writeImportedFile(pid, name string, raw []byte, onConflict string, dryRun bool) (string, error) {
	dir := h.importDirFor(pid)
	path := filepath.Join(dir, name)
	old, err := os.ReadFile(path)
	switch {
	case err == nil:
		if bytes.Equal(old, raw) {
			return "unchanged", nil
		}
		if onConflict == "skip" {
			return "skipped", nil
		}
		if dryRun {
			return "replaced", nil
		}
		if _, werr := writeAuthFile(dir, name, raw); werr != nil {
			return "rejected", werr
		}
		return "replaced", nil
	case os.IsNotExist(err):
		if dryRun {
			return "created", nil
		}
		if _, werr := writeAuthFile(dir, name, raw); werr != nil {
			return "rejected", werr
		}
		return "created", nil
	default:
		return "rejected", fmt.Errorf("检查目标文件失败: %w", err)
	}
}

// badZipName 越界判定（zip-slip）。
func badZipName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return true
	}
	if strings.ContainsAny(name, `/\`) {
		return true
	}
	return filepath.IsAbs(name)
}

// inferProviderFromName 从 `<provider>-<uid>.json` 里猜上游（猜不到就空）。
//
// 只在清单缺 provider、调用方也没传时兜底：前缀必须是**已注册的上游**才算数，
// 否则一个叫 `my-backup-2026.json` 的文件会被当成上游 `my`。
func (h *Handler) inferProviderFromName(name string) string {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	i := strings.Index(base, "-")
	if i <= 0 {
		return ""
	}
	pid := base[:i]
	if _, ok := h.providerByID(pid); ok {
		return pid
	}
	return ""
}

// credentiallessReason 判断"这份内容里没有凭据"，返回人话原因（空 = 有凭据）。
//
// # 为什么要专门判一次
//
// 导出允许"不含凭据"（exportLiteDoc 会写 secrets_included:false）。
// 那种文件被当成迁移包导入时，写进目录的是一份**没有 token 的凭证**：
// 账号会出现在池里、每次请求 401，用户以为迁移完成了。宁可当场拒绝。
func credentiallessReason(raw []byte) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "内容为空"
	}
	if !bytes.HasPrefix(trimmed, []byte("{")) {
		return "内容不像凭证文件（不是 JSON 对象）"
	}
	var doc map[string]any
	if err := json.Unmarshal(trimmed, &doc); err != nil {
		return "内容不是合法 JSON: " + err.Error()
	}
	if v, ok := doc["secrets_included"]; ok {
		if b, isBool := v.(bool); isBool && !b {
			return "这是不含凭据的导出（secrets_included:false）：只能用于清点账号，不能恢复"
		}
	}
	// 递归找任一凭据字段。上游的字段名不统一（accessToken / apiKey / key /
	// token / cookies），这里只判断"有没有"，不解释含义。
	if hasCredentialKey(doc, 0) {
		return ""
	}
	return "文件里找不到任何凭据字段（accessToken / refreshToken / apiKey / key / token / cookies）"
}

// credentialKeys 视为"凭据"的键名（小写比较）。
var credentialKeys = []string{
	"accesstoken", "refreshtoken", "apikey", "api_key", "key", "token", "secret",
	"cookies", "cookie", "sessionkey", "session_key", "accesstokenexpiresat",
}

// hasCredentialKey 深度优先找凭据字段（限深 4 层，防止异常结构打爆栈/耗时）。
func hasCredentialKey(v any, depth int) bool {
	if depth > 4 {
		return false
	}
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			lk := strings.ToLower(k)
			for _, want := range credentialKeys {
				if lk == want {
					// 值非空才算（`"accessToken": ""` 等于没有）
					if s, isStr := child.(string); isStr && strings.TrimSpace(s) == "" {
						continue
					}
					return true
				}
			}
			if hasCredentialKey(child, depth+1) {
				return true
			}
		}
	case []any:
		for _, child := range t {
			if hasCredentialKey(child, depth+1) {
				return true
			}
		}
	}
	return false
}

// uidFromCredential 从凭证里取 uid（用于文件命名，取不到返回空）。
func uidFromCredential(raw []byte) string {
	var doc struct {
		Account struct {
			UID string `json:"uid"`
		} `json:"account"`
		UID string `json:"uid"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ""
	}
	if s := strings.TrimSpace(doc.Account.UID); s != "" {
		return s
	}
	return strings.TrimSpace(doc.UID)
}
