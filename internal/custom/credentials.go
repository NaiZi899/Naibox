// credentials.go —— 合成账号：一个端点默认一条，shards 抬高在途上限。
//
// # 为什么自定义端点也有“账号”
//
// 账号池的调度单位是**凭证**（每个凭证有自己的在途计数、冷却与统计）。
// 自定义端点只有一把密钥，于是它天然只有“一个账号”。但池子的
// `max_in_flight` 是按账号算的：单账号意味着整个端点同时只能跑
// max_in_flight 个对话 —— 对一台自建的推理服务器来说这通常是误伤
// （它自己扛得住 4 个并发）。`shards` 就是给这种情况的出口：
// 同一把密钥切成 N 个**合成账号**，在途上限随之抬到 N × max_in_flight。
//
// # 为什么不落磁盘
//
// 其它上游的凭证是“登录换来的、会过期、要保存”的东西。这里没有：
// 密钥就在 config.json 里，进程一启动就能算出全部（合成）账号。
// 于是 LoadCredentials 的 dir 参数对本包**没有意义** —— 刻意忽略它，
// 而不是去读一个不存在/不该存在的目录（读它只会制造“为什么库里少一个号”
// 这类无法排查的疑问）。
//
// # 为什么每个分片的 secret 都是**独立副本**
//
// 池子按 secret 取回端点（provider.go 的 endpointOf）。共享同一个指针时，
// 将来任何一处“就地补一个头 / 改一次超时”都会同时改到所有分片，
// 而故障现象取决于哪个分片先跑 —— 那是无法复现的那类 bug。
package custom

import (
	"fmt"

	"github.com/NaiZi899/Naibox/internal/gateway"
)

// UIDs 本端点的合成账号 uid 列表："<id>-s1" .. "<id>-sN"。
//
// 为什么带 `-s` 前缀而不是纯序号：uid 在池子里是全局主键，
// 换成别的上游时要能一眼看出属于哪个端点；纯 "1"/"2" 会撞。
func (p *Provider) UIDs() []string {
	n := p.shardCount()
	out := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, p.uidOf(i))
	}
	return out
}

// uidOf 第 i 个（1 起）合成账号的 uid。
func (p *Provider) uidOf(i int) string {
	return fmt.Sprintf("%s-s%d", p.ep.ID, i)
}

// nicknameOf 第 i 个（1 起）合成账号的展示名。
//
// 序号从 1 起：界面上给人看的编号没有 0 号。
func (p *Provider) nicknameOf(i int) string {
	return fmt.Sprintf("%s #%d", p.ep.Name, i)
}

// LoadCredentials 返回合成账号的投影（uid / nickname）。
//
// dir 刻意忽略（见文件头）。它总是成功：没有凭证文件可读，
// 也就没有“文件坏了”这种失败模式。
func (p *Provider) LoadCredentials(dir string) ([]gateway.Credential, error) {
	_ = dir
	n := p.shardCount()
	out := make([]gateway.Credential, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, gateway.Credential{
			Provider: p.ep.ID,
			UID:      p.uidOf(i),
			Nickname: p.nicknameOf(i),
		})
	}
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
	n := p.shardCount()
	out := make([]gateway.CredentialSecret, 0, n)
	for i := 1; i <= n; i++ {
		ep := p.ep.clone() // 每个分片一份副本（见文件头的“为什么不共享指针”）
		out = append(out, gateway.CredentialSecret{
			Credential: gateway.Credential{
				Provider: p.ep.ID,
				UID:      p.uidOf(i),
				Nickname: p.nicknameOf(i),
			},
			Secret: &ep,
		})
	}
	return out, nil
}
