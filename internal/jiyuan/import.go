// import.go 基元律动的批量导入（gateway.AccountImportExt）——**两种形态**。
//
// # 两种形态（用户诉求）
//
// 用户原话：「将批量导入分为 key 导入和登录凭证（sess_xxx）导入，
// 这样有登录凭证的导入登录凭证来查看 key，没有的就只需要填个 key 就行了」。
//
//	形态 `key`      贴 `sk_…`，一行一把（主用法）
//	形态 `session`  贴 `sess_…`，网关替你把该账号下的**全部 key** 拉出来入库
//	                并顺带读余额（余额只认 session，key 打不过去）
//
// # 为什么 `format` 由用户选，而不是从内容嗅探
//
// 两者都是长的不透明串，且**都可能缺失**（用户可能粘错框）。
// 靠前缀（sk_ / sess_）嗅探只在"两者都规整"时成立 —— 一旦上游改了前缀，
// 就会静默把登录凭证当成 key 收下（然后每请求 401）。
// 用户选了哪个框，那才是权威事实；前缀只用来**校验他的选择**。
//
// # 凭据安全纪律
//
// 登录凭证**绝不落盘、绝不出现在回执里** —— 它只活在一次导入的调用栈里。
// 拉出来的 key 才是落盘对象（那本来就是调用凭据）。回执里只带
// 一个脱敏标识（`DerivedFrom`，session 的前 9 位）。
package jiyuan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/NaiZi899/Naibox/internal/gateway"
)

// 导入形态 ID（与 ImportCredentialKinds 上报的一致）。
const (
	importKindKey     = "key"
	importKindSession = "session"
	// importKindSessionCreate 用登录凭证**代开一把新 key**（写上游账号）。
	//
	// 之所以必须是一个**独立形态**而不是 session 的自动分支：它会真实地
	// 在该上游账号里新增一把凭证、消耗账号的 key 配额（上游上限实测 10 把）。
	// 让"贴 sess_"这个只读意图静默升级成写操作，是用户不该承受的意外 ——
	// 所以写成第三个框，由用户自己选。
	importKindSessionCreate = "session-create"
)

// ImportCredentialKinds 实现 gateway.CredentialKindReporter —— 自报三条导入路径。
func (p *Provider) ImportCredentialKinds() []gateway.ImportCredentialKind {
	// ⚠ 顺序即界面顺序：**key 在前**。
	// 只有一把 key 的用户是绝大多数，默认落在第一个框上最省事；
	// 有登录凭证的用户会主动往后切。
	return []gateway.ImportCredentialKind{
		{
			ID:          importKindKey,
			Label:       "API Key",
			Hint:        "每行一把，空格后可跟标签。在用户中心 API Key 页复制 sk_ 开头的值。",
			Placeholder: "sk_tr_…",
			Secret:      false,
		},
		{
			ID:    importKindSession,
			Label: "登录凭证（给已有 Key 补余额）",
			// ⚠ 这里**不能**出现 Markdown 星号：Hint 会原样显示在控制台
			// （前端只做 esc，不渲染 markdown）。改造前写了 `**实时余额**`，
			// 用户看到的就是一对裸星号。
			//
			// ⚠ 也**不能**写"不会保存登录凭证"：这一栏必须把 session 写进
			// 凭证文件，否则周期额度刷新读不到余额，额度列会一直是 `—`。
			// 改造前界面上那句相反的承诺，让用户以为贴了不留痕。
			Hint: "贴 sess_ 开头的登录凭证，网关会读出账号名称、手机尾号与实时余额，" +
				"并把登录凭证保存到该账号的凭证文件里（这样后台每 15 分钟才能刷到最新余额）。\n" +
				"它会把凭证挂到该账号下已导入的那把 key 上（按 key 前缀配对）。" +
				"上游只下发脱敏 key，所以这一栏不会新增任何 key；若池里还没有该账号的 key，" +
				"请先用「API Key」栏导入，或用下面「新建 Key 并入库」。\n" +
				"获取：浏览器登录 tokenrhythm.studio → F12 → Application → Cookies → 复制 tr_session。",
			Placeholder: "sess_…",
			Secret:      true,
		},
		{
			ID:    importKindSessionCreate,
			Label: "登录凭证（新建 Key 并入库）",
			Hint: "会在你 tokenrhythm.studio 账号里真实新建一把 API Key（名 omni2api，若已占用则自动加序号），" +
				"并占用该账号的 key 数量配额（上游上限 10 把）。完整 key 只在这一刻下发一次，" +
				"网关会立刻存入账号池。\n" +
				"注意：这一栏只保存 key，不保存登录凭证，所以导入后额度是空的 —— " +
				"想看余额，请在「给已有 Key 补余额」那一栏再贴一次同一个 sess_。\n" +
				"获取：浏览器登录 tokenrhythm.studio → F12 → Application → Cookies → 复制 tr_session。",
			Placeholder: "sess_…",
			Secret:      true,
		},
	}
}

// ImportCredentials 实现 gateway.AccountImportExt。
//
// format 为空或未知时按 `key` 形态解释（老客户端不带这个参数）。
func (p *Provider) ImportCredentials(pasted string, format string) ([]gateway.ImportedCredential, error) {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case importKindSession:
		return p.importViaSession(context.Background(), pasted)
	case importKindSessionCreate:
		return p.importViaSessionCreate(context.Background(), pasted)
	default:
		return p.importViaKeys(pasted)
	}
}

// importViaKeys 形态一：每行一把 key。
func (p *Provider) importViaKeys(pasted string) ([]gateway.ImportedCredential, error) {
	items, err := parseImportItems(pasted)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, errors.New("没有解析出任何 API Key —— " +
			"请每行贴一把 key（如 sk_tr_…），或贴 JSON 数组")
	}
	out := make([]gateway.ImportedCredential, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, it := range items {
		a := NewAuth(it.key, it.label)
		// 同一段里重复贴同一把 key：只收一次。
		// 不这么做回执会报"导入 2 个"而磁盘上只有一个文件 —— 一个诚实性缺陷。
		if seen[a.UID] {
			continue
		}
		seen[a.UID] = true
		c, err := credentialOf(a, importKindKey)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// sessionCred 归一化用户贴的登录凭证（两个 session 形态共用）。
//
// 去引号/分号、逗号后只取第一段、校验 `sess_` 前缀 —— 每一条都对应
// 一种真实粘错法（从 Cookie 面板复制会带上 `tr_session=` 与分号）。
func normalizeSession(pasted string) (string, error) {
	sess := strings.TrimSpace(pasted)
	if sess == "" {
		return "", errors.New("登录凭证为空 —— 请粘贴 sess_ 开头的值")
	}
	// 去掉可能的引号/分号（用户从 Cookie 面板复制常带这些）。
	sess = strings.Trim(sess, `"' ;`)
	// 分号分隔时只取第一段（有人会把整条 `tr_session=xxx; other=yyy` 粘进来）。
	if i := strings.IndexByte(sess, ';'); i >= 0 {
		sess = strings.TrimSpace(sess[:i])
	}
	// 带 `tr_session=` 前缀时剥掉（Cookie 面板复制常见）。
	if v, ok := strings.CutPrefix(sess, "tr_session="); ok {
		sess = strings.TrimSpace(v)
	}
	if !strings.HasPrefix(sess, "sess_") {
		return "", fmt.Errorf("这不像登录凭证（应以 sess_ 开头，得到 %q）—— "+
			"如果你手上只有一把 API Key（sk_…），请改用「API Key」那一栏", truncateForMsg(sess))
	}
	return sess, nil
}

// importViaSession 形态二：用登录凭证校准该账号下的 key，并写入 session 以启用余额。
//
// # 这条路径能做什么、不能做什么（都是实测结论）
//
// 上游 `/api/api-keys` **只下发脱敏值**（`maskedKey`）与**前缀**
// （`keyPrefix`，如 `sk_tr_EXAMP1`），完整 key 仅在上游 UI 创建它的那一次
// 响应里出现一次（上游 UI 原文：「列表只展示脱敏 Key，不展示完整 Key
// 或系统前缀字段」）。所以"贴 sess_ 就把完整 key 拉进来"**在上游能力上
// 不存在** —— 那是改不掉的事实（详见包注释与 session.go）。
//
// 但它能做一件很有价值的事：用 `keyPrefix` 把登录凭证**关联到已有的 key**。
// 池里已有完整 key（用户之前用「API Key」栏导入过），前缀能对上就是同一个
// 账号。关联之后：
//
//	该账号的昵称/手机尾号被刷新（如实回显）
//	**Session 落到该凭证上** → 额度列从此能显示实时余额（见 quota_ext.go）
//
// 这是"有登录凭证的导入登录凭证来查看余额"这条诉求的**可实现形式**：
// 不必代开新 key（那会占配额），只要把 session 挂到已有的那把 key 上。
//
// # 对不上的情况
//
// 账号下的 key 若一把都没在池里，就**不导入** —— 因为没有完整 key 可用，
// 造一条只有 session 的凭证是没意义的（它连推理都发不出去）。
// 这时给出一条说明，指出该怎么补。
func (p *Provider) importViaSession(ctx context.Context, pasted string) ([]gateway.ImportedCredential, error) {
	sess, err := normalizeSession(pasted)
	if err != nil {
		return nil, err
	}
	ident, err := fetchIdentity(ctx, sess)
	if err != nil {
		return nil, err
	}
	bal, balErr := fetchBalance(ctx, sess)
	// ⚠ 这里的错误**不能吞**（改造前是 `entries, _ :=`）：
	// 吞掉之后，"上游 Key 列表读失败"会掉进下面"一把都没对上"那条说明里，
	// 而那句话说的是"凭证有效、只是没有能配对的 key" —— 用户会跑去补 key，
	// 实际该做的是等一会儿重试（上游 5xx / 429）。
	entries, listErr := fetchAPIKeyEntries(ctx, sess)

	// 上游形状若变了、真的开始下发完整 key，这里就自动能用（不必再改代码）。
	if usable := filterUsableKeys(entries); len(usable) > 0 {
		return p.credentialsFromKeys(usable, ident, bal, balErr, sess)
	}

	// 用前缀把 session 挂到池里已有的 key 上。
	local, _ := LoadDir(p.effectiveDir(""))
	if len(local) > 0 && len(entries) > 0 {
		out := p.attachSessionToExisting(local, entries, ident, bal, sess)
		if len(out) > 0 {
			return out, nil
		}
	}

	// 一把也对不上：如实说明，并给出可行动的出路。
	//
	// ⚠ 措辞纪律：「登录凭证有效」是 /api/me 与 /api/usage-summary 的成功，
	// **不是**「导入完成」。改造前这句话开头就是"凭证有效"，后面跟着
	// "无法凭空造出可用凭证"，用户读成"有效但拿不到东西"—— 于是来问
	// "为什么显示有效却没有内容"。所以这里第一句就把口径说清：
	// 这一栏**本来就不新增账号**，它是给已有 key 挂余额用的。
	desc := "登录凭证有效，但这一栏不新增账号。\n" +
		"  账号：" + firstNonEmpty(ident.Label, "未知") +
		firstNonEmpty("\n  手机尾号："+ident.PhoneTail, "")
	if balErr == nil {
		desc += fmt.Sprintf("\n  余额：%.2f %s", bal.Available, bal.Currency)
		if bal.Expiring > 0 {
			desc += fmt.Sprintf("（其中 %.2f 即将过期）", bal.Expiring)
		}
	}
	desc += "\n\n为什么：上游只下发脱敏 key（没有完整值），所以贴 sess_ 换不出可用的 sk_。\n" +
		"这一栏的作用是：把登录凭证挂到已用「API Key」栏导进池子的那把 key 上，\n" +
		"使「额度」列能读到实时余额。"
	if listErr != nil {
		desc += "\n\n✗ 这次读上游 Key 列表失败：" + listErr.Error() +
			"\n  所以现在分不清是「对不上」还是「读不到」。请稍后在同一栏重试一次；" +
			"\n  若反复失败，说明上游管理面在拒这把登录凭证（与 key 无关，key 照常能推理）。"
	} else {
		desc += "\n\n当前账号下的 key 与池里的凭证一把都没对上，二选一：\n" +
			"  1. 到 tokenrhythm.studio 的 API Key 页复制完整 sk_，用「API Key」栏导入，再回到这一栏贴登录凭证以启用余额；\n" +
			"  2. 用「登录凭证（代开一把 Key 并入库）」栏，由网关替你在上游新建一把并直接入库。"
	}
	// 用 ImportNotice 而不是普通 error：这是一条**内容为真**的结论，
	// 不是"你粘错了"。admin 层会原样展示它，不套「导入内容无法解析：」。
	return nil, gateway.NewImportNotice(desc)
}

// attachSessionToExisting 把登录凭证挂到池里**已存在**的 key 上（按前缀配对）。
//
// # 配对判据：上游的 keyPrefix 是本地完整 key 的前缀
//
// 实测（2026-10）：
//
//	上游 nai     keyPrefix = "sk_tr_EXAMP1"
//	本地 e127caa6a0fb1d05 api_key = "sk_tr_EXAMP1_FAKE_FULL_KEY_FOR_TESTS"
//	                                        ^^^^^^^^^^^ 精确匹配
//
// 用**前缀**而不是名字配对：名字是用户可改的展示值（两把可以同名，
// 实测就有两把叫 omni2api），而前缀来自 key 本身、唯一且不随改名漂移。
//
// 只返回**配对成功**的那些凭证 —— 没配上的不写盘，避免制造
// "有 session 但发不出请求"的半残凭证（它连推理都做不到）。
func (p *Provider) attachSessionToExisting(local []*Auth, entries []apiKey, ident sessionIdentity, bal Balance, sess string) []gateway.ImportedCredential {
	out := make([]gateway.ImportedCredential, 0, len(local))
	seen := make(map[string]bool, len(local))

	for _, a := range local {
		if a == nil || a.UID == "" {
			continue
		}
		if matchingPrefix(a.APIKey, entries) == "" {
			continue // 这把 key 不在该登录凭证的账号下
		}
		if seen[a.UID] {
			continue
		}
		seen[a.UID] = true

		// 刷新可读到的展示事实（都是上游权威值，不猜）。
		//
		// 昵称只在"还是自动派生值"时才覆盖：用户自己打的标签
		//（`<key> <标签>` 那种）比账号名更贴近他的用法，不该被冲掉。
		//
		// ⚠ 改造前本函数**没有**写昵称，而上面 importViaSession 的注释
		// 声称"该账号的昵称/手机尾号被刷新"—— 注释与实现分叉的后果是：
		// 用登录凭证导入之后，表格「昵称」列仍然显示 uid 前 8 位（如 4a632f30），
		// 而我们已经知道它叫什么。用户因此对不上"这是谁的号"。
		if want := strings.TrimSpace(ident.Label); want != "" &&
			(a.Nickname == "" || a.Nickname == ShortUID(a.UID)) {
			a.Nickname = want
		}
		if ident.PhoneTail != "" {
			a.PhoneTail = ident.PhoneTail
		}
		if bal.Available > 0 || bal.Total > 0 {
			a.Balance = bal.Available
			if a.Balance == 0 {
				a.Balance = bal.Total
			}
		}
		// 本次导入的核心动作：把登录凭证留在凭证上。
		// 额度列（QuotaExt → fetchBalance）从此有东西可读。
		a.Session = strings.TrimSpace(sess)

		c, err := credentialOf(a, importKindSession)
		if err != nil {
			continue
		}
		// 回执只带脱敏标识 —— 完整登录态绝不能下发到前端。
		c.DerivedFrom = shortSession(sess)
		out = append(out, c)
	}
	return out
}

// matchingPrefix 找出本地完整 key 在上游列表里对应的那条（返回其前缀，无匹配返回空）。
//
// 比对用 `HasPrefix(本地key, 上游keyPrefix)`：上游给的是**前缀**，
// 而本地持有完整值 —— 方向不能反（拿短的去 HasPrefix 长的永远 false）。
func matchingPrefix(apiKey string, entries []apiKey) string {
	k := strings.TrimSpace(apiKey)
	if k == "" {
		return ""
	}
	for _, e := range entries {
		p := strings.TrimSpace(e.KeyPrefix)
		if p == "" {
			continue
		}
		if strings.HasPrefix(k, p) {
			return p
		}
	}
	return ""
}

// importViaSessionCreate 形态三（**写上游**）：用登录凭证代开一把新 key 并入库。
//
// # 这是本包唯一的写上游动作
//
// 会真实地在该账号里新增一把 API Key、占用其配额（上游上限实测 10 把）。
// 之所以独立成一个形态而不是 session 的自动分支，就是要让"只读"与"写"
// 在界面上是两个框 —— 见 importKindSessionCreate 注释。
//
// # 幂等：靠名字判重，不靠 key 值
//
// ⚠ 这里踩过一次：第一版靠"该账号下有没有完整 key"判重，而完整 key
// 上游**永不下发**（列表只有 maskedKey），于是判重永远不成立、
// 每调一次就在用户账号里多留一把 key（实测连调两次就有了两把同名 key）。
//
// 现在按**名字**判重（列表接口的 `name` 是有值的），名字已被占用时
// 追加序号。这样重复调用最多是多一把被标注过的 key，而不是静默堆积。
func (p *Provider) importViaSessionCreate(ctx context.Context, pasted string) ([]gateway.ImportedCredential, error) {
	sess, err := normalizeSession(pasted)
	if err != nil {
		return nil, err
	}
	ident, err := fetchIdentity(ctx, sess)
	if err != nil {
		return nil, err
	}
	bal, balErr := fetchBalance(ctx, sess)

	// 上游哪天开始下发完整 key，这条路径就不必创建（直接收下现成的）。
	// 用"不过滤"的版本拿全量条目，才能同时看到名字。
	entries, _ := fetchAPIKeyEntries(ctx, sess)
	if usable := filterUsableKeys(entries); len(usable) > 0 {
		return p.credentialsFromKeys(usable, ident, bal, balErr, sess)
	}

	name := pickCreatedKeyName(entries, createdKeyBaseName)
	created, err := createKey(ctx, sess, name)
	if err != nil {
		return nil, err
	}

	a := NewAuth(created.Key, firstNonEmpty(created.Name, ident.Label, name))
	a.PhoneTail = ident.PhoneTail
	if balErr == nil {
		a.Balance = bal.Available
		if a.Balance == 0 && bal.Total > 0 {
			a.Balance = bal.Total
		}
	}
	c, err := credentialOf(a, importKindSessionCreate)
	if err != nil {
		// 落盘序列化失败时，上游那把 key 已经创建出来了 —— 必须告诉用户
		// 它叫什么名字，否则他在上游会看到一把来路不明的 key。
		return nil, fmt.Errorf("上游已创建 key（名称 %s），但本地落盘失败：%w —— "+
			"请到 tokenrhythm.studio 的 API Key 页手动复制它，或删除它后重试", created.Name, err)
	}
	c.DerivedFrom = shortSession(sess)
	return []gateway.ImportedCredential{c}, nil
}

// createdKeyBaseName 网关代开 key 时的基础名（用户在上游能看到，要可辨识）。
const createdKeyBaseName = "omni2api"

// pickCreatedKeyName 在基础名已被占用时挑一个没被占用的名字。
//
// 上游允许同名 key（实测两把都叫 omni2api 也能并存），但重名会让用户
// 在上游列表里分不清哪把是自己建的 —— 所以这里主动避开。
func pickCreatedKeyName(existing []apiKey, base string) string {
	used := make(map[string]bool, len(existing))
	for _, k := range existing {
		used[strings.TrimSpace(k.Name)] = true
	}
	if !used[base] {
		return base
	}
	for i := 2; i <= 100; i++ {
		cand := fmt.Sprintf("%s-%d", base, i)
		if !used[cand] {
			return cand
		}
	}
	return base
}

// credentialsFromKeys 把几把 key 转成可落盘凭证（两个 session 形态共用）。
//
// 目前上游不会给完整 key，所以这条路径在真实环境下取不到值 ——
// 留着它是为了"上游哪天开始下发完整 key"时能立刻工作（不必改代码）。
func (p *Provider) credentialsFromKeys(keys []apiKey, ident sessionIdentity, bal Balance, balErr error, sess string) ([]gateway.ImportedCredential, error) {
	out := make([]gateway.ImportedCredential, 0, len(keys))
	seen := make(map[string]bool, len(keys))
	for _, k := range keys {
		a := NewAuth(k.Key, firstNonEmpty(k.Name, ident.Label))
		a.PhoneTail = ident.PhoneTail
		if balErr == nil {
			a.Balance = bal.Available
			if a.Balance == 0 && bal.Total > 0 {
				a.Balance = bal.Total
			}
		}
		if seen[a.UID] {
			continue
		}
		seen[a.UID] = true
		c, err := credentialOf(a, importKindSession)
		if err != nil {
			return nil, err
		}
		c.DerivedFrom = shortSession(sess)
		out = append(out, c)
	}
	return out, nil
}

// credentialOf 把一份 Auth 序列化成可落盘凭证（两个形态共用）。
func credentialOf(a *Auth, format string) (gateway.ImportedCredential, error) {
	raw, err := MarshalAuthFile(a)
	if err != nil {
		return gateway.ImportedCredential{}, fmt.Errorf("序列化凭证失败（key %s…）: %w", ShortUID(a.UID), err)
	}
	return gateway.ImportedCredential{
		FileName: AuthFileName(a),
		Raw:      raw,
		UID:      a.UID,
		Nickname: a.Nickname,
		Format:   format,
	}, nil
}

// shortSession 取登录凭证的**脱敏标识**（前 9 位），用于回执里的"来自哪"。
//
// 只取前缀：回执会下发到前端，完整登录态绝不能出现在那里。
// 但 9 位足够用户区分"我是用哪个浏览器的会话导入的"（多个账号时有用）。
func shortSession(sess string) string {
	s := strings.TrimSpace(sess)
	if len(s) <= 9 {
		return "sess_…"
	}
	return s[:9] + "…"
}

// truncateForMsg 截断用户输入用于错误信息（避免把一整段凭证回显出来）。
func truncateForMsg(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12] + "…"
}

// ---- 形态一：裸 key / JSON 的解析（与改造前相同）----

// importItem 一段粘贴里解析出的一条待导入凭证（key + 可选展示名）。
type importItem struct {
	key   string
	label string
}

// jsonImportItem JSON 形态的条目。字段名取常见的几种写法，
// 这样用户从别的工具导出（`{"apiKey":…}`、`{"token":…}`）也能直接贴。
type jsonImportItem struct {
	Key         string `json:"key"`
	APIKey      string `json:"api_key"`
	APIKeyAlt   string `json:"apikey"`
	APIKeyCamel string `json:"apiKey"`
	Token       string `json:"token"`
	Name        string `json:"name"`
	Nickname    string `json:"nickname"`
	Label       string `json:"label"`
	Note        string `json:"note"`
}

// parseImportItems 把粘贴文本解析成条目（JSON 形态优先，否则按行解析）。
func parseImportItems(pasted string) ([]importItem, error) {
	trim := strings.TrimSpace(pasted)
	if trim == "" {
		return nil, errors.New("请求体为空 —— 请粘贴 API Key（每行一把）或 JSON 数组")
	}
	if trim[0] == '{' || trim[0] == '[' {
		return parseJSONItems(trim)
	}
	return parseLineItems(trim)
}

// parseJSONItems 解析 JSON 形态（复用核心的切分器认数组/单对象两种外壳）。
func parseJSONItems(trim string) ([]importItem, error) {
	raws, err := gateway.SplitAccountImportItems(trim)
	if err != nil {
		return nil, err
	}
	out := make([]importItem, 0, len(raws))
	for i, raw := range raws {
		var it jsonImportItem
		if err := json.Unmarshal([]byte(raw), &it); err != nil {
			return nil, fmt.Errorf("第 %d 条不是合法对象: %w", i+1, err)
		}
		key := firstNonEmpty(it.Key, it.APIKey, it.APIKeyAlt, it.APIKeyCamel, it.Token)
		if key == "" {
			return nil, fmt.Errorf("第 %d 条缺少 key（可用字段: key / api_key / apiKey / token）", i+1)
		}
		label := firstNonEmpty(it.Name, it.Nickname, it.Label, it.Note)
		normalized, err := validateKey(key, fmt.Sprintf("第 %d 条", i+1))
		if err != nil {
			return nil, err
		}
		out = append(out, importItem{key: normalized, label: strings.TrimSpace(label)})
	}
	return out, nil
}

// parseLineItems 按行解析裸 key（主用法）。
//
// 行语法：
//
//	<key>                  仅 key
//	<key> <标签>           空格或逗号分隔的标签
//	<key> # 备注           注释被丢弃
//	<空行> / # 注释行       跳过
func parseLineItems(trim string) ([]importItem, error) {
	out := make([]importItem, 0, 8)
	for n, line := range strings.Split(trim, "\n") {
		if idx := strings.IndexByte(line, '#'); idx >= 0 {
			line = line[:idx]
		}
		line = strings.ReplaceAll(line, ",", " ")
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		key, err := validateKey(fields[0], fmt.Sprintf("第 %d 行", n+1))
		if err != nil {
			return nil, err
		}
		out = append(out, importItem{
			key:   key,
			label: strings.Join(fields[1:], " "),
		})
	}
	return out, nil
}

// validateKey 校验并归一化一把 key。
//
// ⚠ `sess_` 前缀在这里被**拒绝并指向另一个输入框** —— 这是用户最容易犯的错：
// 两个值都在 Cookie/用户中心里能拿到，粘错框的后果是一个永远 401 的"账号"。
// 错误信息必须同时说明"这不是 key"和"该贴到哪个框"。
func validateKey(key, where string) (string, error) {
	k := strings.TrimSpace(key)
	switch {
	case k == "":
		return "", fmt.Errorf("%s 是空的", where)
	case strings.HasPrefix(k, "sess_"):
		return "", fmt.Errorf("%s 看起来是登录凭证（sess_…），不是 API Key —— "+
			"请把粘到「登录凭证」那一栏（它会自动列出该账号下的全部 key）；"+
			"本栏要的是 sk_ 开头的 API Key", where)
	case len(k) < 16:
		return "", fmt.Errorf("%s 太短（%d 字符），不像是一把 key: %q", where, len(k), k)
	case strings.ContainsAny(k, " \t\"'"):
		return "", fmt.Errorf("%s 含引号或内部空白，无法当作单把 key: %q", where, k)
	}
	return k, nil
}

// firstNonEmpty 返回第一个非空（去空白后）的值。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// compile-time: Provider 实现批量导入。
var _ gateway.AccountImportExt = (*Provider)(nil)
