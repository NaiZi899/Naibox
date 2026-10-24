package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeProfilesStore 只实现 admin 需要的五个方法。
type fakeProfilesStore struct {
	profiles []BrowserProfileView
	browser  BrowserProbeView
	accounts []EgressAccountView

	upserted   string
	upsertView BrowserProfileView
	upsertErr  error

	deletedID   string
	deletedKeep bool
	deleteErr   error

	launchID  string
	launchURL string
	launchRes LaunchResult
	launchErr error

	openDirID  string
	openDirErr error
}

func (f *fakeProfilesStore) List(context.Context) ([]BrowserProfileView, BrowserProbeView, []EgressAccountView) {
	return f.profiles, f.browser, f.accounts
}

func (f *fakeProfilesStore) Upsert(ctx context.Context, raw json.RawMessage) (BrowserProfileView, error) {
	f.upserted = string(raw)
	return f.upsertView, f.upsertErr
}

func (f *fakeProfilesStore) Delete(ctx context.Context, id string, keepDir bool) error {
	f.deletedID, f.deletedKeep = id, keepDir
	return f.deleteErr
}

func (f *fakeProfilesStore) Launch(ctx context.Context, id, url string) (LaunchResult, error) {
	f.launchID, f.launchURL = id, url
	return f.launchRes, f.launchErr
}

func (f *fakeProfilesStore) OpenDir(ctx context.Context, id string) error {
	f.openDirID = id
	return f.openDirErr
}

func profileReq(method, path, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.RemoteAddr = "127.0.0.1:1"
	return r
}

// TestProfilesListEndpoint 列表要一次给出三样：档案、浏览器探测、可绑定账号。
//
// 少任何一样前端就得再打一次请求，而编辑器打开时最需要的就是"能绑哪些号"。
func TestProfilesListEndpoint(t *testing.T) {
	st := &fakeProfilesStore{
		profiles: []BrowserProfileView{{ID: "workbuddy", UID: "u1", AccountName: "阿猫", DiskBytes: 2048}},
		browser:  BrowserProbeView{OK: true, Name: "Microsoft Edge", Path: "C:\\msedge.exe"},
		accounts: []EgressAccountView{{UID: "u1", Nickname: "阿猫", Provider: "workbuddy"}},
	}
	h := New(Config{Profiles: st})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, profileReq(http.MethodGet, "/admin/browser-profiles", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var doc struct {
		Profiles []BrowserProfileView `json:"profiles"`
		Browser  BrowserProbeView     `json:"browser"`
		Accounts []EgressAccountView  `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("不是 JSON: %v", err)
	}
	if len(doc.Profiles) != 1 || doc.Profiles[0].AccountName != "阿猫" {
		t.Fatalf("档案投影不对: %+v", doc.Profiles)
	}
	if !doc.Browser.OK || doc.Browser.Name != "Microsoft Edge" {
		t.Fatalf("浏览器探测结果没下发: %+v", doc.Browser)
	}
	if len(doc.Accounts) != 1 {
		t.Fatalf("账号投影没下发: %+v", doc.Accounts)
	}
}

// TestProfileUpsertPassesBodyThrough 后端不认识档案字段，原样转给宿主。
func TestProfileUpsertPassesBodyThrough(t *testing.T) {
	st := &fakeProfilesStore{upsertView: BrowserProfileView{ID: "p1", UID: "u1", Label: "阿猫"}}
	h := New(Config{Profiles: st})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, profileReq(http.MethodPost, "/admin/browser-profiles",
		`{"id":"p1","uid":"u1","egress_id":"cn-1"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(st.upserted, `"egress_id":"cn-1"`) {
		t.Fatalf("补丁没有被原样转发: %s", st.upserted)
	}
	if !strings.Contains(rec.Body.String(), "阿猫") {
		t.Fatalf("回执里应当带上宿主编好的档案: %s", rec.Body.String())
	}
}

// TestProfileUpsertRejects 宿主报错 → 400（且原因原样透出）。
func TestProfileUpsertRejects(t *testing.T) {
	st := &fakeProfilesStore{upsertErr: fmt.Errorf("账号 u9 不在池里")}
	h := New(Config{Profiles: st})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, profileReq(http.MethodPost, "/admin/browser-profiles", `{"id":"p1","uid":"u9"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "不在池里") {
		t.Fatalf("错误原因应当透出: %s", rec.Body.String())
	}
}

// TestProfileDeleteKeepDirFlag ?keep_dir=1 要真的映射成 keepDir=true。
func TestProfileDeleteKeepDirFlag(t *testing.T) {
	for _, c := range []struct {
		query string
		want  bool
	}{
		{"", false},
		{"?keep_dir=1", true},
		{"?keep_dir=true", true},
		{"?keep_dir=0", false},
	} {
		st := &fakeProfilesStore{}
		h := New(Config{Profiles: st})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, profileReq(http.MethodDelete, "/admin/browser-profiles/p1"+c.query, ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("%q status=%d body=%s", c.query, rec.Code, rec.Body.String())
		}
		if st.deletedID != "p1" || st.deletedKeep != c.want {
			t.Fatalf("%q: id=%q keep=%v want keep=%v", c.query, st.deletedID, st.deletedKeep, c.want)
		}
	}
}

// TestProfileLaunch 启动成功要回浏览器名与端口；失败回 400 + 原因。
func TestProfileLaunch(t *testing.T) {
	st := &fakeProfilesStore{launchRes: LaunchResult{OK: true, Browser: "Microsoft Edge", DebugPort: 51234}}
	h := New(Config{Profiles: st})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, profileReq(http.MethodPost, "/admin/browser-profiles/p1/launch", `{"url":"https://a.example"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if st.launchID != "p1" || st.launchURL != "https://a.example" {
		t.Fatalf("参数没传到宿主: id=%q url=%q", st.launchID, st.launchURL)
	}
	if !strings.Contains(rec.Body.String(), "51234") {
		t.Fatalf("端口没回执: %s", rec.Body.String())
	}

	bad := &fakeProfilesStore{launchErr: fmt.Errorf("代理带认证信息：Chrome 不处理认证")}
	h2 := New(Config{Profiles: bad})
	rec2 := httptest.NewRecorder()
	h2.ServeHTTP(rec2, profileReq(http.MethodPost, "/admin/browser-profiles/p1/launch", "{}"))
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("启动失败应当 400，实际 %d", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), "认证") {
		t.Fatalf("失败原因应当透出: %s", rec2.Body.String())
	}
}

// TestProfileOpenDirUnsupported 服务端形态回 501（而不是假装成功）。
func TestProfileOpenDirUnsupported(t *testing.T) {
	st := &fakeProfilesStore{openDirErr: fmt.Errorf("%w：服务端形态没有桌面", ErrProfilesUnsupported)}
	h := New(Config{Profiles: st})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, profileReq(http.MethodPost, "/admin/browser-profiles/p1/open-dir", "{}"))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("应当 501，实际 %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestProfilesNotWired 未接线不注册路由。
func TestProfilesNotWired(t *testing.T) {
	h := New(Config{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, profileReq(http.MethodGet, "/admin/browser-profiles", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未接线应当 404，实际 %d", rec.Code)
	}
}
