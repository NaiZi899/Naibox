// series.go —— GET /admin/stats/series 时间序列聚合（仪表盘趋势图的数据源）。
//
// # 为什么单独一个端点而不是塞进 /admin/stats
//
// /admin/stats 回答"累计到今天一共多少"，趋势图回答"过去 24h 每小时多少"。
// 两者是不同形状的响应（标量集合 vs 桶序列），拆开各自清晰；
// 数据源复用 statsItems 的缓存（同一个 request-log / 环形缓冲），
// 不重复扫描。
package admin

import (
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// statsSeriesItem 一个时间桶的聚合。
type statsSeriesItem struct {
	TS     string `json:"ts"`     // RFC3339 桶起点（小时/天）
	Total  int64  `json:"total"`  // 请求数
	OK     int64  `json:"ok"`     // 成功数
	Fail   int64  `json:"fail"`   // 失败数
	Tokens int64  `json:"tokens"` // 消耗 token（usage 缺失不计）
}

// registerStatsSeries 注册趋势图端点。
func (h *Handler) registerStatsSeries() {
	h.register("GET /admin/stats/series", h.statsSeries)
}

// statsSeries GET /admin/stats/series
//
// 两种取窗方式（互斥，from/to 优先）：
//
//	?range=24h|7d&bucket=hour|day          固定窗口（仪表盘的快捷档）
//	?from=<unix>&to=<unix>&bucket=5m|1h|day 自定义区间（"看看昨天 14:00 到 18:00"）
//
// 桶按本地时区对齐；空桶补 0（前端只画图，不补缺）。
//
// # 两种模式下"裁尾部空桶"的行为**刻意不同**
//
// range 模式裁掉尾部的全空桶：24h 视图里"最后一小时还没数据"是常态，
// 留着会让折线莫名掉到 0 再截断。
//
// 自定义模式**不裁**：用户明确指定了 [from, to)，那个区间就是他要看的范围。
// 裁掉会把"这段确实没请求"表现成"这段不存在"，比留一条平的 0 更容易误读。
func (h *Handler) statsSeries(w http.ResponseWriter, r *http.Request) {
	win := parseSeriesWindow(r)
	items, _, err := h.statsItems()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取日志失败: "+err.Error())
		return
	}

	out := make([]statsSeriesItem, 0, win.buckets+1)
	// 先铺满空桶（保证前端不需要补缺）
	idx := make(map[int64]int, win.buckets+1)
	for i := 0; i <= win.buckets; i++ {
		ts := win.start.Add(win.bucket * time.Duration(i))
		idx[ts.Unix()] = i
		out = append(out, statsSeriesItem{TS: ts.Format(time.RFC3339)})
	}
	for _, e := range items {
		// 左闭右开：与 from/to 的语义一致，避免边界上的请求被两个相邻区间重复计。
		if e.At.Before(win.start) || !e.At.Before(win.end) {
			continue
		}
		i, ok := idx[e.At.Truncate(win.bucket).Unix()]
		if !ok {
			continue
		}
		out[i].Total++
		if e.Status >= 200 && e.Status < 300 {
			out[i].OK++
		} else {
			out[i].Fail++
		}
		if e.Tokens >= 0 {
			out[i].Tokens += int64(e.Tokens)
		}
	}
	if !win.custom {
		// 去掉尾部全空桶（活跃窗口之外），保留从 start 起的连续段
		for len(out) > 0 && out[len(out)-1].Total == 0 {
			out = out[:len(out)-1]
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"range":  win.span.String(),
		"bucket": win.bucket.String(),
		"source": "log",
		// from/to 回显实际窗口：调用方给的时间会被 Truncate 到桶边界，
		// 不回显的话前端画的横轴与后端算的桶会差最多一个桶宽。
		"from":   win.start.Format(time.RFC3339),
		"to":     win.end.Format(time.RFC3339),
		"custom": win.custom,
		"series": out,
	})
}

// seriesWindow 一次请求的时间窗口。
type seriesWindow struct {
	start, end time.Time
	bucket     time.Duration
	buckets    int
	span       time.Duration
	custom     bool
}

// parseSeriesWindow 解析请求，决定 [start, end)、桶宽与桶数。
//
// 自定义区间的三条保护（都是为了让"前端传了奇怪参数"不至于把服务打歪）：
//   - to 必须晚于 from，且跨度至少 1 分钟
//   - 跨度上限 90 天 —— 超过就回落 24h（日志缓冲只有几千条，扫更远没有数据，
//     却会把桶数组铺到几千个）
//   - 桶数上限 2000 —— 由桶宽自动选档保证，这里再兜一次
func parseSeriesWindow(r *http.Request) seriesWindow {
	q := r.URL.Query()
	now := time.Now()

	if win, ok := customWindow(q); ok {
		return win
	}

	span, bucket := seriesSpan(r)
	start := now.Add(-span).Truncate(bucket)
	return seriesWindow{
		start: start, end: now, bucket: bucket,
		buckets: int(span / bucket), span: span,
	}
}

// customWindow 解析 from/to；参数不成立时返回 ok=false（回落 range 语义）。
func customWindow(q url.Values) (seriesWindow, bool) {
	f, err1 := strconv.ParseInt(q.Get("from"), 10, 64)
	t, err2 := strconv.ParseInt(q.Get("to"), 10, 64)
	if err1 != nil || err2 != nil || f <= 0 || t <= 0 {
		return seriesWindow{}, false
	}
	from, to := time.Unix(f, 0), time.Unix(t, 0)
	span := to.Sub(from)
	if span < time.Minute || span > 90*24*time.Hour {
		return seriesWindow{}, false
	}
	bucket := autoBucket(span)
	// bucket 显式覆盖（只认这三档，别的值忽略 —— 让前端无法把桶数撑爆）
	switch q.Get("bucket") {
	case "5m":
		bucket = 5 * time.Minute
	case "1h":
		bucket = time.Hour
	case "day":
		bucket = 24 * time.Hour
	}
	start := from.Truncate(bucket)
	buckets := int(to.Sub(start) / bucket)
	if buckets > 2000 {
		buckets = 2000
	}
	return seriesWindow{
		start: start, end: to, bucket: bucket,
		buckets: buckets, span: span, custom: true,
	}, true
}

// autoBucket 按跨度挑选桶宽，目标是把桶数控制在几十个的量级
// （图上一屏能看清；几十个点比一千个点更可读，也少画很多 SVG 节点）。
func autoBucket(span time.Duration) time.Duration {
	switch {
	case span <= 2*time.Hour:
		return 5 * time.Minute
	case span <= 48*time.Hour:
		return time.Hour
	default:
		return 24 * time.Hour
	}
}

// seriesSpan 解析 range/bucket 参数；非法回落 24h/hour。
func seriesSpan(r *http.Request) (time.Duration, time.Duration) {
	span := 24 * time.Hour
	bucket := time.Hour
	switch r.URL.Query().Get("range") {
	case "7d":
		span = 7 * 24 * time.Hour
		bucket = 24 * time.Hour
	case "24h":
		span = 24 * time.Hour
		bucket = time.Hour
	}
	// bucket 显式覆盖（默认随 range）。
	//
	// 支持 hour 与 day 两档 —— 原来只认 day，于是 `range=7d&bucket=hour`
	// 会静默得到天桶（参数被忽略、也不报错）。自定义区间那边用的是
	// 5m/1h/day 白名单，这里补上 hour 让两边的可取值对得上。
	switch r.URL.Query().Get("bucket") {
	case "hour":
		bucket = time.Hour
	case "day":
		bucket = 24 * time.Hour
	}
	return span, bucket
}
