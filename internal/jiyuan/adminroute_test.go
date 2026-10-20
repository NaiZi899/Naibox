package jiyuan

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NaiZi899/Naibox/internal/gateway"
)

// ---------------------------------------------------------------------------
// 探测端点与"管理面基址可配"的验收。
//
// 这两件事有一个共同点：**失败时都没有声音**。
//
//	没声明 CapQuotaProbe   界面上就不出现「额度」按钮，用户以为上游不支持
//	mgmt_base 指向错       余额恒为 `—`，没有任何报错（打的是 404/401）
//
// 所以断言要盯住"能不能看出来"，而不只是"函数返回什么"。
// ---------------------------------------------------------------------------

// TestAdminRoutesDeclareProbeEndpoint 能力位与路由必须**成对**出现。
func TestAdminRoutesDeclareProbeEndpoint(t *testing.T) {
	p := New(Config{})

	// 1) 路由非空（契约要求：声明 CapQuotaProbe 就必须有管理端点）。
	ext, ok := gateway.ExtOf[gateway.AdminExt](p)
	if !ok {
		t.Fatal("Provider 应实现 AdminExt —— 否则 CapQuotaProbe 是假声明，契约会拒")
	}
	routes := ext.AdminRoutes()
	if len(routes) == 0 {
		t.Fatal("AdminRoutes 返回空 —— 契约要求非空路由")
	}

	var found *gateway.AdminRoute
	for i := range routes {
		if routes[i].Path == quotaProbePath {
			found = &routes[i]
		}
	}
	if found == nil {
		t.Fatalf("没找到探测端点 %s，实际路由: %+v", quotaProbePath, routes)
	}
	if found.Method != http.MethodPost {
		t.Errorf("方法应为 POST（只读诊断仍用 POST 以避开缓存与预取），得到 %s", found.Method)
	}
	if found.Capability != gateway.CapQuotaProbe {
		t.Errorf("端点应挂在 CapQuotaProbe 能力位下，得到 %v", found.Capability)
	}
	// Hidden：这是诊断入口，不该在左侧导航占一个标签页。
	if !found.Hidden {
		t.Error("探测端点应标 Hidden —— 否则左侧导航会多出一个没人点的标签")
	}
	// Caps 与实现必须一致。
	if !p.Caps().Has(gateway.CapQuotaProbe) {
		t.Error("实现了 AdminExt + QuotaExt，Caps 却没声明 CapQuotaProbe")
	}
}

// TestQuotaProbeReportsThreeStates 三态各自可辨。
func TestQuotaProbeReportsThreeStates(t *testing.T) {
	up := &fakeUpstream{}
	up.serve(t)

	dir := t.TempDir()
	// 有 Session 的账号：额度读得到 → updated
	if err := writeAuth(t, dir, &Auth{UID: "u-known", APIKey: "sk_tr_known", Session: "sess_ok"}); err != nil {
		t.Fatalf("写凭证: %v", err)
	}
	// 无 Session 的账号：读不到 → unknown（这是正常情况，不是失败）
	if err := writeAuth(t, dir, &Auth{UID: "u-nosession", APIKey: "sk_tr_nos", Session: ""}); err != nil {
		t.Fatalf("写凭证: %v", err)
	}

	p := New(Config{AuthDir: dir})

	rec := httptest.NewRecorder()
	p.handleQuotaProbe(rec, httptest.NewRequest(http.MethodPost, quotaProbePath, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("应回 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Provider string             `json:"provider"`
		Count    int                `json:"count"`
		Updated  int                `json:"updated"`
		Results  []quotaProbeResult `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("回执不是合法 JSON: %v", err)
	}
	if body.Provider != providerID {
		t.Errorf("provider 应为 %q，得到 %q", providerID, body.Provider)
	}
	if body.Count != 2 {
		t.Fatalf("应探测 2 个账号，得到 %d（results=%+v）", body.Count, body.Results)
	}
	if body.Updated != 1 {
		t.Errorf("只有带 Session 的那个能读到余额，updated 应为 1，得到 %d", body.Updated)
	}

	states := map[string]string{}
	for _, r := range body.Results {
		states[r.UID] = r.State
	}
	if states["u-known"] != "updated" {
		t.Errorf("u-known 应为 updated，得到 %q", states["u-known"])
	}
	if states["u-nosession"] != "unknown" {
		t.Errorf("无 Session 应为 unknown（不是 failed、不是 0），得到 %q", states["u-nosession"])
	}
}

// TestQuotaProbeResponseLeaksNoSecrets 回执里不能出现 key 或 sess_。
//
// 这是本端点唯一的高敏风险：它返回的是"探测结论"，不是凭据。
func TestQuotaProbeResponseLeaksNoSecrets(t *testing.T) {
	up := &fakeUpstream{}
	up.serve(t)

	dir := t.TempDir()
	const sess = "sess_THIS_IS_A_SECRET_SESSION_VALUE"
	const key = "sk_tr_THIS_IS_A_SECRET_KEY"
	if err := writeAuth(t, dir, &Auth{UID: "u1", APIKey: key, Session: sess}); err != nil {
		t.Fatalf("写凭证: %v", err)
	}

	p := New(Config{AuthDir: dir})
	rec := httptest.NewRecorder()
	p.handleQuotaProbe(rec, httptest.NewRequest(http.MethodPost, quotaProbePath, nil))

	out := rec.Body.String()
	if strings.Contains(out, sess) {
		t.Error("回执里出现了登录凭证明文 —— 它等同登录态，绝不能下发")
	}
	if strings.Contains(out, key) {
		t.Error("回执里出现了 API Key 明文")
	}
}

// TestQuotaProbeUIDFilter 给了 uid 就只探这一个。
func TestQuotaProbeUIDFilter(t *testing.T) {
	up := &fakeUpstream{}
	up.serve(t)

	dir := t.TempDir()
	if err := writeAuth(t, dir, &Auth{UID: "u1", APIKey: "sk_tr_a", Session: "sess_a"}); err != nil {
		t.Fatalf("写凭证: %v", err)
	}
	if err := writeAuth(t, dir, &Auth{UID: "u2", APIKey: "sk_tr_b", Session: "sess_b"}); err != nil {
		t.Fatalf("写凭证: %v", err)
	}

	p := New(Config{AuthDir: dir})
	rec := httptest.NewRecorder()
	p.handleQuotaProbe(rec, httptest.NewRequest(http.MethodPost, quotaProbePath+"?uid=u2", nil))

	var body struct {
		Count   int                `json:"count"`
		Results []quotaProbeResult `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("回执不是合法 JSON: %v", err)
	}
	if body.Count != 1 || body.Results[0].UID != "u2" {
		t.Fatalf("给了 uid=u2 就只该探测它，得到 %+v", body.Results)
	}

	// 不存在的 uid → skipped，而不是错误或空表。
	rec2 := httptest.NewRecorder()
	p.handleQuotaProbe(rec2, httptest.NewRequest(http.MethodPost, quotaProbePath+"?uid=nobody", nil))
	var body2 struct {
		Results []quotaProbeResult `json:"results"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &body2); err != nil {
		t.Fatalf("回执不是合法 JSON: %v", err)
	}
	if len(body2.Results) != 1 || body2.Results[0].State != "skipped" {
		t.Errorf("不归本上游的 uid 应报 skipped，得到 %+v", body2.Results)
	}
}

// TestQuotaProbeEmptyAuthDirIsNotAnError 空目录 = 没有账号，不是故障。
func TestQuotaProbeEmptyAuthDirIsNotAnError(t *testing.T) {
	p := New(Config{AuthDir: t.TempDir()}) // 目录存在但为空
	rec := httptest.NewRecorder()
	p.handleQuotaProbe(rec, httptest.NewRequest(http.MethodPost, quotaProbePath, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("空目录应回 200（没有账号是合法状态），得到 %d", rec.Code)
	}
	var body struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Count != 0 {
		t.Errorf("空目录应得到 count=0，得到 %d", body.Count)
	}
}

// TestMgmtBaseOverrideRoutesBalance 配了 mgmt_base，余额请求必须打到它。
//
// # 这条守的是"反代部署"场景
//
// 很多反代只转发 /v1。那种部署下聊天走代理、余额必须直连官方域名 ——
// 反过来说，配了 mgmt_base 之后，余额请求**必须**改道，否则配置等于没生效，
// 而表现只是"余额永远未知"。
func TestMgmtBaseOverrideRoutesBalance(t *testing.T) {
	up := &fakeUpstream{}
	up.serve(t)
	override := currentMgmtBase() // 记下测试把它指向的 httptest 地址

	dir := t.TempDir()
	if err := writeAuth(t, dir, &Auth{UID: "u1", APIKey: "sk_tr_a", Session: "sess_a"}); err != nil {
		t.Fatalf("写凭证: %v", err)
	}

	// 构造时显式给 mgmt_base = 同一台假上游，验证它被接受且生效。
	p := New(Config{AuthDir: dir, MgmtBase: override})
	if got := currentMgmtBase(); got != strings.TrimRight(override, "/") {
		t.Fatalf("mgmt_base 未生效：期望 %q，实际 %q", override, got)
	}
	qv, ok := p.RefreshQuota("u1")
	if !ok || !qv.HasData {
		t.Fatalf("假上游应返回余额，得到 ok=%v hasData=%v", ok, qv.HasData)
	}
	if up.balanceCalls == 0 {
		t.Error("余额请求没打到配置的 mgmt_base（假上游一次都没被调用）")
	}
}

// TestMgmtBaseEmptyKeepsDefault 留空时保持官方域名，且 New 不覆盖已设值。
//
// ⚠ 后半句是真踩过的坑：New 若无条件 setMgmtBase(空)，会把调用方刚设好的
// 地址冲成官方域名 —— 测试里表现为"所有管理面请求静默打外网"。
func TestMgmtBaseEmptyKeepsDefault(t *testing.T) {
	if DefaultMgmtBase != "https://tokenrhythm.studio" {
		t.Fatalf("官方域名不该被改动，得到 %q", DefaultMgmtBase)
	}
	// 先把当前值改到一个可辨认的地址，再构造一个 MgmtBase 为空的 Provider。
	const marker = "http://127.0.0.1:9/marker"
	setMgmtBase(marker)
	t.Cleanup(func() { setMgmtBase(DefaultMgmtBase) })

	New(Config{}) // MgmtBase 为空
	if got := currentMgmtBase(); got != marker {
		t.Errorf("MgmtBase 为空时 New 不该改动当前基址：期望 %q，实际 %q。"+
			"被改掉的表现是请求静默打到官方域名", marker, got)
	}
}

// TestQuotaImpliesUsableForJiyuan 本上游的余额是直接门禁。
func TestQuotaImpliesUsableForJiyuan(t *testing.T) {
	p := New(Config{})
	ext, ok := gateway.ExtOf[gateway.QuotaUsabilityExt](p)
	if !ok {
		t.Fatal("Provider 应实现 QuotaUsabilityExt —— 否则充值后不会自动解冻")
	}
	if !ext.QuotaImpliesUsable() {
		t.Error("基元律动是静态 key + 余额唯一门禁，应返回 true；" +
			"返回 false 会让充值后的账号卡在冷却里等人工清")
	}
}
