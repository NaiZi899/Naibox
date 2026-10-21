package about

import (
	"strings"
	"testing"
)

func TestEmbeddedContentIsValid(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatalf("内嵌 about.json 不合法: %v", err)
	}
	if c.Name == "" || c.Tagline == "" || c.ProjectURL == "" {
		t.Fatalf("必填字段为空: %+v", c)
	}
	if len(c.Intro) == 0 {
		t.Fatal("intro 为空")
	}
	for i, f := range c.Recommended {
		if f.Name == "" || f.URL == "" {
			t.Errorf("recommended[%d] 不完整: %+v", i, f)
		}
	}
}

func TestParseRejectsBadInput(t *testing.T) {
	good := `{"name":"A","tagline":"B","intro":["C"],"project_url":"https://example.com"}`
	cases := []struct {
		name string
		body string
		want string
	}{
		{"非法 JSON", `{`, "合法 JSON"},
		{"缺 name", `{"tagline":"B","intro":["C"],"project_url":"https://example.com"}`, "name"},
		{"缺 tagline", `{"name":"A","intro":["C"],"project_url":"https://example.com"}`, "tagline"},
		{"空 intro", `{"name":"A","tagline":"B","intro":[],"project_url":"https://example.com"}`, "intro"},
		{"file 协议", `{"name":"A","tagline":"B","intro":["C"],"project_url":"file:///etc/passwd"}`, "http/https"},
		{"缺主机", `{"name":"A","tagline":"B","intro":["C"],"project_url":"https://"}`, "主机名"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parse([]byte(c.body))
			if err == nil {
				t.Fatalf("应报错")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("错误里应包含 %q: %v", c.want, err)
			}
		})
	}
	if _, err := parse([]byte(good)); err != nil {
		t.Fatalf("合法输入被拒: %v", err)
	}
}

func TestParseTrimsAndFilters(t *testing.T) {
	c, err := parse([]byte(`{
	  "name":"  A  ","tagline":" B ","intro":[" C ","D"],
	  "project_url":" https://example.com/x ",
	  "recommended":[{"name":" R ","url":"https://example.org"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "A" || c.Tagline != "B" || c.ProjectURL != "https://example.com/x" {
		t.Errorf("首尾空白未清理: %+v", c)
	}
	if len(c.Intro) != 2 || c.Intro[0] != "C" {
		t.Errorf("intro 不符: %+v", c.Intro)
	}
	if c.Recommended[0].Name != "R" {
		t.Errorf("推荐项目未清理: %+v", c.Recommended[0])
	}
}
