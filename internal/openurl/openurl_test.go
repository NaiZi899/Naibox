package openurl

import (
	"strings"
	"testing"
)

func TestValidateAcceptsHTTPAndHTTPS(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"https://github.com/NaiZi899/Naibox", "https://github.com/NaiZi899/Naibox"},
		{"http://example.com/a?b=1&c=2", "http://example.com/a?b=1&c=2"},
		{"  https://example.com/x  ", "https://example.com/x"},
	}
	for _, c := range cases {
		got, err := validate(c.raw)
		if err != nil {
			t.Errorf("应接受 %q: %v", c.raw, err)
			continue
		}
		if got != c.want {
			t.Errorf("validate(%q) = %q，期望 %q", c.raw, got, c.want)
		}
	}
}

func TestValidateRejectsNonHTTP(t *testing.T) {
	cases := []string{
		"", "file:///etc/passwd", "javascript:alert(1)", "ftp://example.com",
		"https://", "http://a b", "https://example.com/\r\nX-Test: 1",
		strings.Repeat("a", maxURLLength+1),
	}
	for _, raw := range cases {
		if _, err := validate(raw); err == nil {
			t.Errorf("应拒绝 %q", raw)
		}
	}
}
