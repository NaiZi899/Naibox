// accountexport_ext.go 上游的「凭证导出」扩展点。
//
// # 为什么导出也要一个扩展点（与 AccountImportExt 对称）
//
// 导入时核心不解析凭证（"那段 JSON 是什么意思"由上游回答）。导出是同一个
// 问题的反方向：**只有上游知道自己的凭证长什么样、哪些字段是必需的**。
// codearts 的 STS + DPoP 私钥、cline 的单把 key、workbuddy 的嵌套 token，
// 核心一个都不认识 —— 让核心去拼这些文件，等于把九个上游的凭证格式
// 复制进核心，而"加新上游核心零改动"这条判据会当场失效。
//
// # 与导入的对称性（这是本接口的全部价值）
//
// 导入返回 `(FileName, Raw, UID, Nickname)`，导出返回同样的四个字段。
// 一个形状两个方向，于是"导出的东西能被导入原样吃回去"由**类型**保证，
// 而不是靠两份实现碰巧一致。
package gateway

// ExportedCredential 一条可再导入的凭证原文。
//
// 字段语义与 AccountImportExt.ImportCredentials 的返回值一一对应：
//
//	FileName 落盘文件名（不含目录）。空串 = 核心自己取名。
//	Raw      文件内容（上游自己的序列化结果）。
//	UID      账号池主键。空串 = 无法确定身份，该条视为导出失败。
//	Nickname 展示名（可空）。
type ExportedCredential struct {
	FileName string
	Raw      []byte
	UID      string
	Nickname string
}

// AccountExportExt 上游的凭证导出扩展（可选实现）。
//
// 没实现的上游由核心回落"把磁盘上的凭证文件逐字节拷出来"——
// 那同样保证往返（导入时读的就是同一份文件），只是核心不解释它。
//
// ⚠ 实现必须**幂等且无副作用**：不要在这里写盘、不要碰账号池。
// 与 AccountImportExt 同一条纪律。
type AccountExportExt interface {
	// ExportCredentials 导出指定账号的凭证原文。
	//
	// uids 为空 = 导出该上游全部账号（由核心决定范围时可能传空）。
	// 返回顺序不要求与 uids 一致（核心按 UID 建索引）。
	//
	// ⚠ 凭据拿不到时**必须返回带原因的 error 或跳过该条**，不能返回一个
	// 空 Raw 的条目 —— 那会让用户拿到一个"看起来有、其实打不开"的文件。
	ExportCredentials(uids []string) ([]ExportedCredential, error)

	// ExportNote 一句给用户看的说明（例如"导出的是 codearts 的 STS 凭证，
	// 含 DPoP 私钥，只能在本机使用"）。空串 = 不显示。
	ExportNote() string
}
