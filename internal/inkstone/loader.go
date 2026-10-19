// loader.go 书生·端砚的凭证加载（gateway.AuthDirExt / CredentialLoader / CredentialSecretLoader）。
//
// 与 loomy 完全同构：凭证是目录下的 `inkstone-*.json`，加载时把 uid/nickname
// 投影给核心，Secret 原样交给池子保管。
package inkstone

import (
	"strings"

	"github.com/NaiZi899/Naibox/internal/gateway"
)

// AuthDir 实现 gateway.AuthDirExt —— 本上游凭证目录。
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
		})
	}
	return out, nil
}

// LoadCredentialsWithSecrets 与 LoadCredentials 同源，但额外给出 Secret
// （gateway.CredentialSecretLoader）。Secret 必须是同一份 *Auth
// —— 否则池里那份与这里这份分叉，Chat 取不到 key。
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
func (p *Provider) DisplayName() string { return "书生·端砚 (InkStone)" }

// compile-time 断言：Provider 实现它声称支持的全部扩展点。
var (
	_ gateway.AuthDirExt             = (*Provider)(nil)
	_ gateway.CredentialLoader       = (*Provider)(nil)
	_ gateway.CredentialSecretLoader = (*Provider)(nil)
	_ gateway.DisplayNameExt         = (*Provider)(nil)
)
