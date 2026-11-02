// legacyconfig.go 清理已删除功能在 config.json 里留下的段。
//
// # 为什么物理删除，而不是"读的时候忽略"
//
// 提示词网关（mode / file / 降级重试）已经整体删除。字段从结构体里消失之后，
// 配置文件里那段 JSON 会变成纯粹的死键：不报错、不生效、也没人知道它没用。
// 留着它的代价不是字节数，是误导 —— 后来者看到 prompt.mode 会以为还能配。
//
// 所以启动时清一次：删键、原子写回、其它键原样保留。
//
// # 为什么失败只记日志不阻断启动
//
// 老配置 + 只读盘（容器只挂载只读 config、或权限收紧的部署）是常见组合。
// 让"清不掉一个废弃键"变成"服务起不来"，是把整洁当成了可用性。
// 下次有写权限时再清即可 —— 这个清理是幂等的。
package boot

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// legacyPromptSection 已删除功能的配置段名。
const legacyPromptSection = "prompt"

// stripLegacyPromptSection 删除 config.json 里废弃的 prompt 段。
//
// 返回是否真的改写了文件。以下情形一律返回 (false, nil)，**不碰文件**：
//
//	path 为空 / 文件不存在      → 没有可清理的对象
//	JSON 解析失败               → 用户手改坏了，宁可留着也不写坏（写回会把它规范化)
//	本来就没有这个键            → 幂等：第二次启动不产生备份、不产生空写
//
// 解析失败这一条尤其重要：此时把整个文档反序列化再序列化回去，会把用户
// 正在手工修的内容按 map 顺序重排。诊断现场最不需要的就是这个。
func stripLegacyPromptSection(path string) (bool, error) {
	if path == "" {
		return false, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return false, nil
	}
	if _, ok := doc[legacyPromptSection]; !ok {
		return false, nil
	}
	delete(doc, legacyPromptSection)

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return false, err
	}
	out = append(out, '\n')

	// 原子写回：先写同名临时文件再 rename，避免半截文件。
	// 权限沿用 0600（配置里可能有 api_key）。
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	return true, nil
}

// legacyConfigDir 返回配置文件所在目录（供测试与日志使用；空路径返回空串）。
func legacyConfigDir(path string) string {
	if path == "" {
		return ""
	}
	if d := filepath.Dir(path); d != "." {
		return d
	}
	return ""
}
