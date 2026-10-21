// policy.go 提示词注入策略：把 mode 与降级状态翻译成一次具体的 body 改写。
//
// # 为什么需要这一层
//
// body 该怎么改写，取决于两个互相独立的输入：用户选的 mode，和当前是否处于
// 降级期（见 gate.go）。把这段路由散在调用方（HTTP handler、出站客户端）里，
// 每加一个 mode 就要在每个调用点补一遍分支，而且"降级期该用哪种形态"这条
// 判断会飘。集中成一个纯函数之后，路由规则只有一份，可以直接用真值表锁死。
//
// # 为什么降级期一律走 Rewrite(Degraded)，而不是"叠加式降级"
//
// 降级期的目标是**尽可能不带指纹的文本**。passthrough 保留客户端 system，
// append 更是在它后面再叠一段 —— 两种形态里客户端那段 system 的模板句
// （"You are Claude Code" 之类）一个都没消掉，所以降级重试必然再撞一次 400，
// 白付一次完整的失败往返。
//
// 叠加式降级只会让出站 body 同时带着客户端指纹**和**网关提示词：
// 比原来更长、命中逐字匹配表的面积更大。那与降级的目的正好相反。
//
// 所以降级期只有一种形态：整段换成中性提示词。append 在降级期与 custom
// 同路，不是因为它们语义相近，而是因为它们对"消除指纹"这件事同样是零贡献。
package prompt

// Profile 一份提示词配置。
//
// 本期只有 default 一份，Name 仅用于日志与可观测，不参与路由判断 ——
// 路由只看 Mode 与降级状态，多一份配置进来不该改变任何既有请求的形态。
//
// 之所以保留 Name：人格出问题时日志里必须能说出"是哪一份配置生效了"，
// 否则只剩一段文本可看，无法判断它来自哪个配置项。
type Profile struct {
	Name string
	Mode string
	Text string
}

// Inject 按 mode 与降级状态决定怎么改写请求体。
//
// 真值表（这是本包的验收线）：
//
//	Mode                     degraded=false            degraded=true
//	ModePassthrough / 空      body 原样                 Rewrite(body, Degraded)
//	ModeAppend               Append(body, p.Text)      Rewrite(body, Degraded)
//	ModeCustom / 其它未知值   Rewrite(body, p.Text)     Rewrite(body, Degraded)
//
// # 空 mode 为什么落 passthrough
//
// 配置缺省（没有 prompt 段、或 prompt.mode 为空）代表"这个部署没有选择任何
// 改写姿势"，那就一个字节都不动 —— 这是 2026-10 调整后的产品默认值：
// 改写会改变上游看到的渠道画像，而上游按累积采样判定，代价要由选择改写的人承担，
// 不该由"什么都没配"的人默认承担。
//
// # 未知值为什么仍然落 custom
//
// 拼错（"passtrhu"）与"没配置"是两件事：前者说明用户明确想做点什么，
// 静默变成透传会让"我明明配了"变成无从对证的话。配置层已经在启动时对未知值
// 报错，这里的兜底只是最后一道防线，选"仍然改写"而不是"什么都不做"。
func Inject(body []byte, p Profile, degraded bool) []byte {
	if degraded {
		return Rewrite(body, Degraded)
	}
	switch p.Mode {
	case "", ModePassthrough:
		return body
	case ModeAppend:
		return Append(body, p.Text)
	default:
		return Rewrite(body, p.Text)
	}
}
