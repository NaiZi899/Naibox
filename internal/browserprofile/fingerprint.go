package browserprofile

// fingerprint.go 每个档案一套**可复现**的指纹参数，以及"这个浏览器到底吃不吃"的探测。
//
// # 为什么参数要固化在档案里，而不是让用户手敲
//
// 指纹的价值在于**稳定**：同一个档案每次启动必须报出同一套值，否则上游看到的
// 是"同一台设备每五分钟换一次显卡"，比不伪装更显眼。手敲参数做不到这点 ——
// 忘了填、填错、复制少一位，都看不出来。
//
// # 为什么只给这些参数名
//
// 下面每个开关都来自**实测证据**（拆 AntWorkBuddy 时从它的命令里捞到的
// `--fingerprint-*` 一族），不是我按感觉编的。没有证据的维度（例如时区）这里
// 不做：编一个不存在的参数，chromium 会**静默忽略**，表现为"配了没生效"，
// 比不支持更坏。
//
// # 与"支持探测"的配合
//
// 原版 Chrome / Edge 不认识这些参数，会静默丢掉。所以启动前必须先问一句
// "这个可执行文件里到底有没有这些开关"（见 SupportsFingerprint），
// 没有就明确拒绝启动，而不是开一个指纹没变的窗口骗自己。

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Fingerprint 一套指纹参数。零值 = 不启用指纹（启动参数里一个 `--fingerprint-*` 都不出现）。
type Fingerprint struct {
	// CanvasNoise 给 canvas 读回的像素加噪声。
	CanvasNoise bool `json:"canvas_noise,omitempty"`
	// ClientRectsNoise 给 getBoundingClientRect 的数字加噪声（防 DOM 尺寸指纹）。
	ClientRectsNoise bool `json:"client_rects_noise,omitempty"`
	// GPUVendor / GPURenderer WebGL 的 unmasked vendor/renderer。
	GPUVendor   string `json:"gpu_vendor,omitempty"`
	GPURenderer string `json:"gpu_renderer,omitempty"`
	// WebGLVendor WebGL vendor 字符串（部分变体单独提供）。
	WebGLVendor string `json:"webgl_vendor,omitempty"`
	// Platform navigator.platform（如 "Win32" / "MacIntel"）。
	Platform string `json:"platform,omitempty"`
	// PlatformVersion navigator.userAgentData.platformVersion 的覆写。
	PlatformVersion string `json:"platform_version,omitempty"`
	// ScreenWidth 报告的屏幕宽度（像素）。变体只提供宽度这一项。
	ScreenWidth int `json:"screen_width,omitempty"`
	// TouchPoints navigator.maxTouchPoints。
	TouchPoints int `json:"touch_points,omitempty"`
	// DoNotTrack 发送 DNT: 1。
	DoNotTrack bool `json:"do_not_track,omitempty"`
	// Lang 语言（同时写 --lang 与 --accept-lang）。
	Lang string `json:"lang,omitempty"`
}

// Empty 是否等于"不启用指纹"。
func (f Fingerprint) Empty() bool {
	return !f.CanvasNoise && !f.ClientRectsNoise && f.GPUVendor == "" && f.GPURenderer == "" &&
		f.WebGLVendor == "" && f.Platform == "" && f.PlatformVersion == "" &&
		f.ScreenWidth == 0 && f.TouchPoints == 0 && !f.DoNotTrack && f.Lang == ""
}

// Normalize 校验并归一。范围取"真实设备可能存在"的区间，超界直接报错：
// 一个 4000 宽的"手机"或 99 个触点的"桌面"本身就是异常特征。
func (f *Fingerprint) Normalize() error {
	trim := func(s string) string {
		out := make([]rune, 0, len(s))
		for _, r := range s {
			if r == '\n' || r == '\r' || r == 0 {
				continue
			}
			out = append(out, r)
		}
		return string(out)
	}
	f.GPUVendor = trim(f.GPUVendor)
	f.GPURenderer = trim(f.GPURenderer)
	f.WebGLVendor = trim(f.WebGLVendor)
	f.Platform = trim(f.Platform)
	f.PlatformVersion = trim(f.PlatformVersion)
	f.Lang = trim(f.Lang)

	if f.ScreenWidth != 0 && (f.ScreenWidth < 800 || f.ScreenWidth > 7680) {
		return fmt.Errorf("screen_width=%d 超出合理区间（800–7680）", f.ScreenWidth)
	}
	if f.TouchPoints < 0 || f.TouchPoints > 10 {
		return fmt.Errorf("touch_points=%d 超出合理区间（0–10）", f.TouchPoints)
	}
	if f.Lang != "" {
		for _, r := range f.Lang {
			ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '-' || r == '_' || r >= '0' && r <= '9'
			if !ok {
				return fmt.Errorf("lang=%q 含非法字符（只要如 zh-CN / en-US）", f.Lang)
			}
		}
	}
	for label, v := range map[string]string{
		"gpu_vendor": f.GPUVendor, "gpu_renderer": f.GPURenderer,
		"webgl_vendor": f.WebGLVendor, "platform": f.Platform, "platform_version": f.PlatformVersion,
	} {
		if len(v) > 120 {
			return fmt.Errorf("%s 过长（最多 120 字符）", label)
		}
	}
	return nil
}

// Args 生成启动参数。
//
// # 顺序是固定的
//
// 同一份 Fingerprint 每次必须产出**逐字节相同**的参数向量 —— 那正是"固化"
// 的含义。map 遍历在这里是禁止的（Go 的 map 顺序随机），所以逐项显式 append。
func (f Fingerprint) Args() []string {
	if f.Empty() {
		return nil
	}
	var out []string
	if f.CanvasNoise {
		out = append(out, "--fingerprint-canvas-noise")
	}
	if f.ClientRectsNoise {
		out = append(out, "--fingerprint-client-rects-noise")
	}
	if f.GPUVendor != "" {
		out = append(out, "--fingerprint-gpu-vendor="+f.GPUVendor)
	}
	if f.GPURenderer != "" {
		out = append(out, "--fingerprint-gpu-renderer="+f.GPURenderer)
	}
	if f.WebGLVendor != "" {
		out = append(out, "--fingerprint-webgl-vendor="+f.WebGLVendor)
	}
	if f.Platform != "" {
		out = append(out, "--fingerprint-platform="+f.Platform)
	}
	if f.PlatformVersion != "" {
		out = append(out, "--fingerprint-platform-version="+f.PlatformVersion)
	}
	if f.ScreenWidth > 0 {
		out = append(out, "--fingerprint-screen-width="+itoa(f.ScreenWidth))
	}
	if f.TouchPoints > 0 {
		out = append(out, "--fingerprint-touch-points="+itoa(f.TouchPoints))
	}
	if f.DoNotTrack {
		out = append(out, "--fingerprint-do-not-track")
	}
	if f.Lang != "" {
		// 同时给两个头：navigator.language 看 --lang，HTTP 的 Accept-Language
		// 看 --accept-lang。只给一个会出现"页面内是中英混排"的割裂。
		out = append(out, "--lang="+f.Lang, "--accept-lang="+f.Lang)
	}
	return out
}

// itoa 小整数转字符串（避免为几个字段引入 strconv 的额外分支）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// fingerprintMarkers 变体里一定出现的开关名。
//
// 取多个而不是一个：某个构建可能裁掉某个功能，但只要还有一个在，它就是
// 变体而不是原版。原版 Chrome / Edge 里一个都不会有。
var fingerprintMarkers = [][]byte{
	[]byte("--fingerprint-canvas-noise"),
	[]byte("--fingerprint-webgl-vendor"),
	[]byte("--fingerprint-gpu-vendor"),
	[]byte("--fingerprint-platform"),
	[]byte("--fingerprint-screen-width"),
	[]byte("--fingerprint-client-rects-noise"),
}

// supportCache 探测结果缓存。
//
// 扫一个 200 MB 的浏览器可执行文件要一两秒；档案页每次刷新都扫一遍是不可接受的。
// 键带上大小与修改时间：换了浏览器（或升级了版本）自然重新探测。
var (
	supportMu    sync.Mutex
	supportCache = map[string]supportEntry{}
)

type supportEntry struct {
	size  int64
	mtime time.Time
	ok    bool
	err   string
}

// SupportsFingerprint 判断这个可执行文件是不是"认识 --fingerprint-* 的变体"。
//
// # 为什么是扫文件，而不是启动一次试探
//
// 原版浏览器对未知开关**不报错**（静默忽略），启动一次什么也证明不了 —— 除非
// 再去读渲染出来的 navigator.gpu，那要 CDP，代价与稳定性都不划算。
// 直接在二进制里找这些开关的字符串是确定性的：有就是有。
//
// 返回 false 且 err 为 nil 表示"读到了，但里面没有这些开关"（= 原版）。
func SupportsFingerprint(exePath string) (bool, error) {
	st, err := os.Stat(exePath)
	if err != nil {
		return false, fmt.Errorf("读取浏览器失败: %w", err)
	}
	if st.IsDir() {
		return false, fmt.Errorf("%s 是目录，不是可执行文件", exePath)
	}
	key := exePath + "|" + itoa(int(st.Size())) + "|" + st.ModTime().UTC().Format(time.RFC3339Nano)
	supportMu.Lock()
	if e, ok := supportCache[key]; ok {
		supportMu.Unlock()
		if e.err != "" {
			return false, fmt.Errorf("%s", e.err)
		}
		return e.ok, nil
	}
	supportMu.Unlock()

	ok, scanErr := scanForMarkers(exePath)
	entry := supportEntry{size: st.Size(), mtime: st.ModTime(), ok: ok}
	if scanErr != nil {
		entry.err = scanErr.Error()
	}
	supportMu.Lock()
	supportCache[key] = entry
	supportMu.Unlock()
	if scanErr != nil {
		return false, scanErr
	}
	return ok, nil
}

// scanForMarkers 流式扫一遍文件，找任意一个 marker。
//
// 分块读 + 相邻块重叠（重叠长度 = 最长 marker - 1），否则跨块边界的匹配会漏。
func scanForMarkers(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("打开浏览器失败: %w", err)
	}
	defer f.Close()

	longest := 0
	for _, m := range fingerprintMarkers {
		if len(m) > longest {
			longest = len(m)
		}
	}
	const chunk = 1 << 20
	r := bufio.NewReaderSize(f, chunk)
	buf := make([]byte, chunk+longest)
	keep := 0
	for {
		n, rerr := io.ReadFull(r, buf[keep:keep+chunk])
		window := buf[:keep+n]
		for _, m := range fingerprintMarkers {
			if bytes.Contains(window, m) {
				return true, nil
			}
		}
		if rerr != nil {
			if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
				return false, nil
			}
			return false, fmt.Errorf("读取浏览器失败: %w", rerr)
		}
		// 把尾部一段挪到开头，供下一块拼接
		keep = longest - 1
		if keep > 0 {
			copy(buf[:keep], window[len(window)-keep:])
		}
	}
}
