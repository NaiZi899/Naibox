// Package about 提供「关于软件」页的静态内容。
//
// 内容住在同目录的 about.json，编译期由 //go:embed 绑进二进制。
// 改介绍、项目地址或推荐项目时只改那个 JSON，然后重新编译即可，
// 不需要动任何 Go 代码。
package about

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
)

//go:embed about.json
var raw []byte

// Friend 推荐项目。只保留名字和地址，页面按卡片平铺。
type Friend struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// Content 关于页的全部数据。
type Content struct {
	Name        string   `json:"name"`
	Tagline     string   `json:"tagline"`
	Intro       []string `json:"intro"`
	ProjectURL  string   `json:"project_url"`
	Recommended []Friend `json:"recommended"`
}

var (
	once   sync.Once
	cached Content
	errVal error
)

// Load 解析并缓存内嵌的 about.json。
//
// 只在首次调用时解析。内容在编译期固定，运行期不会变，重复解析没有意义。
func Load() (Content, error) {
	once.Do(func() {
		cached, errVal = parse(raw)
	})
	return cached, errVal
}

func parse(data []byte) (Content, error) {
	var c Content
	if err := json.Unmarshal(data, &c); err != nil {
		return Content{}, fmt.Errorf("about.json 不是合法 JSON: %w", err)
	}
	c.Name = strings.TrimSpace(c.Name)
	c.Tagline = strings.TrimSpace(c.Tagline)
	c.ProjectURL = strings.TrimSpace(c.ProjectURL)
	if c.Name == "" {
		return Content{}, fmt.Errorf("about.json 缺少 name")
	}
	if c.Tagline == "" {
		return Content{}, fmt.Errorf("about.json 缺少 tagline")
	}
	if err := checkURL("project_url", c.ProjectURL); err != nil {
		return Content{}, err
	}
	intro := make([]string, 0, len(c.Intro))
	for i, p := range c.Intro {
		p = strings.TrimSpace(p)
		if p == "" {
			return Content{}, fmt.Errorf("about.json 的 intro[%d] 是空段落", i)
		}
		intro = append(intro, p)
	}
	if len(intro) == 0 {
		return Content{}, fmt.Errorf("about.json 至少需要一段 intro")
	}
	c.Intro = intro
	for i := range c.Recommended {
		c.Recommended[i].Name = strings.TrimSpace(c.Recommended[i].Name)
		c.Recommended[i].URL = strings.TrimSpace(c.Recommended[i].URL)
		if c.Recommended[i].Name == "" {
			return Content{}, fmt.Errorf("about.json 的 recommended[%d] 缺少 name", i)
		}
		if err := checkURL(fmt.Sprintf("recommended[%d].url", i), c.Recommended[i].URL); err != nil {
			return Content{}, err
		}
	}
	return c, nil
}

// checkURL 只放行 http/https，且必须有主机名。
//
// 关于页的链接会被后端交给系统浏览器打开，file:// 与 javascript: 不能进。
func checkURL(field, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("about.json 的 %s 不是合法地址: %w", field, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("about.json 的 %s 只支持 http/https", field)
	}
	if u.Host == "" {
		return fmt.Errorf("about.json 的 %s 缺少主机名", field)
	}
	return nil
}
