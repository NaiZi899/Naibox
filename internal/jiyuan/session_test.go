// session_test.go 管理面（`/api/*`）回执解析的测试。
//
// # 为什么这个文件迟到了（写给下一个改它的人）
//
// `session.go` 上线时**没有任何测试** —— 因为它的解析逻辑直接长在
// `fetchIdentity/fetchBalance` 里，要测就得打真实网络。于是它带着一个
// 致命的形状假设进了生产：以为字段在 JSON **顶层**，而实际上游把业务字段
// 全包在 `{"code":0,"message":"ok","data":{…}}` 的 `data` 里。
//
// 后果是**静默零值**：`json.Unmarshal` 从顶层取不存在的字段不报错，
// 只是留个零值。用户贴了有效凭证（余额 ¥99.75、两个 key），界面却报
// "没有可用的 API Key"、余额 0 —— 而代码一路"成功"返回。
//
// 所以本次修复的纪律是：**把解析从传输里剥出来**（sessionParse* 三个纯函数），
// 再用**真实回执**做夹具。下面的 JSON 全部是 2026-10 从
// tokenrhythm.studio 实测抓来的（会话标识与 key 已脱敏，形状逐字保留）。
package jiyuan

import (
	"math"
	"strings"
	"testing"
)

// ---- 真实回执夹具（形状逐字保留，值已脱敏）----

// realMeBody `GET /api/me` 的实测回执。
const realMeBody = `{"code":0,"message":"ok","data":{"id":"11111111-1111-4111-8111-111111111111","analyticsIdentity":{"userRef":"11111111-1111-4111-8111-111111111111","environment":"production"},"name":"demo-user","emailMasked":null,"phoneMasked":"138****8000","status":"active","role":"user","joinedAt":"2026-10-05T15:52:41.217Z","features":{"manualBalanceCreditEnabled":false,"referralTestIdentityEnabled":false},"releaseCohort":null},"traceId":"trace_00000000-0000-4000-8000-000000000001"}`

// realUsageBody `GET /api/usage-summary` 的实测回执。
const realUsageBody = `{"code":0,"message":"ok","data":{"calls":0,"successCalls":0,"errorCalls":0,"abortedCalls":0,"tokenRequestCount":0,"imageRequestCount":0,"successfulImageCount":0,"inputTokens":0,"outputTokens":0,"costCny":"0.00000000","tokenCostCny":"0.00000000","imageCostCny":"0.00000000","balanceCny":"99.75000000","frozenBalanceCny":"0.00000000","availableBalanceCny":"99.75000000","expiringBalanceCny":"80.75000000","nextExpiryAt":"2026-11-05T15:53:05.007Z","currency":"CNY"},"traceId":"trace_00000000-0000-4000-8000-000000000002"}`

// realKeysBody `GET /api/api-keys` 的实测回执。
//
// ⚠ 注意条目里**只有 `maskedKey`**，没有 `key`。这是上游的既定行为
// （上游 UI 原文：「列表只展示脱敏 Key，不展示完整 Key 或系统前缀字段」），
// 见 TestParseAPIKeyListUpstreamSendsMaskedOnly。
const realKeysBody = `{"code":0,"message":"ok","data":[{"id":"22222222-2222-4222-8222-222222222221","name":"demo-a","maskedKey":"sk_tr_EX****AMPLE1","keyPrefix":"sk_tr_EXAMP1","status":"enabled","lastUsedAt":null,"createdAt":"2026-10-13T12:24:28.716Z"},{"id":"22222222-2222-4222-8222-222222222222","name":"demo-b","maskedKey":"sk_tr_SA****AMPLE2","keyPrefix":"sk_tr_SAMPLE","status":"enabled","lastUsedAt":null,"createdAt":"2026-10-13T11:17:34.608Z"}],"traceId":"trace_00000000-0000-4000-8000-000000000003"}`

// ---- 身份 ----

// TestSessionParseIdentityRealEnvelope 实测回执必须解出真实的账号名与尾号。
//
// 修复前这两个字段**都是空的**（顶层没有 name/phone），于是 Label 退化成
// 字符串 "尾号" —— 用户看到的报错里账号名是空白，无法确认是不是自己的号。
func TestSessionParseIdentityRealEnvelope(t *testing.T) {
	id, err := sessionParseIdentity([]byte(realMeBody))
	if err != nil {
		t.Fatalf("sessionParseIdentity: %v", err)
	}
	if id.Name != "demo-user" {
		t.Errorf("账号名应从 data.name 取出，得到 %q", id.Name)
	}
	// 上游给的是 phoneMasked（"138****8000"）—— 取末 4 位。
	if id.PhoneTail != "8000" {
		t.Errorf("尾号应取 phoneMasked 末 4 位，得到 %q", id.PhoneTail)
	}
	if id.Label != "demo-user" {
		t.Errorf("Label 应优先用账号名，得到 %q", id.Label)
	}
}

// TestSessionParseIdentityFallsBackToTail 没给名字时 Label 回落到尾号。
func TestSessionParseIdentityFallsBackToTail(t *testing.T) {
	id, err := sessionParseIdentity([]byte(`{"code":0,"message":"ok","data":{"name":"","phoneMasked":"138****6789"}}`))
	if err != nil {
		t.Fatalf("sessionParseIdentity: %v", err)
	}
	if id.Name != "" {
		t.Errorf("上游没给名字时 Name 应为空，得到 %q", id.Name)
	}
	if id.Label != "尾号6789" {
		t.Errorf("Label 应回落到尾号，得到 %q", id.Label)
	}
}

// TestSessionParseIdentityAcceptsPlainPhone 上游若给未脱敏的 phone，尾号取法同样成立。
func TestSessionParseIdentityAcceptsPlainPhone(t *testing.T) {
	id, err := sessionParseIdentity([]byte(`{"code":0,"message":"ok","data":{"phone":"13800138000"}}`))
	if err != nil {
		t.Fatalf("sessionParseIdentity: %v", err)
	}
	if id.PhoneTail != "8000" {
		t.Errorf("尾号应为 8000，得到 %q", id.PhoneTail)
	}
}

// TestSessionParseIdentityRejectsJunk 非 JSON 必须报格式错，不能静默给个空身份。
func TestSessionParseIdentityRejectsJunk(t *testing.T) {
	for _, body := range []string{`not json`, `[1,2,3]`, `"a string"`} {
		if _, err := sessionParseIdentity([]byte(body)); err == nil {
			t.Errorf("回执 %q 应报格式错", body)
		}
	}
}

// ---- 余额 ----

// TestSessionParseBalanceRealEnvelope 实测回执必须解出真实金额。
//
// 修复前 Total/Available 恒为 0（顶层读不到 balanceCny），
// 而 import.go 拿这个 0 写进凭证快照 —— 用户看到"余额 0"。
func TestSessionParseBalanceRealEnvelope(t *testing.T) {
	b, err := sessionParseBalance([]byte(realUsageBody))
	if err != nil {
		t.Fatalf("sessionParseBalance: %v", err)
	}
	// 金额上游返回的是字符串，必须按字符串解析（不是数字）。
	if math.Abs(b.Total-99.75) > 1e-9 {
		t.Errorf("总余额应为 99.75，得到 %v", b.Total)
	}
	if math.Abs(b.Available-99.75) > 1e-9 {
		t.Errorf("可用余额应为 99.75，得到 %v", b.Available)
	}
	if math.Abs(b.Frozen-0) > 1e-9 {
		t.Errorf("冻结余额应为 0，得到 %v", b.Frozen)
	}
	if math.Abs(b.Expiring-80.75) > 1e-9 {
		t.Errorf("即将过期余额应为 80.75，得到 %v", b.Expiring)
	}
	if b.Currency != "CNY" {
		t.Errorf("币种应为 CNY，得到 %q", b.Currency)
	}
}

// TestSessionParseBalanceCurrencyDefaults 上游不给 currency 时回落 CNY。
func TestSessionParseBalanceCurrencyDefaults(t *testing.T) {
	b, err := sessionParseBalance([]byte(`{"code":0,"message":"ok","data":{"balanceCny":"1.5"}}`))
	if err != nil {
		t.Fatalf("sessionParseBalance: %v", err)
	}
	if b.Currency != "CNY" {
		t.Errorf("币种应回落 CNY，得到 %q", b.Currency)
	}
	if math.Abs(b.Total-1.5) > 1e-9 {
		t.Errorf("总余额应为 1.5，得到 %v", b.Total)
	}
}

// TestSessionParseBalanceAcceptsNumbers 金额是数字（而非字符串）时也要能解。
//
// 上游当前给字符串，但这是"上游的实现细节"——把两种都吃下，
// 免得某天上游一改序列化方式，余额又静默变 0。
func TestSessionParseBalanceAcceptsNumbers(t *testing.T) {
	b, err := sessionParseBalance([]byte(`{"code":0,"message":"ok","data":{"balanceCny":12.5,"availableBalanceCny":12.5}}`))
	if err != nil {
		t.Fatalf("sessionParseBalance: %v", err)
	}
	if math.Abs(b.Total-12.5) > 1e-9 {
		t.Errorf("总余额应为 12.5，得到 %v", b.Total)
	}
}

// TestSessionParseBalanceRejectsJunk 非 JSON 必须报格式错。
func TestSessionParseBalanceRejectsJunk(t *testing.T) {
	if _, err := sessionParseBalance([]byte(`<html>502</html>`)); err == nil {
		t.Error("HTML 回执应报格式错，而不是给一个全零余额")
	}
}

// ---- 信封剥离 ----

// TestUnwrapMgmtEnvelope 信封剥离的边界：有 data 就剥，没有就原样返回。
func TestUnwrapMgmtEnvelope(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"标准信封", `{"code":0,"message":"ok","data":{"a":1}}`, `{"a":1}`},
		{"data 为 null", `{"code":0,"message":"ok","data":null}`, `{"code":0,"message":"ok","data":null}`},
		{"data 缺席", `{"code":0,"message":"ok"}`, `{"code":0,"message":"ok"}`},
		{"裸数组", `[{"a":1}]`, `[{"a":1}]`},
		{"非 JSON", `oops`, `oops`},
		{"空串", ``, ``},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := strings.TrimSpace(string(unwrapMgmtEnvelope([]byte(c.in))))
			if got != c.want {
				t.Errorf("unwrapMgmtEnvelope(%s) = %s，want %s", c.in, got, c.want)
			}
		})
	}
}

// TestUnwrapMgmtEnvelopeKeepsOpenAIShape /v1 面的标准 OpenAI 形状**不该**被剥。
//
// 这条钉住的是"信封只属于 /api/*"这条分界：
// /v1 的成功回执里也有个 `data` 字段（如 `{"object":"list","data":[…]}`），
// 若把信封逻辑误用到 /v1 上，会把它的 data 剥出来、丢掉 object 字段。
func TestUnwrapMgmtEnvelopeKeepsOpenAIShape(t *testing.T) {
	in := `{"object":"list","data":[{"id":"deepseek-flash"}]}`
	got := strings.TrimSpace(string(unwrapMgmtEnvelope([]byte(in))))
	if !strings.Contains(got, `"object":"list"`) {
		t.Errorf("OpenAI 形状不应被剥掉外壳，得到 %s", got)
	}
}

// ---- Key 列表（记录上游事实，不是我们的缺陷）----

// TestParseAPIKeyListUpstreamSendsMaskedOnly 钉住一个**上游限制**。
//
// 上游 `/api/api-keys` 只下发 `maskedKey`（如 `sk_tr_EX****AMPLE1`），
// 完整 key 只在上游 UI 创建它的那一次响应里出现一次。所以用 sess_ 导入
// **无法**把该账号下的 key 拉进网关 —— 这不是解析缺陷，是上游不提供。
//
// ⚠ 本测试的价值在于"记录事实"：哪天有人看到"贴了 sess_ 却没导入 key"
// 想当然地去改解析代码，这里会告诉他解析没问题、是上游不给。
// 若哪天上游开始下发完整 `key`，这条测试会失败 —— 那时才是改代码的信号。
func TestParseAPIKeyListUpstreamSendsMaskedOnly(t *testing.T) {
	keys, err := parseAPIKeyList([]byte(realKeysBody))
	if err != nil {
		t.Fatalf("parseAPIKeyList 不应报错（回执格式是合法的）: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("上游只给 maskedKey，应解析出 0 把可用 key，得到 %d：%+v", len(keys), keys)
	}
}

// TestParseAPIKeyListAcceptsRealKeys 形状解析本身是正确的：若上游给了完整 key 就能收下。
//
// 这条把"解析能力"与"上游是否给值"分开：解析逻辑没问题，
// 缺的只是上游的下发内容。
func TestParseAPIKeyListAcceptsRealKeys(t *testing.T) {
	body := `{"code":0,"message":"ok","data":[{"id":"a","name":"demo-a","key":"sk_tr_1a2b3c4d5e6f7a8b9c0d","status":"enabled"},{"id":"b","name":"old","key":"sk_tr_99887766554433221100","status":"disabled"}]}`
	keys, err := parseAPIKeyList([]byte(body))
	if err != nil {
		t.Fatalf("parseAPIKeyList: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("应只保留 enabled 的那把，得到 %d", len(keys))
	}
	if keys[0].Key != "sk_tr_1a2b3c4d5e6f7a8b9c0d" {
		t.Errorf("key 取值不对: %q", keys[0].Key)
	}
}

// TestParseAPIKeyListBareArrayStillWorks 裸数组形态（无信封）保持兼容。
func TestParseAPIKeyListBareArrayStillWorks(t *testing.T) {
	keys, err := parseAPIKeyList([]byte(`[{"key":"sk_tr_1a2b3c4d5e6f7a8b9c0d","status":"enabled"}]`))
	if err != nil {
		t.Fatalf("parseAPIKeyList: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("裸数组应能解析，得到 %d", len(keys))
	}
}

// ---- 代开一把（写上游）----

// realCreateBody `POST /api/api-keys` 的成功回执（实测，key 已脱敏但形状逐字保留）。
const realCreateBody = `{"code":0,"message":"ok","data":{"id":"33333333-3333-4333-8333-333333333333","name":"omni2api","maskedKey":"sk_tr_FI****XTURE","keyPrefix":"sk_tr_FIXTUR","status":"enabled","lastUsedAt":null,"createdAt":"2026-10-13T12:49:28.603Z","key":"sk_tr_FIXTUR0Z2Gbs_EXAMPLE_REPLACED"},"traceId":"trace_00000000-0000-4000-8000-000000000004"}`

// TestSessionParseCreatedKeyRealEnvelope 创建回执必须取出 `data.key`（完整值）。
//
// ⚠ 回执里同时有 `key`（完整）与 `maskedKey`（脱敏）。取错字段的后果
// 比报错更糟：一把含 `****` 的假 key 会流进账号池，表现得像一个
// "导入成功的账号"，然后每个请求 401。
func TestSessionParseCreatedKeyRealEnvelope(t *testing.T) {
	k, err := sessionParseCreatedKey([]byte(realCreateBody))
	if err != nil {
		t.Fatalf("sessionParseCreatedKey: %v", err)
	}
	if k.Key != "sk_tr_FIXTUR0Z2Gbs_EXAMPLE_REPLACED" {
		t.Errorf("必须取完整 key（data.key），得到 %q", k.Key)
	}
	if strings.Contains(k.Key, "****") {
		t.Errorf("取到了脱敏值 —— 那会落盘一把假 key: %q", k.Key)
	}
	if k.Name != "omni2api" {
		t.Errorf("名称应为 omni2api，得到 %q", k.Name)
	}
	if k.ID == "" {
		t.Error("id 缺失 —— 用户要拿它在界面上对应到上游那一行")
	}
}

// TestSessionParseCreatedKeyRejectsMasked 回执里 key 字段是脱敏值时必须**报错**。
//
// 静默回退到 maskedKey 会落盘一把假 key；报错至少让用户去手动复制。
func TestSessionParseCreatedKeyRejectsMasked(t *testing.T) {
	body := `{"code":0,"message":"ok","data":{"id":"x","name":"omni2api","key":"sk_tr_FI****XTURE"}}`
	if _, err := sessionParseCreatedKey([]byte(body)); err == nil {
		t.Fatal("脱敏值被当作可用 key 收下了 —— 那会是一个永远 401 的账号")
	}
}

// TestSessionParseCreatedKeyRejectsMissingKey key 字段缺席时必须报错。
//
// 上游改了回执形状时，宁可让用户看到"去手动复制"，也不要落盘一个空 key。
func TestSessionParseCreatedKeyRejectsMissingKey(t *testing.T) {
	body := `{"code":0,"message":"ok","data":{"id":"x","name":"omni2api","maskedKey":"sk_tr_FI****XTURE"}}`
	_, err := sessionParseCreatedKey([]byte(body))
	if err == nil {
		t.Fatal("key 缺席时应报错")
	}
	// 错误里要给出一条可走的路。
	if !strings.Contains(err.Error(), "API Key") {
		t.Errorf("错误信息应指向手动复制的办法，实际: %s", err)
	}
}

// TestParseAPIKeyEntriesKeepsNames 不过滤的版本必须能看到**名字**。
//
// 这是代开前的重名检查唯一的数据来源：按上游现状条目都过不了
// "有完整 key"这一关，若用过滤后的版本做检查，就会永远认为"名字没被占用"，
// 于是每调一次代开就在用户账号里多留一把同名 key（实测踩过）。
func TestParseAPIKeyEntriesKeepsNames(t *testing.T) {
	entries, err := parseAPIKeyEntries([]byte(realKeysBody))
	if err != nil {
		t.Fatalf("parseAPIKeyEntries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("应保留全部 2 条（不过滤），得到 %d", len(entries))
	}
	if entries[0].Name != "demo-a" || entries[1].Name != "demo-b" {
		t.Errorf("名字应被解出，得到 %q / %q", entries[0].Name, entries[1].Name)
	}
	// 同一份回执走"过滤"版本必须是空的 —— 两者差异就是本测试的价值。
	if usable, _ := parseAPIKeyList([]byte(realKeysBody)); len(usable) != 0 {
		t.Errorf("过滤版本应为 0 条（上游不给完整 key），得到 %d", len(usable))
	}
}

// TestPickCreatedKeyName 重名时追加序号，不覆盖也不重复。
func TestPickCreatedKeyName(t *testing.T) {
	cases := []struct {
		name     string
		existing []string
		want     string
	}{
		{"没人占用", nil, "omni2api"},
		{"别的名字", []string{"demo-a", "demo-b"}, "omni2api"},
		{"已被占用", []string{"demo-a", "omni2api"}, "omni2api-2"},
		{"连续占用", []string{"omni2api", "omni2api-2"}, "omni2api-3"},
		{"名字带空白", []string{"  omni2api  "}, "omni2api-2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			list := make([]apiKey, 0, len(c.existing))
			for _, n := range c.existing {
				list = append(list, apiKey{Name: n})
			}
			if got := pickCreatedKeyName(list, "omni2api"); got != c.want {
				t.Errorf("pickCreatedKeyName = %q，want %q", got, c.want)
			}
		})
	}
}

// ---- 登录凭证归一化 ----

// TestNormalizeSession 用户实际会粘贴的各种形态都要能归一化。
func TestNormalizeSession(t *testing.T) {
	const raw = "sess_jmgf8IQpthi_vqLi_8Axh2HCPDQhKVfIcW95YaaL_BA"
	ok := []struct {
		name string
		in   string
	}{
		{"裸值", raw},
		{"带引号", `"` + raw + `"`},
		{"带分号与后面的 cookie", raw + "; other=1"},
		{"带 tr_session= 前缀", "tr_session=" + raw},
		{"带前缀且带分号", "tr_session=" + raw + "; path=/"},
		{"前后空白", "  " + raw + "  "},
	}
	for _, c := range ok {
		t.Run(c.name, func(t *testing.T) {
			got, err := normalizeSession(c.in)
			if err != nil {
				t.Fatalf("应归一化成功: %v", err)
			}
			if got != raw {
				t.Errorf("归一化结果不对: %q", got)
			}
		})
	}

	bad := []struct{ name, in string }{
		{"空", ""},
		{"只有空格", "   "},
		{"key 形态", "sk_tr_1a2b3c4d5e6f7a8b9c0d"},
		{"随便一段", "hello world"},
	}
	for _, c := range bad {
		t.Run("拒绝_"+c.name, func(t *testing.T) {
			if _, err := normalizeSession(c.in); err == nil {
				t.Errorf("%q 应被拒绝", c.in)
			}
		})
	}
}

// TestIsErrorCode 区分信封里的成功码（数字 0）与失败码（字符串枚举）。
//
// sessionDo 用它决定要不要把回执当错误 —— 弄错的后果是**把成功当失败**，
// 而那会让"刚创建的 key"被判为失败（用户以为没建成，实际成了）。
func TestIsErrorCode(t *testing.T) {
	errors := []string{`"UNAUTHORIZED"`, `"CSRF_INVALID"`, `"NOT_FOUND"`, `"INSUFFICIENT_BALANCE"`}
	for _, c := range errors {
		if !isErrorCode(c) {
			t.Errorf("%s 应判为失败码", c)
		}
	}
	success := []string{`0`, `0.0`, `-1`, ``, `  `, `"0"`}
	for _, c := range success {
		if isErrorCode(c) {
			t.Errorf("%q 不该判为失败码（成功回执的 code 是数字 0）", c)
		}
	}
}
