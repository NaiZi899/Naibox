// unit_test.go 额度量纲（QuotaView.Unit）的透传与展示。
//
// # 这个字段修的是什么
//
// 账号池「额度」列原本只显示一个裸整数。而不同上游量纲不同：
//
//	workbuddy / codearts → 积分
//	jiyuan               → 人民币**分**（int64 装不下小数金额）
//
// 于是 jiyuan 的 `9975`（= ¥99.75）与 workbuddy 的 `9975`（= 9975 积分）
// 在界面上长得一模一样，用户无法区分。
//
// # 为什么必须专门测"透传"
//
// Unit 要穿过**三个类型 + 三个转换点**才到界面：
//
//	jiyuan.Auth → gateway.QuotaView --toPoolQuota--> pool.QuotaView → JSON → 前端
//
// 任何一处漏写 `Unit:` 都不会编译失败、不会报错、不会让其它测试变红 ——
// 界面只是**静默地**少了那两个字。这是本项目反复出现的那类缺陷
// （中间层丢字段），所以每一跳都要有断言。
package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCreditsQuotaUnitCarriesUnit 带单位的构造函数必须把单位放进去。
func TestCreditsQuotaUnitCarriesUnit(t *testing.T) {
	q := CreditsQuotaUnit(9975, QuotaUnitFen)
	if q.Unit != "分" {
		t.Errorf("单位应为 分，得到 %q", q.Unit)
	}
	if !q.HasData {
		t.Error("带单位的构造也必须置 HasData —— 否则界面显示 `—`，数字白算")
	}
	if q.Remaining != 9975 {
		t.Errorf("数值应为 9975，得到 %d", q.Remaining)
	}
	if q.Kind != QuotaKindCredits {
		t.Errorf("Kind 应为 credits，得到 %q", q.Kind)
	}
}

// TestCreditsQuotaUnitTrimsSpace 用户/上游传来的单位词前后空白要清掉。
//
// 不清的后果：界面上出现 `9975  分`（两个空格）或 `9975 分 `（尾随空格
// 在 HTML 里会被折叠，但导出/复制时会带出去）。
func TestCreditsQuotaUnitTrimsSpace(t *testing.T) {
	for _, in := range []string{" 分", "分 ", "  分  ", "\t分\n"} {
		if q := CreditsQuotaUnit(1, in); q.Unit != "分" {
			t.Errorf("QuotaUnit(%q) 应被清理成 分，得到 %q", in, q.Unit)
		}
	}
}

// TestCreditsQuotaLeavesUnitEmpty 不带单位的构造函数**必须**留空。
//
// 留空的语义是"无量纲"→ 界面只显示数字（与改造前逐字节相同）。
// 若这里被填上任何默认值，既有 workbuddy/codearts 的显示就变了 ——
// 那是本次改动不该产生的副作用。
func TestCreditsQuotaLeavesUnitEmpty(t *testing.T) {
	if q := CreditsQuota(4200); q.Unit != "" {
		t.Errorf("无量纲额度不该有单位，得到 %q", q.Unit)
	}
}

// TestUnknownQuotaHasNoUnit 未知额度不该带单位（它连数值都没有）。
func TestUnknownQuotaHasNoUnit(t *testing.T) {
	q := UnknownQuota()
	if q.Unit != "" {
		t.Errorf("未知额度不该带单位，得到 %q", q.Unit)
	}
}

// TestPerModelQuotaHasNoUnit 按模型额度不带单位（逐模型量纲可能不同，不该一概而论）。
func TestPerModelQuotaHasNoUnit(t *testing.T) {
	q := PerModelQuota(map[string]int64{"m": 1})
	if q.Unit != "" {
		t.Errorf("按模型额度不该带单位，得到 %q", q.Unit)
	}
	if PerModelQuota(nil).Unit != "" {
		t.Error("空表的未知额度也不该带单位")
	}
}

// TestQuotaUnitConstants 单位词是**给人看的中文短词**，不是内部标识。
//
// 它们会原样下发到界面，所以 `"credits"`/`"points"` 这类内部标识是错的
// （用户会看到 `9975 credits`）。这条把取值钉住。
func TestQuotaUnitConstants(t *testing.T) {
	if QuotaUnitFen != "分" {
		t.Errorf("QuotaUnitFen 应为 分，得到 %q", QuotaUnitFen)
	}
	if QuotaUnitPoint != "积分" {
		t.Errorf("QuotaUnitPoint 应为 积分，得到 %q", QuotaUnitPoint)
	}
	for _, u := range []string{QuotaUnitFen, QuotaUnitPoint} {
		if strings.ContainsAny(u, "abcdefghijklmnopqrstuvwxyz") {
			t.Errorf("单位词 %q 含拉丁字母 —— 它是下发给界面给人看的词，应为中文短词", u)
		}
		if strings.TrimSpace(u) != u || u == "" {
			t.Errorf("单位词 %q 不该有空白或为空", u)
		}
	}
}

// TestQuotaViewUnitJSONRoundTrip Unit 的线格式字段名是 `unit`。
//
// 前端读 `q.unit` —— 字段名漂移（或漏 tag）会让前端恒读到 undefined，
// 表现就是"标注整片消失"而且不报错。
func TestQuotaViewUnitJSONRoundTrip(t *testing.T) {
	raw, err := json.Marshal(CreditsQuotaUnit(9975, QuotaUnitFen))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["unit"] != "分" {
		t.Errorf("线格式字段应为 unit=分，实际 %v（原始 %s）", m["unit"], raw)
	}
	// 空单位必须**不出现**在 JSON 里（omitempty）—— 那是"与改造前逐字节相同"
	// 这条承诺的线格式基础。
	rawEmpty, _ := json.Marshal(CreditsQuota(4200))
	if strings.Contains(string(rawEmpty), `"unit"`) {
		t.Errorf("空单位不该出现在线格式里（否则老前端会读到空串字段），实际 %s", rawEmpty)
	}
}
