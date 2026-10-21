package server

import (
	"errors"
	"strings"
	"testing"

	"github.com/NaiZi899/Naibox/internal/gateway"
)

// TestUpstreamFailureRedactedOnlyForAccountRestricted 只对会被客户端误判的类别剥原文。
//
// # 两个方向都要守
//
//	风控类   必须剥：正文里同时有 403 与 "request illegal"，客户端据此
//	         判成 AUTH 并显示「API 密钥无效」，把用户和客服都带到错方向
//	模型/余额类 必须留：那是用户自己能处理的东西（换模型、充值），
//	         剥掉只剩"上游 200（none）"，把可操作的错误变成不可操作的
func TestUpstreamFailureRedactedOnlyForAccountRestricted(t *testing.T) {
	restricted := &chatError{
		Kind:   gateway.ErrKindAccountRestricted,
		Status: 403,
		Msg:    `{"code":11140,"msg":"request illegal","displayMsg":{"en":"Your account has been restricted due to unusual activity"}}`,
	}
	got := upstreamFailureRedacted(restricted)
	if got == "" {
		t.Fatal("风控类应返回替代文案（原文必须被剥掉）")
	}
	for _, leak := range []string{"request illegal", "11140", "403"} {
		if strings.Contains(got, leak) {
			t.Errorf("替代文案里仍含 %q：%q —— 客户端会据此判成 AUTH", leak, got)
		}
	}

	modelErr := &chatError{
		Kind:   gateway.ErrKindNone,
		Status: 200,
		Msg:    "InferHub.002002009.404: The model is not registered, please request other model",
	}
	if got := upstreamFailureRedacted(modelErr); got != "" {
		t.Errorf("模型类不该被剥（原文必须透出给客户端），得到 %q", got)
	}

	quotaErr := &chatError{Kind: gateway.ErrKindHardCredit, Status: 402, Msg: "insufficient credit"}
	if got := upstreamFailureRedacted(quotaErr); got != "" {
		t.Errorf("余额类不该被剥，得到 %q", got)
	}

	if got := upstreamFailureRedacted(errors.New("transport boom")); got != "" {
		t.Errorf("非 chatError 应返回空串（调用方保留原文），得到 %q", got)
	}
}
