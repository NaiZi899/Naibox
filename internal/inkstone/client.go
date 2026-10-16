// client.go 书生·端砚推理与管理 API 的 HTTP 客户端。
//
// # 两个 base 域（逆向实测，2026-10）
//
//	推理 API   https://discovery-api.intern-ai.org.cn/v1   OpenAI 兼容（Bearer API Key）
//	管理 API   https://discovery.intern-ai.org.cn          （Bearer SSO uaa-token，不是 API Key！）
//
// 推理（Chat / Models）用 API Key 即可广播鉴权。
// 余额（balance）是管理 API，用 **uaa-token**（SSO 登录后的用户令牌）而不是 API Key
// —— 实测用 API Key 调 balance 返回 `A0211 user token expired`。
package inkstone

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// 默认端点（可被 Config 覆盖）。
const (
	DefaultInferBase = "https://discovery-api.intern-ai.org.cn/v1"
	DefaultMgmtBase  = "https://discovery.intern-ai.org.cn"
)

// httpTimeout 单次上游请求总时限（推理流式由调用方 ctx 控制，这里给短请求与握手用）。
const httpTimeout = 120 * time.Second

// userTokenErrStr 余额查询缺用户令牌时的提示（当凭证只有 API Key 时）。
const userTokenErrStr = "额度未知：该账号只配置了 API Key，未配置 SSO 用户令牌；请补 user_token 后重查"

// apiBase 推理端点（去尾斜杠）。
func apiBase() string { return strings.TrimRight(DefaultInferBase, "/") }

// mgmtBase 管理端点（去尾斜杠）。
func mgmtBase() string { return strings.TrimRight(DefaultMgmtBase, "/") }

// chatRequest 转发一次对话：body 原样 POST 到推理 API（model 已是裸名），
// 用 API Key 鉴权，透传状态码与流。
// 返回受控错误（网络层），业务错误（非 2xx）仍在 resp 里由调用方判断。
func chatRequest(ctx context.Context, apiKey string, body []byte) (*http.Response, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("inkstone: 凭证缺少 api_key")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		apiBase()+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("inkstone: 构造请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
	cli := &http.Client{Timeout: httpTimeout}
	return cli.Do(req)
}

// modelList 拉取推理端点实时模型目录（供诊断）。
func modelList(ctx context.Context, apiKey string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		apiBase()+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
	cli := &http.Client{Timeout: 30 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("inkstone: models http %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var d struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	var out []string
	for _, m := range d.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	return out, nil
}

// balance 查询墨点余额。
//
// ⚠ 需要 SSO uaa-token（用户令牌），不是 API Key。
// 返回可用的墨点余额（available_credits，字符串形式，可能是小数）。
// 失败返回 HasData=false 语义（由调用方决定界面上是否显示 `—`）。
func balance(ctx context.Context, userToken string) (*Balance, error) {
	if strings.TrimSpace(userToken) == "" {
		return nil, errors.New(userTokenErrStr)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		mgmtBase()+"/api/tokenplan/v1/credits/balance", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(userToken))
	cli := &http.Client{Timeout: 30 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var wrap struct {
		Code int `json:"code"`
		Data struct {
			AvailableCredits string `json:"available_credits"`
			RPMLimit         int    `json:"rpm_limit"`
			TPMLimit         int    `json:"tpm_limit"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return nil, err
	}
	if wrap.Code != 0 {
		return nil, fmt.Errorf("inkstone: balance code=%d body=%s", wrap.Code, strings.TrimSpace(string(raw)))
	}
	b := &Balance{
		AvailableCredits: strings.TrimSpace(wrap.Data.AvailableCredits),
		RPMLimit:         wrap.Data.RPMLimit,
		TPMLimit:         wrap.Data.TPMLimit,
	}
	return b, nil
}

// Balance 墨点余额（来自管理 API）。
type Balance struct {
	// AvailableCredits 可选墨点（字符串，可能是小数如 "10.000000"）。
	AvailableCredits string
	// RPMLimit 每分钟请求上限。
	RPMLimit int
	// TPMLimit 每分钟 token 上限。
	TPMLimit int
}

// ParseAvailableCredits 把 available_credits 字符串解析成整数墨点（向下取整）。
// 解析失败返回 0 与错误（调用方据此回落 UnknownQuota）。
func ParseAvailableCredits(s string) (int64, error) {
	if strings.TrimSpace(s) == "" {
		return 0, errors.New("empty available_credits")
	}
	// 形如 "10.000000" / "8.5" / "0.000000"
	whole := s
	if i := strings.IndexByte(s, '.'); i >= 0 {
		whole = s[:i]
	}
	var n int64
	if _, err := fmt.Sscanf(strings.TrimSpace(whole), "%d", &n); err != nil {
		return 0, err
	}
	return n, nil
}
