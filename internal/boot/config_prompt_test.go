// config_prompt_test.go 提示词配置的**启动期**契约：模式收敛、env 覆盖、指纹自检。
//
// # 为什么这几条必须锁在 boot 层
//
// internal/prompt 的测试只能证明"给定 mode 时怎么改写 body"，
// 证明不了"这个 mode 从哪里来、写错时会发生什么"。线上最容易踩的三件事
// 全在装配层：
//
//	① mode 拼错（"apend"）被静默当成 custom —— 用户以为切了模式，其实没有
//	② env 覆盖（回滚通道）不生效，或非法值把配置里的选择踩掉
//	③ 提示词正文自己带指纹 —— 配置里看到的与上游收到的不是同一份
//
// 所以这里全部走 Load（= Default + json + applyEnv + normalize），
// 而不是直接构造 Config 再调 normalize：env 那两条只有走完整管线才测得到，
// 而"env 在 normalize 之前生效"正是需要被钉住的顺序。
package boot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NaiZi899/Naibox/internal/prompt"
)

// promptConfigWire 只带 prompt 段的最小配置镜像。
//
// 用 Marshal 生成 JSON 而不是手拼字符串：Windows 临时目录里的路径带反斜杠，
// 手拼进 JSON 少转义一个就得到一个"看起来对但解不出来"的配置，
// 而失败信息会指向 JSON 解析而不是本意要测的校验逻辑。
type promptConfigWire struct {
	Prompt struct {
		Mode string `json:"mode"`
		File string `json:"file"`
	} `json:"prompt"`
}

// writePromptConfig 在 t.TempDir() 里落一份 config.json，返回其路径。
//
// # 为什么每次都清空 NAIBOX_PROMPT_MODE
//
// 那是**覆盖**而非默认值：机器上残留一个 NAIBOX_PROMPT_MODE 就会让
// "配置决定模式"这类断言假红或假绿，而失败原因与被测代码毫无关系。
// 空串在 applyEnv 里等价于"未设置"（`v != ""` 才覆盖）。
func writePromptConfig(t *testing.T, mode, file string) string {
	t.Helper()
	t.Setenv("NAIBOX_PROMPT_MODE", "")

	var w promptConfigWire
	w.Prompt.Mode = mode
	w.Prompt.File = file
	raw, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("序列化测试配置: %v", err)
	}
	fp := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(fp, raw, 0o600); err != nil {
		t.Fatalf("写 config.json: %v", err)
	}
	return fp
}

// writePromptText 在临时目录里落一份提示词文件，返回其路径。
func writePromptText(t *testing.T, body string) string {
	t.Helper()
	fp := filepath.Join(t.TempDir(), "prompt.md")
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatalf("写提示词文件: %v", err)
	}
	return fp
}

// TestPromptModeAppendAccepted mode=append 合法，且正文被真的加载进来。
//
// 断言 PromptText 非空是这条用例的重点：Load 之后 PromptText 才是出站
// 客户端实际会用的那段文本。若 normalize 只认了 mode 却没加载正文，
// wire 层的 applyPrompt 会拿到空串并回落内置默认 —— 表现为
// "配了追加提示词，实际追加的是内置人格"，静默且难查。
func TestPromptModeAppendAccepted(t *testing.T) {
	const body = "追加人格：只写差异，不复述内置默认。\n"
	pf := writePromptText(t, body)
	c, err := Load(writePromptConfig(t, prompt.ModeAppend, pf))
	if err != nil {
		t.Fatalf("mode=append 应合法，却启动失败: %v", err)
	}
	if c.PromptMode != prompt.ModeAppend {
		t.Errorf("PromptMode = %q，期望 %q", c.PromptMode, prompt.ModeAppend)
	}
	if strings.TrimSpace(c.PromptText) == "" {
		t.Fatal("PromptText 为空 —— append 会退化成'追加一份内置默认人格'")
	}
	if c.PromptText != body {
		t.Errorf("PromptText = %q，期望文件原文 %q", c.PromptText, body)
	}
}

// TestPromptModeTypoIsFatalAndNamesAllThree 拼错的 mode 必须启动失败，
// 且错误信息列出三个合法值。
//
// # 为什么不能静默当 custom
//
// "apend" 静默按 custom 处理，等于把客户端 system 整段删掉：用户以为
// 保留了客户端人格（他想要 append 就是为了保留），实际被替换了。
// 现象是"我的项目约定怎么不管用了"，而配置里明明写着 append。
func TestPromptModeTypoIsFatalAndNamesAllThree(t *testing.T) {
	_, err := Load(writePromptConfig(t, "apend", ""))
	if err == nil {
		t.Fatal("mode=apend 必须启动失败（静默当 custom = 静默失效）")
	}
	for _, want := range []string{prompt.ModeCustom, prompt.ModeAppend, prompt.ModePassthrough} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息里应列出合法值 %q，实际: %v", want, err)
		}
	}
}

// TestPromptModeEnvOverridesConfig env 覆盖发生在 applyEnv，早于 normalize。
//
// 这条钉的是**顺序**：若 env 在 normalize 之后才生效，config 里的 custom
// 会被先写成 PromptMode=custom，随后 env 只改了 c.Prompt.Mode 而没人再读一次，
// 于是回滚通道完全失效 —— 而现场止血时唯一能用的就是它。
func TestPromptModeEnvOverridesConfig(t *testing.T) {
	fp := writePromptConfig(t, prompt.ModeCustom, "")
	t.Setenv("NAIBOX_PROMPT_MODE", prompt.ModePassthrough)

	c, err := Load(fp)
	if err != nil {
		t.Fatalf("env 覆盖 passthrough 时不该报错: %v", err)
	}
	if c.PromptMode != prompt.ModePassthrough {
		t.Errorf("PromptMode = %q，期望被 NAIBOX_PROMPT_MODE 覆盖成 %q", c.PromptMode, prompt.ModePassthrough)
	}
}

// TestPromptModeEnvInvalidIgnored env 非法值被忽略，配置值照常生效。
//
// 与 config.json 里的非法值处理刻意**不同**：那边报错，这边只打日志。
// 理由是这个 env 是应急通道 —— 值写错时仍然要能起来，否则回滚工具本身
// 变成新的故障点。断言"配置值不被踩掉"正是这条差异的验收点。
func TestPromptModeEnvInvalidIgnored(t *testing.T) {
	fp := writePromptConfig(t, prompt.ModeCustom, "")
	t.Setenv("NAIBOX_PROMPT_MODE", "passthru") // 拼错

	c, err := Load(fp)
	if err != nil {
		t.Fatalf("env 非法值不该让启动失败（它是应急通道）: %v", err)
	}
	if c.PromptMode != prompt.ModeCustom {
		t.Errorf("PromptMode = %q，期望仍是配置里的 %q（非法 env 应被忽略而非覆盖）", c.PromptMode, prompt.ModeCustom)
	}
}

// TestPromptFingerprintFileFatalForCustomAndAppend custom / append 下，
// 正文命中脱敏指纹 → 启动失败。
//
// # 为什么这两种模式要 fail fast
//
// 这两条路径都会把正文**真发出去**，而 sanitize 层会在出站前把命中串
// 逐串改写（裸 11128 → 11-128）。于是"配置文件里写的"和"上游收到的"
// 不是同一份 —— 静默失真，用户对着配置查半天也查不出差异。
func TestPromptFingerprintFileFatalForCustomAndAppend(t *testing.T) {
	const dirty = "调试备忘：拦截码 11128 会整单被拒。\n"
	for _, mode := range []string{prompt.ModeCustom, prompt.ModeAppend} {
		t.Run(mode, func(t *testing.T) {
			pf := writePromptText(t, dirty)
			_, err := Load(writePromptConfig(t, mode, pf))
			if err == nil {
				t.Fatalf("mode=%s 且正文含指纹时必须启动失败（否则会被 sanitize 静默改写）", mode)
			}
			if !strings.Contains(err.Error(), "指纹") {
				t.Errorf("错误信息应说明是指纹问题，实际: %v", err)
			}
			if !strings.Contains(err.Error(), pf) {
				t.Errorf("错误信息应指出来源文件 %s，实际: %v", pf, err)
			}
		})
	}
}

// TestPromptFingerprintFileToleratedInPassthrough passthrough 下同一份
// 含指纹的文件只告警、不阻断。
//
// # 为什么这里必须放行（而不是继续 fail fast）
//
// passthrough 根本不会发出这段文本（文件只被校验、不被使用），所以
// "被 sanitize 改写"在它身上不成立。更要紧的是：passthrough 正是
// NAIBOX_PROMPT_MODE 这条回滚通道要落到的模式。若此处照样阻断，
// 用户在现场止血时会撞上"切成 passthrough 也起不来，因为人格文件本身带指纹"
// —— 唯一能救火的那条路当场失效。
func TestPromptFingerprintFileToleratedInPassthrough(t *testing.T) {
	const dirty = "调试备忘：拦截码 11128 会整单被拒。\n"
	pf := writePromptText(t, dirty)

	c, err := Load(writePromptConfig(t, prompt.ModePassthrough, pf))
	if err != nil {
		t.Fatalf("mode=passthrough 且文件含指纹时不应阻断启动（它是回滚通道）: %v", err)
	}
	if c.PromptMode != prompt.ModePassthrough {
		t.Errorf("PromptMode = %q，期望 %q", c.PromptMode, prompt.ModePassthrough)
	}
}
