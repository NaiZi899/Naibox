package admin

import "testing"

// TestStreamTimeoutFieldsAreRestartRequired 守住"新增上游超时字段必须登记"。
//
// # 为什么这条值得一个测试
//
// 设置页把字段分成两类，判据就是下面这两个白名单。一个字段若只在
// Settings 结构体里声明而**没进 restartRequiredFields**，前端不会把它
// 当"需重启"处理 —— 而这三个 timeout 确实要重启才生效（Upstream.Client
// 的超时是启动期一次性装配的）。后果是"保存了、界面显示已生效、实际没生效"，
// 属于最难查的那类失效。
//
// 它也是"加了配置项但忘了登记"这个**结构性**遗漏的闸门：本仓库已经因为
// 同类遗漏付出过代价（见 settings_store.go 里 applyToMemory 与
// currentSettings 必须字段对称的注释）。
func TestStreamTimeoutFieldsAreRestartRequired(t *testing.T) {
	want := []string{
		"upstream_timeout_seconds",
		"upstream_header_timeout_seconds",
		"upstream_idle_timeout_seconds",
		// 本次新增的流式两道闸门：与上面三项同一条链路。
		"upstream_stream_max_seconds",
		"upstream_first_byte_seconds",
	}
	have := map[string]bool{}
	for _, k := range restartRequiredFields {
		have[k] = true
	}
	for _, k := range want {
		if !have[k] {
			t.Errorf("restartRequiredFields 缺少 %q —— "+
				"前端会把它当成可在运行时改的项，于是「保存了但没生效」且不提示重启", k)
		}
	}
}

// TestStreamTimeoutFieldsAreNotRuntimeFields 反向：三个 timeout 都不该出现在
// runtimeFields 里（那会向用户谎报"改完立即生效"）。
func TestStreamTimeoutFieldsAreNotRuntimeFields(t *testing.T) {
	for _, k := range runtimeFields {
		switch k {
		case "upstream_timeout_seconds", "upstream_header_timeout_seconds",
			"upstream_idle_timeout_seconds", "upstream_stream_max_seconds",
			"upstream_first_byte_seconds":
			t.Errorf("%q 同时出现在 runtimeFields 里 —— "+
				"超时是启动期装配的，谎报「立即生效」会让用户以为配置坏了", k)
		}
	}
}
