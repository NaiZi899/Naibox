package admin

import "testing"

// need_restart 只应包含本次真的改了的字段。
//
// 前端保存设置时提交的是完整字段表；如果后端照抄静态重启清单，
// 用户只改一个日志保留天数也会看到"16 项需重启"。
func TestChangedKeysOnlyReportsRealChanges(t *testing.T) {
	before := map[string]any{
		"upstream_timeout_seconds": float64(120),
		"prompt_mode":              "passthrough",
		"prompt_file":              "",
		"pool_max_in_flight":       float64(3),
	}
	after := map[string]any{
		"upstream_timeout_seconds": float64(120), // 没改
		"prompt_mode":              "append",     // 改了
		"prompt_file":              "",           // 没改
		"pool_max_in_flight":       float64(5),   // 改了
	}
	got := changedKeys([]string{
		"upstream_timeout_seconds", "prompt_mode", "prompt_file", "pool_max_in_flight", "prompt_mode",
	}, before, after)
	want := []string{"prompt_mode", "pool_max_in_flight"}
	if len(got) != len(want) {
		t.Fatalf("changedKeys = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("changedKeys = %v，期望 %v", got, want)
			break
		}
	}
}

func TestChangedKeysEmptyWhenNothingChanged(t *testing.T) {
	m := map[string]any{"prompt_mode": "append"}
	if got := changedKeys([]string{"prompt_mode"}, m, m); len(got) != 0 {
		t.Errorf("没有变化时不应报重启项: %v", got)
	}
}
