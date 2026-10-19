package desktop

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/NaiZi899/Naibox/internal/icon"
)

// trayAction 托盘菜单动作。
//
// 用常量而不是字符串：菜单文案会改（"打开控制台"→"显示控制台"），
// 但动作集合不会。文案与动作分离后，改文案不会漏改 switch 分支。
type trayAction int

const (
	actOpenConsole trayAction = iota
	actOpenBrowser
	actOpenDataDir
	actOpenTodayLog
	actBackupNow
	actToggleAutostart
	// 关窗行为三项是互斥单选（托盘菜单里没有真正的 radio，用三个勾选项 +
	// 手动互斥实现，见 app.setCloseBehavior）。
	actCloseAsk
	actCloseMinimize
	actCloseQuit
	actRestartService
	actQuit
)

// trayIconSize 托盘图标源尺寸。
//
// Windows 通知区域在 100%/125% 缩放下取 16/20/24 px，32 px 源图缩下去
// 最耐看（也覆盖 200% DPI）。
const trayIconSize = 32

// trayIconFileNames 允许覆盖托盘图标的外部文件名（exe 同级或数据根）。
//
// 只认这两个名字：NaiBox.ico 是改名后的正式名，omni2api.ico 留给升级上来的
// 老用户（他们的图标文件还叫这个）。泛化到 icon.ico 会误吃用户放在同目录里
// 别的程序留下的图标。
var trayIconFileNames = []string{"NaiBox.ico", "omni2api.ico"}

// trayItem 一个菜单项。
type trayItem struct {
	Title           string
	Tooltip         string
	Action          trayAction
	SeparatorBefore bool
	// Checkable 勾选项（开机自启）。
	Checkable bool
	Checked   bool
}

// traySpec 整份菜单。
type traySpec struct {
	Tooltip string
	Items   []trayItem
}

// trayHandle 已启动的托盘，供退出与状态同步使用。
//
// 实现方必须保证 Quit 可重入、且可从任意 goroutine 调用（退出路径可能由
// 窗口关闭、托盘退出、致命错误三条路同时触发）。
type trayHandle interface {
	// SetChecked 同步某个勾选项的显示状态。
	SetChecked(action trayAction, checked bool)
	// SetTooltip 换托盘图标提示（用来显示"服务已启动/端口"等状态）。
	SetTooltip(text string)
	Quit()
}

// trayState 生成菜单时需要在界面上反映出来的当前设置。
type trayState struct {
	Autostart     bool
	CloseBehavior CloseBehavior
}

// newTraySpec 生成菜单。
//
// 文案顺序即用户的操作频次顺序：看控制台 > 用浏览器打开 > 找数据/日志 >
// 备份 > 设置 > 重启/退出。危险动作（重启服务、退出）在最后一段，
// 与"备份数据"之间用分隔线隔开，避免误点。
//
// 「关闭窗口：…」三项是单选：菜单里只显示一个勾，切换时由 app.setCloseBehavior
// 手动互斥（systray 的 MenuItem 没有 radio 语义，勾选状态是三个独立布尔）。
// 它们排在「重启服务/退出」之前并且隔一条线 —— 用户在找"怎么退出"时不该
// 误点成"以后点 X 就直接退出"。
func newTraySpec(tooltip string, st trayState) traySpec {
	cur := normalizeCloseBehavior(string(st.CloseBehavior))
	return traySpec{
		Tooltip: tooltip,
		Items: []trayItem{
			{Title: "打开控制台", Tooltip: "显示内嵌窗口（隐藏到托盘后也从这里恢复）", Action: actOpenConsole},
			{Title: "在浏览器中打开", Tooltip: "用系统默认浏览器打开 /ui", Action: actOpenBrowser},
			{Title: "打开数据目录", Tooltip: "config.json / auths / data 所在位置", Action: actOpenDataDir, SeparatorBefore: true},
			{Title: "查看今日日志", Tooltip: "打开今天的 server-YYYYMMDD.log", Action: actOpenTodayLog},
			{Title: "备份数据", Tooltip: "把 config.json + auths + data 打成一个 zip", Action: actBackupNow},
			{Title: "开机自启", Tooltip: "登录 Windows 后自动运行", Action: actToggleAutostart, SeparatorBefore: true, Checkable: true, Checked: st.Autostart},
			{Title: "关闭窗口：每次询问", Tooltip: "点 X 时弹框问：最小化到托盘，还是退出", Action: actCloseAsk, SeparatorBefore: true, Checkable: true, Checked: cur == CloseAsk},
			{Title: "关闭窗口：最小化到托盘", Tooltip: "点 X 只隐藏窗口，网关继续在后台运行", Action: actCloseMinimize, Checkable: true, Checked: cur == CloseMinimize},
			{Title: "关闭窗口：直接退出", Tooltip: "点 X 就退出（先落盘再停机）", Action: actCloseQuit, Checkable: true, Checked: cur == CloseQuit},
			{Title: "重启服务", Tooltip: "先落盘再重新装配（改完配置不用重开窗口）", Action: actRestartService, SeparatorBefore: true},
			{Title: "退出", Tooltip: "落盘后退出", Action: actQuit},
		},
	}
}

// trayIconBytes 取托盘图标字节，**Windows 形状的 ICO**。
//
// 优先级：外置 omni2api.ico（exe 同级或数据根，用户可替换）→ internal/icon 的
// icon.ICO(32)。像素/编码只有一处来源，本包不画图也不写 ICO 编码器。
//
// # 为什么必须是 ICO 而不是 PNG
//
// energye/systray 的 Windows 分支把字节落到临时文件后调
// `LoadImageW(..., IMAGE_ICON, ..., LR_LOADFROMFILE)`，该 API 只认 .ico/.cur
// **内容**，不按内容嗅探格式。喂 PNG 时它只打一行
// `systray error: unable to set icon` 就返回 —— 不报错、不 panic，
// 托盘图标直接不可见。所以图标契约在桌面这一侧就固定成 ICO。
//
// 返回 error 时调用方应视作"托盘不可用"：没有图标的托盘等于没有托盘
// （图标不可见就点不到菜单），硬起一个空图标比明确失败更难查。
func trayIconBytes(l Layout) ([]byte, error) {
	for _, dir := range []string{executableDir(), l.DataDir} {
		for _, name := range trayIconFileNames {
			p := filepath.Join(dir, name)
			st, err := os.Stat(p)
			if err != nil || st.IsDir() || st.Size() > 1<<20 || st.Size() == 0 {
				continue
			}
			b, err := os.ReadFile(p)
			if err != nil || len(b) == 0 {
				continue
			}
			return b, nil
		}
	}
	ico, err := icon.ICO(trayIconSize)
	if err != nil {
		return nil, fmt.Errorf("生成托盘图标（internal/icon）: %w", err)
	}
	if len(ico) == 0 {
		return nil, fmt.Errorf("internal/icon 返回了空图标")
	}
	return ico, nil
}
