// manual.go 把内嵌的使用说明（manual.md）渲染成关于页要的 HTML。
//
// # 为什么自己写一个小渲染器
//
// 关于页在 webui.html 里，页面本身没有 Markdown 依赖；为了显示一份说明去引入
// 一个前端 Markdown 库（还要处理它的 XSS 面），代价大于收益。这里只支持说明书
// 真正用到的语法：标题、无序列表、表格、围栏代码块、行内代码、粗体、链接、段落。
//
// # 分章
//
// 每个二级标题会被包成 `<div class="md-sec">`：关于页按「三章一页」分页，
// 分页单位是章 —— 按字数切会把一段话劈成两半。
//
// # 安全
//
// 内容是我们自己内嵌的 md 文件，不是用户输入。即便如此也逐段做 HTML 转义，
// 只放出固定的一组标签 —— 说明书里一旦被人粘进 `<script>`，那是发布事故。
package about

import (
	_ "embed"
	"html"
	"strings"
)

//go:embed manual.md
var manualMarkdown string

// ManualMarkdown 返回内嵌的说明原文（测试与排障用）。
func ManualMarkdown() string { return manualMarkdown }

// ManualHTML 返回渲染后的 HTML。
func ManualHTML() string { return renderMarkdown(manualMarkdown) }

// renderMarkdown 行级状态机：代码块 / 表格 / 列表 / 标题 / 段落。
//
// 不做嵌套列表：说明书里没有用到，支持它只会扩大渲染面。
func renderMarkdown(md string) string {
	md = strings.ReplaceAll(md, "\r\n", "\n")
	lines := strings.Split(md, "\n")

	var b strings.Builder
	inCode, inList, inTable, tableHead, inSection := false, false, false, false, false
	var para []string

	flushPara := func() {
		if len(para) == 0 {
			return
		}
		b.WriteString("<p>" + inline(strings.Join(para, " ")) + "</p>\n")
		para = para[:0]
	}
	closeList := func() {
		if inList {
			b.WriteString("</ul>\n")
			inList = false
		}
	}
	closeTable := func() {
		if inTable {
			b.WriteString("</table>\n")
			inTable, tableHead = false, false
		}
	}
	closeBlocks := func() {
		flushPara()
		closeList()
		closeTable()
	}
	closeSection := func() {
		if inSection {
			b.WriteString("</div>\n")
			inSection = false
		}
	}

	for _, raw := range lines {
		line := strings.TrimRight(raw, " \t")
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			closeBlocks()
			if inCode {
				b.WriteString("</code></pre>\n")
				inCode = false
			} else {
				b.WriteString("<pre><code>")
				inCode = true
			}
			continue
		}
		if inCode {
			b.WriteString(html.EscapeString(line))
			b.WriteByte('\n')
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			closeBlocks()
			continue
		}
		switch {
		case strings.HasPrefix(trimmed, "### "):
			closeBlocks()
			b.WriteString("<h4>" + inline(strings.TrimPrefix(trimmed, "### ")) + "</h4>\n")
		case strings.HasPrefix(trimmed, "## "):
			closeBlocks()
			// 二级标题 = 一章 = 分页单位。
			closeSection()
			b.WriteString("<div class=\"md-sec\">")
			inSection = true
			b.WriteString("<h3>" + inline(strings.TrimPrefix(trimmed, "## ")) + "</h3>\n")
		case strings.HasPrefix(trimmed, "# "):
			closeBlocks()
			closeSection()
			b.WriteString("<h2>" + inline(strings.TrimPrefix(trimmed, "# ")) + "</h2>\n")
		case strings.HasPrefix(trimmed, "|"):
			flushPara()
			closeList()
			if !inTable {
				b.WriteString("<table>\n")
				inTable, tableHead = true, true
			}
			if isTableDivider(trimmed) {
				tableHead = false
				continue
			}
			tag := "td"
			if tableHead {
				tag = "th"
			}
			b.WriteString("<tr>")
			for _, c := range splitTableRow(trimmed) {
				b.WriteString("<" + tag + ">" + inline(c) + "</" + tag + ">")
			}
			b.WriteString("</tr>\n")
		case strings.HasPrefix(trimmed, "- "):
			flushPara()
			closeTable()
			if !inList {
				b.WriteString("<ul>\n")
				inList = true
			}
			b.WriteString("<li>" + inline(strings.TrimPrefix(trimmed, "- ")) + "</li>\n")
		default:
			closeTable()
			para = append(para, trimmed)
		}
	}
	closeBlocks()
	closeSection()
	if inCode { // 文末未闭合的代码块：补上，宁可有瑕疵也不吐半个标签
		b.WriteString("</code></pre>\n")
	}
	return b.String()
}

// inline 行内语法：先转义，再替换代码/粗体/链接。
//
// 顺序不能反：先替换再加标签，转义会把刚生成的 `<strong>` 也转掉。
func inline(s string) string {
	out := html.EscapeString(s)
	out = replacePairs(out, "`", "`", "<code>", "</code>")
	out = replacePairs(out, "**", "**", "<strong>", "</strong>")
	out = renderLinks(out)
	return out
}

// replacePairs 把 `open … close` 形式的成对标记换成标签（从左到右，不成对就原样留着）。
func replacePairs(s, open, close, openTag, closeTag string) string {
	var b strings.Builder
	rest := s
	for {
		i := strings.Index(rest, open)
		if i < 0 {
			b.WriteString(rest)
			return b.String()
		}
		j := strings.Index(rest[i+len(open):], close)
		if j < 0 {
			b.WriteString(rest)
			return b.String()
		}
		b.WriteString(rest[:i])
		b.WriteString(openTag)
		b.WriteString(rest[i+len(open) : i+len(open)+j])
		b.WriteString(closeTag)
		rest = rest[i+len(open)+j+len(close):]
	}
}

// renderLinks 把 [文字](https://…) 换成 <a>。
//
// 只认 http/https：说明里出现的都是项目地址与文档链接，别的协议（javascript:、
// file:）一律当普通文本留着。
func renderLinks(s string) string {
	var b strings.Builder
	rest := s
	for {
		i := strings.Index(rest, "[")
		if i < 0 {
			b.WriteString(rest)
			return b.String()
		}
		j := strings.Index(rest[i:], "](")
		if j < 0 {
			b.WriteString(rest)
			return b.String()
		}
		k := strings.Index(rest[i+j:], ")")
		if k < 0 {
			b.WriteString(rest)
			return b.String()
		}
		text := rest[i+1 : i+j]
		url := rest[i+j+2 : i+j+k]
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			b.WriteString(rest[:i+1])
			rest = rest[i+1:]
			continue
		}
		b.WriteString(rest[:i])
		b.WriteString(`<a href="` + url + `" target="_blank" rel="noreferrer">` + text + `</a>`)
		rest = rest[i+j+k+1:]
	}
}

// isTableDivider 判断 `|---|---|` 这种分隔行（它只表达"上一行是表头"）。
func isTableDivider(line string) bool {
	body := strings.TrimSpace(strings.Trim(line, "|"))
	if body == "" {
		return false
	}
	hasDash := false
	for _, r := range body {
		switch r {
		case '-':
			hasDash = true
		case ':', '|', ' ':
		default:
			return false
		}
	}
	return hasDash
}

// splitTableRow 拆一行表格（去掉首尾竖线，按 | 切）。
func splitTableRow(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	parts := strings.Split(line, "|")
	out := make([]string, 0, len(parts))
	for _, one := range parts {
		out = append(out, strings.TrimSpace(one))
	}
	return out
}
