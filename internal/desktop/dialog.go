package desktop

// 消息框图标风格。数值与 Win32 的 MB_ICON* 一致，非 Windows 平台忽略。
//
// 放在公共文件而不是 platform_windows.go：app.go 是无 build tag 的，
// 直接引用平台文件里的常量会让 GOOS=linux 的整仓编译失败 —— 而这正是
// "build tag 隔离是否有效"的验收项。
const (
	mbIconError    uint32 = 0x00000010
	mbIconWarning  uint32 = 0x00000030
	mbIconQuestion uint32 = 0x00000020
	// mbYesNo 是/否两个按钮（关窗询问框用它）。
	mbYesNo uint32 = 0x00000004
)

// 消息框按钮的返回值（MB_YESNO 只有这两个有语义）。
const (
	idNo  int32 = 7
	idYes int32 = 6
)
