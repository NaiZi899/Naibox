// series_custom_test.go —— /admin/stats/series 自定义区间的解析守卫。
//
// # 为什么单独测解析而不是只测 HTTP
//
// 这里真正容易错的是**窗口边界的算法**（Truncate 到桶、左闭右开、跨度与桶数
// 的保护），而不是 JSON 编码。测纯函数 `parseSeriesWindow` 能把这些边界
// 一条条钉住；HTTP 层已有既有测试覆盖"能不能返回 200"。
//
// 三条保护是重点：跨度下限 1 分钟、上限 90 天、桶数上限 2000。
// 少了任何一个，前端传 `from=0&to=9999999999` 就能让后端铺出上百万个桶。
package admin

import (
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// TestParseSeriesWindowCustomInterval 守自定义区间的窗口与桶宽选择。
func TestParseSeriesWindowCustomInterval(t *testing.T) {
	base := time.Date(2026, 10, 17, 14, 0, 0, 0, time.Local)

	cases := []struct {
		name        string
		span        time.Duration
		bucket      string
		wantBucket  time.Duration
		wantBuckets int // 0 = 该用例由桶宽覆盖，不单独断言桶数
	}{
		// ≤2h 用 5 分钟桶：2h/5m = 24 个点，图上一屏看得清
		{"2 小时 → 5m", 2 * time.Hour, "", 5 * time.Minute, 24},
		// ≤48h 用小时桶：36h/1h = 36 个点
		{"36 小时 → 1h", 36 * time.Hour, "", time.Hour, 36},
		// 超过 48h 用天桶：30d/1d = 30 个点
		{"30 天 → 1d", 30 * 24 * time.Hour, "", 24 * time.Hour, 30},
		// 显式桶宽覆盖（白名单只有三档）
		{"显式 5m 覆盖", 2 * time.Hour, "5m", 5 * time.Minute, 0},
		{"显式 day 覆盖", 2 * time.Hour, "day", 24 * time.Hour, 0},
	}

	for _, tc := range cases {
		from := base
		to := base.Add(tc.span)
		q := "/admin/stats/series?from=" + strconv.FormatInt(from.Unix(), 10) +
			"&to=" + strconv.FormatInt(to.Unix(), 10)
		if tc.bucket != "" {
			q += "&bucket=" + tc.bucket
		}
		win := parseSeriesWindow(httptest.NewRequest("GET", q, nil))

		if !win.custom {
			t.Errorf("%s: custom=false，from/to 没生效", tc.name)
			continue
		}
		if win.bucket != tc.wantBucket {
			t.Errorf("%s: bucket=%v want %v", tc.name, win.bucket, tc.wantBucket)
		}
		// 左闭右开：[start, end)，end 必须就是用户给的上界（不该被改写）
		if !win.end.Equal(to) {
			t.Errorf("%s: end=%v want %v（to 不该被改写）", tc.name, win.end, to)
		}
		// start 被 Truncate 到桶边界，所以可能早于 from，但差值必须 < 一个桶
		if win.start.After(from) || from.Sub(win.start) >= win.bucket {
			t.Errorf("%s: start=%v 与 from=%v 的关系不对（应 Truncate 到桶边界且不晚于 from）",
				tc.name, win.start, from)
		}
		if tc.wantBuckets > 0 && win.buckets != tc.wantBuckets {
			t.Errorf("%s: buckets=%d want %d", tc.name, win.buckets, tc.wantBuckets)
		}
		if win.buckets > 2000 {
			t.Errorf("%s: buckets=%d 超过上限 2000", tc.name, win.buckets)
		}
	}
}

// TestParseSeriesWindowRejectsBadCustom 守三条保护：非法/倒置/超限都回落 range 语义。
//
// 回落而不是报错，是刻意的：这个端点的调用方是前端图表，参数坏了它更希望
// 看到"默认 24h 图"，而不是一个 400 加一块空白面板。
func TestParseSeriesWindowRejectsBadCustom(t *testing.T) {
	base := time.Now().Add(-24 * time.Hour).Unix()
	u := func(v int64) string { return strconv.FormatInt(v, 10) }

	bad := []struct{ name, query string }{
		{"缺 to", "/admin/stats/series?from=" + u(base)},
		{"缺 from", "/admin/stats/series?to=" + u(base)},
		{"倒置", "/admin/stats/series?from=" + u(base+3600) + "&to=" + u(base)},
		{"跨度不足 1 分钟", "/admin/stats/series?from=" + u(base) + "&to=" + u(base+30)},
		{"跨度超 90 天", "/admin/stats/series?from=" + u(base) + "&to=" + u(base+91*24*3600)},
		{"非数字", "/admin/stats/series?from=abc&to=def"},
		{"零值", "/admin/stats/series?from=0&to=0"},
	}
	for _, tc := range bad {
		win := parseSeriesWindow(httptest.NewRequest("GET", tc.query, nil))
		if win.custom {
			t.Errorf("%s: 应当回落到 range 语义，却 custom=true（start=%v end=%v）",
				tc.name, win.start, win.end)
		}
		// 回落必须真的是 24h/1h，而不是零值窗口（零值会让图表画成空白）
		if win.bucket != time.Hour {
			t.Errorf("%s: 回落后的 bucket=%v want 1h", tc.name, win.bucket)
		}
		if win.buckets <= 0 {
			t.Errorf("%s: 回落后的 buckets=%d，零值窗口会让前端画不出图", tc.name, win.buckets)
		}
	}
}

// TestParseSeriesWindowRangeUnaffected range 语义必须与改造前完全一致
// （前端的两档快捷按钮依赖它，改坏了是静默的图表变化）。
func TestParseSeriesWindowRangeUnaffected(t *testing.T) {
	for _, tc := range []struct {
		q      string
		bucket time.Duration
	}{
		{"/admin/stats/series", time.Hour},                      // 缺省 24h/1h
		{"/admin/stats/series?range=24h", time.Hour},            //
		{"/admin/stats/series?range=7d", 24 * time.Hour},        //
		{"/admin/stats/series?range=7d&bucket=hour", time.Hour}, // 显式覆盖
	} {
		win := parseSeriesWindow(httptest.NewRequest("GET", tc.q, nil))
		if win.custom {
			t.Errorf("%s: range 模式不该是 custom", tc.q)
		}
		if win.bucket != tc.bucket {
			t.Errorf("%s: bucket=%v want %v", tc.q, win.bucket, tc.bucket)
		}
	}
}
