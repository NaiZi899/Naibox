package boot

import (
	"encoding/json"
	"strings"
	"testing"
)

// unmarshalInto 在**默认值之上**套一层 JSON 覆盖。
//
// 语义与生产一致：`Default()` 先给全套默认，`json.Unmarshal` 只覆盖出现过的键。
// 所以"键缺席"用例走的正是真实路径（得到默认 900），不是特判出来的。
func unmarshalInto(t *testing.T, cfg *Config, raw string) error {
	t.Helper()
	return json.Unmarshal([]byte(raw), cfg)
}

// ---------------------------------------------------------------------------
// 两个新配置项的形态校验。
//
// 这两项的错法都很**静默**，所以校验必须发生在启动期：
//
//	quota_refresh_interval_seconds=0   会被误当"未配置"回落成 900 → 用户关不掉
//	mgmt_base 写错一个字符             余额永远显示 `—`，没有任何报错
// ---------------------------------------------------------------------------

// TestPoolQuotaRefreshInterval 负数报错、0 合法、未配置走默认。
func TestPoolQuotaRefreshInterval(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    int
		wantErr bool
	}{
		// 键缺席 → Default() 的 900。
		{"未配置", `{}`, 900, false},
		// 显式 0 必须保留成 0：用户用它关掉周期刷新。
		{"显式关闭", `{"pool":{"quota_refresh_interval_seconds":0}}`, 0, false},
		{"常规值", `{"pool":{"quota_refresh_interval_seconds":300}}`, 300, false},
		// 负数是笔误（如把 900 写成 -900），不该被当成"关闭"。
		{"负数", `{"pool":{"quota_refresh_interval_seconds":-1}}`, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := Default()
			if err := unmarshalInto(t, cfg, c.raw); err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			err := cfg.normalize()
			if c.wantErr {
				if err == nil {
					t.Fatalf("应报错（负数不是关闭开关的写法），得到 nil；"+
						"当前值 %d", cfg.Pool.QuotaRefreshIntervalSeconds)
				}
				if !strings.Contains(err.Error(), "quota_refresh_interval_seconds") {
					t.Errorf("错误信息应点名配置项，得到: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("不该报错: %v", err)
			}
			if cfg.Pool.QuotaRefreshIntervalSeconds != c.want {
				t.Errorf("得到 %d，期望 %d", cfg.Pool.QuotaRefreshIntervalSeconds, c.want)
			}
		})
	}
}

// TestJiyuanMgmtBaseConfig mgmt_base 的七种形态。
func TestJiyuanMgmtBaseConfig(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{"留空用官方域名", `{"jiyuan":{"mgmt_base":""}}`, "", false},
		{"键缺席", `{"jiyuan":{"enabled":true}}`, "", false},
		{"https 常规", `{"jiyuan":{"mgmt_base":"https://tokenrhythm.studio"}}`, "https://tokenrhythm.studio", false},
		// 尾斜杠归一：否则拼出 `https://x//api/me`，抓包与日志里持续双斜杠。
		{"去尾斜杠", `{"jiyuan":{"mgmt_base":"https://mirror.example.com/"}}`, "https://mirror.example.com", false},
		{"带端口", `{"jiyuan":{"mgmt_base":"http://127.0.0.1:8080"}}`, "http://127.0.0.1:8080", false},
		// 缺协议是最常见的笔误：不拦的话表现为"余额永远未知"。
		{"缺协议", `{"jiyuan":{"mgmt_base":"tokenrhythm.studio"}}`, "", true},
		{"协议非法", `{"jiyuan":{"mgmt_base":"ftp://tokenrhythm.studio"}}`, "", true},
		{"缺主机名", `{"jiyuan":{"mgmt_base":"https://"}}`, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := Default()
			if err := unmarshalInto(t, cfg, c.raw); err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			err := cfg.normalize()
			if c.wantErr {
				if err == nil {
					t.Fatalf("形态非法应启动失败，得到 nil（值 %q）；"+
						"静默接受的表现是余额永远显示「—」", cfg.Jiyuan.MgmtBase)
				}
				if !strings.Contains(err.Error(), "mgmt_base") {
					t.Errorf("错误信息应点名配置项，得到: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("不该报错: %v", err)
			}
			if cfg.Jiyuan.MgmtBase != c.want {
				t.Errorf("得到 %q，期望 %q", cfg.Jiyuan.MgmtBase, c.want)
			}
		})
	}
}

// TestNormalizeBaseURLRejectsEmptyHost 主机名检查不能漏。
func TestNormalizeBaseURLRejectsEmptyHost(t *testing.T) {
	for _, raw := range []string{"https://", "https:///path", "http:///"} {
		if _, err := normalizeBaseURL(raw); err == nil {
			t.Errorf("%q 应被拒（没有主机名，请求会打到空主机）", raw)
		}
	}
}
