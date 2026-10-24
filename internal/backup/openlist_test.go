package backup

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// openListFixture 假 OpenList：/api/me、PUT /api/fs/put、POST /api/fs/list、GET /d/…
type openListFixture struct {
	mu     sync.Mutex
	puts   map[string]string
	header []string
	token  bool
}

func (f *openListFixture) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.header = append(f.header, r.Method+" "+r.URL.Path+" auth="+r.Header.Get("Authorization")+" file-path="+r.Header.Get("File-Path"))
		tokenOK := f.token
		f.mu.Unlock()
		if !tokenOK {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/me":
			io.WriteString(w, `{"code":200,"message":"success","data":{"id":1,"username":"admin"}}`)
		case r.Method == http.MethodPut && r.URL.Path == "/api/fs/put":
			b, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.puts[r.Header.Get("File-Path")] = string(b)
			f.mu.Unlock()
			io.WriteString(w, `{"code":200,"message":"success"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/fs/list":
			io.WriteString(w, `{"code":200,"message":"success","data":{"content":[
				{"name":"omni2api-backup-20261024-120000.zip","size":42,"is_dir":false,"modified":"2026-10-24T12:00:00Z"},
				{"name":"notes.md","size":5,"is_dir":false,"modified":"2026-10-24T12:00:00Z"}]}}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/d/"):
			io.WriteString(w, "ZIPDATA")
		default:
			w.WriteHeader(http.StatusNotImplemented)
		}
	})
}

func newTestOpenList(t *testing.T) (*OpenListChannel, *openListFixture) {
	t.Helper()
	fx := &openListFixture{puts: map[string]string{}, token: true}
	srv := httptest.NewServer(fx.handler())
	t.Cleanup(srv.Close)
	c, err := NewOpenListChannel("ol", OpenListOptions{
		Endpoint: srv.URL, Token: "secret-token", Dir: "/naibox-backups",
	})
	if err != nil {
		t.Fatalf("NewOpenListChannel: %v", err)
	}
	return c, fx
}

func TestOpenListPutListGet(t *testing.T) {
	c, fx := newTestOpenList(t)
	ctx := context.Background()

	if err := c.Test(ctx); err != nil {
		t.Fatalf("Test: %v", err)
	}
	name := "omni2api-backup-20261024-120000.zip"
	loc, err := c.Put(ctx, name, strings.NewReader("ZIPDATA"), int64(len("ZIPDATA")))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !strings.Contains(loc, "/d/naibox-backups/"+name) {
		t.Fatalf("返回位置不符: %s", loc)
	}
	if fx.puts["/naibox-backups/"+name] != "ZIPDATA" {
		t.Fatalf("远端内容不符: %v", fx.puts)
	}

	list, err := c.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].Name != name {
		t.Fatalf("应当只列出备份包，实际 %+v", list)
	}

	rc, err := c.Get(ctx, name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	body, _ := io.ReadAll(rc)
	rc.Close()
	if string(body) != "ZIPDATA" {
		t.Fatalf("取回内容不符: %q", body)
	}

	// 非备份包名不得被当作可读写对象（防误删/误读用户文件）
	if _, err := c.Get(ctx, "../etc/passwd"); err == nil {
		t.Fatal("非法文件名应当被拒绝")
	}
	if _, err := c.Put(ctx, "evil.sh", strings.NewReader("x"), 1); err == nil {
		t.Fatal("非备份包名不得上传")
	}
}

func TestOpenListTokenRejected(t *testing.T) {
	c, fx := newTestOpenList(t)
	fx.token = false
	err := c.Test(context.Background())
	if err == nil {
		t.Fatal("401 应当报错")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Fatalf("错误信息应当指向 token，实际: %v", err)
	}
}
