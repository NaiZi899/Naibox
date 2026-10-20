// adminroute.go 基元律动的只读诊断端点（额度探测）。
//
// # 为什么需要它
//
// 本上游的余额读得到（quota_ext.go 已实现 QuotaExt），但改造前**没有任何
// 入口**能单独触发一次探测：账号行的「额度」按钮走的是核心的
// `/admin/accounts/quota/refresh`，它刷的是**全池**或**整个上游**，
// 排障时想问"就这一个号现在多少钱"得等一整轮。
//
// 更重要的是能力位。`CapQuotaProbe` 的契约要求同时具备 QuotaExt 与
// AdminExt（见 provider.go 的 Caps 注释），缺一个就是假声明。所以这个文件
// 也是"能把 CapQuotaProbe 写进 Caps"的前提。
//
// # 它不写账号池
//
// 与核心的 `/admin/accounts/quota/refresh` 刻意分工：
//
//	核心那条   刷新 + **写回池**（用户点「刷新额度」要的就是这个）
//	本端点     只探测 + 回报（诊断"这个号到底读到什么"，不改变任何状态）
//
// 诊断端点若也写池，排障时的一次随手调用就会覆盖掉池里的额度快照，
// 事后无法还原"刷新前是什么样"。
//
// # 回执不含凭据
//
// 登录凭证（sess_）是本包唯一的高敏数据，绝不进回执。返回的只有
// 三态结论与原因。
package jiyuan

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"

	"github.com/NaiZi899/Naibox/internal/gateway"
)

// quotaProbePath 探测端点路径。
const quotaProbePath = "/admin/jiyuan/quota-probe"

// AdminRoutes 实现 gateway.AdminExt。
//
// Hidden: true —— 这是诊断端点，不该在左侧导航里占一个标签页。
// 面板入口由 CapQuotaProbe 能力位驱动（账号行与分组头的「额度」按钮）。
func (p *Provider) AdminRoutes() []gateway.AdminRoute {
	return []gateway.AdminRoute{
		{
			Method:     http.MethodPost,
			Path:       quotaProbePath,
			Handler:    p.handleQuotaProbe,
			Capability: gateway.CapQuotaProbe,
			Title:      "额度探测",
			Hidden:     true,
		},
	}
}

// quotaProbeResult 单个账号的探测结论。
//
// State 三态与核心 quotaRefresher 的计数口径一致：
//
//	updated  读到了余额
//	unknown  问了，但拿不到（无 Session / sess_ 过期 / 上游 5xx / 非 CNY）
//	skipped  这个 uid 不归本上游（池里没有，或凭证读不出来）
//
// ⚠ 没有 failed：本端点是**只读诊断**，"读不到"在这里是正常结论而非故障。
// 若把读不到记成 failed，一次上游抖动就会被读成"网关坏了"。
type quotaProbeResult struct {
	UID    string `json:"uid"`
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
	// Remaining / Unit 仅在 State=updated 时有意义。
	Remaining int64  `json:"remaining,omitempty"`
	Unit      string `json:"unit,omitempty"`
}

// handleQuotaProbe POST /admin/jiyuan/quota-probe[?uid=xxx]
//
// 给了 uid 就只探这一个；没给就探本上游在池里的全部账号（按 uid 排序，
// 保证回执可比对）。
func (p *Provider) handleQuotaProbe(w http.ResponseWriter, r *http.Request) {
	uids, err := p.probeUIDs(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}

	results := make([]quotaProbeResult, 0, len(uids))
	updated := 0
	for _, uid := range uids {
		res := p.probeOne(r.Context(), uid)
		if res.State == "updated" {
			updated++
		}
		results = append(results, res)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"provider": providerID,
		"count":    len(results),
		"updated":  updated,
		"results":  results,
	})
}

// probeUIDs 决定本次要探哪些 uid。
//
// 显式给了 ?uid= 就只探它（哪怕池里没有 —— 让调用方能确认"这个号不归我"）。
// 没给则枚举本上游的凭证目录。
//
// # 为什么用 LoadDir 而不是问账号池
//
// 上游包**不得** import internal/pool（架构判据 3），核心也没给本包注入
// 池访问器。而凭证目录就是本上游账号的权威来源：导入时写盘，删除时删文件。
// 用它能做到"不新增任何跨层接线"就拿到账号清单。
//
// 代价：内存里有、磁盘还没落盘的账号不会出现在全量探测里。导入流程是
// 先写盘再入池（见 import.go），所以这个窗口只存在于写盘失败时 ——
// 那时本来就该显示"没导入成功"。
func (p *Provider) probeUIDs(r *http.Request) ([]string, error) {
	if uid := r.URL.Query().Get("uid"); uid != "" {
		return []string{uid}, nil
	}
	list, err := LoadDir(p.cfg.AuthDir)
	if err != nil {
		// 目录读不了是**真故障**（权限/IO），要报出来 —— 否则"0 个账号"
		// 会被读成"池是空的"，而真相是"读不到"。
		return nil, err
	}
	uids := make([]string, 0, len(list))
	for _, a := range list {
		if a != nil && a.UID != "" {
			uids = append(uids, a.UID)
		}
	}
	// 稳定顺序：回执要能逐行比对上一次的结果。
	sort.Strings(uids)
	return uids, nil
}

// probeOne 探测单个账号，把 QuotaExt 的三态翻译成本端的结论。
func (p *Provider) probeOne(ctx context.Context, uid string) quotaProbeResult {
	res := quotaProbeResult{UID: uid}

	qv, ok := p.RefreshQuota(uid)
	if !ok {
		res.State = "skipped"
		res.Reason = "账号不在本上游（池里没有该 uid，或凭证读不出来）"
		return res
	}
	if !qv.HasData {
		res.State = "unknown"
		// 具体原因由 RefreshQuota 内部记录（见 quota_ext.go 的三条分支）：
		// 无 Session / 请求失败 / 币种不认识。这里给一句面向人的概括。
		res.Reason = "上游没有给出余额（未挂登录凭证、凭证失效，或上游暂时不可用）"
		return res
	}
	res.State = "updated"
	res.Remaining = qv.Remaining
	res.Unit = qv.Unit
	return res
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// 编译期断言：AdminExt。
var _ gateway.AdminExt = (*Provider)(nil)
