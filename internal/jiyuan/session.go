// session.go 基元律动的**登录凭证（tr_session）**能力：列 Key、读余额。
//
// # 这是用户要的第二条导入路径
//
// 用户原话：「将批量导入分为 key 导入和登录凭证（sess_xxx）导入，
// 这样有登录凭证的导入登录凭证来查看 key，没有的就只需要填个 key 就行了」。
//
// 也就是说，同一个上游有两种上号方式：
//
//	只有 key      → 直接贴 key（最简单，但读不到余额）
//	有登录凭证    → 贴 sess_，网关替你把该账号下的**全部 key** 拉出来入库，
//	                并顺带读到余额（余额只认 session，不认 key）
//
// # 两个面的鉴权差异（实测，2026-10）
//
//	调用面 /v1/*   只认 `Authorization: Bearer sk_…`
//	管理面 /api/*  只认 `Cookie: tr_session=sess_…`（**key 打过去是 401**）
//
// 所以余额与 Key 列表**只能**靠 session 拿 —— 这就是为什么
// 「有登录凭证时顺带读余额」不是锦上添花，而是唯一途径。
package jiyuan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/NaiZi899/omni2api/internal/gateway"
)

// mgmtBase 管理面基址（`/api/*` 挂在这个域名下，与 /v1 同域）。
//
// ⚠ 这是 `var` 而不是 `const`，**唯一**理由是让包内测试能把它指向
// httptest 服务器。原因值得写下来：
//
// 本文件最初把业务字段读在 JSON **顶层**（真值在信封的 `data` 里），
// 这个缺陷上线了 —— 根本原因是解析逻辑长在 `fetch*` 里、而 `fetch*`
// 又要打真实网络，于是**没有任何测试能覆盖它**。变成 `var` 之后，
// session_flow_test.go 能用本地假上游跑完整流程（信封 + POST + 重名检查），
// 这类"字段读错层级"的问题在下一次提交时就会被拦下。
//
// 生产代码**从不修改**它；测试也不并行跑，因此不存在数据竞争。
var mgmtBase = "https://tokenrhythm.studio"

// sessionTimeout 管理面单次请求上限（比推理短很多：这些是给人点的短操作）。
const sessionTimeout = 25 * time.Second

// mgmtEnvelope 管理面 `/api/*` 的**统一外层信封**（实测，2026-10）。
//
//	{"code":0,"message":"ok","data":{…},"traceId":"trace_…"}
//
// `code` 是**混合类型**：成功是数字 0，失败是字符串枚举
// （`"UNAUTHORIZED"` / `"NOT_FOUND"` / `"INSUFFICIENT_BALANCE"`）。
// 所以判据只能落在它的**字面形态**上，不能 unmarshal 成 int 或 string
// —— 两种写法各有一半回执解不出来。
//
// ⚠ 这个信封是 `/api/*` 与 `/v1/*` 的**分界**。`/v1/*` 是标准 OpenAI 形状
// （错误在 `error` 对象、成功体直接在顶层），**不走**这一层；把信封套到
// /v1 上会把成功回执解成空。
//
// # 为什么必须剥这一层
//
// 真正的业务字段全在 `data` 里：账号名 `data.name`、尾号 `data.phoneMasked`、
// 余额 `data.balanceCny`。直接在顶层取会**静默取到零值**（不报错），
// 表现为"凭证明明有效，界面却说没有 key、余额 0"—— 这条踩过，
// 见 sessionParse 的注释。
// # 判据为什么是"顶层有 code"
//
// 只认"有 `data` 就剥"是不行的：`/v1/*` 的**标准 OpenAI 回执**里也有
// 一个 `data` 字段（`{"object":"list","data":[…]}`），一刀切会把它的外壳
// 剥掉、连 `object` 一起丢。`code` 才是本信封的定义字段 ——
// 成功是数字 0、失败是字符串枚举，两者都存在。
func unwrapMgmtEnvelope(body []byte) json.RawMessage {
	trim := bytes.TrimSpace(body)
	if len(trim) == 0 || trim[0] != '{' {
		return body
	}
	var env struct {
		Code json.RawMessage `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(trim, &env); err != nil {
		return body
	}
	// 顶层没有 code → 这不是本信封（可能是 /v1 的 OpenAI 形状），原样返回。
	if len(bytes.TrimSpace(env.Code)) == 0 {
		return body
	}
	// data 缺席/为 null → 信封里没有业务体，原样返回让调用方按原形状解。
	if len(bytes.TrimSpace(env.Data)) == 0 || string(bytes.TrimSpace(env.Data)) == "null" {
		return body
	}
	return env.Data
}

// apiKey 上游 Key 列表里的一条（实测字段名，见 apiKeyList）。
//
// 上游还会回 status / created_at / last_used_at 等 —— 本包只取
// "能落盘 + 能展示"所需要的最小集，其余字段不抄（抄了也没人消费，
// 反而会随上游改版而过期）。
type apiKey struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Key    string `json:"key"`
	Status string `json:"status"`
	// KeyPrefix 上游给的**未脱敏前缀**（如 `sk_tr_EXAMP1`）。
	//
	// # 这是本包唯一能把登录凭证与已存在的 key 关联起来的字段
	//
	// 上游列表只下发 `maskedKey`（带 `****`），那个值无法与任何东西配对。
	// 而 `keyPrefix` 是真实前缀 —— 本地持有完整 key 的账号可以用
	// `strings.HasPrefix(本地key, keyPrefix)` 精确对上。
	// 有了这条配对，才能把 session 挂到已有的 key 上（见 import.go 的
	// attachSessionToExisting），从而让额度列显示实时余额 ——
	// **不必**代开新 key（那会占用户账号的 key 配额）。
	KeyPrefix string `json:"keyPrefix"`
}

// apiKeyList `GET /api/api-keys` 的响应体。
//
// ⚠ 响应外层形状（裸数组 / {data:[…]} / {list:[…]}）在实测中**需要确认**：
// 上游对这条接口未给出文档。这里做三种容错，理由见 parseAPIKeyList ——
// 猜错形状的代价是"凭证有效却导入 0 个"，而那会被误判成"session 过期"。
type apiKeyList struct {
	Data  []apiKey `json:"data"`
	List  []apiKey `json:"list"`
	Items []apiKey `json:"items"`
}

// sessionIdentity 一份登录凭证指向的账号（脱敏后可下发）。
type sessionIdentity struct {
	// Name 账号名（上游返回，可为空）。
	Name string
	// PhoneTail 手机号后四位（脱敏后的展示用；空 = 上游没给）。
	PhoneTail string
	// Label 用于落盘昵称与回执的短标识。
	Label string
}

// fetchIdentity 用登录凭证读账号信息（`GET /api/me`）。
func fetchIdentity(ctx context.Context, sess string) (sessionIdentity, error) {
	body, err := sessionGet(ctx, sess, "/api/me")
	if err != nil {
		return sessionIdentity{}, err
	}
	return sessionParseIdentity(body)
}

// sessionParseIdentity 解析 `/api/me` 回执（纯函数，可离线测）。
//
// 真实回执（2026-10 实测，已脱敏）：
//
//	{"code":0,"message":"ok","data":{"id":"…","name":"demo-user",
//	 "phoneMasked":"138****8000","status":"active",…}}
//
// ⚠ 上游给的是 **`phoneMasked`（已脱敏）**，不是 `phone` —— 所以尾号取的是
// 掩码串的最后 4 位（`"138****8000"` → `"8000"`）。真手机号上游不下发。
func sessionParseIdentity(body []byte) (sessionIdentity, error) {
	inner := unwrapMgmtEnvelope(body)
	var me struct {
		Name        string `json:"name"`
		Phone       string `json:"phone"`
		PhoneMasked string `json:"phoneMasked"`
	}
	if err := json.Unmarshal(inner, &me); err != nil {
		return sessionIdentity{}, errors.New("jiyuan: 账号信息回执格式不对: " + err.Error())
	}
	id := sessionIdentity{Name: strings.TrimSpace(me.Name)}
	phone := firstNonEmpty(me.PhoneMasked, me.Phone)
	if len(phone) >= 4 {
		id.PhoneTail = phone[len(phone)-4:]
	}
	id.Label = firstNonEmpty(id.Name, "尾号"+id.PhoneTail)
	return id, nil
}

// fetchAPIKeys 用登录凭证列出该账号下的 API Key。
//
// ⚠ 上游对这条接口**只下发脱敏值**（`maskedKey`，如 `sk_tr_EX****AMPLE1`），
// 完整 key 只在上游 UI 创建它的那一次响应里出现一次。所以这里**取不到
// 可用的 key**（见 apiKey 的注释）。本函数目前只在识别账号场景下有值，
// 其返回值不进入账号池。
func fetchAPIKeys(ctx context.Context, sess string) ([]apiKey, error) {
	entries, err := fetchAPIKeyEntries(ctx, sess)
	if err != nil {
		return nil, err
	}
	return filterUsableKeys(entries), nil
}

// fetchAPIKeyEntries 取某账号的全量 Key 条目（**不做可用性过滤**）。
//
// 与 fetchAPIKeys 的差别：按上游现状（只下发 maskedKey）前者恒为空，
// 而后者仍能给出**名字** —— 代开前的重名检查依赖它。
func fetchAPIKeyEntries(ctx context.Context, sess string) ([]apiKey, error) {
	body, err := sessionGet(ctx, sess, "/api/api-keys")
	if err != nil {
		return nil, err
	}
	return parseAPIKeyEntries(body)
}

// parseAPIKeyList 解析 Key 列表并**只保留可用条目**（有 key 值且未停用）。
//
// ⚠ 按上游现状（只有 maskedKey）它会返回空 —— 那是正确的：
// 没有可用 key 就不该有"可用条目"。要看列表里**有哪些名字**（代开前的
// 重名检查要用），用 parseAPIKeyEntries。
func parseAPIKeyList(body []byte) ([]apiKey, error) {
	all, err := parseAPIKeyEntries(body)
	if err != nil {
		return nil, err
	}
	return filterUsableKeys(all), nil
}

// parseAPIKeyEntries 解析 Key 列表，**不做可用性过滤**（保留全部条目）。
//
// # 为什么需要"不过滤"的版本
//
// 上游只下发 maskedKey，所以任何条目都过不了"有完整 key"这一关。
// 但条目里的 **name 是有值的** —— 而代开前的重名检查必须看到它，
// 否则同一账号会被反复新建同名 key（实测踩过：连调两次就有了两把
// `omni2api`，白占配额）。
//
// 容错三种外层形状的说明见下。
func parseAPIKeyEntries(body []byte) ([]apiKey, error) {
	inner := unwrapMgmtEnvelope(body)
	trim := strings.TrimSpace(string(inner))
	if trim == "" {
		return nil, errors.New("jiyuan: Key 列表回执为空")
	}
	// 先试裸数组
	if trim[0] == '[' {
		var arr []apiKey
		if err := json.Unmarshal([]byte(trim), &arr); err == nil {
			return arr, nil
		}
	}
	// 再试三种信封
	var env apiKeyList
	if err := json.Unmarshal([]byte(trim), &env); err != nil {
		return nil, errors.New("jiyuan: Key 列表回执格式不对: " + err.Error())
	}
	for _, cand := range [][]apiKey{env.Data, env.List, env.Items} {
		if len(cand) > 0 {
			return cand, nil
		}
	}
	// 三种都空：可能确实没有 key，也可能是形状没猜到。
	// 返回空切片而不是错误 —— 调用方据此给出"该账号下没有可用的 key"，
	// 那比"回执格式不对"更接近事实（且用户能据此去建一把）。
	return nil, nil
}

// filterUsableKeys 只保留有 key 值且未被停用的条目。
//
// 上游的 status 取值实测有 enabled / disabled（与前端表格里的
// `t.status === 'enabled'` 判断一致）。停用的 key 拿到池子里必然 401，
// 收进来只会制造一个"看起来健康、实际永远失败"的账号。
func filterUsableKeys(in []apiKey) []apiKey {
	out := make([]apiKey, 0, len(in))
	for _, k := range in {
		key := strings.TrimSpace(k.Key)
		if key == "" {
			continue
		}
		// 只排除**明确说停用**的；status 缺失（空串）时收下 ——
		// 拿不准时收下比丢掉好：丢一个可用 key 是净损失，
		// 收一个停用 key 最多是池子里多一个会被 401 淘汰的号。
		if strings.EqualFold(strings.TrimSpace(k.Status), "disabled") {
			continue
		}
		out = append(out, k)
	}
	return out
}

// sessionGet 带登录凭证打一次管理面 GET。
func sessionGet(ctx context.Context, sess, path string) ([]byte, error) {
	return sessionDo(ctx, http.MethodGet, sess, path, nil)
}

// sessionPost 带登录凭证打一次管理面 POST（body 为 JSON 字节，可为 nil）。
func sessionPost(ctx context.Context, sess, path string, body []byte) ([]byte, error) {
	return sessionDo(ctx, http.MethodPost, sess, path, body)
}

// sessionDo 管理面请求的唯一实现（GET / POST 共用鉴权与错误判据）。
//
// # 鉴权：为什么同时带 Cookie 与 Authorization
//
// 上游两个面收凭证的方式不同（见包注释），而管理面**两者都认**：
//
//	Cookie: tr_session=sess_…        读接口用
//	Authorization: Bearer sess_…     写接口**必须**带，理由见下
//
// # CSRF：写接口为什么必须带 Authorization
//
// 上游对非 GET 请求做 CSRF 双提交校验（前端源码：cookie `tr_csrf` →
// header `X-CSRF-Token`），且**带 `Authorization` 头时整体跳过该校验**
// （前端源码原文：`if (n.has("Authorization") || n.has("X-CSRF-Token")) return !1`）。
//
// 实测（2026-10）：只带 Cookie POST `/api/api-keys` → 403 `CSRF_INVALID`；
// 加上 `Authorization: Bearer <sess_>` → 200 成功。所以这一行不是装饰，
// 而是写路径**能否成立**的前提。
//
// 注：这里不复刻 `tr_csrf` 双提交 —— 那需要先 GET 拿 cookie 再回填 header，
// 而 Authorization 这条已被上游明确认可、且会话本身就是 Bearer 形态的凭证。
//
// 错误分三类，都带**可行动**的中文提示：
//
//	401 → 登录凭证无效/过期（要重新复制）
//	403 → 凭证有效但权限不足（少见）
//	其他 → 原样带出上游 message
func sessionDo(ctx context.Context, method, sess, path string, body []byte) ([]byte, error) {
	sess = strings.TrimSpace(sess)
	if sess == "" {
		return nil, errors.New("jiyuan: 登录凭证为空")
	}
	ctx, cancel := context.WithTimeout(ctx, sessionTimeout)
	defer cancel()

	var rdr io.Reader
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, mgmtBase+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cookie", "tr_session="+sess)
	req.Header.Set("Authorization", "Bearer "+sess)
	req.Header.Set("User-Agent", "omni2api/jiyuan (+OpenAI-compatible gateway)")
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := (&http.Client{Timeout: sessionTimeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := readAllLimited(resp.Body, 4<<20)

	// 上游错误信封是**顶层 code**（不是 OpenAI 的 error 对象）——
	// 判据必须落在它上面，否则会把 401 当成成功（踩过一次）。
	//
	// ⚠ 成功回执也有 `code`（数字 0），所以这里不能只看"有没有 code"：
	// 必须**同时**要求 code 落在已知的失败枚举上，否则会把成功当失败。
	if e, ok := parseUpstreamError(string(respBody)); ok && isErrorCode(e.Code) {
		switch strings.ToUpper(strings.TrimSpace(e.Code)) {
		case "UNAUTHORIZED":
			return nil, errors.New("登录凭证无效或已过期 —— " +
				"请在浏览器登录 tokenrhythm.studio 后，F12 → Application → Cookies " +
				"→ 复制 tr_session 的值（sess_ 开头）")
		case "FORBIDDEN", "CSRF_INVALID":
			return nil, errors.New("登录凭证权限不足或请求被安全校验拦下（" + e.Code + "）—— " +
				firstNonEmpty(e.Message, "该账号可能被限制"))
		}
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errors.New("登录凭证无效或已过期（401）—— 请重新复制 tr_session")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errors.New("jiyuan: 管理接口返回 HTTP " + itoaStatus(resp.StatusCode) +
			firstNonEmpty("（"+string(respBody)+"）", ""))
	}
	return respBody, nil
}

// isErrorCode 判断信封里的 code 是不是**失败**枚举。
//
// 成功是数字 0（用 json.RawMessage 看得到原文 "0"），失败是字符串枚举。
// 这个函数专门用来把两者分开 —— 见 sessionDo 的注释。
func isErrorCode(code string) bool {
	c := strings.TrimSpace(code)
	// 先去引号再判形态：`json.RawMessage` 之外的调用方可能给带引号的原文，
	// 而 `"0"` 与 `0` 都是成功码 —— 顺序反了会把成功判成失败。
	c = strings.Trim(strings.TrimSpace(c), `"`)
	if c = strings.TrimSpace(c); c == "" {
		return false
	}
	// 数字形态（0 / -1 / 0.0）→ 成功，不是错误码。
	if c[0] == '-' || c[0] == '+' || (c[0] >= '0' && c[0] <= '9') {
		return false
	}
	return true
}

// ---- 代开一把 key（写上游账号，用户显式要求）----

// createdKey `POST /api/api-keys` 的成功回执（`data` 层）。
//
// ⚠ 这是**唯一**能看到完整 key 的时刻。上游 UI 原文：
// 「列表只展示脱敏 Key，不展示完整 Key 或系统前缀字段」，且创建弹窗提示
// 「离开或刷新页面后将无法再次查看」。列表接口（apiKey）只给 `maskedKey`，
// 所以这张回执必须**当场**取出 `key`，否则这把 key 就永久看不到了。
type createdKey struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Key       string `json:"key"`
	MaskedKey string `json:"maskedKey"`
	Status    string `json:"status"`
}

// createKey 用登录凭证在上游**新建**一把 API Key（写操作）。
//
// # 这是本包唯一的写上游动作
//
// 由用户显式选择触发（网关控制台选「代开一把」），不做任何静默调用。
// 它会真实地在该上游账号里增加一把凭证，并消耗账号的 key 数量配额
// （上游上限实测 dd=10，见 import.go）。
//
// name 为空时上游会自动生成一个名字。
func createKey(ctx context.Context, sess, name string) (createdKey, error) {
	payload, err := json.Marshal(map[string]string{"name": strings.TrimSpace(name)})
	if err != nil {
		return createdKey{}, err
	}
	body, err := sessionPost(ctx, sess, "/api/api-keys", payload)
	if err != nil {
		return createdKey{}, err
	}
	return sessionParseCreatedKey(body)
}

// sessionParseCreatedKey 解析创建回执（纯函数，可离线测）。
//
// 真实回执（2026-10 实测，key 已脱敏但形状逐字保留）：
//
//	{"code":0,"message":"ok","data":{"id":"…","name":"omni2api",
//	 "maskedKey":"sk_tr_FI****XTURE","keyPrefix":"sk_tr_FIXTUR",
//	 "status":"enabled","key":"sk_tr_FIXTUR0Z2Gbs_…"},"traceId":"…"}
//
// 注意 data 里**同时**有 `key`（完整）与 `maskedKey`（脱敏）。
// 必须取 `key` —— 取 maskedKey 会得到一个含 `****` 的假 key，
// 那种值进了账号池就是一个永远 401 的账号（比报错更糟：它看起来是成功的）。
func sessionParseCreatedKey(body []byte) (createdKey, error) {
	inner := unwrapMgmtEnvelope(body)
	var k createdKey
	if err := json.Unmarshal(inner, &k); err != nil {
		return createdKey{}, errors.New("jiyuan: 创建 Key 回执格式不对: " + err.Error())
	}
	k.Key = strings.TrimSpace(k.Key)
	if k.Key == "" {
		// data 在、但 key 缺失 —— 上游可能改了回执形状。
		// 这时**必须报错**：静默回退到 maskedKey 会落盘一把假 key。
		return createdKey{}, errors.New("jiyuan: 上游创建成功但回执里没有完整 key —— " +
			"完整 key 只在创建时下发，请到 tokenrhythm.studio 的 API Key 页手动复制后用「API Key」导入")
	}
	if strings.Contains(k.Key, "****") {
		// 万一上游把脱敏值填进了 key 字段（形状漂移），同样拦下。
		return createdKey{}, errors.New("jiyuan: 上游回执里的 key 是脱敏值（含 ****），不是可用凭证 —— " +
			"请到 tokenrhythm.studio 的 API Key 页手动复制后用「API Key」导入")
	}
	return k, nil
}

// ---- 余额（只有 session 能读）----

// Balance 账号余额（人民币）。零值 + ok=false 表示读不到。
type Balance struct {
	Total     float64
	Available float64
	Frozen    float64
	Expiring  float64
	Currency  string
}

// fetchBalance 用登录凭证读余额（`GET /api/usage-summary`）。
func fetchBalance(ctx context.Context, sess string) (Balance, error) {
	body, err := sessionGet(ctx, sess, "/api/usage-summary")
	if err != nil {
		return Balance{}, err
	}
	return sessionParseBalance(body)
}

// sessionParseBalance 解析 `/api/usage-summary` 回执（纯函数，可离线测）。
//
// 真实回执（2026-10 实测，已脱敏）：
//
//	{"code":0,"message":"ok","data":{"calls":0,…,
//	 "balanceCny":"99.75000000","frozenBalanceCny":"0.00000000",
//	 "availableBalanceCny":"99.75000000","expiringBalanceCny":"80.75000000",
//	 "currency":"CNY"},"traceId":"…"}
//
// ⚠ 金额上游返回的是**字符串**（如 "99.75000000"），不是数字 —— 实测确认。
// 直接 json.Unmarshal 到 float64 会失败，所以这里按 any 收再转（toFloat）。
//
// ⚠ 字段全在信封的 `data` 里（先剥信封）。在顶层读会拿到全零 ——
// 这正是"余额明明是 ¥99.75 却显示没有余额"的直接原因。
func sessionParseBalance(body []byte) (Balance, error) {
	inner := unwrapMgmtEnvelope(body)
	var raw map[string]any
	if err := json.Unmarshal(inner, &raw); err != nil {
		return Balance{}, errors.New("jiyuan: 余额回执格式不对: " + err.Error())
	}
	b := Balance{
		Total:     toFloat(raw["balanceCny"]),
		Available: toFloat(raw["availableBalanceCny"]),
		Frozen:    toFloat(raw["frozenBalanceCny"]),
		Expiring:  toFloat(raw["expiringBalanceCny"]),
		Currency:  strings.TrimSpace(toString(raw["currency"])),
	}
	if b.Currency == "" {
		b.Currency = "CNY"
	}
	return b, nil
}

// toFloat 把上游的"字符串数字"转成 float64（解不出返回 0）。
func toFloat(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0
		}
		var f float64
		if err := json.Unmarshal([]byte(s), &f); err == nil {
			return f
		}
	}
	return 0
}

// toString 宽松取字符串（数字也转成字符串）。
func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return itoaStatus(int(t))
	}
	return ""
}

// compile-time: Provider 实现凭证形态自报。
var _ gateway.CredentialKindReporter = (*Provider)(nil)
