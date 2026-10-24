package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeEgressStore 只实现 admin 需要的那四个方法，行为由字段控制。
type fakeEgressStore struct {
	lines      []EgressView
	accounts   []EgressAccountView
	probeErr   error
	probeCalls int
	saved      string
	bindErr    error
	bindUID    string
	bindID     string
}

func (f *fakeEgressStore) List(context.Context) ([]EgressView, []EgressAccountView) {
	return f.lines, f.accounts
}

func (f *fakeEgressStore) Probe(ctx context.Context, ids []string, force bool) ([]EgressView, error) {
	f.probeCalls++
	if f.probeErr != nil {
		return nil, f.probeErr
	}
	return f.lines, nil
}

func (f *fakeEgressStore) Save(raw json.RawMessage) ([]string, []string, error) {
	f.saved = string(raw)
	return []string{"egress"}, []string{"egress"}, nil
}

func (f *fakeEgressStore) Bind(uid, egressID string) error {
	f.bindUID, f.bindID = uid, egressID
	return f.bindErr
}

// TestEgressListEndpoint GET /admin/egress 下发线路 + 账号投影。
func TestEgressListEndpoint(t *testing.T) {
	st := &fakeEgressStore{
		lines:    []EgressView{{ID: "cn-1", Proxy: "socks5://***@1.2.3.4:1080", Bound: 1, MaxAccounts: 1}},
		accounts: []EgressAccountView{{UID: "u1", Channel: "cn", Egress: "cn-1"}},
	}
	h := New(Config{Egress: st})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminReq(http.MethodGet, "/admin/egress"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var doc struct {
		Lines    []EgressView        `json:"lines"`
		Accounts []EgressAccountView `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("不是 JSON: %v", err)
	}
	if len(doc.Lines) != 1 || doc.Lines[0].Bound != 1 {
		t.Fatalf("线路投影不符: %+v", doc.Lines)
	}
	if len(doc.Accounts) != 1 || doc.Accounts[0].Egress != "cn-1" {
		t.Fatalf("账号投影不符: %+v", doc.Accounts)
	}
}

// TestEgressProbeEndpoint 探活端点要真的调到 Store，并把结果原样下发。
func TestEgressProbeEndpoint(t *testing.T) {
	st := &fakeEgressStore{lines: []EgressView{{
		ID:    "cn-1",
		Probe: &EgressProbeView{OK: true, IP: "1.2.3.4", Country: "CN", LatencyMS: 88},
	}}}
	h := New(Config{Egress: st})
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/admin/egress/probe", strings.NewReader(`{"ids":["cn-1"],"force":true}`))
	r.RemoteAddr = "127.0.0.1:1"
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if st.probeCalls != 1 {
		t.Fatalf("应当调用一次 Probe，实际 %d", st.probeCalls)
	}
	if !strings.Contains(rec.Body.String(), "1.2.3.4") {
		t.Fatalf("探活结果没有下发: %s", rec.Body.String())
	}
}

// TestEgressSaveEndpoint PUT 保存后要明确告诉前端"需重启"。
func TestEgressSaveEndpoint(t *testing.T) {
	st := &fakeEgressStore{}
	h := New(Config{Egress: st})
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPut, "/admin/egress",
		strings.NewReader(`[{"id":"cn-1","proxy":"socks5://127.0.0.1:1080"}]`))
	r.RemoteAddr = "127.0.0.1:1"
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var doc struct {
		NeedRestart []string `json:"need_restart"`
	}
	json.Unmarshal(rec.Body.Bytes(), &doc)
	if len(doc.NeedRestart) != 1 || doc.NeedRestart[0] != "egress" {
		t.Fatalf("必须声明需要重启（egress 的 Transport 是装配期编译的）: %+v", doc)
	}
}

// TestAccountEgressBindingStatus 三类失败映射成三个状态码。
//
// 这条守的是"用户看到的话对不对"：满员是 409（换个线路就行），
// 不存在/地区不符是 400（配置写错了）。都回 500 会让人以为是服务端 bug。
func TestAccountEgressBindingStatus(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"成功", nil, http.StatusOK},
		{"满员", fmt.Errorf("%w：已有 1 个账号", ErrEgressFull), http.StatusConflict},
		{"线路不存在", fmt.Errorf("%w：%q", ErrEgressUnknown, "ghost"), http.StatusBadRequest},
		{"地区不符", fmt.Errorf("%w：cn 挂 intl", ErrEgressRealmMismatch), http.StatusBadRequest},
		{"其它错误", errors.New("磁盘写失败"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			st := &fakeEgressStore{bindErr: c.err}
			h := New(Config{Egress: st})
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/admin/accounts/u1/egress",
				strings.NewReader(`{"egress_id":"cn-1"}`))
			r.RemoteAddr = "127.0.0.1:1"
			h.ServeHTTP(rec, r)
			if rec.Code != c.want {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, c.want, rec.Body.String())
			}
			if st.bindUID != "u1" || st.bindID != "cn-1" {
				t.Fatalf("绑定参数没传到 Store: uid=%q id=%q", st.bindUID, st.bindID)
			}
		})
	}
}

// TestEgressNotWired 未接线时不注册路由（而不是回一条"未接线"）。
func TestEgressNotWired(t *testing.T) {
	h := New(Config{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminReq(http.MethodGet, "/admin/egress"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未接线时应当 404，实际 %d", rec.Code)
	}
}
