// inkstonecreds.go 把书生端砚凭证并入核心账号池。
//
// 与 loomy 同构：凭证是每个号一个 API Key（无 TTL、无 refresh token），
// 所以这里只做"目录扫描 → 投影成核心认识的形状"，没有续期/单一所有者那套。
// 上游凭证 *inkstone.Auth 本身（含 APIKey，可能还有 UserToken）作为 secret 交池子保管。
package boot

import (
	"log"

	"github.com/NaiZi899/Naibox/internal/auth"
	"github.com/NaiZi899/Naibox/internal/inkstone"
	"github.com/NaiZi899/Naibox/internal/pool"
)

// dedupeInkstoneByUID 把目录扫描结果投影成 (核心账号, 不透明 secret)。
// 上游凭证 *inkstone.Auth 作为 secret，池子不解释其字段。
func dedupeInkstoneByUID(list []*inkstone.Auth) ([]*auth.Auth, map[string]any) {
	auths := make([]*auth.Auth, 0, len(list))
	secrets := make(map[string]any, len(list))
	for _, a := range list {
		if a == nil || a.UID == "" {
			continue // 防御：空 UID 的账号无法按 uid 寻址
		}
		secrets[a.UID] = a
		auths = append(auths, &auth.Auth{UID: a.UID, Nickname: a.Nickname, FilePath: a.FilePath})
	}
	return auths, secrets
}

// syncInkstoneAccounts 把书生端砚凭证目录里的账号并入核心账号池。
//
// 语义与 syncLoomyAccounts 完全对齐：目录为空仍 SyncToDir（把已删除账号剔除）、
// 读目录失败只留日志不动池子。
func syncInkstoneAccounts(p *pool.Pool, dir string) int {
	list, err := inkstone.LoadDir(dir)
	if err != nil {
		log.Printf("inkstone: 读取凭证目录失败（账号池未并入）: %v", err)
		return 0
	}
	if len(list) == 0 {
		p.SyncToDirWithSecrets(inkstone.ProviderID, nil, nil)
		return 0
	}
	auths, secrets := dedupeInkstoneByUID(list)
	p.SyncToDirWithSecrets(inkstone.ProviderID, auths, secrets)
	return len(auths)
}
