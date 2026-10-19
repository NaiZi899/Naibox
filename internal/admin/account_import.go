// account_import.go 通用的「批量粘贴导入」端点。
//
// # 为什么是**一个**端点而不是每个上游一个（用户要求）
//
// 用户原话：「那个批量导入全部上游都可以制作这个功能，所以就统一一下。」
//
// 改造前只有 workbuddy 与 loomy 各写了一份 `handleImport` + 自己的
// `/admin/<id>/import` 路由，其余 7 个上游界面上根本没有这个按钮。
// 每个上游再抄一遍的话：**判据会抄多份，只有一份被改对**（本项目反复吃
// 这个亏 —— 见 `runRefresh` 那几处的注释）。
//
// 现在核心只提供一个端点，按请求体里的 `provider` 分派：
//
//	POST /admin/accounts/import
//	{"provider":"cline","data":"{…}"}        ← data 是用户粘的**原文**
//
// 落盘与池对齐都在这里做；而"那段 JSON 是什么意思"完全交给上游
// （`gateway.AccountImportExt`）—— 核心不解析、不理解凭证内容。
//
// # 为什么路由参数是 `provider` 而不是"从路径里取"
//
// 与 `/admin/accounts/quota/refresh`、`/admin/login/start` 同一条既有约定：
// 指定了上游就必须有它自己的实现，否则**明确失败**（501），不回落默认上游。
// 路由写成 `/admin/<id>/import` 会让"上游没实现"变成一个 404 ——
// 而 404 在本项目里已经被用于"账号不存在"，两种含义混在一起无法区分。
package admin

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/NaiZi899/Naibox/internal/gateway"
)

// importMaxBytes 请求体上限。
//
// 8 MB 与既有两条导入路径一致（workbuddy / loomy 的 handleImport）——
// 用户粘几十条凭证也就几十 KB，8 MB 是给"一次导入几百个号"留的余量，
// 同时挡住"把整个 auths 目录的压缩包贴进来"这种误操作。
const importMaxBytes = 8 << 20

// accountsImport POST /admin/accounts/import —— 通用批量导入。
//
// 请求体：
//
//	{"provider":"<id>","data":"<用户粘贴的原文>","format":"<形态 id，可空>"}
//
// `data` 可以是单个对象或数组、也可以是逐行文本，**由上游自己决定认哪些形状**。
// `format` 是用户在**哪个输入框**里粘的（见 gateway.ImportCredentialKind）——
// 上游据此区分"这是一把 key 还是一个登录凭证"。缺省为空时按上游的默认形态解释，
// 老客户端（不带这个字段）因此行为逐字节不变。
//
// 回执（与既有两个导入端点同形，前端不用区分）：
//
//	{ok, imported, failed, results:[{index, ok, uid?, nickname?, format?, derived_from?, error?}]}
func (h *Handler) accountsImport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider string `json:"provider"`
		Data     string `json:"data"`
		Format   string `json:"format"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, importMaxBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "读取请求体失败")
		return
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON（需要 {provider, data}）: "+err.Error())
		return
	}
	pid := strings.TrimSpace(body.Provider)
	if pid == "" {
		writeError(w, http.StatusBadRequest, "缺少 provider —— 导入必须指明是哪个上游")
		return
	}
	if strings.TrimSpace(body.Data) == "" {
		writeError(w, http.StatusBadRequest, "没有可导入的内容（data 为空）")
		return
	}

	p, ok := h.providerByID(pid)
	if !ok {
		writeError(w, http.StatusNotFound, "上游 "+pid+" 不存在（未注册或未启用）")
		return
	}
	// ⚠ 与其余分派端点同一条：**没实现就明确 501**，不回落。
	// 回落会让"导入了但进了别的上游"变成一个静默的成功。
	ext, ok := gateway.ExtOf[gateway.AccountImportExt](p)
	if !ok {
		writeError(w, http.StatusNotImplemented,
			"上游 "+pid+" 不支持批量导入（它没有声明 AccountImportExt）")
		return
	}

	format := strings.TrimSpace(body.Format)
	// 用户指定了形态，但该上游不认它 —— 明确拒绝，不静默按默认解释。
	//
	// # 为什么这条检查必须存在
	//
	// 前端按上游自报的形态清单渲染输入框（见 CredentialKindReporter）。
	// 若下游把形态 id 拼错、或上游改了 id 却忘了改清单，那么
	// "用户在 session 框里粘了 sess_，下游却按 key 形态解释"会发生 ——
	// 后果是把登录凭证当 key 存进池子，每请求 401，而用户以为导入成功了。
	//
	// 拦在这里的代价是一句话，放过去的代价是一个永远不会好的账号。
	if format != "" {
		if reporter, rok := gateway.ExtOf[gateway.CredentialKindReporter](p); rok {
			known := false
			for _, k := range reporter.ImportCredentialKinds() {
				if k.ID == format {
					known = true
					break
				}
			}
			if !known {
				writeError(w, http.StatusBadRequest,
					"上游 "+pid+" 不认识导入形态 "+format+"（它只支持其自报的那几种）")
				return
			}
		}
	}

	creds, err := ext.ImportCredentials(body.Data, format)
	if err != nil {
		// 两类 error 要分开说，否则界面上会出现自相矛盾的一句话。
		//
		//	解析失败（用户粘错了）      → 套「导入内容无法解析：」
		//	说明性结论（凭证有效但不导入）→ **原样展示**
		//
		// 后者是真实存在的场景：基元律动的登录凭证能读出余额，但上游只下发
		// 脱敏 key，所以"这一栏不导入任何账号"。套上"无法解析"会让一句
		// 「凭证有效，账号 demo-user，余额 99.75」变成看起来像用户的错。
		if gateway.IsImportNotice(err) {
			// 说明性结论（凭证有效但不导入）**不是失败**：不能回 4xx，也不能让
			// 界面把它打红。改造前这里回 400，前端 catch 到就 toast「导入失败：
			// 登录凭证有效……」—— 一句"有效"配一个红色失败提示，用户只能读成
			// "凭证有效但拿不到内容"。现在改成 200 + notice 字段，由前端渲染成
			// 中性信息弹窗（需要读完的结论，不该塞进 3 秒的 toast）。
			//
			// ok=false 仍然保留：确实没有导入任何东西，历史客户端据此也不会误判。
			writeJSON(w, http.StatusOK, map[string]any{
				"ok":       false,
				"imported": 0,
				"failed":   0,
				"notice":   err.Error(),
			})
			return
		}
		// 解析失败是**用户输入问题**（粘错了内容），不是服务端故障 ——
		// 400 + 原因，让界面能照实说清哪不对。
		writeError(w, http.StatusBadRequest, "导入内容无法解析："+err.Error())
		return
	}

	dir := h.importDirFor(pid)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeError(w, http.StatusInternalServerError, "创建凭证目录失败: "+err.Error())
		return
	}

	results := make([]map[string]any, 0, len(creds))
	okN := 0
	for i, c := range creds {
		res := map[string]any{"index": i + 1}
		if err := h.importOne(dir, c); err != nil {
			res["ok"] = false
			res["error"] = err.Error()
		} else {
			okN++
			res["ok"] = true
			res["uid"] = c.UID
			if c.Nickname != "" {
				res["nickname"] = c.Nickname
			}
			// 形态与来源透回前端：用户要知道"这条是从哪个框进来的、
			// 是不是我用登录凭证换来的"。
			if c.Format != "" {
				res["format"] = c.Format
			}
			if c.DerivedFrom != "" {
				res["derived_from"] = c.DerivedFrom
			}
		}
		results = append(results, res)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       okN == len(creds),
		"imported": okN,
		"failed":   len(creds) - okN,
		"results":  results,
	})
}

// importOne 把一条上游产出的凭证落盘。
//
// # 为什么核心只做"写文件"
//
// 文件名与内容都是**上游给的**（`ImportedCredential`）—— 核心不拼、不改。
// 这样导入产出的文件与页内登录产出的**逐字节同形**，下游（LoadDir /
// 账号池对齐）不需要区分"这条是导入的还是登录来的"。
//
// ⚠ 文件名必须**不含路径分隔符**：它会与 dir 拼在一起，而 `FileName`
// 来自上游（可能用了 uid 拼接）。这里做最后一道校验 —— 上游若有 bug，
// 表现必须是"这一条导入失败"，而不是"写到了别的目录"。
func (h *Handler) importOne(dir string, c gateway.ImportedCredential) error {
	name := strings.TrimSpace(c.FileName)
	if name == "" {
		return errEmptyFileName
	}
	if strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return errBadFileName
	}
	if len(c.Raw) == 0 {
		return errEmptyContent
	}
	_, err := writeAuthFile(dir, name, c.Raw)
	return err
}

// importDirFor 取该上游的凭证目录。
//
// 判据与 `accountsReload` **逐字一致**（照抄那边的顺序）：
//
//	上游自报的 AuthDir（实现 AuthDirExt 时）—— 目录是**上游的事实**
//	否则回落 <AuthsBase>/<provider>
//
// ⚠ 必须与 reload 用同一个目录，否则会出现"导入写进了 A 目录、
// 重载只扫 B 目录"→ 界面报"导入成功"但账号不出现。这类"两条路径各算
// 一次目录"的缺陷在本项目出现过（见 codearts 的目录拼接注释），
// 所以这里刻意复用同一个判据顺序而不是自己再写一遍。
func (h *Handler) importDirFor(pid string) string {
	base := strings.TrimSpace(h.cfg.AuthsBase)
	if base == "" {
		base = strings.TrimSpace(h.cfg.AuthDir)
	}
	if base == "" {
		base = "auths"
	}
	if p, ok := h.providerByID(pid); ok {
		if ax, ok := gateway.ExtOf[gateway.AuthDirExt](p); ok {
			if d := strings.TrimSpace(ax.AuthDir()); d != "" {
				return d
			}
		}
	}
	return filepath.Join(base, pid)
}

// 导入落盘时的三种"这条不可落盘"。分成三个值而不是一个笼统的 error：
// 用户看到的原因不同（文件名空 = 上游的 bug；内容空 = 解析出了个空凭证）。
var (
	errEmptyFileName = errors.New("落盘文件名为空（上游未给出文件名）")
	errBadFileName   = errors.New("落盘文件名含路径分隔符（拒绝写入 —— 防越出凭证目录）")
	errEmptyContent  = errors.New("落盘内容为空（解析出的凭证没有可写字段）")
)
