// custom.go 自定义上游的装配：配置 → Provider → 合成账号入池。
//
// # 为什么合成账号而不是给核心开一条"无账号上游"的旁路
//
// 出口层的整条链路（选号、在途名额、失败冷却、统计、账号表）都建立在
// "一次请求 = 一个池里的账号"这个前提上。给自定义上游单开一条路径，等于
// 在核心复制一遍这些逻辑，而且两套判据迟早漂移。
//
// 合成账号把这批端点接进同一条链路：
//
//	UID       <id>-s1 .. <id>-sN（shards 决定个数，稳定可复现）
//	Secret    *custom.Endpoint（池子只搬运，不解释）
//	额度      永远未知（HasData=false），不参与额度权重，也不会被当成"0 额度"
//
// # 为什么注册在所有内置上游之后
//
// 裸模型名走 registry.First()，而下一条兼容性要求是"默认仍是 workbuddy"。
// 自定义上游一律通过 `<id>/<模型>` 前缀调用 —— 这条顺序就是它的保证。
package boot

import (
	"fmt"
	"log"

	"github.com/NaiZi899/Naibox/internal/auth"
	"github.com/NaiZi899/Naibox/internal/custom"
	"github.com/NaiZi899/Naibox/internal/gateway"
	"github.com/NaiZi899/Naibox/internal/pool"
)

// registerCustomUpstreams 注册全部启用中的自定义上游，并把合成账号并入池子。
//
// 账号来源直接用 Provider 自己的 LoadCredentialsWithSecrets：它与管理页的
// 「重载 auths」走同一个函数，两条路径不会对"该有几个号"产生分歧。
func registerCustomUpstreams(cfg *Config, reg *gateway.Registry, p *pool.Pool, logf func(string, ...any)) error {
	if logf == nil {
		logf = log.Printf
	}
	for _, ep := range cfg.CustomEndpoints {
		prov := custom.New(ep, log.Default())
		if err := reg.Register(prov); err != nil {
			return fmt.Errorf("自定义上游 %q 注册失败: %w", ep.ID, err)
		}
		creds, err := prov.LoadCredentialsWithSecrets("")
		if err != nil {
			return fmt.Errorf("自定义上游 %q 生成合成账号失败: %w", ep.ID, err)
		}
		auths := make([]*auth.Auth, 0, len(creds))
		secrets := make(map[string]any, len(creds))
		for _, c := range creds {
			auths = append(auths, &auth.Auth{UID: c.UID, Nickname: c.Nickname})
			secrets[c.UID] = c.Secret
		}
		if p != nil {
			p.SyncToDirWithSecrets(ep.ID, auths, secrets)
		}
		// 只打 id / 端点 / 分片数：key 永远不进日志（连尾号也不打，
		// 这里没有排障价值，而它会把"日志可分享"这条性质破坏掉）。
		logf("custom: 已启用 %s（%s，模型前缀 %s/，%d 个分片）",
			ep.Name, ep.BaseURL, ep.ID, ep.Shards)
		if ep.InsecureSkipVerify {
			logf("custom: 警告 —— %s 关闭了 TLS 证书校验（insecure_skip_verify=true）", ep.ID)
		}
	}
	return nil
}
