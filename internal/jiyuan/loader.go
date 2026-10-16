// loader.go 基元律动的凭证加载（gateway.AuthDirExt / CredentialLoader /
// CredentialSecretLoader / DisplayNameExt）。
//
// 与 loomy / inkstone 完全同构：凭证是目录下的 `jiyuan-*.json`，
// 加载时把 uid/nickname 投影给核心，Secret 原样交给池子保管。
package jiyuan

import (
	"strings"

	"github.com/NaiZi899/omni2api/internal/gateway"
)

// AuthDir 实现 gateway.AuthDirExt —— 本上游凭证目录。
//
// ⚠ 读（按上游重载 auths）与写（批量导入落盘）必须问同一个目录，
// 否则会出现"导入报成功但池子里没有账号"这种无法解释的状态
// （核心的 importDirFor 与本方法共用同一个判据顺序）。
func (p *Provider) AuthDir() string { return p.cfg.AuthDir }

// LoadCredentials 读取 dir 下全部凭证（gateway.CredentialLoader，投影版）。
func (p *Provider) LoadCredentials(dir string) ([]gateway.Credential, error) {
	auths, err := LoadDir(p.effectiveDir(dir))
	if err != nil {
		return nil, err
	}
	out := make([]gateway.Credential, 0, len(auths))
	for _, a := range auths {
		out = append(out, gateway.Credential{
			Provider: ProviderID,
			UID:      a.UID,
			Nickname: a.Nickname,
			// 凭证在磁盘上的落点：核心拿它做"文件"列展示与删除账号时连文件移除。
			FilePath: a.FilePath,
		})
	}
	return out, nil
}

// LoadCredentialsWithSecrets 与 LoadCredentials 同源，但额外给出 Secret
// （gateway.CredentialSecretLoader）。
//
// ⚠ Secret 必须是**同一份** *Auth —— 否则池里那份与本文件这份分叉，
// Chat 取不到 key（表现为"账号健康但请求报凭证缺失"）。
func (p *Provider) LoadCredentialsWithSecrets(dir string) ([]gateway.CredentialSecret, error) {
	auths, err := LoadDir(p.effectiveDir(dir))
	if err != nil {
		return nil, err
	}
	out := make([]gateway.CredentialSecret, 0, len(auths))
	for _, a := range auths {
		out = append(out, gateway.CredentialSecret{
			Credential: gateway.Credential{
				Provider: ProviderID,
				UID:      a.UID,
				Nickname: a.Nickname,
				FilePath: a.FilePath,
			},
			Secret: a,
		})
	}
	return out, nil
}

// effectiveDir 决定用哪个目录：显式入参优先，否则用装配注入的。
func (p *Provider) effectiveDir(dir string) string {
	if strings.TrimSpace(dir) != "" {
		return dir
	}
	return p.cfg.AuthDir
}

// DisplayName 实现 gateway.DisplayNameExt —— 控制台分组名。
func (p *Provider) DisplayName() string { return "基元律动 (TokenRhythm)" }

// compile-time 断言：Provider 实现它声称支持的全部扩展点。
var (
	_ gateway.AuthDirExt             = (*Provider)(nil)
	_ gateway.CredentialLoader       = (*Provider)(nil)
	_ gateway.CredentialSecretLoader = (*Provider)(nil)
	_ gateway.DisplayNameExt         = (*Provider)(nil)
)
