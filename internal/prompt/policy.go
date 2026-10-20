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
//	ModePassthrough          body 原样                 Rewrite(body, Degraded)
//	ModeAppend               Append(body, p.Text)      Rewrite(body, Degraded)
//	ModeCustom / 空 / 未知   Rewrite(body, p.Text)     Rewrite(body, Degraded)
//
// # 为什么空串与未知值都落到 custom
//
// 配置项写错（拼写、大小写、旧版本残留）时，唯一安全的默认是"用网关自有提示词
// 整体替换"：它一定会消除 system 来源的指纹。若默认落到 passthrough，
// 写错的配置会变成一个完全静默的失败 —— 网关照常转发，只是功能一点没生效，
// 而用户看到的现象是"偶发 400"，与配置无关的样子。
//
// # 为什么先判降级再判 mode
//
// 降级状态压过用户选择：降级期已经确认过"当前提示词形态会被拦"，
// 再按 mode 走一遍只是把已知的失败重演一次。
func Inject(body []byte, p Profile, degraded bool) []byte {
	if degraded {
		return Rewrite(body, Degraded)
	}
	switch p.Mode {
	case ModePassthrough:
		return body
	case ModeAppend:
		return Append(body, p.Text)
	default:
		return Rewrite(body, p.Text)
	}
}
