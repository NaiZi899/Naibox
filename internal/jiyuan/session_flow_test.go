// session_flow_test.go 管理面**完整流程**的离线测试（假上游 + 真实 HTTP）。
//
// # 为什么必须有这个文件
//
// session_test.go 只测纯解析函数（sessionParse*），那能挡住"字段读错层级"
// 的问题，却挡不住**流程**里的问题：某个分支要不要读余额、判重用的是
// 过滤前还是过滤后的列表、写操作会不会重复触发 —— 这些都在 `fetch*` 与
// `importViaSession*` 的**调用顺序**里。
//
// 而在这个文件之前，`importViaSessionCreate` 里恰好就有这样一个缺陷：
// 判重靠"该账号有没有完整 key"（上游永不下发完整 key），于是每次调用都
// 会在用户账号里多新建一把 key。当时**没有任何测试能发现它** ——
// 因为要触发它就得打真实网络、在真实账号里留下真实凭证。
//
// 修法是把 mgmtBase 从 const 改成 var（见 session.go 的说明），
// 让这里可以用 httptest 起一个假上游。于是：
//
//	改动前：这个缺陷只能靠"在用户账号里反复试"发现
//	改动后：下面两条测试（判重 / 余额不分叉）当场就能抓到
//
// # 纪律：这里跑的是**真实 HTTP**，不是 mock 接口
//
// 假上游是一个真的 http.Server，走真的 net/http 客户端、真的 JSON 编解码。
// 唯一被替换的是"域名指向"。所以信封剥离、CSRF 头、POST body、
// 错误码判断这些都可能在这里被验证。
package jiyuan

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/NaiZi899/Naibox/internal/gateway"
)

// fakeUpstream 一个可编排的假上游。
//
// 记账两件事：创建过几次 key、最后一次创建用的名字 —— 判重是否生效
// 就看这两个值。
type fakeUpstream struct {
	mu sync.Mutex
	// keys 当前账号下的 key 条目（name → 完整 key），模拟"列表 + 创建"共享的状态。
	keys map[string]string
	// creates 收到的 POST /api/api-keys 次数。
	creates int
	// createdNames 每次创建用的名字，按序。
	createdNames []string
	// lastCreatedKey 最后一次创建下发的完整 key。
	lastCreatedKey string
	// keyPrefixes name → 该 key 的前缀（上游列表会下发未脱敏前缀）。
	//
	// 这是把 session 挂到已有 key 上的唯一配对依据（见 import.go）。
	keyPrefixes map[string]string
	// overrideUsage 若非空，`/api/usage-summary` 直接回它（用于测币种等边界）。
	overrideUsage string
	// balanceCalls 收到 `/api/usage-summary` 的次数。
	//
	// 用来证明"余额请求确实打到了这台假上游"—— 只看返回值区分不出
	// "读到了余额"与"读的是另一台服务器（比如官方域名）"。
	balanceCalls int
	// keysStatus 非 0 时 `/api/api-keys` 直接回这个状态码（模拟上游 5xx/429）。
	//
	// 用来钉住一条**曾经把用户带偏**的分支：列表读失败时若把错误吞掉，
	// 用户会看到「凭证有效、只是没有能配对的 key」，于是跑去补 key ——
	// 实际该做的是等一会儿重试。
	keysStatus int
}

func newFakeUpstream() *fakeUpstream {
	return &fakeUpstream{
		keys:        map[string]string{},
		keyPrefixes: map[string]string{},
	}
}

// serve 起一个 httptest 服务器并把 mgmtBase 指向它。
func (f *fakeUpstream) serve(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)

	old := currentMgmtBase()
	setMgmtBase(srv.URL)
	t.Cleanup(func() { setMgmtBase(old) })
}

func (f *fakeUpstream) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/me":
		writeJSONRaw(w, `{"code":0,"message":"ok","data":{"name":"demo-user","phoneMasked":"138****8000","status":"active"}}`)

	case r.Method == http.MethodGet && r.URL.Path == "/api/usage-summary":
		// 外层（handle 开头）已持 f.mu —— 这里**不能**再 Lock，
		// sync.Mutex 不可重入，重入会把这个 handler 永久挂住（表现为
		// httptest.Server 关闭时卡在 "blocked in Close"）。
		f.balanceCalls++
		if f.overrideUsage != "" {
			writeJSONRaw(w, f.overrideUsage)
			return
		}
		// ⚠ 刻意让 Total ≠ Available：真实账号里
		// `balanceCny = available + frozen`，而**冻结的钱花不出去**。
		// 若两者相等，测试就分不出"取 Available"与"取 Total"的差别
		//（变异验证实测踩过：那种夹具下改错也全绿）。
		writeJSONRaw(w, `{"code":0,"message":"ok","data":{"balanceCny":"120.00000000","availableBalanceCny":"99.75000000","frozenBalanceCny":"20.25000000","expiringBalanceCny":"80.75000000","currency":"CNY"}}`)

	case r.Method == http.MethodGet && r.URL.Path == "/api/api-keys":
		if f.keysStatus != 0 {
			w.WriteHeader(f.keysStatus)
			writeJSONRaw(w, `{"code":"UPSTREAM_ERROR","message":"上游暂时不可用"}`)
			return
		}
		// **关键**：列表只给 maskedKey（上游事实），但给**未脱敏的 keyPrefix**
		//（那是 session↔key 配对的唯一依据，见 import.go）。
		var items []string
		for name := range f.keys {
			prefix := f.keyPrefixes[name]
			if prefix == "" {
				prefix = "sk_tr_XX"
			}
			items = append(items, `{"id":"id-`+name+`","name":"`+name+`",`+
				`"maskedKey":"sk_tr_XX****ZZ","keyPrefix":"`+prefix+`","status":"enabled"}`)
		}
		writeJSONRaw(w, `{"code":0,"message":"ok","data":[`+strings.Join(items, ",")+`]}`)

	case r.Method == http.MethodPost && r.URL.Path == "/api/api-keys":
		// 真实上游要求写接口带 Authorization（否则 CSRF 403）——
		// 假上游同样要求，这样"忘了带"会在这里暴露。
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusForbidden)
			writeJSONRaw(w, `{"code":"CSRF_INVALID","message":"请求安全校验失败"}`)
			return
		}
		name := f.extractName(r)
		f.creates++
		f.createdNames = append(f.createdNames, name)
		full := "sk_tr_FAKE_" + name + "_SECRETVALUE"
		f.keys[name] = full
		// 前缀取前 12 位（与上游形状一致：`sk_tr_` + 6 位）。
		f.keyPrefixes[name] = full[:12]
		f.lastCreatedKey = full
		writeJSONRaw(w, `{"code":0,"message":"ok","data":{"id":"id-`+name+`","name":"`+name+`",`+
			`"maskedKey":"sk_tr_FA****ALUE","status":"enabled","key":"`+full+`"}}`)

	default:
		w.WriteHeader(http.StatusNotFound)
		writeJSONRaw(w, `{"code":"NOT_FOUND","message":"接口不存在"}`)
	}
}

// extractName 从 POST body 里取 name（不引依赖，手工取够用）。
func (f *fakeUpstream) extractName(r *http.Request) string {
	buf := make([]byte, 4096)
	n, _ := r.Body.Read(buf)
	body := string(buf[:n])
	const marker = `"name":"`
	i := strings.Index(body, marker)
	if i < 0 {
		return ""
	}
	rest := body[i+len(marker):]
	if j := strings.IndexByte(rest, '"'); j >= 0 {
		return rest[:j]
	}
	return rest
}

func writeJSONRaw(w http.ResponseWriter, s string) {
	_, _ = w.Write([]byte(s))
}

// writeAuth 把一份凭证按本包的落盘约定写进目录（测试辅助）。
//
// 用 MarshalAuthFile —— **不手写 JSON**：手写会让测试与真实的磁盘格式
// 漂移（比如将来加字段时测试仍通过，而生产解析失败）。
func writeAuth(t *testing.T, dir string, a *Auth) error {
	t.Helper()
	raw, err := MarshalAuthFile(a)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, AuthFileName(a)), raw, 0o600)
}

// TestFlowReadOnlyReportsIdentityAndBalance 只读路径：真实身份 + 真实余额 + 不导入。
//
// 这条钉住的是修复前那个**静默零值**缺陷：账号名与余额都在信封的 data 里，
// 读顶层会拿到空串与 0。若这里失败，说明信封剥离又丢了。
func TestFlowReadOnlyReportsIdentityAndBalance(t *testing.T) {
	f := newFakeUpstream()
	f.serve(t)

	p := New(Config{})
	_, err := p.ImportCredentials("sess_fake_session_value", importKindSession)
	if err == nil {
		t.Fatal("只读路径不该导入账号（上游不下发完整 key），必须返回说明性错误")
	}
	msg := err.Error()
	for _, want := range []string{"demo-user", "8000", "99.75", "API Key"} {
		if !strings.Contains(msg, want) {
			t.Errorf("回执应包含 %q，实际:\n%s", want, msg)
		}
	}
	// 只读路径**绝不能**创建 key。
	if f.creates != 0 {
		t.Errorf("只读路径创建了 %d 次 key —— 只读意图不该有写副作用", f.creates)
	}
}

// TestFlowCreateWritesOnce 代开路径：写一次、名字正确、拿到完整 key。
func TestFlowCreateWritesOnce(t *testing.T) {
	f := newFakeUpstream()
	f.serve(t)

	p := New(Config{})
	creds, err := p.ImportCredentials("sess_fake_session_value", importKindSessionCreate)
	if err != nil {
		t.Fatalf("代开失败: %v", err)
	}
	if f.creates != 1 {
		t.Errorf("应恰好创建 1 次，实际 %d 次", f.creates)
	}
	if len(creds) != 1 {
		t.Fatalf("应导入 1 条，得到 %d", len(creds))
	}
	c := creds[0]
	if !strings.Contains(string(c.Raw), f.lastCreatedKey) {
		t.Errorf("落盘内容应含上游下发的完整 key，实际:\n%s", string(c.Raw))
	}
	// 完整 key 才有效；脱敏值落盘会造成一个永远 401 的账号。
	if strings.Contains(string(c.Raw), "****") {
		t.Errorf("落盘内容含脱敏值:\n%s", string(c.Raw))
	}
	// 登录凭证绝不落盘。
	if strings.Contains(string(c.Raw), "sess_fake_session_value") {
		t.Errorf("登录凭证被写进了凭证文件（安全缺陷）:\n%s", string(c.Raw))
	}
	// 余额与手机尾号要跟着入库。
	if !strings.Contains(string(c.Raw), "99.75") {
		t.Errorf("余额快照缺失:\n%s", string(c.Raw))
	}
	if !strings.Contains(string(c.Raw), "8000") {
		t.Errorf("手机尾号缺失:\n%s", string(c.Raw))
	}
}

// TestFlowCreateAvoidsDuplicateNames **判重**：名字已被占用时必须换名。
//
// # 这条测试的存在理由（一次真实的教训）
//
// 第一版代开靠"该账号下有没有完整 key"判重 —— 而列表只有 maskedKey，
// 判重永远不成立。后果是每调一次就在用户账号里多一把同名 key
// （实测连调两次就有了两把 `omni2api`，白占配额，而上限只有 10 把）。
//
// 这个缺陷用纯解析测试**抓不到**（解析是对的，错的是"用哪份列表判重"），
// 必须跑流程才能暴露 —— 这正是本文件存在的意义。
func TestFlowCreateAvoidsDuplicateNames(t *testing.T) {
	f := newFakeUpstream()
	f.keys["omni2api"] = "sk_tr_EXISTING_OLD" // 账号里已有一把同名的
	f.serve(t)

	p := New(Config{})
	if _, err := p.ImportCredentials("sess_fake_session_value", importKindSessionCreate); err != nil {
		t.Fatalf("代开失败: %v", err)
	}
	if len(f.createdNames) != 1 {
		t.Fatalf("应创建 1 次，得到 %d", len(f.createdNames))
	}
	if got := f.createdNames[0]; got == "omni2api" {
		t.Errorf("名字已被占用，应换名（否则用户在上游看到两把同名 key 分不清），实际用了 %q", got)
	}
	if got := f.createdNames[0]; got != "omni2api-2" {
		t.Errorf("换名规则应为 omni2api-2，得到 %q", got)
	}
}

// TestFlowCreateTwiceDoesNotPileUp 连调两次**不会**堆积同名 key。
//
// 这是上面那个缺陷的**用户视角复现**：第二次调用必须看到第一次建的那把
// （列表里已有 omni2api），从而换名而不是再建一把同名的。
func TestFlowCreateTwiceDoesNotPileUp(t *testing.T) {
	f := newFakeUpstream()
	f.serve(t)

	p := New(Config{})
	for i := 1; i <= 2; i++ {
		if _, err := p.ImportCredentials("sess_fake_session_value", importKindSessionCreate); err != nil {
			t.Fatalf("第 %d 次代开失败: %v", i, err)
		}
	}
	if f.creates != 2 {
		t.Fatalf("两次调用应创建 2 把（每次都是用户显式点的），得到 %d", f.creates)
	}
	if f.createdNames[0] == f.createdNames[1] {
		t.Errorf("两次创建用了同一个名字 %q —— 上游列表里会出现两把无法区分的 key", f.createdNames[0])
	}
	if f.createdNames[1] != "omni2api-2" {
		t.Errorf("第二次应用 omni2api-2，得到 %q", f.createdNames[1])
	}
}

// TestFlowSessionAttachesToExistingKey session 栏把凭证挂到已有 key 上。
//
// # 这是"额度列能显示余额"的完整链路
//
//	池里已有一把 key（用户之前用 API Key 栏导入过）
//	→ 贴 sess_，上游列表回该 key 的前缀
//	→ 前缀配对成功 → 把 session 写进那份凭证
//	→ QuotaExt 从此能读到实时余额
//
// 若这条断了，额度列会永远显示 `—`（用户报的那个问题）。
func TestFlowSessionAttachesToExistingKey(t *testing.T) {
	f := newFakeUpstream()
	f.serve(t)

	dir := t.TempDir()
	// 池里已有的 key（用 API Key 栏导入的形态）。
	const full = "sk_tr_EXAMP1_FAKE_FULL_KEY_FOR_TESTS"
	existing := NewAuth(full, "已有账号")
	if err := writeAuth(t, dir, existing); err != nil {
		t.Fatalf("写凭证: %v", err)
	}
	// 上游账号下确实有这把 key（前缀与本地完整 key 匹配）。
	f.keys["demo-a"] = full
	f.keyPrefixes["demo-a"] = full[:12]

	p := New(Config{AuthDir: dir})
	creds, err := p.ImportCredentials("sess_fake_session_value", importKindSession)
	if err != nil {
		t.Fatalf("session 栏应成功挂载，得到: %v", err)
	}
	if len(creds) != 1 {
		t.Fatalf("应挂载 1 份凭证，得到 %d", len(creds))
	}
	raw := string(creds[0].Raw)

	// ① session 必须被写进凭证（否则额度列读不到余额）。
	if !strings.Contains(raw, "sess_fake_session_value") {
		t.Errorf("凭证里应写入 session（额度列靠它读余额）:\n%s", raw)
	}
	// ② 完整 key 必须保留（不能因为导入 session 就把 key 弄丢）。
	if !strings.Contains(raw, full) {
		t.Errorf("凭证必须保留完整 key:\n%s", raw)
	}
	// ③ 回执只带脱敏标识，绝不能下发完整登录态。
	if creds[0].DerivedFrom == "" || strings.Contains(creds[0].DerivedFrom, "sess_fake_session_value") {
		t.Errorf("DerivedFrom 应是脱敏前缀，得到 %q", creds[0].DerivedFrom)
	}
	// ④ 这个账号的额度从此可读。
	//
	// ⚠ 必须先落盘：`RefreshQuota` → `credential(uid)` 在未接线时
	// **回落目录扫描**（provider.go 的设计：这样独立部署也能取到 key）。
	// 所以它读的是**磁盘上**那份，而不是本次导入返回的内存副本。
	// 真实流程里 core 会把 ImportedCredential 写盘后才刷新额度 —— 测试
	// 必须照同样的顺序，否则测的是"写盘前"的状态。
	if err := os.WriteFile(filepath.Join(dir, creds[0].FileName), creds[0].Raw, 0o600); err != nil {
		t.Fatalf("落盘: %v", err)
	}
	qv, ok := p.RefreshQuota(existing.UID)
	if !ok || !qv.HasData {
		t.Fatalf("挂载后额度应可读，得到 ok=%v qv=%+v", ok, qv)
	}
	if qv.Remaining != 9975 {
		t.Errorf("余额应为 9975 分，得到 %d", qv.Remaining)
	}
}

// TestFlowSessionIgnoresUnmatchedKeys 前缀对不上的 key **不动**（不写半残凭证）。
//
// 若把 session 挂到不属于它的 key 上，那条账号会带着别人的登录态 ——
// 后果是"用 A 号的 key 读 B 号的余额"，比不挂更糟。
func TestFlowSessionIgnoresUnmatchedKeys(t *testing.T) {
	f := newFakeUpstream()
	f.serve(t)

	dir := t.TempDir()
	other := NewAuth("sk_tr_UNRELATED_KEY_VALUE_1234567890", "别人的号")
	if err := writeAuth(t, dir, other); err != nil {
		t.Fatalf("写凭证: %v", err)
	}
	// 上游账号下的 key 与本地那把**不匹配**（前缀对不上）。
	f.keys["demo-a"] = "sk_tr_OTHERKEY_FAKE_VALUE_7654321"
	f.keyPrefixes["demo-a"] = "sk_tr_EXAMP1"

	p := New(Config{AuthDir: dir})
	_, err := p.ImportCredentials("sess_fake_session_value", importKindSession)
	if err == nil {
		t.Fatal("一把都没对上时应返回说明性结论，而不是静默成功")
	}
	if !gateway.IsImportNotice(err) {
		t.Errorf("应是说明性结论（不套\"无法解析\"前缀），得到: %T", err)
	}
	if !strings.Contains(err.Error(), "一把都没对上") {
		t.Errorf("应说明对不上，实际: %s", err)
	}
	// 关键：别人的凭证不能被改写。
	back, _ := LoadDir(dir)
	if len(back) != 1 || back[0].Session != "" {
		t.Errorf("对不上的凭证不该被写入 session: %+v", back[0])
	}
}

func TestFlowRejectsKeyInSessionSlot(t *testing.T) {
	f := newFakeUpstream()
	f.serve(t)

	p := New(Config{})
	if _, err := p.ImportCredentials("sk_tr_1a2b3c4d5e6f7a8b9c0d", importKindSessionCreate); err == nil {
		t.Fatal("key 不该被凭证栏收下")
	}
	if f.creates != 0 {
		t.Errorf("输入非法时不该打上游创建接口，实际创建 %d 次", f.creates)
	}
}

// TestFlowCreateRequiresAuthorization 假上游要求 Authorization 头 —— 没有就 403。
//
// 这条把"写接口必须带 Authorization（否则上游 CSRF 校验拦下）"这条
// 实测结论钉在流程层：若哪天有人把那个头去掉，这里会以"权限不足"的
// 形式失败，而不是在生产里变成一次 403 用户报障。
func TestFlowCreateRequiresAuthorization(t *testing.T) {
	f := newFakeUpstream()
	// 用一个**只检查 Authorization 的**最小服务器，绕开上面的 handle。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusForbidden)
			writeJSONRaw(w, `{"code":"CSRF_INVALID","message":"请求安全校验失败"}`)
			return
		}
		f.handle(w, r)
	}))
	t.Cleanup(srv.Close)
	old := currentMgmtBase()
	setMgmtBase(srv.URL)
	t.Cleanup(func() { setMgmtBase(old) })

	p := New(Config{})
	if _, err := p.ImportCredentials("sess_fake_session_value", importKindSessionCreate); err != nil {
		t.Fatalf("带上 Authorization 后应成功，得到: %v", err)
	}
	if f.creates != 1 {
		t.Errorf("应创建 1 次，得到 %d", f.creates)
	}
}

// TestFlowSessionRefreshesAutoNickname 挂 session 时刷新**自动派生**的昵称。
//
// 现象（改造前）：用「API Key」栏导入的号昵称是 uid 前 8 位，
// 之后再贴 sess_ 导入，明明从 /api/me 拿到了账号名（demo-user），
// 表格里却还是那串 uid —— 注释声称"昵称会被刷新"，实现里根本没有那一行。
func TestFlowSessionRefreshesAutoNickname(t *testing.T) {
	f := newFakeUpstream()
	f.serve(t)

	dir := t.TempDir()
	const full = "sk_tr_EXAMP1_FAKE_FULL_KEY_FOR_TESTS"
	// 用 API Key 栏导入的形态：昵称留空 → NewAuth 自动派生成 uid 前 8 位。
	auto := NewAuth(full, "")
	if auto.Nickname == "" || auto.Nickname != ShortUID(auto.UID) {
		t.Fatalf("夹具前提不成立：自动昵称应等于 uid 前缀，得到 %q / uid=%q", auto.Nickname, auto.UID)
	}
	if err := writeAuth(t, dir, auto); err != nil {
		t.Fatalf("写凭证: %v", err)
	}
	f.keys["demo-a"] = full
	f.keyPrefixes["demo-a"] = full[:12]

	p := New(Config{AuthDir: dir})
	creds, err := p.ImportCredentials("sess_fake_session_value", importKindSession)
	if err != nil {
		t.Fatalf("应挂载成功: %v", err)
	}
	if len(creds) != 1 {
		t.Fatalf("应挂载 1 份，得到 %d", len(creds))
	}
	if creds[0].Nickname != "demo-user" {
		t.Errorf("自动派生的昵称应被上游账号名刷新，得到 %q", creds[0].Nickname)
	}
	// 落盘内容必须与回执一致（否则界面显示对了、磁盘还是旧的）。
	if !strings.Contains(string(creds[0].Raw), "demo-user") {
		t.Errorf("落盘凭证里应是刷新后的昵称:\n%s", string(creds[0].Raw))
	}
}

// TestFlowSessionKeepsUserNickname 用户手打的标签**不**被上游账号名冲掉。
//
// 用户用 `<key> <标签>` 起的名字更贴近他自己的用法（"主力号" / "测试用"），
// 拿账号名盖掉等于替他改写数据 —— 而这次改动只是为了补上"从没写过昵称"
// 那个缺口，不该顺手扩大行为。
func TestFlowSessionKeepsUserNickname(t *testing.T) {
	f := newFakeUpstream()
	f.serve(t)

	dir := t.TempDir()
	const full = "sk_tr_EXAMP1_FAKE_FULL_KEY_FOR_TESTS"
	labeled := NewAuth(full, "主力号")
	if err := writeAuth(t, dir, labeled); err != nil {
		t.Fatalf("写凭证: %v", err)
	}
	f.keys["demo-a"] = full
	f.keyPrefixes["demo-a"] = full[:12]

	p := New(Config{AuthDir: dir})
	creds, err := p.ImportCredentials("sess_fake_session_value", importKindSession)
	if err != nil {
		t.Fatalf("应挂载成功: %v", err)
	}
	if len(creds) != 1 {
		t.Fatalf("应挂载 1 份，得到 %d", len(creds))
	}
	if creds[0].Nickname != "主力号" {
		t.Errorf("用户标签不该被改写，得到 %q（期望 主力号）", creds[0].Nickname)
	}
}

// TestFlowSessionListFailureIsReported 上游 Key 列表读失败必须**说出来**。
//
// # 这条守的是什么
//
// 改造前是 `entries, _ := fetchAPIKeyEntries(...)` —— 错误被吞掉之后，
// 无论"上游 5xx 读不到"还是"读到了但一把都对不上"，用户看到的都是
// 同一句话：「当前账号下的 key 与池里的凭证一把都没对上，二选一」。
// 前者该做的是等一会儿重试，后者该做的是去复制完整 key —— 一句话把
// 用户引到错的动作上，就是这条用例要防的回归。
func TestFlowSessionListFailureIsReported(t *testing.T) {
	f := newFakeUpstream()
	f.keysStatus = http.StatusServiceUnavailable // 上游管理面挂了
	f.serve(t)

	dir := t.TempDir()
	const full = "sk_tr_EXAMP1_FAKE_FULL_KEY_FOR_TESTS"
	if err := writeAuth(t, dir, NewAuth(full, "已有账号")); err != nil {
		t.Fatalf("写凭证: %v", err)
	}
	f.keys["demo-a"] = full
	f.keyPrefixes["demo-a"] = full[:12]

	p := New(Config{AuthDir: dir})
	_, err := p.ImportCredentials("sess_fake_session_value", importKindSession)
	if err == nil {
		t.Fatal("列表读失败时必须有说明，不能静默成功")
	}
	if !gateway.IsImportNotice(err) {
		t.Errorf("应仍是说明性结论（不是解析失败），得到: %T", err)
	}
	msg := err.Error()
	// ① 必须点明"读不到"，而不是"对不上"。
	if !strings.Contains(msg, "读上游 Key 列表失败") {
		t.Errorf("应明确说'读不到 key 列表'，实际:\n%s", msg)
	}
	// ② 不能再说"一把都没对上" —— 那种情形下我们**根本不知道**对不对得上。
	if strings.Contains(msg, "一把都没对上") {
		t.Errorf("读失败时不得断言'一把都没对上'（这是把用户引向错的动作），实际:\n%s", msg)
	}
	// ③ 必须给出正确动作：重试。
	if !strings.Contains(msg, "重试") {
		t.Errorf("应提示稍后重试，实际:\n%s", msg)
	}
}
