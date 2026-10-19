// jiyuancreds.go 把基元律动凭证并入核心账号池。
//
// 与 loomy / inkstone 同构：凭证是每把 key 一份（无 TTL、无 refresh token），
// 所以这里只做"目录扫描 → 投影成核心认识的形状"，没有续期/单一所有者那套。
// 上游凭证 *jiyuan.Auth 本身（含 APIKey）作为 secret 交池子保管。
package boot

import (
	"log"

	"github.com/NaiZi899/Naibox/internal/auth"
	"github.com/NaiZi899/Naibox/internal/jiyuan"
	"github.com/NaiZi899/Naibox/internal/pool"
)

// dedupeJiyuanByUID 把目录扫描结果投影成 (核心账号, 不透明 secret)。
//
// uid 由 key 派生（见 jiyuan.deriveUID），所以同一把 key 重复落盘会
// 得到同一个 uid —— 这里的 map 覆盖语义天然去重，不会堆出重复账号。
func dedupeJiyuanByUID(list []*jiyuan.Auth) ([]*auth.Auth, map[string]any) {
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

// syncJiyuanAccounts 把基元律动凭证目录里的账号并入核心账号池。
//
// 语义与 syncLoomyAccounts / syncInkstoneAccounts 完全对齐：
// 目录为空仍 SyncToDir（把已删除账号剔除）、读目录失败只留日志不动池子。
func syncJiyuanAccounts(p *pool.Pool, dir string) int {
	list, err := jiyuan.LoadDir(dir)
	if err != nil {
		log.Printf("jiyuan: 读取凭证目录失败（账号池未并入）: %v", err)
		return 0
	}
	if len(list) == 0 {
		p.SyncToDirWithSecrets(jiyuan.ProviderID, nil, nil)
		return 0
	}
	auths, secrets := dedupeJiyuanByUID(list)
	p.SyncToDirWithSecrets(jiyuan.ProviderID, auths, secrets)
	return len(auths)
}
