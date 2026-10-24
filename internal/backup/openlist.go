package backup

// openlist.go 走 OpenList / AList 的 HTTP API。
//
// # 为什么选它作为第三方通道
//
// 它把"网盘"抽象成一个统一的 HTTP 目录树：用户在自己那边挂好百度/阿里/
// OneDrive，这里只需要 PUT 一个文件到某个路径。相比逐个云盘写 SDK，这套
// 接口只有三条命令（`/api/fs/put`、`/api/fs/list`、`/d/…`），且不绑定任何
// 具体服务商 —— 与"通道可插拔"的判据一致。
//
// # 凭据
//
// OpenList 的 token 是长期有效的访问令牌（在它的「其他设置 → 令牌」里生成）。
// 本包只把它放进 Authorization 头，不落任何缓存。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OpenListOptions 一条 OpenList 通道的配置。
type OpenListOptions struct {
	Endpoint string // http://127.0.0.1:5244
	Token    string
	Dir      string // 远端目录（默认 /naibox-backups）
}

// OpenListChannel OpenList 通道。
type OpenListChannel struct {
	id   string
	opt  OpenListOptions
	hc   *http.Client
	now  func() time.Time
	keep int
}

// NewOpenListChannel 构造。
//
// 端点允许 http：OpenList 绝大多数部署在局域网/本机（默认 5244），
// 强求 https 会让这个通道在真实场景里没法用。凭据风险由"只填内网地址"
// 这条使用约定承担 —— 管理台的提示里写明了。
func NewOpenListChannel(id string, opt OpenListOptions) (*OpenListChannel, error) {
	if strings.TrimSpace(opt.Endpoint) == "" {
		return nil, errors.New("openlist: endpoint 为空")
	}
	if strings.TrimSpace(opt.Token) == "" {
		return nil, errors.New("openlist: token 为空")
	}
	if _, err := url.Parse(opt.Endpoint); err != nil {
		return nil, fmt.Errorf("openlist: endpoint 解析失败: %w", err)
	}
	dir := strings.TrimSpace(opt.Dir)
	if dir == "" {
		dir = "/naibox-backups"
	}
	if !strings.HasPrefix(dir, "/") {
		dir = "/" + dir
	}
	opt.Dir = strings.TrimRight(dir, "/")
	opt.Endpoint = strings.TrimRight(opt.Endpoint, "/")
	return &OpenListChannel{
		id:   id,
		opt:  opt,
		hc:   &http.Client{Timeout: 15 * time.Minute},
		now:  time.Now,
		keep: 10,
	}, nil
}

func (c *OpenListChannel) ID() string   { return c.id }
func (c *OpenListChannel) Kind() string { return "openlist" }
func (c *OpenListChannel) SetKeep(n int) {
	if n > 0 {
		c.keep = n
	}
}

// apiEnvelope OpenList 的统一回执：{code, message, data}。code != 200 即失败。
type apiEnvelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// Test 取当前用户信息（`/api/me`）—— 它同时验证"服务在"与"token 有效"。
func (c *OpenListChannel) Test(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.opt.Endpoint+"/api/me", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.opt.Token)
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("连接失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return errors.New("token 被拒绝（401/403）：在 OpenList「其他设置 → 令牌」里重新生成")
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("端点返回 %s: %s", resp.Status, snippet(raw))
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("响应不是 OpenList 的 JSON（endpoint 填错了？）: %w", err)
	}
	if env.Code != 200 {
		return fmt.Errorf("接口报错 code=%d: %s", env.Code, env.Message)
	}
	return nil
}

// Put 上传一个包。
//
// 接口是 `PUT /api/fs/put`，目标路径走 `File-Path` 头（不是 body），
// body 就是文件本体。`As-Task: false` 要求同步完成 —— 备份只有一份，
// 我们需要"返回 200 时对象真的在了"。
func (c *OpenListChannel) Put(ctx context.Context, name string, r io.Reader, size int64) (string, error) {
	if !isBackupName(name) {
		return "", errors.New("不是备份包文件名")
	}
	target := c.opt.Dir + "/" + name
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.opt.Endpoint+"/api/fs/put", ctxReader(ctx, r))
	if err != nil {
		return "", err
	}
	if size > 0 {
		req.ContentLength = size
	}
	req.Header.Set("Authorization", c.opt.Token)
	req.Header.Set("File-Path", target)
	req.Header.Set("As-Task", "false")
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("上传失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("上传被拒绝（%s）: %s", resp.Status, snippet(raw))
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err == nil && env.Code != 200 {
		return "", fmt.Errorf("接口报错 code=%d: %s", env.Code, env.Message)
	}
	return c.opt.Endpoint + "/d" + target, nil
}

// List 列远端目录里已有的备份包（新的在前）。
func (c *OpenListChannel) List(ctx context.Context) ([]Object, error) {
	body, _ := json.Marshal(map[string]any{
		"path": c.opt.Dir, "password": "", "page": 1, "per_page": 200, "refresh": false,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.opt.Endpoint+"/api/fs/list", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", c.opt.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("列举失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("列举被拒绝（%s）: %s", resp.Status, snippet(raw))
	}
	var env struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Content []struct {
				Name     string `json:"name"`
				Size     int64  `json:"size"`
				IsDir    bool   `json:"is_dir"`
				Modified string `json:"modified"`
			} `json:"content"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("列举响应无法解析: %w", err)
	}
	if env.Code != 200 {
		// 目录不存在时 OpenList 也会走这里（code=500）。当作"还没有备份"，
		// 不当作错误 —— 首次备份前查列表是正常操作。
		if strings.Contains(strings.ToLower(env.Message), "not exist") ||
			strings.Contains(env.Message, "不存在") {
			return nil, nil
		}
		return nil, fmt.Errorf("接口报错 code=%d: %s", env.Code, env.Message)
	}
	var out []Object
	for _, it := range env.Data.Content {
		if it.IsDir || !isBackupName(it.Name) {
			continue
		}
		t, _ := time.Parse(time.RFC3339, it.Modified)
		if t.IsZero() {
			t, _ = time.Parse("2006-01-02T15:04:05.000Z07:00", it.Modified)
		}
		out = append(out, Object{Name: it.Name, Size: it.Size, ModTime: t})
	}
	sortNewestFirst(out)
	return out, nil
}

// Get 取回一个包。
func (c *OpenListChannel) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	if !isBackupName(name) {
		return nil, errors.New("不是备份包文件名")
	}
	u := c.opt.Endpoint + "/d" + c.opt.Dir + "/" + url.PathEscape(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", c.opt.Token)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载失败: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		return nil, fmt.Errorf("下载被拒绝（%s）: %s", resp.Status, snippet(raw))
	}
	return resp.Body, nil
}

// snippet 截断响应体用于错误信息（避免把整页 HTML 灌进日志）。
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	if s == "" {
		return "(空响应体)"
	}
	return s
}
