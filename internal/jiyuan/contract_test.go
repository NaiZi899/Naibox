// contract_test.go 基元律动的契约测试与单元测试。
//
// 全部**离线可跑**（`Config.LiveModels` 零值 = 纯静态目录，不发网络）。
// 对真实上游的验证单独放在 TestAgainstRealKeys，用环境变量开启。
package jiyuan

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/NaiZi899/Naibox/internal/gateway"
)

// TestContract 跑 gateway 行为契约（ID 合法且稳定、Caps 含 CapChat、
// 不 panic、声明的能力真可用、流可 Close、ctx 取消被尊重）。
func TestContract(t *testing.T) {
	gateway.RunProviderContract(t, NewProvider)
}

// TestCapsDeclareTheFourWeUse 钉住能力位声明。
//
// 这条不是形式主义：多声明一个会让契约要求对应扩展点。以 CapQuotaProbe
// 为例，它同时要求 QuotaExt 与 AdminExt —— 只声明不实现会被
// `RunProviderContract` 挡下（inkstone 第一版就是这么被抓的）。
//
// ⚠ 本上游现在**声明** CapQuotaProbe（名额来源：quota_ext.go 的 QuotaExt
// 与 adminroute.go 的 AdminExt 都已落地）。这条用例的作用因此反转成
// 正向断言：少了它，界面上不会出现「额度」按钮，用户只能靠 curl 看余额。
func TestCapsDeclareTheFourWeUse(t *testing.T) {
	p := New(Config{})
	caps := p.Caps()
	for _, want := range []gateway.Capability{
		gateway.CapChat, gateway.CapModels, gateway.CapQuotaProbe, gateway.CapImport,
	} {
		if !caps.Has(want) {
			t.Errorf("Caps() 缺少 %v", want)
		}
	}
	for _, unwanted := range []gateway.Capability{
		gateway.CapCheckin, gateway.CapGrowth,
		gateway.CapTravel, gateway.CapWelfare, gateway.CapTasks, gateway.CapInvite,
	} {
		if caps.Has(unwanted) {
			t.Errorf("Caps() 多声明了 %v —— 契约会要求对应扩展点，而本上游没有那个能力", unwanted)
		}
	}
}

// TestDeriveUIDDeterministic 同一把 key 派生稳定 uid，不同 key 不同。
//
// 稳定性是池子的正确性前提：不稳定会让每次重载都多出一个"新账号"，
// 而旧的那个永远冷却不掉（幽灵条目）。
func TestDeriveUIDDeterministic(t *testing.T) {
	k1 := "sk_tr_1a2b3c4d5e6f7a8b9c0d"
	k2 := "sk_tr_99887766554433221100"

	a1 := NewAuth(k1, "")
	a2 := NewAuth(k1, "")
	if a1.UID == "" {
		t.Fatal("uid 为空")
	}
	if a1.UID != a2.UID {
		t.Fatalf("同一 key 派生 uid 不稳定: %q vs %q", a1.UID, a2.UID)
	}
	if a1.UID == NewAuth(k2, "").UID {
		t.Fatalf("不同 key 派生相同 uid: %q", a1.UID)
	}
	if len(a1.UID) != 16 {
		t.Errorf("uid 长度应为 16 位 hex，得到 %d: %q", len(a1.UID), a1.UID)
	}
	// 前后空白不该影响结果（用户从表格里复制常带空格）。
	if a1.UID != NewAuth("  "+k1+"\n", "").UID {
		t.Error("key 前后空白改变了 uid —— 会导致同一把 key 被当成两个账号")
	}
}

// TestNicknameDefaultsToShortUID 没给标签时昵称回落 uid 前缀。
func TestNicknameDefaultsToShortUID(t *testing.T) {
	a := NewAuth("sk_tr_1a2b3c4d5e6f7a8b9c0d", "")
	if a.Nickname == "" || len(a.Nickname) != 8 {
		t.Fatalf("缺省昵称应为 8 位 uid 前缀，得到 %q", a.Nickname)
	}
	b := NewAuth("sk_tr_1a2b3c4d5e6f7a8b9c0d", "账号A")
	if b.Nickname != "账号A" {
		t.Fatalf("给了标签就该用它，得到 %q", b.Nickname)
	}
}

// TestAuthFileRoundTrip 落盘 → 读回，字段不丢。
func TestAuthFileRoundTrip(t *testing.T) {
	a := NewAuth("sk_tr_1a2b3c4d5e6f7a8b9c0d", "账号A")
	raw, err := MarshalAuthFile(a)
	if err != nil {
		t.Fatalf("MarshalAuthFile: %v", err)
	}
	back, err := ParseCredential(raw)
	if err != nil {
		t.Fatalf("ParseCredential: %v", err)
	}
	if back.APIKey != a.APIKey || back.UID != a.UID || back.Nickname != a.Nickname {
		t.Fatalf("回读丢字段: %+v vs %+v", back, a)
	}
	if file := AuthFileName(a); file != "jiyuan-"+a.UID+".json" {
		t.Errorf("文件名不符约定: %q", file)
	}
}

// TestParseCredentialFillsMissingUID 手写一份只有 api_key 的文件也能被收下。
func TestParseCredentialFillsMissingUID(t *testing.T) {
	a, err := ParseCredential([]byte(`{"api_key":"sk_tr_1a2b3c4d5e6f7a8b9c0d"}`))
	if err != nil {
		t.Fatalf("ParseCredential: %v", err)
	}
	if a.UID != deriveUID("sk_tr_1a2b3c4d5e6f7a8b9c0d") {
		t.Errorf("uid 未按 key 补算: %q", a.UID)
	}
	if _, err := ParseCredential([]byte(`{}`)); err == nil {
		t.Error("缺 api_key 应当报错")
	}
}

// TestClassify 钉住错误映射表。
//
// 这张表的每一条都对应一个具体的池子动作（冷却/禁用/只换号），
// 所以它是**行为**而不是格式 —— 改错一条会让"余额不足的号"
// 永远留在轮换里。
func TestClassify(t *testing.T) {
	p := New(Config{})
	cases := []struct {
		name   string
		status int
		body   string
		want   gateway.ErrorKind
	}{
		{
			name:   "余额不足（上游 code）",
			status: 402,
			body:   `{"code":"INSUFFICIENT_BALANCE","message":"余额不足","traceId":"t"}`,
			want:   gateway.ErrKindHardCredit,
		},
		{
			name:   "余额不足（只有中文提示）",
			status: 402,
			body:   `{"message":"余额不足"}`,
			want:   gateway.ErrKindHardCredit,
		},
		{
			name:   "key 失效（上游 code）",
			status: 401,
			body:   `{"code":"UNAUTHORIZED","message":"未认证或登录已过期","traceId":"t"}`,
			want:   gateway.ErrKindSessionDead,
		},
		{
			name:   "403 也当 key 不行（本上游只有一种凭证）",
			status: 403,
			body:   `{"code":"FORBIDDEN"}`,
			want:   gateway.ErrKindSessionDead,
		},
		{
			name:   "限流",
			status: 429,
			body:   `{"code":"RATE_LIMIT","message":"too many requests"}`,
			want:   gateway.ErrKindSoftRate,
		},
		{
			name:   "404 路由不存在",
			status: 404,
			body:   `{"code":"NOT_FOUND","message":"接口不存在"}`,
			want:   gateway.ErrKindNotFound,
		},
		{
			name:   "上游 5xx",
			status: 502,
			body:   `bad gateway`,
			want:   gateway.ErrKindServer,
		},
		{
			name:   "其他 4xx",
			status: 400,
			body:   `{"code":"BAD_REQUEST"}`,
			want:   gateway.ErrKindClient,
		},
		{
			name:   "不认识",
			status: 200,
			body:   `{}`,
			want:   gateway.ErrKindNone,
		},
	}
	for _, c := range cases {
		if got := p.Classify(c.status, c.body); got != c.want {
			t.Errorf("%s: Classify(%d, %s) = %v，期望 %v", c.name, c.status, c.body, got, c.want)
		}
	}
}

// TestModelsStaticIsNonEmptyAndOffline 零值 Config 下目录非空且不发网络。
//
// 这是契约测试能离线跑的前提（LiveModels 缺省 false）。
func TestModelsStaticIsNonEmptyAndOffline(t *testing.T) {
	p := New(Config{})
	cred := gateway.Credential{
		Provider: ProviderID, UID: "u1",
		Secret: &Auth{APIKey: "sk_tr_1a2b3c4d5e6f7a8b9c0d", UID: "u1"},
	}
	ms, err := p.Models(context.Background(), cred)
	if err != nil {
		t.Fatalf("Models() 不应报错（静态表兜底）: %v", err)
	}
	if len(ms) == 0 {
		t.Fatal("Models() 返回空目录 —— 会让 /v1/models 静默变空")
	}
	for i, m := range ms {
		if strings.TrimSpace(m.ID) == "" {
			t.Errorf("Models()[%d].ID 为空", i)
		}
	}
}

// TestModelsFallbackWhenLiveFails 开启实时但凭证无效时，仍然返回非空目录。
func TestModelsFallbackWhenLiveFails(t *testing.T) {
	p := New(Config{LiveModels: true, BaseURL: "http://127.0.0.1:1/v1"}) // 必然连不上
	cred := gateway.Credential{
		Provider: ProviderID, UID: "u1",
		Secret: &Auth{APIKey: "sk_tr_1a2b3c4d5e6f7a8b9c0d", UID: "u1"},
	}
	ms, err := p.Models(context.Background(), cred)
	if err != nil {
		t.Fatalf("实时失败时应回落静态表而不是报错: %v", err)
	}
	if len(ms) == 0 {
		t.Fatal("回落路径也必须是非空目录")
	}
}

// TestNormalizeModels 去空 id / 去重 / 排序。
func TestNormalizeModels(t *testing.T) {
	got := normalizeModels([]upstreamModel{
		{ID: "b-model", ContextLength: 100},
		{ID: "", ContextLength: 1},
		{ID: "a-model", ContextLength: 200},
		{ID: "b-model", ContextLength: 999},
	})
	if len(got) != 2 {
		t.Fatalf("应去重并去掉空 id，得到 %d 条: %+v", len(got), got)
	}
	if got[0].ID != "a-model" || got[1].ID != "b-model" {
		t.Errorf("应按 id 排序，得到 %+v", got)
	}
	if got[1].ContextWindow != 100 {
		t.Errorf("重复 id 应保留首次出现的那条，得到 %d", got[1].ContextWindow)
	}
}

// TestChatBodyShaping 出站整形：剥空 tools、强制 stream。
func TestChatBodyShaping(t *testing.T) {
	// 空 tools 剥掉
	out := stripEmptyTools([]byte(`{"model":"m","tools":[],"messages":[]}`))
	if strings.Contains(string(out), "tools") {
		t.Errorf("空 tools 未被剥掉: %s", out)
	}
	// 非空 tools 原样保留
	keep := `{"model":"m","tools":[{"type":"function"}]}`
	if got := stripEmptyTools([]byte(keep)); string(got) != keep {
		t.Errorf("非空 tools 不该被改动: %s", got)
	}
	// 缺字段不造字段
	plain := `{"model":"m"}`
	if got := stripEmptyTools([]byte(plain)); string(got) != plain {
		t.Errorf("没有 tools 字段时不该改动: %s", got)
	}

	// stream 被强制为 true
	got := forceStream([]byte(`{"model":"m","stream":false}`))
	if !strings.Contains(string(got), `"stream":true`) {
		t.Errorf("stream 未被强制: %s", got)
	}
	// 已经是 true 时不动
	already := `{"model":"m","stream":true}`
	if got := forceStream([]byte(already)); string(got) != already {
		t.Errorf("stream 已为 true 时不该改动: %s", got)
	}
	// 非 JSON 原样返回（不 panic）
	bad := []byte(`not json`)
	if got := forceStream(bad); string(got) != string(bad) {
		t.Errorf("非 JSON 应原样返回: %s", got)
	}
}

// TestExplainAgreesWithClassify **解释与处置必须一致**。
//
// 这是本功能最关键的不变量：如果 Explain 说"余额不足"而 Classify 判成
// "只换号不罚"，用户会看到互相矛盾的界面（状态列说没钱、账号却一直健康）。
// 两者共用 matchKind，本测试钉住那条共用在任何输入下都成立。
func TestExplainAgreesWithClassify(t *testing.T) {
	p := New(Config{})
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"余额不足", 402, `{"code":"INSUFFICIENT_BALANCE","message":"余额不足"}`},
		{"余额不足-流式带重试", 402, `{"code":"INSUFFICIENT_BALANCE","message":"余额不足","data":{"retryAfterSeconds":60}}`},
		{"中文兜底-欠费", 402, `{"message":"账户欠费"}`},
		{"key失效", 401, `{"code":"UNAUTHORIZED","message":"未认证或登录已过期"}`},
		{"key失效-中文", 401, `{"message":"登录已过期"}`},
		{"限流", 429, `{"code":"RATE_LIMIT","message":"too many requests"}`},
		{"上游故障", 502, `bad gateway`},
		{"路由不存在", 404, `{"code":"NOT_FOUND","message":"接口不存在"}`},
		{"请求被拒", 400, `{"code":"BAD_REQUEST","message":"invalid"}`},
		{"模型不存在", 400, `{"code":"MODEL_NOT_FOUND","message":"no such model"}`},
	}
	for _, c := range cases {
		kind := p.Classify(c.status, c.body)
		ex, ok := p.Explain(c.status, c.body)
		if !ok {
			t.Errorf("%s: Explain 应当给出解读（这是可识别的错误）", c.name)
			continue
		}
		if ex.Kind != kind {
			t.Errorf("%s: Explain.Kind=%v 与 Classify=%v **不一致** —— "+
				"界面会说一套、处置做另一套", c.name, ex.Kind, kind)
		}
		if ex.Summary == "" {
			t.Errorf("%s: Summary 为空（状态列会显示空白）", c.name)
		}
		if ex.Detail == "" {
			t.Errorf("%s: Detail 为空（用户看不到怎么办）", c.name)
		}
		// 状态码必须原样带出：用户要拿它去搜/对日志。
		if ex.HTTPStatus != c.status {
			t.Errorf("%s: HTTPStatus=%d，期望 %d", c.name, ex.HTTPStatus, c.status)
		}
	}
}

// TestExplainCarriesUpstreamCode 上游码必须原样出现在解读里。
//
// 用户要的就是"状态码解读" —— 不把码透出来，这句话就无从谈起。
func TestExplainCarriesUpstreamCode(t *testing.T) {
	p := New(Config{})
	ex, ok := p.Explain(402, `{"code":"INSUFFICIENT_BALANCE","message":"余额不足"}`)
	if !ok {
		t.Fatal("应当给出解读")
	}
	if ex.Code != "INSUFFICIENT_BALANCE" {
		t.Errorf("上游码未原样带出: %q", ex.Code)
	}
	// code 缺失时回落 "HTTP <status>"，至少能对上日志。
	ex2, ok := p.Explain(402, `{"message":"余额不足"}`)
	if !ok {
		t.Fatal("应当给出解读")
	}
	if ex2.Code != "HTTP 402" {
		t.Errorf("无上游码时应回落 HTTP 状态码，得到 %q", ex2.Code)
	}
}

// TestExplainRetryHint 上游给的 retryAfterSeconds 要透出来（有依据的等待时间）。
func TestExplainRetryHint(t *testing.T) {
	p := New(Config{})
	ex, ok := p.Explain(402,
		`{"code":"INSUFFICIENT_BALANCE","message":"余额不足","data":{"retryAfterSeconds":60}}`)
	if !ok {
		t.Fatal("应当给出解读")
	}
	if !strings.Contains(ex.Detail, "60") {
		t.Errorf("Detail 应含上游提示的 60 秒，实际: %s", ex.Detail)
	}
	// 没有该字段时不该凭空写一个秒数。
	ex2, _ := p.Explain(402, `{"code":"INSUFFICIENT_BALANCE"}`)
	if strings.Contains(ex2.Detail, "秒后可重试") {
		t.Errorf("上游没给 retryAfterSeconds 时不该编一个: %s", ex2.Detail)
	}
}

// TestExplainUnknownReturnsFalse 不认识的错误要如实说"不认识"。
//
// 编造解读比沉默更糟：用户会据此做一个错误的动作。
func TestExplainUnknownReturnsFalse(t *testing.T) {
	p := New(Config{})
	if _, ok := p.Explain(200, `{"object":"list","data":[]}`); ok {
		t.Error("成功响应不该给出错误解读")
	}
	if _, ok := p.Explain(200, ``); ok {
		t.Error("空响应不该给出错误解读")
	}
}

// TestExplainNeverPanics 契约要求：任何输入都不得 panic。
func TestExplainNeverPanics(t *testing.T) {
	p := New(Config{})
	inputs := []string{"", "{", "null", "[]", `{"code":null}`, strings.Repeat("x", 10000),
		`{"code":123}`, `{"message":{}}`}
	for _, in := range inputs {
		for _, st := range []int{0, 200, 400, 401, 402, 404, 429, 500, 599} {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("Explain(%d, %q) panic: %v", st, in, r)
					}
				}()
				_, _ = p.Explain(st, in)
			}()
		}
	}
}

// TestParseUpstreamError 解析上游错误信封的边界。
func TestParseUpstreamError(t *testing.T) {
	// 真错误信封
	e, ok := parseUpstreamError(`{"code":"INSUFFICIENT_BALANCE","message":"余额不足"}`)
	if !ok || e.Code != "INSUFFICIENT_BALANCE" {
		t.Errorf("未解出错误信封: ok=%v code=%q", ok, e.Code)
	}
	// 只有 message（code 缺失，实测会出现）
	e2, ok := parseUpstreamError(`{"message":"登录已过期"}`)
	if !ok || e2.Message != "登录已过期" {
		t.Errorf("只有 message 的信封应被识别: ok=%v", ok)
	}
	// 成功响应的形状**不能**被当成错误信封
	if _, ok := parseUpstreamError(`{"object":"list","data":[{"id":"x"}]}`); ok {
		t.Error("成功响应被误判成错误信封 —— 会让健康账号显示错误解读")
	}
	// 非 JSON
	if _, ok := parseUpstreamError("not json"); ok {
		t.Error("非 JSON 不该被识别")
	}
}

// TestAgainstRealKeys 对真实上游做一次端到端验证。
//
// 用法：JIYUAN_TEST_KEYS="sk_tr_aaa,sk_tr_bbb" go test ./internal/jiyuan/ -run RealKeys -v
func TestAgainstRealKeys(t *testing.T) {
	raw := strings.TrimSpace(os.Getenv("JIYUAN_TEST_KEYS"))
	if raw == "" {
		t.Skip("JIYUAN_TEST_KEYS 未设置，跳过对真实上游的验证")
	}
	keys := strings.Split(raw, ",")
	p := New(Config{LiveModels: true})
	ctx := context.Background()

	for i, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		a := NewAuth(k, "")
		cred := gateway.Credential{Provider: ProviderID, UID: a.UID, Nickname: a.Nickname, Secret: a}

		ms, err := p.Models(ctx, cred)
		if err != nil {
			t.Errorf("key#%d: Models() 错误: %v", i+1, err)
			continue
		}
		t.Logf("key#%d (uid=%s): 目录 %d 个模型，首个=%s", i+1, a.UID, len(ms), ms[0].ID)

		cs, err := p.Chat(ctx, cred, []byte(
			`{"model":"deepseek-flash","messages":[{"role":"user","content":"reply OK"}],"max_tokens":16}`))
		if err != nil {
			t.Errorf("key#%d: Chat() 错误: %v", i+1, err)
			continue
		}
		kind := p.Classify(cs.Status, "")
		t.Logf("key#%d: Chat status=%d → kind=%v", i+1, cs.Status, kind)
		if cs.Body != nil {
			_ = cs.Body.Close()
		}
	}
}
