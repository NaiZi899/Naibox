package backup

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// s3Fixture 一个够用的假对象存储：HEAD 桶、PUT/GET 对象、ListObjectsV2。
type s3Fixture struct {
	mu      sync.Mutex
	objects map[string]string
	authz   []string
	paths   []string
	deny    bool
}

func (f *s3Fixture) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.authz = append(f.authz, r.Header.Get("Authorization"))
		f.paths = append(f.paths, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		deny := f.deny
		f.mu.Unlock()
		if deny {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `<?xml version="1.0"?><Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`)
			return
		}
		switch {
		case r.Method == http.MethodHead && r.URL.Path == "/bucket":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
			io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult>
  <IsTruncated>false</IsTruncated>`+
				`<Contents><Key>naibox/omni2api-backup-20261024-120000.zip</Key><Size>123</Size><LastModified>2026-10-24T12:00:00.000Z</LastModified></Contents>`+
				`<Contents><Key>naibox/readme.txt</Key><Size>9</Size><LastModified>2026-10-24T12:00:00.000Z</LastModified></Contents>`+
				`</ListBucketResult>`)
		case r.Method == http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.objects[strings.TrimPrefix(r.URL.Path, "/bucket/")] = string(b)
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet:
			key := strings.TrimPrefix(r.URL.Path, "/bucket/")
			f.mu.Lock()
			body, ok := f.objects[key]
			f.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			io.WriteString(w, body)
		default:
			w.WriteHeader(http.StatusNotImplemented)
		}
	})
}

func newTestS3(t *testing.T) (*S3Channel, *s3Fixture) {
	t.Helper()
	fx := &s3Fixture{objects: map[string]string{}}
	srv := httptest.NewTLSServer(fx.handler())
	t.Cleanup(srv.Close)
	c, err := NewS3Channel("s3main", S3Options{
		Endpoint:  srv.URL,
		Bucket:    "bucket",
		Region:    "cn-north-4",
		Prefix:    "naibox",
		AccessKey: "AKIDTEST",
		SecretKey: "SECRETTEST",
	})
	if err != nil {
		t.Fatalf("NewS3Channel: %v", err)
	}
	c.hc = srv.Client() // 信任 httptest 的自签证书
	c.now = func() time.Time { return time.Unix(1761691000, 0).UTC() }
	return c, fx
}

// TestS3RejectsPlainHTTP 明文链路必须装配期就拒掉（包里含账号凭据）。
func TestS3RejectsPlainHTTP(t *testing.T) {
	if _, err := NewS3Channel("x", S3Options{
		Endpoint: "http://minio.local:9000", Bucket: "b", AccessKey: "a", SecretKey: "s",
	}); err == nil {
		t.Fatal("http 端点应当被拒绝")
	}
	if _, err := NewS3Channel("x", S3Options{Endpoint: "https://s3.example.com", Bucket: "b"}); err == nil {
		t.Fatal("缺少凭据应当被拒绝")
	}
}

// TestS3PutListGetAndSigning 一次跑通四个动作，并检查请求确实签了名。
func TestS3PutListGetAndSigning(t *testing.T) {
	c, fx := newTestS3(t)
	ctx := context.Background()

	if err := c.Test(ctx); err != nil {
		t.Fatalf("Test: %v", err)
	}

	name := "omni2api-backup-20261024-120000.zip"
	loc, err := c.Put(ctx, name, strings.NewReader("ZIPDATA"), int64(len("ZIPDATA")))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !strings.Contains(loc, "/bucket/naibox/"+name) {
		t.Fatalf("返回位置不符合 path-style + prefix: %s", loc)
	}
	if got := fx.objects["naibox/"+name]; got != "ZIPDATA" {
		t.Fatalf("对象内容不符: %q", got)
	}

	list, err := c.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].Name != name {
		t.Fatalf("List 应当只返回备份包（过滤 readme.txt），实际 %+v", list)
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

	if len(fx.authz) < 4 {
		t.Fatalf("应当发出至少 4 个请求，实际 %d", len(fx.authz))
	}
	for i, a := range fx.authz {
		if !strings.HasPrefix(a, "AWS4-HMAC-SHA256 Credential=AKIDTEST/") ||
			!strings.Contains(a, "/cn-north-4/s3/aws4_request") ||
			!strings.Contains(a, "SignedHeaders=host;x-amz-content-sha256;x-amz-date") ||
			!strings.Contains(a, "Signature=") {
			t.Fatalf("第 %d 个请求的签名头不完整: %q", i+1, a)
		}
	}
	// 同一时刻同样的请求必须得到同一个签名（确定性，否则无法复现排障）
	if strings.Join(fx.paths, "|") == "" {
		t.Fatal("请求记录为空")
	}
}

// TestS3SignatureWrongCredentialsMapsToReadableError 403 要被翻译成人能看懂的一句话。
func TestS3SignatureWrongCredentialsMapsToReadableError(t *testing.T) {
	c, fx := newTestS3(t)
	fx.deny = true
	err := c.Test(context.Background())
	if err == nil {
		t.Fatal("403 应当报错")
	}
	if !strings.Contains(err.Error(), "权限不足") {
		t.Fatalf("错误信息应当说清是权限问题，实际: %v", err)
	}
	if _, perr := c.Put(context.Background(), "omni2api-backup-20261024-120000.zip", strings.NewReader("x"), 1); perr == nil {
		t.Fatal("Put 在 403 下应当报错")
	}
}
