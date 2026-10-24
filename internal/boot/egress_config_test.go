// egress_config_test.go 出站线路（egress 段）的配置校验。
//
// 判据都是"坏配置必须 fail fast"：静默忽略的表现是**账号还是走共用出口**，
// 而共用出口正是这一层要消掉的东西 —— 配了没生效比配错更难查。
package boot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeEgressConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoadEgressHappyPath 合法线路被归一（max_accounts 缺省 → 1）。
func TestLoadEgressHappyPath(t *testing.T) {
	cfgPath := writeEgressConfig(t, `{
	  "listen": "127.0.0.1:0",
	  "egress": [
	    {"id":"cn-home-01","proxy":"socks5://u:p@127.0.0.1:1081","realm":"cn","region":"CN-ZJ"},
	    {"id":"intl-01","proxy":"http://127.0.0.1:20001","realm":"INTL","max_accounts":2}
	  ]
	}`)
	cfg, err := LoadAt(cfgPath, t.TempDir())
	if err != nil {
		t.Fatalf("合法 egress 不该报错: %v", err)
	}
	if len(cfg.Egress) != 2 {
		t.Fatalf("egress 条目 = %d，期望 2", len(cfg.Egress))
	}
	if cfg.Egress[0].MaxAccounts != 1 {
		t.Errorf("max_accounts 缺省应为 1（IP 复用即关联），实际 %d", cfg.Egress[0].MaxAccounts)
	}
	// realm 大小写不敏感（用户写 INTL 不该被当成非法）。
	if cfg.Egress[1].Realm != "intl" {
		t.Errorf("realm 应归一成小写，实际 %q", cfg.Egress[1].Realm)
	}
}

// TestLoadEgressRejectsBadEntries 五类坏项都必须让启动失败。
func TestLoadEgressRejectsBadEntries(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string // 错误里必须出现的关键词
	}{
		{
			name: "id 为空",
			body: `[{"id":"","proxy":"http://127.0.0.1:1"}]`,
			want: "id 不能为空",
		},
		{
			name: "id 重复",
			body: `[{"id":"a","proxy":"http://127.0.0.1:1"},{"id":"a","proxy":"http://127.0.0.1:2"}]`,
			want: "重复",
		},
		{
			name: "proxy 为空",
			body: `[{"id":"a","proxy":""}]`,
			want: "proxy 不能为空",
		},
		{
			name: "协议不支持",
			body: `[{"id":"a","proxy":"ftp://127.0.0.1:1"}]`,
			want: "不受支持",
		},
		{
			name: "realm 非法",
			body: `[{"id":"a","proxy":"http://127.0.0.1:1","realm":"mars"}]`,
			want: "realm",
		},
		{
			name: "账号数超过上限",
			body: `[{"id":"a","proxy":"http://127.0.0.1:1","max_accounts":4}]`,
			want: "max_accounts",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath := writeEgressConfig(t, `{"listen":"127.0.0.1:0","egress":`+tc.body+`}`)
			_, err := LoadAt(cfgPath, t.TempDir())
			if err == nil {
				t.Fatalf("坏配置必须 fail fast（静默忽略会让账号继续走共用出口）")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息应包含 %q，实际: %v", tc.want, err)
			}
		})
	}
}

// TestLoadRestrictedRotateMaxDefaultZero 默认 0（立即停手），负数报错。
func TestLoadRestrictedRotateMaxDefaultZero(t *testing.T) {
	cfg, err := LoadAt("", t.TempDir())
	if err != nil {
		t.Fatalf("默认配置加载失败: %v", err)
	}
	if cfg.Pool.RestrictedRotateMax != 0 {
		t.Errorf("restricted_rotate_max 默认必须是 0（风控命中即停手），实际 %d",
			cfg.Pool.RestrictedRotateMax)
	}

	bad := writeEgressConfig(t, `{"listen":"127.0.0.1:0","pool":{"restricted_rotate_max":-1}}`)
	if _, err := LoadAt(bad, t.TempDir()); err == nil {
		t.Error("负数必须报错（0 是合法且有意义的默认值，不能被 <=0 回落规则吞掉）")
	}
}

// TestRedactProxy 日志里的代理凭据必须被抹掉。
//
// 粘性会话的 session-id 就写在代理用户名里，整串进日志等于写进日志文件
// 与 issue 附件。
func TestRedactProxy(t *testing.T) {
	got := redactProxy("socks5://session-abc:secret@127.0.0.1:1081")
	if strings.Contains(got, "secret") || strings.Contains(got, "session-abc") {
		t.Errorf("凭据泄漏: %q", got)
	}
	if !strings.HasPrefix(got, "socks5://") || !strings.Contains(got, "127.0.0.1:1081") {
		t.Errorf("应保留 scheme 与 host 便于认线路: %q", got)
	}
}
