// accountimport_ext.go 上游自报「怎么把一段 JSON 变成我的一份凭证」。
//
// # 为什么需要这个扩展点（用户要求：批量导入统一到所有上游）
//
// 用户原话：「那个批量导入全部上游都可以制作这个功能，所以就统一一下。」
//
// 事实是：**批量导入此前只有两个上游有**（workbuddy、loomy），各写了一份
// `handleImport`。其余 7 个上游在界面上没有这个按钮 —— 而它们的凭证形态
// 各不相同，前端无法自己拼。
//
// # ⚠ 关键发现：绝大多数上游**已经有**这两步
//
// 实测（2026-09-30）每个上游都已有：
//
//	ParseCredential(raw []byte) (*Auth, error)   把磁盘那段 JSON 解成凭证
//	MarshalAuthFile(a *Auth) ([]byte, error)     把凭证据序列化成磁盘那段 JSON
//
// 而"导入"恰好就是这两步的**复合**：把用户粘的 JSON 解析成凭证、再落盘。
// 所以每个上游要写的只有**三行**（调用上面两个函数），不需要手写字段映射
// —— 手写映射是"看起来成功、实际字段错位"的高发区。
//
// # 为什么 Core 不认识凭证 JSON
//
// 与 `CredentialLoader` / `CredentialRefresher` 同一条判据：凭证格式是
// **上游的事实**（loomy 是 `session`、codearts 是 AK/SK+DPoP 私钥、
// qoder 是 `device_token`…）。核心只把"用户粘的那段文本"原样交给上游，
// 上游回给它"该写成哪个文件名 + 写什么字节"，核心负责落盘。
//
// 核心**不解析、不校验、不理解**那段 JSON —— 否则加一个上游就要改核心。
package gateway

import "errors"

// AccountImportExt 上游自报「怎么把一段 JSON 变成我的一份凭证」。
type AccountImportExt interface {
	// ImportCredentials 把一段用户粘贴的 JSON 转成可以落盘的凭证。
	//
	// 输入是**原文**（未解析的字符串）：上游自己决定认哪些形状
	//（嵌套形 `{"auth":{…},"account":{…}}`、扁平形、甚至中文键）。
	//
	// 返回的每一条是 `(文件名, 文件内容, 该账号的 uid, 展示名)`：
	//
	//	FileName  落盘的文件名（不含目录）。空串表示该条无法落盘。
	//	Raw       文件内容（上游自己的序列化结果）。
	//	UID       账号池主键。空串 = 无法确定账号身份 → 该条算失败。
	//	Nickname  展示名（可空）。
	//
	// ⚠ **必须是"解析失败 → 返回带原因的错误"，不能静默返回空切片**：
	// 用户粘错东西时，界面要告诉他"哪里不对"，而不是"导入了 0 个"。
	//
	// ⚠ 实现必须**幂等且无副作用**：不要在这个方法里写盘、不要碰账号池。
	// 落盘与池对齐由核心负责（核心才知道 AuthDir 在哪、池怎么对齐）。
	// 这样上游包仍然不依赖 `internal/pool`（架构硬约束）。
	//
	// ⚠⚠ 这个方法接收 `format`（用户在哪个输入框里粘的，见
	// ImportCredentialKind），**而不是保留旧签名**。理由是它必须能
	// 区分"这段文本是一把 key 还是一个登录凭证"—— 靠内容嗅探不可靠
	//（两者都可能缺失、都可能是很长的不透明串），用户的选择才是权威事实。
	//
	// 不认识 format（含空串）时按自己的默认形态解释：老客户端不带它，
	// 行为必须与改造前逐字节相同。
	//
	// ⚠ 若这次"导入"得到的结论不是"失败"而是一条**必须让用户知道的事实**
	//（例：基元律动的登录凭证只能读余额、上游不下发完整 key），用
	// NewImportNotice 返回 —— 调用方会照实展示，而不会把它说成"解析失败"。
	ImportCredentials(pasted string, format string) ([]ImportedCredential, error)
}

// importNotice 标记"这不是解析失败，而是一条要照实告诉用户的结论"。
//
// # 为什么需要它（一次真实的措辞矛盾）
//
// `importViaSession` 要告诉用户一个**内容为真**的结论：「登录凭证有效，
// 账号 demo-user，余额 99.75 —— 但上游只下发脱敏 key，所以这一栏不导入」。
// 这是一条有用的说明，不是错误。
//
// 而 admin 层对 `ImportCredentials` 的 error 一律套前缀
// 「导入内容无法解析：」—— 于是界面上出现自相矛盾的一句：
//
//	导入内容无法解析：登录凭证有效。账号：demo-user 余额：99.75 …
//
// 前半句说"解析失败"，后半句说"凭证有效"。用户读到的是"我明明粘对了"。
//
// 所以这里把两类 error 分开：
//
//	普通 error     → 用户输入有问题，套「导入内容无法解析：」（既有行为）
//	importNotice   → 上游的说明性结论，**原样展示**
//
// 上游不认识这个类型也能正常工作（返回普通 error），所以对既有实现零影响。
type importNotice struct{ msg string }

func (e *importNotice) Error() string { return e.msg }

// NewImportNotice 构造一条"说明性结论"（见 importNotice 的说明）。
//
// 上游包用它返回"凭证有效但不导入"这类**非失败**的结论。
func NewImportNotice(msg string) error { return &importNotice{msg: msg} }

// IsImportNotice 判断一个 error 是否是说明性结论（而非解析失败）。
func IsImportNotice(err error) bool {
	var n *importNotice
	return errors.As(err, &n)
}

// ImportCredentialKind 一种**可导入的凭证形态**（由上游自报）。
//
// # 为什么需要这个（用户诉求）
//
// 用户原话：「将批量导入分为 key 导入和登录凭证（sess_xxx）导入，
// 这样有登录凭证的导入登录凭证来查看 key，没有的就只需要填个 key 就行了」。
//
// 也就是说：同一个上游的导入框需要**两个标签页/两种语义**，
// 而它们的差别是**上游的事实**（基元律动的 key 是 `sk_`、登录凭证是
// `sess_`；别的上游可能是 `token` + `cookie`，或根本没有第二种）。
//
// # 为什么不让前端写 if (provider === 'jiyuan')
//
// 那是本项目最硬的判据（见 AccountColumnsExt 的注释）：前端不认识任何
// 上游名。所以这里让上游**自报形态清单**，前端只按数据渲染输入区。
// 一个只支持单一 key 的上游不实现这个方法 → 界面与改造前逐字节相同。
// ⚠ 每个字段**必须有 json tag**，且必须是 snake/lowercase。
//
// 这个结构体直接下发到前端（manifest 的 `import_kinds`），前端按
// `k.id` / `k.label` / `k.secret` 读。没有 tag 时 Go 会用**字段名原样**
// 序列化（"ID"/"Label"/"Secret"），于是前端全部读到 undefined ——
// 界面表现是"弹窗里两个标签都是空的、输入框没有占位符"，
// 而接口调试看起来一切正常（字段确实下发了两条）。
//
// 实测踩过一次（上线后看 manifest 才发现），所以这里 tags 是硬要求，
// 并由 accountimport_kinds_test.go 钉住字段名。
type ImportCredentialKind struct {
	// ID 形态标识（上游自己定，前端只做回传）。例："key" / "session"。
	ID string `json:"id"`
	// Label 输入区的标题。例："API Key" / "登录凭证（tr_session）"。
	Label string `json:"label"`
	// Hint 给用户的操作指引（可空）。例："在用户中心 API Key 页复制 sk_ 开头的值"。
	Hint string `json:"hint,omitempty"`
	// Placeholder 输入框占位符（可空）。例："sk_tr_…"。
	Placeholder string `json:"placeholder,omitempty"`
	// Secret 该形态的输入**是否等同登录态**（前端据此决定要不要用密码框）。
	//
	// 登录凭证（tr_session）能读账号下的全部 key 与余额，泄漏后果比
	// 单把 key 严重 —— 前端据此把它渲染成 password 输入并加一句警告。
	//
	// 不用 omitempty：`false` 是**有意义的取值**（"这一栏不是敏感输入"），
	// 省掉它会让前端分不清"明确说不是"与"上游没提供这个字段"。
	Secret bool `json:"secret"`
}

// CredentialKindReporter 上游自报「我的导入框支持哪几种凭证形态」（可选）。
//
// 不实现它 = 只有一种形态（前端沿用改造前的单输入框）；
// 实现了 = 前端按返回的顺序渲染多个输入区。
//
// ⚠ 上报的 ID 必须能被 ImportCredentials 认出来（见 ImportedCredential.Format）——
// 报了一个自己解析不了的形态，用户会在那个框里粘东西然后失败。
type CredentialKindReporter interface {
	ImportCredentialKinds() []ImportCredentialKind
}

// ImportedCredential 一条可落盘的凭证（`AccountImportExt` 的产物）。
//
// # 为什么是"文件名 + 内容"而不是"直接给个 *Auth"
//
// 因为落盘格式只有**上游**知道（文件名前缀、嵌套还是扁平、字段名 casing）。
// 核心拿着这三样就能写文件，且与页内登录产的凭证**逐字节同形** ——
// 下游（LoadDir / 账号池对齐）不用区分"这条是导入的还是登录来的"。
type ImportedCredential struct {
	// FileName 落盘文件名（不含目录）。必须**不含路径分隔符** ——
	// 它会与 AuthDir 拼在一起。空串 = 该条不可落盘（算失败）。
	FileName string
	// Raw 文件内容（上游的序列化结果）。
	Raw []byte
	// UID 账号池主键。空串 = 无法确定账号身份 → 该条算失败。
	UID string
	// Nickname 展示名（可空；空则由核心按 UID 前缀显示）。
	Nickname string
	// Format 这条凭证来自哪种输入形态（对应 ImportCredentialKind.ID，可空）。
	//
	// 空串 = 单形态导入（改造前的行为）。
	Format string
	// DerivedFrom 这条凭证是**靠登录凭证换来的**（可空）。
	//
	// # 这条路径解决了什么（用户诉求）
	//
	// 「有登录凭证的导入登录凭证来查看 key」—— 用户手上只有 `sess_`，
	// 但调用面要的是 `sk_`。上游可以用 sess_ 调它的 Key 管理接口，
	// **替用户把 key 拉出来并存进池子**，用户不必自己逐个复制。
	//
	// 值是那个登录凭证的**标识**（上游自己定，通常是被脱敏的 session 前缀），
	// 不是凭证本身 —— 回执会被下发到前端，**绝不能带登录态**。
	DerivedFrom string
}
