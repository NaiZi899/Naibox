package boot

import (
	"errors"
	"strings"
	"testing"

	"github.com/NaiZi899/Naibox/internal/auth"
)

// TestCheckEgressBinding 三条判据 + 解绑永远允许。
//
// 这是"装配期喊日志 / 运行期拒绝"共用的同一份判据 —— 所以它自己的边界
// 必须被钉死，否则两处会一起错。
func TestCheckEgressBinding(t *testing.T) {
	egress := []EgressConfig{
		{ID: "cn-1", Proxy: "socks5://127.0.0.1:1080", Realm: "cn", MaxAccounts: 1},
		{ID: "intl-1", Proxy: "socks5://127.0.0.1:1081", Realm: "intl", MaxAccounts: 2},
		{ID: "any", Proxy: "http://127.0.0.1:8080", Realm: "", MaxAccounts: 1},
	}

	cases := []struct {
		name    string
		channel string
		wantID  string
		bound   int
		wantErr error
	}{
		{"正常绑定", "cn", "cn-1", 0, nil},
		{"解绑永远允许（哪怕线路不存在）", "cn", "", 99, nil},
		{"线路不存在", "cn", "ghost", 0, ErrEgressUnknown},
		{"国内号挂海外线", "cn", "intl-1", 0, ErrEgressRealmMismatch},
		{"上限已满", "cn", "cn-1", 1, ErrEgressFull},
		{"未满（上限 2 用掉 1）", "intl", "intl-1", 1, nil},
		{"realm 为空 = 不限地区", "intl", "any", 0, nil},
		{"渠道未知时不判 realm", "", "intl-1", 0, nil},
	}
	for _, c := range cases {
		err := CheckEgressBinding(egress, c.channel, c.wantID, c.bound)
		if c.wantErr == nil {
			if err != nil {
				t.Errorf("%s: 应当允许，实际 %v", c.name, err)
			}
			continue
		}
		if !errors.Is(err, c.wantErr) {
			t.Errorf("%s: 应当返回 %v，实际 %v", c.name, c.wantErr, err)
		}
	}
}

// TestCountEgressBindings 计数要能排除自己（改绑时不能把自己的旧名额算进去）。
func TestCountEgressBindings(t *testing.T) {
	auths := []*auth.Auth{
		{UID: "u1", EgressID: "cn-1"},
		{UID: "u2", EgressID: "cn-1"},
		{UID: "u3", EgressID: ""},
		nil,
	}
	got := CountEgressBindings(auths, "")
	if got["cn-1"] != 2 {
		t.Fatalf("cn-1 应当有 2 个账号，实际 %d", got["cn-1"])
	}
	if _, ok := got[""]; ok {
		t.Fatal("未绑定的账号不该进计数（否则空 id 会变成一条线路）")
	}
	excl := CountEgressBindings(auths, "u1")
	if excl["cn-1"] != 1 {
		t.Fatalf("排除 u1 之后应当剩 1 个，实际 %d", excl["cn-1"])
	}
}

// TestDecodeEgressPatch 两种请求体都认（裸数组 / {"lines":[…]}）。
func TestDecodeEgressPatch(t *testing.T) {
	arr, err := decodeEgressPatch([]byte(`[{"id":"a","proxy":"socks5://127.0.0.1:1"}]`))
	if err != nil || len(arr) != 1 || arr[0].ID != "a" {
		t.Fatalf("裸数组解析失败: %v %+v", err, arr)
	}
	obj, err := decodeEgressPatch([]byte(`{"lines":[{"id":"b","proxy":"socks5://127.0.0.1:2"}]}`))
	if err != nil || len(obj) != 1 || obj[0].ID != "b" {
		t.Fatalf("对象形态解析失败: %v %+v", err, obj)
	}
	empty, err := decodeEgressPatch([]byte(`null`))
	if err != nil || empty != nil {
		t.Fatalf("null 应当解析成空表: %v %+v", err, empty)
	}
	if _, err := decodeEgressPatch([]byte(`"oops"`)); err == nil {
		t.Fatal("非数组非对象应当报错")
	}
}

// TestNormalizeEgressRejectsBadLines 坏线路 fail fast（保存与启动共用同一条校验）。
func TestNormalizeEgressRejectsBadLines(t *testing.T) {
	cases := []struct{ name, body, wantIn string }{
		{"空 id", `[{"id":"","proxy":"socks5://h:1"}]`, "id 不能为空"},
		{"重复 id", `[{"id":"a","proxy":"socks5://h:1"},{"id":"a","proxy":"socks5://h:2"}]`, "重复"},
		{"缺 proxy", `[{"id":"a","proxy":""}]`, "proxy 不能为空"},
		{"不支持的协议", `[{"id":"a","proxy":"ftp://h:1"}]`, "不受支持"},
		{"缺少主机", `[{"id":"a","proxy":"socks5://"}]`, "缺少主机名"},
		{"realm 非法", `[{"id":"a","proxy":"socks5://h:1","realm":"mars"}]`, "realm"},
		{"max_accounts 过大", `[{"id":"a","proxy":"socks5://h:1","max_accounts":9}]`, "max_accounts"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			lines, err := decodeEgressPatch([]byte(c.body))
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			cfg := Config{Egress: lines}
			err = cfg.normalizeEgress()
			if err == nil {
				t.Fatalf("应当拒绝: %s", c.body)
			}
			if !strings.Contains(err.Error(), c.wantIn) {
				t.Fatalf("错误信息应当提到 %q，实际 %q", c.wantIn, err.Error())
			}
		})
	}
}
