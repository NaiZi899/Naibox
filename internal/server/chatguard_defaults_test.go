package server

import (
	"testing"
	"time"

	"github.com/NaiZi899/Naibox/internal/auth"
)

// TestStreamGuardDefaultsAreSafe 守住"配置缺省方向必须更安全"。
//
// # 为什么这条要独立存在
//
// 闸门的值来自 cfg，而 NewHandler 是**唯一的**兜底点（装配层漏传、
// 测试只给最小 Config 都会走到这里）。若这里不兜底，零值意味着
// "闸门不装" —— 也就是**改造前的挂死形态**（实测 380282ms / tokens=0）。
// 缺省必须往安全方向倒。
//
// 同时反向钉住"不误杀正常长回答"：15 分钟是上限不是目标，
// 所以默认绝不能小到把正常慢回答掐掉。
func TestStreamGuardDefaultsAreSafe(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, nil),
	})
	if h.cfg.StreamMax <= 0 {
		t.Errorf("StreamMax=%v —— 零值/负值意味着「不装总时长闸门」，"+
			"也就是改造前的挂死形态。缺省必须兜底。", h.cfg.StreamMax)
	}
	if h.cfg.StreamFirstByte <= 0 {
		t.Errorf("StreamFirstByte=%v —— 缺省必须兜底（0 = 不装看门狗）", h.cfg.StreamFirstByte)
	}
	// 上限必须是"天花板"量级：远大于任何正常回答，同时是个有限的数。
	if h.cfg.StreamMax < time.Minute {
		t.Errorf("StreamMax=%v 太激进 —— 会把正常的长回答误杀", h.cfg.StreamMax)
	}
	if h.cfg.StreamMax > time.Hour {
		t.Errorf("StreamMax=%v 太大 —— 挂死请求仍会占用连接很久（15 分钟量级才是意图）", h.cfg.StreamMax)
	}
	// 首帧看门狗必须比总时长短得多，否则它永远不会先到点、形同不存在。
	if h.cfg.StreamFirstByte >= h.cfg.StreamMax {
		t.Errorf("StreamFirstByte=%v >= StreamMax=%v —— 看门狗永远不会先触发",
			h.cfg.StreamFirstByte, h.cfg.StreamMax)
	}
}

// TestStreamGuardExplicitValuesWin 显式配置必须被尊重（兜底不能把用户的值盖掉）。
func TestStreamGuardExplicitValuesWin(t *testing.T) {
	h := NewHandler(Config{
		Pool:            testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:        newFakeUpstream(t, nil),
		StreamMax:       90 * time.Second,
		StreamFirstByte: 5 * time.Second,
	})
	if h.cfg.StreamMax != 90*time.Second {
		t.Errorf("StreamMax=%v，显式配置被兜底盖掉了", h.cfg.StreamMax)
	}
	if h.cfg.StreamFirstByte != 5*time.Second {
		t.Errorf("StreamFirstByte=%v，显式配置被兜底盖掉了", h.cfg.StreamFirstByte)
	}
}
