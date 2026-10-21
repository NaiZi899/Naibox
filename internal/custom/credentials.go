// credentials.go —— 合成账号：每把 key × shards 分片各一条。
//
// # 账号从哪来
//
// `custom_upstreams` 里可以写多把 key（api_keys），每把 key 进池后是一个
// **独立账号**：走账号池的轮转、冷却与故障转移，但模型前缀仍然是同一个
// `<id>/模型`。shards 再在这把 key 上切成 N 个并发槽位，抬高单个账号的
// 在途上限（`max_in_flight` 是按账号算的）。
//
//	账号总数 = len(keys) × shards
//
// 单 key 时保持旧的 `<id>-s1` uid，避免升级后旧 state.json 里的账号全部
// 换名；多 key 时用 `<id>-k1s1`，让"哪把 key 的哪个分片"一眼可辨。
//
// # 为什么不落磁盘
//
// 其它上游的凭证是“登录换来的、会过期、要保存”的东西。这里没有：
// 密钥就在 config.json 里，进程一启动就能算出全部（合成）账号。
// 于是 LoadCredentials 的 dir 参数对本包**没有意义** —— 刻意忽略它，
// 而不是去读一个不存在/不该存在的目录（读它只会制造“为什么库里少一个号”
// 这类无法排查的疑问）。
//
// # 为什么每个账号的 secret 都是**独立副本**
//
// 池子按 secret 取回端点（provider.go 的 endpointOf）。共享同一个指针时，
// 将来任何一处“就地补一个头 / 改一次超时”都会同时改到所有账号，
// 而故障现象取决于哪个账号先跑 —— 那是无法复现的那类 bug。多 key 时
// secret 还必须带上**自己那把** key，否则所有账号会用同一把。
package custom

import (
	"fmt"

	"github.com/NaiZi899/Naibox/internal/gateway"
)

// keys 该端点配置的全部密钥（至少一把）。
func (p *Provider) keys() []string {
	keys := p.ep.resolvedKeys()
	if len(keys) == 0 {
		return []string{""}
	}
	return keys
}

// multiKey 是否配置了多把 key（决定错误分类与账号命名）。
func (p *Provider) multiKey() bool { return len(p.keys()) > 1 }

// UIDs 本端点的合成账号 uid 列表。
func (p *Provider) UIDs() []string {
	keys, shards := p.keys(), p.shardCount()
	out := make([]string, 0, len(keys)*shards)
	for k := 1; k <= len(keys); k++ {
		for s := 1; s <= shards; s++ {
			out = append(out, p.uidOf(k, s))
		}
	}
	return out
}

// uidOf 第 k 把 key 的第 s 个分片（均从 1 起）的 uid。
//
// 单 key 保持历史命名 `<id>-sN`；多 key 用 `<id>-kKsS`。
// uid 在池子里是全局主键，带上前缀才能看出它属于哪个端点、哪把 key。
func (p *Provider) uidOf(k, s int) string {
	if !p.multiKey() {
		return fmt.Sprintf("%s-s%d", p.ep.ID, s)
	}
	return fmt.Sprintf("%s-k%ds%d", p.ep.ID, k, s)
}

// nicknameOf 展示名。单 key 是 `名字 #1`，多 key 是 `名字 #1-1`（key-分片）。
func (p *Provider) nicknameOf(k, s int) string {
	if !p.multiKey() {
		return fmt.Sprintf("%s #%d", p.ep.Name, s)
	}
	return fmt.Sprintf("%s #%d-%d", p.ep.Name, k, s)
}

// eachAccount 遍历全部 (keyIndex, shardIndex)，索引均从 1 起。
func (p *Provider) eachAccount(fn func(k, s int)) {
	keys, shards := p.keys(), p.shardCount()
	for k := 1; k <= len(keys); k++ {
		for s := 1; s <= shards; s++ {
			fn(k, s)
		}
	}
}

// LoadCredentials 返回合成账号的投影（uid / nickname）。
//
// dir 刻意忽略（见文件头）。它总是成功：没有凭证文件可读，
// 也就没有“文件坏了”这种失败模式。
func (p *Provider) LoadCredentials(dir string) ([]gateway.Credential, error) {
	_ = dir
	out := make([]gateway.Credential, 0, len(p.keys())*p.shardCount())
	p.eachAccount(func(k, s int) {
		out = append(out, gateway.Credential{
			Provider: p.ep.ID,
			UID:      p.uidOf(k, s),
			Nickname: p.nicknameOf(k, s),
		})
	})
	return out, nil
}

// LoadCredentialsWithSecrets 与 LoadCredentials 同源，但额外给出 secret。
//
// 这里必须实现（而不只是 CredentialLoader）：池子在“重载 auths”时会走
// 带 secret 的那条路径；只给投影的话，新进池的账号会拿到 nil secret，
// 而 endpointOf 回落到构造时的 ep —— 那是**同一个端点**，行为看似正常，
// 但热改配置后老账号会悄悄跟着变（正是 provider.go 里要避免的事）。
func (p *Provider) LoadCredentialsWithSecrets(dir string) ([]gateway.CredentialSecret, error) {
	_ = dir
	keys := p.keys()
	out := make([]gateway.CredentialSecret, 0, len(keys)*p.shardCount())
	p.eachAccount(func(k, s int) {
		ep := p.ep.clone()
		// secret 只带自己那把 key，Keys 置空避免"账号里还藏着一整份 key 列表"
		// 被后续代码误用（endpointOf 取的是 APIKey）。
		ep.APIKey = keys[k-1]
		ep.Keys = nil
		out = append(out, gateway.CredentialSecret{
			Credential: gateway.Credential{
				Provider: p.ep.ID,
				UID:      p.uidOf(k, s),
				Nickname: p.nicknameOf(k, s),
			},
			Secret: &ep,
		})
	})
	return out, nil
}
