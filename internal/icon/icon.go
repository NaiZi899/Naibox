// Package icon 用代码画出 omni2api 的图标。
//
// # 为什么图标是代码而不是文件
//
// 仓库里不保留 .ico / .png：图标是**构建期派生物**，唯一入库的图形描述是
// assets/icon/app.svg（给人看的规格）与本文件（真正被调用的实现）。
// 这样"改图标"只有一处要改，也避免了二进制资产的常见病：进了版本库之后
// 没人敢动、改了看不出 diff、多份尺寸之间悄悄不一致。
//
// 图形语言见 assets/icon/app.svg：**浅蓝渐变圆角底板 + 深蓝字母 N**（NaiBox 首字母）。
// 底板留了内边距 —— 16px 的托盘图标里，贴边的底板会被任务栏视觉裁掉一圈。
//
// # 为什么 N 是深蓝而不是白色
//
// 白字压在浅蓝底上对比度只有约 1.3:1（两者亮度太接近），16px 下会糊成一片白斑，
// 任务栏里根本认不出是个字母。深蓝 #0d4a75 对浅蓝底约 6.5:1，小尺寸仍然清晰。
// 这也是"图标可辨认"的硬要求：托盘图标实际渲染高度往往只有 16–20px。
//
// # 三个入口
//
//	Draw(size)  画出 size×size 的 RGBA 图（唯一一份图形代码）
//	PNG(size)   单尺寸 PNG 字节（人眼校验、以及 ICO 里的大尺寸帧用）
//	ICO(sizes)  多尺寸/单尺寸 ICO；托盘图标与 exe 资源都用它
//
// # 托盘图标必须是 ICO 内容，不能给 PNG（这条是实测踩出来的）
//
// Windows 托盘的调用链是：字节 → %TEMP%\systray_temp_icon_<md5>（**无扩展名**）
// → LoadImageW(0, path, IMAGE_ICON, 0, 0, LR_LOADFROMFILE|LR_DEFAULTSIZE)。
// LoadImage 只按 ICO/CUR 的**内容**解析，不嗅探 PNG：喂 PNG 进去，它内部打一行
// `systray error: unable to set icon` 就返回 —— 不 panic、不报错、托盘直接没有
// 图标。这种静默失败最难查，所以：
//
//   - 托盘图标请用 ICO(...)（单尺寸调用会自动选 DIB 帧，见下）
//   - PNG() 只用于构建期与人工查看，**不要**把它当托盘图标的交付路径
//
// # 帧格式的选择规则
//
// 多尺寸（exe 资源，走 RT_ICON，Vista+ 全支持 PNG 帧）：≥64px 用 PNG 压，
// <64px 用 32bpp DIB。单尺寸调用则**一律 DIB**：单帧 ICO 的唯一已知消费者是
// LoadImageW，它对 PNG 帧的容忍度没有保证，而 32px 的 DIB 帧才 4KB。
//
// 只用标准库：image、image/color、image/png、encoding/binary、math。
// 不引第三方绘图/SVG 库 —— 这份图形只有三个基本形状，为它加一个依赖不划算。
package icon

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/png"
	"math"
	"sort"
)

// DefaultSizes 是 ICO 的默认尺寸集合。
//
// 16/32 是任务栏与资源管理器的实际取样档；48 是"中等图标"视图；256 是
// "大图标"与属性对话框。64/128 夹在中间，Windows 在 150%/200% 缩放下会挑它们。
var DefaultSizes = []int{16, 32, 48, 64, 128, 256}

// PNGFrameMinSize 是"用 PNG 帧"的下界：≥64px 用 PNG（压缩后几 KB，
// Vista+ 全支持），<64px 用 32bpp DIB（老工具、老资源编辑器只认这个格式，
// 而小尺寸用 DIB 每帧才 1–9KB，压不压无所谓）。
const PNGFrameMinSize = 64

// 几何常量：全部用 64×64 的设计栅格，与 assets/icon/app.svg 的 viewBox 一致。
// 改形状时改这里（并同步 SVG），两者的数字应当一一对应。
const (
	// 底板：3..61 的圆角方，圆角半径 15。比旧标多留了 1px 内边距。
	plateX0, plateY0, plateX1, plateY1 = 3.0, 3.0, 61.0, 61.0
	plateR                             = 15.0

	// 字母 N 的三条笔画：左竖 / 对角 / 右竖，圆头圆角（stroke 语义）。
	//
	// 尺寸按 16px 反推过：笔画直径 7.5 在 64 栅格上占 11.7%，到 16px 是 1.9 像素 ——
	// 再细就会在任务栏里断成一串点。横跨 22（21→43）保证字面占满底板的中间 2/3，
	// 这是图标字体常见的"视觉居中"比例（四周留白略大于笔画宽度）。
	nLeftX  = 21.0
	nRightX = 43.0
	nTopY   = 16.0
	nBotY   = 48.0
	// nStroke 是笔画**直径**（不是半径）。7.5 在 64 栅格上 ≈ 12% 边宽。
	nStroke = 7.5
)

// 配色。与 assets/icon/app.svg 的 <linearGradient> 取值相同。
//
// 浅蓝底：上浅下略深（#a8dcf7 → #6fbfef），避免纯色底板在大尺寸下显得死板。
// N 用深蓝 #0d4a75：白色在浅蓝上只有 1.3:1，见包注释里的实测理由。
var (
	plateFrom = [3]float64{0xa8, 0xdc, 0xf7}
	plateTo   = [3]float64{0x6f, 0xbf, 0xef}
	letterRGB = [3]float64{0x0d, 0x4a, 0x75}
)

const (
	// ss 超采样倍数：形状边缘每个方向采 4 个点再取平均。形状是解析式
	// （圆角矩形 / 三条带圆头的线段），没有纹理，4×4 已足够平滑；再高只是白烧 CPU。
	ss = 4
)

// Draw 画出 size×size 的图标（RGBA，alpha 预乘，符合 image.RGBA 的约定）。
//
// size ≤ 0 时按 1 处理：调用方传了脏数据的时候，返回一张 1×1 的图比 panic
// 更容易在日志里看出问题。
func Draw(size int) *image.RGBA {
	if size <= 0 {
		size = 1
	}
	img := image.NewRGBA(image.Rect(0, 0, size, size))

	const n = ss * ss
	// 逐通道累加**预乘**后的值，最后除以采样数即可 —— 预乘域的线性平均
	// 等价于"面积覆盖率加权"，不需要分别处理 alpha。
	acc := make([]uint32, size*size*4)
	step := 64.0 / float64(size) / float64(ss)

	for sy := 0; sy < size*ss; sy++ {
		v := (float64(sy) + 0.5) * step
		rowBase := (sy / ss) * size * 4
		for sx := 0; sx < size*ss; sx++ {
			u := (float64(sx) + 0.5) * step
			r, g, b, a := shade(u, v)
			i := rowBase + (sx/ss)*4
			acc[i+0] += uint32(r)
			acc[i+1] += uint32(g)
			acc[i+2] += uint32(b)
			acc[i+3] += uint32(a)
		}
	}

	for i := 0; i < size*size; i++ {
		for c := 0; c < 4; c++ {
			img.Pix[i*4+c] = uint8((acc[i*4+c] + n/2) / n)
		}
	}
	return img
}

// shade 返回一个采样点的预乘 RGBA（0..255）。
//
// 只有两层：浅蓝底板与深蓝字母 N。两者不重叠，所以 N 直接覆盖颜色，
// 不需要 alpha 混合 —— 旧版"底板 → 白气泡 → 蓝星"那三层 source-over 的
// 累加逻辑随图形一起删掉了。
func shade(u, v float64) (r, g, b, a uint32) {
	if !inRoundedRect(u, v, plateX0, plateY0, plateX1, plateY1, plateR) {
		// 预乘语义：alpha=0 时颜色分量也必须是 0，否则两者不自洽。
		return 0, 0, 0, 0
	}

	t := gradientT(u, v)
	cr := plateFrom[0] + (plateTo[0]-plateFrom[0])*t
	cg := plateFrom[1] + (plateTo[1]-plateFrom[1])*t
	cb := plateFrom[2] + (plateTo[2]-plateFrom[2])*t

	if inLetterN(u, v) {
		cr, cg, cb = letterRGB[0], letterRGB[1], letterRGB[2]
	}
	return clamp8(cr), clamp8(cg), clamp8(cb), 255
}

// gradientT 把点投影到底板的对角线上（左上→右下），返回 0..1。
// 与 SVG 里 objectBoundingBox 的 x1=0,y1=0,x2=1,y2=1 等价。
func gradientT(u, v float64) float64 {
	du := (u - plateX0) / (plateX1 - plateX0)
	dv := (v - plateY0) / (plateY1 - plateY0)
	t := (du + dv) / 2
	if t < 0 {
		return 0
	}
	if t > 1 {
		return 1
	}
	return t
}

func clamp8(x float64) uint32 {
	if x <= 0 {
		return 0
	}
	if x >= 255 {
		return 255
	}
	return uint32(math.Round(x))
}

// inRoundedRect 判断点是否落在圆角矩形内。做法是"把点钳到内矩形上，
// 超出的部分与角的半径比"—— 只有角上那段圆弧会用到平方，直边零成本。
func inRoundedRect(u, v, x0, y0, x1, y1, r float64) bool {
	if u < x0 || u > x1 || v < y0 || v > y1 {
		return false
	}
	dx := math.Max(0, math.Max(x0+r-u, u-(x1-r)))
	dy := math.Max(0, math.Max(y0+r-v, v-(y1-r)))
	return dx*dx+dy*dy <= r*r
}

// inLetterN 判断点是否落在字母 N 的笔画上。
//
// # 为什么用"到线段的距离"而不是多边形填充
//
// N 是三条**带圆头的粗线**（stroke 语义），不是一个闭合轮廓。用"点到线段
// 距离 ≤ 半径"直接就能得到圆头、圆角、以及正确的笔画宽度，不需要再手工
// 枚举拐角顶点 —— 旧版星形用射线法是因为它是凹多边形，N 不属于那种形状。
//
// 三条笔画：左竖（上→下）、对角（左上→右下）、右竖（下→上）。对角写正向
// 与反向对距离函数没有影响，这里按"从上往下"写便于与 SVG 的 path 对照。
func inLetterN(u, v float64) bool {
	r := nStroke / 2
	if segDist(u, v, nLeftX, nTopY, nLeftX, nBotY) <= r {
		return true
	}
	if segDist(u, v, nRightX, nBotY, nRightX, nTopY) <= r {
		return true
	}
	return segDist(u, v, nLeftX, nTopY, nRightX, nBotY) <= r
}

// segDist 返回点 (px,py) 到线段 (x1,y1)-(x2,y2) 的距离。
//
// 先求投影参数 t 并钳到 [0,1]（这一步就是"圆头"的来源：钳位后端点外侧的
// 点会量到端点本身，于是端部呈半圆），再算到投影点的欧氏距离。
func segDist(px, py, x1, y1, x2, y2 float64) float64 {
	dx, dy := x2-x1, y2-y1
	l2 := dx*dx + dy*dy
	if l2 == 0 {
		return math.Hypot(px-x1, py-y1)
	}
	t := ((px-x1)*dx + (py-y1)*dy) / l2
	if t < 0 {
		t = 0
	} else if t > 1 {
		t = 1
	}
	return math.Hypot(px-(x1+t*dx), py-(y1+t*dy))
}

// PNG 返回 size×size 的 PNG 字节（运行时托盘图标用）。
func PNG(size int) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, Draw(size)); err != nil {
		return nil, fmt.Errorf("icon: 编码 %dpx PNG: %w", size, err)
	}
	return buf.Bytes(), nil
}

// ICO 返回 ICO 字节。sizes 为空时用 DefaultSizes。
//
// 尺寸大于 256 会报错 —— ICO 的目录项用一个字节存宽高，256 只能编码成 0，
// 再大就表达不了。
//
// 帧格式：多尺寸时按尺寸混装（≥PNGFrameMinSize 用 PNG，其余 DIB）；
// **单尺寸调用一律用 DIB 帧** —— 托盘图标走 LoadImageW，见包注释里那条实测。
func ICO(sizes ...int) ([]byte, error) {
	single := len(sizes) == 1
	if len(sizes) == 0 {
		sizes = append([]int(nil), DefaultSizes...)
	}
	uniq := map[int]bool{}
	list := make([]int, 0, len(sizes))
	for _, s := range sizes {
		if s < 1 || s > 256 {
			return nil, fmt.Errorf("icon: ICO 尺寸 %d 越界（1..256）", s)
		}
		if uniq[s] {
			continue
		}
		uniq[s] = true
		list = append(list, s)
	}
	sort.Ints(list)

	type frame struct {
		size int
		data []byte
	}
	frames := make([]frame, 0, len(list))
	for _, s := range list {
		var (
			data []byte
			err  error
		)
		if s >= PNGFrameMinSize && !single {
			data, err = PNG(s)
			if err != nil {
				return nil, err
			}
		} else {
			data = encodeDIB(Draw(s))
		}
		frames = append(frames, frame{size: s, data: data})
	}

	// ICONDIR(6) + ICONDIRENTRY(16) × N + 各帧数据
	var out bytes.Buffer
	out.Grow(6 + 16*len(frames) + 64*1024)
	writeLE(&out, uint16(0), uint16(1), uint16(len(frames))) // reserved, type=1(icon), count

	offset := 6 + 16*len(frames)
	for _, f := range frames {
		dims := uint8(f.size)
		if f.size == 256 {
			dims = 0 // 256 在目录项里只能写成 0
		}
		out.WriteByte(dims)      // bWidth
		out.WriteByte(dims)      // bHeight
		out.WriteByte(0)         // bColorCount：32bpp 无调色板
		out.WriteByte(0)         // bReserved
		writeLE(&out, uint16(1)) // wPlanes
		writeLE(&out, uint16(32))
		writeLE(&out, uint32(len(f.data)))
		writeLE(&out, uint32(offset))
		offset += len(f.data)
	}
	for _, f := range frames {
		out.Write(f.data)
	}
	return out.Bytes(), nil
}

// encodeDIB 把图编码成 ICO 里的 32bpp 位图帧：
// BITMAPINFOHEADER + 自下而上的 **非预乘** BGRA + 1bpp AND 掩膜。
//
// 两处容易写错的地方：
//  1. ICO 的 DIB 是非预乘的（Windows 用自己的规则合成），而 image.RGBA 是预乘的
//     —— 所以这里必须除回去，否则半透明像素会偏亮。
//  2. biHeight 要写成 2×高度：上半是颜色图，下半是 AND 掩膜。
func encodeDIB(img *image.RGBA) []byte {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	var colorBuf bytes.Buffer
	colorBuf.Grow(w * h * 4)
	for y := h - 1; y >= 0; y-- { // 自下而上
		for x := 0; x < w; x++ {
			i := y*img.Stride + x*4
			a := img.Pix[i+3]
			r, g, b := img.Pix[i], img.Pix[i+1], img.Pix[i+2]
			if a > 0 && a < 255 {
				// 预乘 → 非预乘
				r = uint8(uint32(r) * 255 / uint32(a))
				g = uint8(uint32(g) * 255 / uint32(a))
				b = uint8(uint32(b) * 255 / uint32(a))
			}
			colorBuf.Write([]byte{b, g, r, a})
		}
	}

	stride := ((w + 31) / 32) * 4
	mask := make([]byte, 0, stride*h)
	for y := h - 1; y >= 0; y-- {
		row := make([]byte, stride)
		for x := 0; x < w; x++ {
			if img.Pix[y*img.Stride+x*4+3] == 0 {
				row[x>>3] |= 0x80 >> (uint(x) & 7) // 1 = 透明
			}
		}
		mask = append(mask, row...)
	}

	var out bytes.Buffer
	out.Grow(40 + colorBuf.Len() + len(mask))
	writeLE(&out,
		uint32(40),                       // biSize
		int32(w),                         // biWidth
		int32(h*2),                       // biHeight（颜色 + 掩膜）
		uint16(1),                        // biPlanes
		uint16(32),                       // biBitCount
		uint32(0),                        // biCompression = BI_RGB
		uint32(colorBuf.Len()+len(mask)), // biSizeImage
		int32(0), int32(0),               // 分辨率
		uint32(0), uint32(0), // 调色板
	)
	out.Write(colorBuf.Bytes())
	out.Write(mask)
	return out.Bytes()
}

// writeLE 把若干定长整数按小端写进 w。调用处按类型列出参数，读起来就是
// 结构体的字段顺序。
func writeLE(w *bytes.Buffer, vals ...any) {
	for _, v := range vals {
		_ = binary.Write(w, binary.LittleEndian, v)
	}
}
