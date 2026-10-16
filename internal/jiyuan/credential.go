// Package jiyuan 把 基元律动（TokenRhythm, tokenrhythm.studio）适配为 gateway.Provider。
//
// # 上游概况（2026-10 实测）
//
//	推理 API   https://tokenrhythm.studio/v1   OpenAI 兼容（Bearer API Key）
//	模型目录   GET  /v1/models                 25 个，带定价/上下文/能力位
//	对话       POST /v1/chat/completions       同时支持 /v1/messages（Anthropic）
//	凭证       每账号一个 API Key（sk_…，无 TTL、无 refresh token）
//
// # 本包刻意不实现的两件事（都是实测结论，不是偷懒）
//
//  1. 页内登录（LoginFlow）。上号形态是"用户手上已有一批 key"，
//     唯一入口是批量粘贴（见 import.go 的 `CapImport`）。
//
//  2. CredentialRefresher。key 无 TTL，没有可刷的东西；
//     实现它只会让后台每分钟空转一遍。
//
// # 额度（额度列那个 `—`）
//
// 本包**实现** gateway.QuotaExt（见 quota_ext.go）。额度只能靠登录凭证
// （`tr_session`）读 —— 实测：单把 `sk_` 打管理面全部 401，`/v1/*` 下
// 没有任何余额端点。所以有 session 的账号实时读余额，没有的显示未知。
//
// 它**独立**走基元律动的接口，不与其它上游共用任何余额路径
// （用户明确要求：「jiyuan 这个模块不要跟其他上游共用余额接口」）。
//
// # 多账号怎么进池
//
// 每把 key 一份凭证，落盘 `auths/jiyuan/jiyuan-<uid>.json`，uid 由 key
// 确定性派生（见 deriveUID —— 为什么必须派生而不是取上游账号 id：
// 拿 key 调不到 `/api/me`，账号身份根本取不到）。
// 核心通过 CredentialLoader(+Secret) 扫描入池；Chat 按 uid 取对应 key 直连。
//
// # 健康语义
//
//	余额不足（402）  → ErrKindHardCredit：冷却 + 立即换号（确定性失败，不必重试）
//	key 失效（401）  → ErrKindSessionDead：连续 3 次后禁用（需换一把新 key）
//	其他 4xx        → ErrKindClient：只换号不罚
package jiyuan

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

// filePrefix 凭证文件名前缀。LoadDir 按 `jiyuan-*.json` 通配。
// 各上游在自己的目录按自己的前缀扫，避免把别家凭证读进来。
const filePrefix = "jiyuan"

// fileSuffix 凭证文件后缀（与 workbuddy/loomy/inkstone 的
// `<prefix>-<uid>.json` 一致）。
const fileSuffix = ".json"

// Auth 基元律动一份凭证。
//
// 字段刻意只有三个：这个上游的凭证就是一把 key，多一个字段都是"看起来
// 有用但取不到值"的东西（账号 id / 手机号都要 session 才拿得到）。
type Auth struct {
	// APIKey sk_… 唯一鉴权材料（Bearer），无 TTL。
	APIKey string `json:"api_key"`
	// UID 账号池主键，由 APIKey 确定性派生（见 deriveUID）。
	UID string `json:"uid"`
	// Nickname 展示名。批量导入时可由用户同行给出（`<key> <标签>`）；
	// 留空则回落 UID 前缀 —— 至少能区分不同号。
	Nickname string `json:"nickname,omitempty"`
	// PhoneTail 手机号后四位（可空）。
	//
	// 只在**用登录凭证导入**时才有（那时能调 `/api/me`）。它的价值是
	// 让用户分辨"这把 key 是哪个号的"—— 一把 sk_ 看不出归属，
	// 而四个尾号能对上他手机里的号。
	PhoneTail string `json:"phone_tail,omitempty"`
	// Balance 余额快照（可空，0 表示未知）。
	//
	// ⚠ 这是**导入那一刻的快照**，不是实时余额。
	//
	// 余额的**实时**读取需要 Session（见下）。有 Session 的账号走
	// QuotaExt 实时读；没有的账号只能显示这个历史值（或未知）。
	//
	// 用它做实时展示是错的 —— 那会显示一个永远不更新的数字，
	// 用户以为余额没变，实际早已花光。
	Balance float64 `json:"balance,omitempty"`
	// Session 登录凭证（`tr_session`，sess_… 开头，可空）。
	//
	// # 为什么这个字段必须存在（否则余额列永远是 `—`）
	//
	// 基元律动的两个面鉴权不同（实测）：
	//
	//	调用面 /v1/*   只认 `Authorization: Bearer sk_…`
	//	管理面 /api/*  只认 `Cookie: tr_session=sess_…`
	//
	// 我实测穷举过：拿单把 `sk_` 打 `/api/usage-summary`、`/api/me`、
	// `/api/api-keys` **全部 401**；`/v1/*` 下也没有任何余额端点
	//（`/v1/credits`、`/v1/balance`、`/dashboard/billing/*` 全 404 或要求登录）；
	// 推理响应里也只有 `cost_cny`（本次调用费用），没有余额。
	//
	// 结论：**余额只能靠登录凭证读**，没有第二条路。所以要让账号池面板的
	// 「额度」列有值，就必须把登录凭证留在磁盘上。
	//
	// # 安全代价（必须知情）
	//
	// `sess_` **等同登录态** —— 泄漏后果比单把 `sk_` 重得多（能读账号信息、
	// 能列/建/删该账号下的 Key）。所以：
	//
	//	落盘权限 0600（见 MarshalAuthFile 的调用方）
	//	**绝不**出现在任何 HTTP 回执、日志、界面里（导入回执只带 DerivedFrom 脱敏前缀）
	//	用户可随时在浏览器退出登录使全部 sess_ 失效
	//
	// 这是用户显式选择的取舍（"凭证文件存 sess_，独立 QuotaExt"）——
	// 换来的是额度列真正可用。不想要就别用 session 导入。
	Session string `json:"session,omitempty"`
	// FilePath 凭证文件位置（加载时记录，不参与序列化；
	// 删除账号要连文件一起移除）。
	FilePath string `json:"-"`
}

// deriveUID 由 APIKey 派生稳定 UID。
//
// # 为什么必须派生（这个上游的特殊约束）
//
// 别的上游能拿到真实账号 id（workbuddy 取 JWT 的 sub、inkstone 取 key
// 派生的稳定值）。基元律动的账号身份在 `/api/me`，而那个端点**只认
// session cookie** —— 拿 API Key 打过去是 401（实测）。
// 也就是说：**只给 key 的部署里，上游不提供任何账号标识**。
//
// 于是 uid 只能由 key 派生。取 sha256 前 8 字节（16 位 hex）：
//
//	确定性   同一把 key 每次派生结果一致 → 重启/重复导入不产生幽灵账号
//	脱敏     日志与界面里不出现完整 key
//	幂等     重复粘贴同一把 key = 覆盖同一文件，天然去重
func deriveUID(apiKey string) string {
	key := strings.TrimSpace(apiKey)
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8])
}

// NewAuth 从 APIKey 构造一份凭证（昵称缺省时用派生 UID 前缀）。
func NewAuth(apiKey, nickname string) *Auth {
	key := strings.TrimSpace(apiKey)
	a := &Auth{
		APIKey:   key,
		UID:      deriveUID(key),
		Nickname: strings.TrimSpace(nickname),
	}
	if a.Nickname == "" {
		a.Nickname = ShortUID(a.UID)
	}
	return a
}

// ShortUID 返回 uid 前 8 位（日志与缺省昵称用，脱敏）。
func ShortUID(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

// MarshalAuthFile 序列化一份凭证为磁盘上的 JSON 字节。
//
// 显式列出字段而非直接 marshal Auth：FilePath 带 `json:"-"`，
// 但显式写出来能让"磁盘格式"这件事一眼可见（与 inkstone 同款）。
//
// ⚠⚠ 本函数的输出是**唯一**会把 `Session` 写进磁盘的地方。
// 调用方必须保证落盘权限 0600 —— 那等同登录态（见 Auth.Session 的说明）。
func MarshalAuthFile(a *Auth) ([]byte, error) {
	if a == nil {
		return nil, fmt.Errorf("jiyuan: 凭证为空")
	}
	return json.MarshalIndent(struct {
		APIKey    string  `json:"api_key"`
		UID       string  `json:"uid"`
		Nickname  string  `json:"nickname,omitempty"`
		PhoneTail string  `json:"phone_tail,omitempty"`
		Balance   float64 `json:"balance,omitempty"`
		Session   string  `json:"session,omitempty"`
	}{
		APIKey:    a.APIKey,
		UID:       a.UID,
		Nickname:  a.Nickname,
		PhoneTail: a.PhoneTail,
		Balance:   a.Balance,
		Session:   strings.TrimSpace(a.Session),
	}, "", "  ")
}

// ParseCredential 把磁盘上的 JSON 解析回 Auth（集成导入要走它）。
//
// 幂等、无副作用：不写盘、不碰账号池 —— 只做"文本 → 凭证"。
// uid 缺失时按 key 补算，于是手写一份只有 `{"api_key":"sk_…"}` 的文件
// 也能被正确收下。
func ParseCredential(raw []byte) (*Auth, error) {
	var a Auth
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("jiyuan: 解析凭证失败: %w", err)
	}
	a.APIKey = strings.TrimSpace(a.APIKey)
	if a.APIKey == "" {
		return nil, fmt.Errorf("jiyuan: 凭证缺少 api_key")
	}
	a.Session = strings.TrimSpace(a.Session)
	if a.UID = strings.TrimSpace(a.UID); a.UID == "" {
		a.UID = deriveUID(a.APIKey)
	}
	if a.Nickname = strings.TrimSpace(a.Nickname); a.Nickname == "" {
		a.Nickname = ShortUID(a.UID)
	}
	return &a, nil
}

// AuthFileName 返回一份凭证应落盘的文件名（不含目录）。
func AuthFileName(a *Auth) string {
	base := strings.TrimSpace(a.UID)
	if base == "" && a != nil {
		base = deriveUID(a.APIKey)
	}
	return filePrefix + "-" + base + fileSuffix
}

// loadDir 读取目录下全部本上游凭证。
//
// 单个文件读失败/解析失败只记日志并跳过，不中断整目录 ——
// 一个坏文件不该让另外几十个号消失。
func loadDir(dir string) ([]*Auth, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	files, err := filepath.Glob(filepath.Join(dir, filePrefix+"-*"+fileSuffix))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	out := make([]*Auth, 0, len(files))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			log.Printf("jiyuan: 读取凭证 %s 失败: %v", filepath.Base(f), err)
			continue
		}
		a, perr := ParseCredential(raw)
		if perr != nil {
			log.Printf("jiyuan: 解析凭证 %s 失败: %v", filepath.Base(f), perr)
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

// compile-time: filePrefix 是落盘约定的一部分（与同结构上游对齐）。
var _ = filePrefix
