// egressbinding.go "账号 ↔ 出站线路"的绑定判据。
//
// # 为什么把它抽成一个函数
//
// 同一套判据要在两个时机生效：
//
//	装配期（boot.validateEgressBindings）  喊日志，不阻断启动
//	运行期（管理台绑定线路）               直接拒绝，并说清为什么
//
// 两份实现是这类判据最常见的腐坏方式：装配期加了一条新规则（比如"同一条线
// 上不允许两个同渠道的号"），运行期那份没跟上 —— 于是"启动时说有问题、
// 界面却让你点成功"。所以判据只有一份，两处都调它。
package boot

import (
	"errors"
	"fmt"

	"github.com/NaiZi899/Naibox/internal/auth"
)

// 绑定失败的三种原因。调用方据此决定回 400 还是 409 —— 也便于前端给出
// 可操作的提示（"换一条线" / "先解绑一个号" / "这条线不存在"）。
var (
	// ErrEgressUnknown 线路 id 不在 config.egress 里。
	ErrEgressUnknown = errors.New("线路不存在")
	// ErrEgressFull 该线路绑定的账号数已达上限。
	ErrEgressFull = errors.New("线路上的账号数已达上限")
	// ErrEgressRealmMismatch 国内号挂到了海外出口（或反之）。
	ErrEgressRealmMismatch = errors.New("渠道与线路地区不一致")
)

// CheckEgressBinding 校验"把 channel 渠道的账号绑到 wantID 线路"这件事。
//
// 参数：
//
//	egress  当前线路表
//	channel 账号渠道（cn / intl / 空=未知，空则不判 realm）
//	wantID  目标线路 id（**空串 = 解绑，恒允许**）
//	bound   该线路上**除本账号外**已绑定的账号数
//
// 返回 nil 表示可以绑。错误用上面的三个哨兵包一层说明，便于展示。
func CheckEgressBinding(egress []EgressConfig, channel, wantID string, bound int) error {
	if wantID == "" {
		return nil // 解绑永远允许：把人从一条有风险的线上摘下来不该被拦
	}
	var line *EgressConfig
	for i := range egress {
		if egress[i].ID == wantID {
			line = &egress[i]
			break
		}
	}
	if line == nil {
		return fmt.Errorf("%w：%q（先在「出口线路」里加一条，或改用已存在的线路）",
			ErrEgressUnknown, wantID)
	}
	// realm 一致：空 = 不限，账号渠道未知时不判（未知不等于对不上）。
	if line.Realm != "" && channel != "" && line.Realm != channel {
		return fmt.Errorf("%w：该号是 %s 渠道，线路 %q 只服务 %s —— %s",
			ErrEgressRealmMismatch, channel, wantID, line.Realm, realmMismatchHint(channel))
	}
	maxAccounts := line.MaxAccounts
	if maxAccounts <= 0 {
		maxAccounts = 1
	}
	if bound >= maxAccounts {
		return fmt.Errorf("%w：%q 上已有 %d 个账号（上限 %d）—— 同一出口上的账号越多，"+
			"\"IP 复用\"这条关联证据越硬；换一条线，或先解绑一个号",
			ErrEgressFull, wantID, bound, maxAccounts)
	}
	return nil
}

// CountEgressBindings 统计每条线路上的账号数。
//
// excludeUID 非空时不计入该账号（运行期改绑用：一个号从 A 改到 B，
// 判断 B 是否已满时不该把它自己算进去）。
func CountEgressBindings(auths []*auth.Auth, excludeUID string) map[string]int {
	out := map[string]int{}
	for _, a := range auths {
		if a == nil || a.EgressID == "" {
			continue
		}
		if excludeUID != "" && a.UID == excludeUID {
			continue
		}
		out[a.EgressID]++
	}
	return out
}
