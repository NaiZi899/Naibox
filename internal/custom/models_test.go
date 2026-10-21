package custom

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NaiZi899/Naibox/internal/gateway"
)

// Models 的错误正文是**上游可控**的：实测有网关会在错误体里回显请求头里的
// key。这里钉住"先脱敏再截断"这条纪律 —— 反过来的话，跨过 200 字符边界的
// key 只会匹配到一半，剩下的前缀会被原样显示在管理面板与日志里。
func TestModelsErrorRedactsKey(t *testing.T) {
	const key = "sk-super-secret-key-9999"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"invalid key `+key+`"}`)
	}))
	defer srv.Close()

	ep := mustEndpoint(t, specFor(srv.URL))
	// specFor 用固定 key；这里把 key 换成带足够长度的哨兵，确保截断后仍可辨认。
	ep.APIKey = key
	p := New(ep, nil)
	_, err := p.Models(context.Background(), gateway.Credential{Provider: ep.ID, Secret: &ep})
	if err == nil {
		t.Fatal("401 应返回错误")
	}
	msg := err.Error()
	if strings.Contains(msg, key) {
		t.Fatalf("错误里出现了完整密钥: %s", msg)
	}
	if strings.Contains(msg, key[:8]) {
		t.Fatalf("错误里出现了密钥前缀: %s", msg)
	}
	if !strings.Contains(msg, MaskKey(key)) {
		t.Errorf("脱敏后应保留尾号 %s: %s", MaskKey(key), msg)
	}
}

// TestModelsErrorRedactsAcrossTruncation 密钥跨过截断边界时也不能漏前缀。
func TestModelsErrorRedactsAcrossTruncation(t *testing.T) {
	const key = "sk-boundary-secret-1234"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, strings.Repeat("a", 190)+key+"tail")
	}))
	defer srv.Close()

	ep := mustEndpoint(t, specFor(srv.URL))
	ep.APIKey = key
	p := New(ep, nil)
	_, err := p.Models(context.Background(), gateway.Credential{Provider: ep.ID, Secret: &ep})
	if err == nil {
		t.Fatal("500 应返回错误")
	}
	if strings.Contains(err.Error(), key[:8]) {
		t.Fatalf("截断边界处漏出了密钥前缀: %s", err.Error())
	}
}
