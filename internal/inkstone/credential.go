// Package inkstone 把书生·端砚（Intern InkStone）TokenPlan 适配为 gateway.Provider。
//
// # 上游概况（2026-10 逆向实测）
//
//	推理 API   https://discovery-api.intern-ai.org.cn/v1   OpenAI 兼容（Bearer API Key）
//	管理 API   https://discovery.intern-ai.org.cn        （注意：在 discovery，不在 discovery-api）
//	凭证       每账号一个 API Key（sk-…，默认有效 6 个月）
//	额度       墨点（每月免费送 10 墨点，5h 限 10 / 7d 限 50）
//	登录       浏览器过 SSO（手机号/微信 + 阿里云滑块）后，在 TokenPlan 页建 Key
//
// # 与 Loomy 的相似性（本包刻意贴近它的结构）
//
//	凭证无 TTL、无 refresh token → 不实现 CredentialRefresher（同 loomy 的逻辑）
//	额度单值（墨点）            → QuotaExt 返回 CreditsQuota
//	多账号各持独立 Key          → 每 Key 一个 Auth，pool 轮转
//
// # 多账号怎么进池
//
// 每个账号一份 Auth，落盘 `auths/inkstone/inkstone-<uid>.json`（uid 由 key 派生，稳定）。
// 核心通过 CredentialLoader(+Secret) 扫描并入池；Chat 按 uid 取对应 key 直连推理 API。
// 健康：invalid_api_key → 会话失效（禁用）；额度耗尽/过载 → 冷却。
package inkstone

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// filePrefix 凭证文件名前缀。LoadDir 按 `inkstone-*.json` 通配。
// 各上游在自己的目录按自己的前缀扫，避免把别家凭证读进来。
const filePrefix = "inkstone"

// fileSuffix 凭证文件后缀（与 workbuddy/loomy 的 `<prefix>-<uid>.json` 一致）。
const fileSuffix = ".json"

// Auth 书生端砚一份凭证。
//
// 字段：
//
//	APIKey   sk-… 唯一鉴权材料（推理与管理通用，Bearer）
//	UID      账号池主键（由 APIKey 确定性派生，见 deriveUID）
//	Nickname 展示名（可为空，界面按 UID 前缀显示）
//	Telephone 登录手机号/微信号说明（仅展示）
//	FilePath 凭证文件位置（加载时记录，不参与序列化；删除账号要连文件移除）
type Auth struct {
	APIKey    string `json:"api_key"`
	UID       string `json:"uid"`
	Nickname  string `json:"nickname,omitempty"`
	Telephone string `json:"telephone,omitempty"`
	Source    string `json:"source,omitempty"` // 微信登录 / 手机号登录（仅展示，探明登录方式）
	// UserToken 可选：SSO 用户令牌（uaa-token），用于查墨点余额。
	// ⚠ 推理只认 APIKey；余额是管理 API，只认 uaa-token（逆向实测：
	// 用 APIKey 调 balance 返回 `A0211 user token expired`）。
	// 缺省不配表额度，/admin/accounts 的额度列显示 `—`。
	UserToken string `json:"user_token,omitempty"`
	FilePath  string `json:"-"`
}

// deriveUID 由 APIKey 派生稳定 UID。
//
// 与 loomy 同思路：取 sha256 前 16 位 hex，避免把完整 key 当账号标识出现在
// 日志/界面，且同一 key 每次派生结果一致（否则池子里堆幽灵条目）。
func deriveUID(apiKey string) string {
	if apiKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(apiKey)))
	return hex.EncodeToString(sum[:8])
}

// NewAuth 从 APIKey 构造一份凭证（确定昵称缺省时用派生 UID 前缀）。
func NewAuth(apiKey, nickname, telephone string) *Auth {
	key := strings.TrimSpace(apiKey)
	a := &Auth{
		APIKey:    key,
		UID:       deriveUID(key),
		Nickname:  strings.TrimSpace(nickname),
		Telephone: strings.TrimSpace(telephone),
	}
	if a.Nickname == "" {
		// 没给昵称就用 UID 前缀展示，至少能区分不同号。
		a.Nickname = shortUID(a.UID)
	}
	return a
}

// shortUID 返回 uid 前 8 位（日志与缺省昵称用，脱敏）。
func shortUID(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

// MarshalAuthFile 序列化一份凭证为磁盘上的 JSON 字节。
func MarshalAuthFile(a *Auth) ([]byte, error) {
	return json.MarshalIndent(struct {
		APIKey    string `json:"api_key"`
		UID       string `json:"uid"`
		Nickname  string `json:"nickname,omitempty"`
		Telephone string `json:"telephone,omitempty"`
		Source    string `json:"source,omitempty"`
		UserToken string `json:"user_token,omitempty"`
	}{
		APIKey:    a.APIKey,
		UID:       a.UID,
		Nickname:  a.Nickname,
		Telephone: a.Telephone,
		Source:    a.Source,
		UserToken: a.UserToken,
	}, "", "  ")
}

// ParseCredential 把磁盘上的 JSON 解析回 Auth（集成导入要走它）。
//
// 幂等、无副作用：不写盘、不碰账号池——只做"文本 → 凭证"。
func ParseCredential(raw []byte) (*Auth, error) {
	var a Auth
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("inkstone: 解析凭证失败: %w", err)
	}
	if strings.TrimSpace(a.APIKey) == "" {
		return nil, fmt.Errorf("inkstone: 凭证缺少 api_key")
	}
	a.APIKey = strings.TrimSpace(a.APIKey)
	if a.UID == "" {
		a.UID = deriveUID(a.APIKey)
	}
	return &a, nil
}

// AuthFileName 返回一份凭证应落盘的文件名（不含目录）。
func AuthFileName(a *Auth) string {
	base := a.UID
	if base == "" {
		base = deriveUID(a.APIKey)
	}
	return filePrefix + "-" + base + fileSuffix
}

// loadDir 读取目录下全部本上游凭证。
func loadDir(dir string) ([]*Auth, error) {
	files, err := filepath.Glob(filepath.Join(dir, filePrefix+"-*"+fileSuffix))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			// 单个文件读失败：记录但不中断（少读一个号不该让整目录失败）。
			log.Printf("inkstone: 读取凭证 %s 失败: %v", filepath.Base(f), err)
			continue
		}
		a, perr := ParseCredential(raw)
		if perr != nil {
			// 解析失败：单文件跳过并留痕（否则"少了一个号"无法排查）。
			log.Printf("inkstone: 解析凭证 %s 失败: %v", filepath.Base(f), perr)
			continue
		}
		a.FilePath = f
		out = append(out, a)
	}
	return out, nil
}

// LoadDir 读取目录下全部本上游凭证（供装配层与测试用）。
func LoadDir(dir string) ([]*Auth, error) { return loadDir(dir) }

// UIDOf 返回某 APIKey 的账号池主键。
func UIDOf(apiKey string) string { return deriveUID(apiKey) }

// compile-time: Auth 承载 file 前缀约定（与同结构上游对齐）。
var _ = filePrefix
