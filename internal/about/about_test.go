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

// TestManualRenders 说明书必须渲染出可用的 HTML：标题、列表、代码块、行内代码都在。
func TestManualRenders(t *testing.T) {
	html := ManualHTML()
	if html == "" {
		t.Fatal("说明渲染为空")
	}
	for _, want := range []string{"<h2>", "<h3>", "<ul>", "<li>", "<pre><code>", "<code>"} {
		if !strings.Contains(html, want) {
			t.Fatalf("渲染结果缺 %s", want)
		}
	}
	// 说明书里的关键小节不能丢（这几节是用户真正要查的）
	for _, section := range []string{"接客户端", "出问题怎么查", "已知限制", "安全提醒"} {
		if !strings.Contains(html, section) {
			t.Fatalf("说明书缺小节：%s", section)
		}
	}
	// 未转义的尖括号会直接变成标签 —— 逐段转义之后应当只剩下我们生成的标签
	if strings.Contains(html, "<script") {
		t.Fatal("渲染结果里出现了 script 标签")
	}
}

// TestInlineEscapes 行内渲染必须先转义再加标签。
func TestInlineEscapes(t *testing.T) {
	got := inline(`<script>alert(1)</script> 与 **粗体** 和 ` + "`代码`")
	if strings.Contains(got, "<script") {
		t.Fatalf("未转义: %s", got)
	}
	if !strings.Contains(got, "&lt;script&gt;") {
		t.Fatalf("应当保留为文本: %s", got)
	}
	if !strings.Contains(got, "<strong>粗体</strong>") || !strings.Contains(got, "<code>代码</code>") {
		t.Fatalf("行内标记没生效: %s", got)
	}
}

// TestRenderLinksOnlyAllowsHTTP 只放行 http/https。
func TestRenderLinksOnlyAllowsHTTP(t *testing.T) {
	ok := inline("[项目](https://example.com/x)")
	if !strings.Contains(ok, `<a href="https://example.com/x"`) {
		t.Fatalf("https 链接应当渲染: %s", ok)
	}
	bad := inline("[点我](javascript:alert(1))")
	if strings.Contains(bad, "<a ") {
		t.Fatalf("javascript: 不该变成链接: %s", bad)
	}
}

// TestMarkdownCodeBlockEscaped 代码块内容也要转义（里面有 < > 是常态）。
func TestMarkdownCodeBlockEscaped(t *testing.T) {
	got := renderMarkdown("```\ncurl -H \"A: <b>\" x\n```\n")
	if strings.Contains(got, "<b>") {
		t.Fatalf("代码块内容未转义: %s", got)
	}
	if !strings.Contains(got, "&lt;b&gt;") {
		t.Fatalf("代码块应当转义成实体: %s", got)
	}
}

// TestManualRendersTable 说明书里的"现象 / 多半是"那张表必须渲染成真表格：
// 退化成一行行竖线文本，排错时几乎读不了。
func TestManualRendersTable(t *testing.T) {
	html := ManualHTML()
	if !strings.Contains(html, "<table>") || !strings.Contains(html, "<th>") {
		t.Fatal("表格没有渲染成 <table>/<th>")
	}
	if !strings.Contains(html, "401") && !strings.Contains(html, "413") {
		t.Fatal("表格内容丢了")
	}
	if strings.Contains(html, "|---") {
		t.Fatal("分隔行不该出现在输出里")
	}
	// 未闭合/错位时表头标记不能泄漏到正文
	if strings.Contains(html, "<td>---") {
		t.Fatal("分隔行被当成数据行")
	}
}

// TestManualSectionsPerPage 分页单位是「章」：渲染结果里每章一个 .md-sec，
// 章数必须与 md 里的二级标题数一致（少一个就会把两章并到一页）。
func TestManualSectionsPerPage(t *testing.T) {
	md := ManualMarkdown()
	wantSec := strings.Count(md, "\n## ")
	got := strings.Count(ManualHTML(), `<div class="md-sec">`)
	if wantSec == 0 {
		t.Fatal("说明书里没有二级标题 —— 分页会退化成整篇一页")
	}
	if got != wantSec {
		t.Fatalf("分章数不一致：md 有 %d 个二级标题，渲染出 %d 个 .md-sec", wantSec, got)
	}
	// 三章一页：章数应当能整除 3，页码才不会出现半页（12 章 → 4 页）
	if wantSec%3 != 0 {
		t.Logf("提示：当前 %d 章，按三章一页最后一页只有 %d 章", wantSec, wantSec%3)
	}
	if !strings.Contains(ManualHTML(), "<h2>NaiBox 使用说明</h2>") {
		t.Fatal("一级标题（页头）不该被卷进分页")
	}
}
