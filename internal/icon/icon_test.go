package icon

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"testing"
)

// 测试只依赖标准库与内存里的字节：不读仓库文件、不读环境变量。图标是纯函数
// 产物，测试也应当是纯函数测试 —— 否则"图标对不对"会变成"某台机器上对不对"。

func TestDrawDimensionsAndAlpha(t *testing.T) {
	for _, size := range []int{9, 16, 32, 48, 64} {
		img := Draw(size)
		if got := img.Bounds().Dx(); got != size {
			t.Fatalf("Draw(%d) 宽度 = %d", size, got)
		}
		if got := img.Bounds().Dy(); got != size {
			t.Errorf("Draw(%d) 高度 = %d，应为正方形", size, got)
		}
		if len(img.Pix) != size*size*4 {
			t.Fatalf("Draw(%d) Pix 长度 = %d，应为 %d", size, len(img.Pix), size*size*4)
		}

		// 四个角落在圆角底板之外 → 必须完全透明（预乘图里 alpha=0 且 RGB=0）
		corners := [][2]int{{0, 0}, {size - 1, 0}, {0, size - 1}, {size - 1, size - 1}}
		for _, c := range corners {
			i := c[1]*img.Stride + c[0]*4
			if a := img.Pix[i+3]; a != 0 {
				t.Errorf("Draw(%d) 角点 (%d,%d) alpha = %d，应为 0（圆角外应透明）", size, c[0], c[1], a)
			}
			if img.Pix[i] != 0 || img.Pix[i+1] != 0 || img.Pix[i+2] != 0 {
				t.Errorf("Draw(%d) 角点 (%d,%d) 有颜色残留，预乘语义要求 alpha=0 时 RGB 也为 0",
					size, c[0], c[1])
			}
		}

		// 中心在气泡内部：必须完全不透明
		cx, cy := size/2, size/2
		i := cy*img.Stride + cx*4
		if a := img.Pix[i+3]; a != 255 {
			t.Errorf("Draw(%d) 中心 alpha = %d，应为 255", size, a)
		}

		// 至少要有一些不透明像素，否则说明形状被画到了画布外面
		opaque := 0
		for p := 3; p < len(img.Pix); p += 4 {
			if img.Pix[p] > 200 {
				opaque++
			}
		}
		if opaque < size*size/4 {
			t.Errorf("Draw(%d) 不透明像素只有 %d/%d，形状可能没落进画布", size, opaque, size*size)
		}
	}
}

func TestDrawIsDeterministic(t *testing.T) {
	a := Draw(24)
	b := Draw(24)
	if !bytes.Equal(a.Pix, b.Pix) {
		t.Fatal("同一尺寸两次 Draw 结果不同：图标必须是可复现派生物")
	}
}

func TestDrawNonPositiveSizeDoesNotPanic(t *testing.T) {
	for _, size := range []int{0, -7} {
		img := Draw(size)
		if img.Bounds().Dx() != 1 {
			t.Errorf("Draw(%d) 应退化为 1×1，实际 %v", size, img.Bounds())
		}
	}
}

func TestPNGRoundTrip(t *testing.T) {
	data, err := PNG(32)
	if err != nil {
		t.Fatalf("PNG(32) 失败: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("PNG(32) 解不回来: %v", err)
	}
	if img.Bounds().Dx() != 32 || img.Bounds().Dy() != 32 {
		t.Fatalf("PNG(32) 尺寸 = %v", img.Bounds())
	}
}

// icoEntry 是 ICONDIRENTRY 的解析结果。
type icoEntry struct {
	width, height int
	bpp           int
	bytes         int
	offset        int
	isPNG         bool
}

// parseICOHeader 独立解析 ICO 结构（不经 ICO() 内部实现），这样测试验的是
// "产出的字节符合 ICO 规范"，而不是"产出的字节和实现自己以为的一致"。
func parseICOHeader(t *testing.T, data []byte) []icoEntry {
	t.Helper()
	if len(data) < 6 {
		t.Fatalf("ICO 太短：%d 字节", len(data))
	}
	if r := binary.LittleEndian.Uint16(data[0:]); r != 0 {
		t.Fatalf("ICONDIR.reserved = %d，应为 0", r)
	}
	if typ := binary.LittleEndian.Uint16(data[2:]); typ != 1 {
		t.Fatalf("ICONDIR.type = %d，应为 1(icon)", typ)
	}
	count := int(binary.LittleEndian.Uint16(data[4:]))
	if len(data) < 6+16*count {
		t.Fatalf("声明 %d 帧，但目录区长度不足（共 %d 字节）", count, len(data))
	}
	entries := make([]icoEntry, count)
	for i := 0; i < count; i++ {
		off := 6 + 16*i
		w := int(data[off])
		if w == 0 {
			w = 256 // 0 是 256 的编码
		}
		h := int(data[off+1])
		if h == 0 {
			h = 256
		}
		entries[i] = icoEntry{
			width:  w,
			height: h,
			bpp:    int(binary.LittleEndian.Uint16(data[off+6:])),
			bytes:  int(binary.LittleEndian.Uint32(data[off+8:])),
			offset: int(binary.LittleEndian.Uint32(data[off+12:])),
		}
		if entries[i].offset+entries[i].bytes > len(data) {
			t.Fatalf("第 %d 帧越界：offset=%d bytes=%d 总长=%d",
				i, entries[i].offset, entries[i].bytes, len(data))
		}
		frame := data[entries[i].offset : entries[i].offset+entries[i].bytes]
		entries[i].isPNG = bytes.HasPrefix(frame, []byte("\x89PNG\r\n\x1a\n"))
	}
	return entries
}

func TestICOHeaderAndFrames(t *testing.T) {
	data, err := ICO()
	if err != nil {
		t.Fatalf("ICO() 失败: %v", err)
	}
	entries := parseICOHeader(t, data)

	if len(entries) != len(DefaultSizes) {
		t.Fatalf("帧数 = %d，应为 %d", len(entries), len(DefaultSizes))
	}
	for i, want := range DefaultSizes {
		got := entries[i]
		if got.width != want || got.height != want {
			t.Errorf("第 %d 帧 = %d×%d，应为 %d×%d", i, got.width, got.height, want, want)
		}
		if got.bpp != 32 {
			t.Errorf("第 %d 帧 bpp = %d，应为 32", i, got.bpp)
		}
		// ≥64 用 PNG 帧，<64 用 DIB 帧
		wantPNG := want >= PNGFrameMinSize
		if got.isPNG != wantPNG {
			t.Errorf("%dpx 帧 isPNG = %v，应为 %v", want, got.isPNG, wantPNG)
		}
		if !got.isPNG {
			// DIB 帧的第一个字段是 BITMAPINFOHEADER.biSize = 40
			frame := data[got.offset : got.offset+got.bytes]
			if biSize := binary.LittleEndian.Uint32(frame); biSize != 40 {
				t.Errorf("%dpx DIB 帧 biSize = %d，应为 40", want, biSize)
			}
			// 帧长必须自洽：头 40 + 像素 w*h*4 + AND 掩膜 stride*h
			stride := ((want + 31) / 32) * 4
			if wantLen := 40 + want*want*4 + stride*want; got.bytes != wantLen {
				t.Errorf("%dpx DIB 帧长 = %d，按结构应为 %d", want, got.bytes, wantLen)
			}
		}
	}

	// 256 帧必须存在：任务栏大图标与属性对话框都用它
	has256 := false
	for _, e := range entries {
		if e.width == 256 && e.height == 256 && e.isPNG {
			has256 = true
		}
	}
	if !has256 {
		t.Fatal("ICO 里没有 256×256 的 PNG 帧")
	}
}

func TestICOSizeSelectionAndDedupe(t *testing.T) {
	data, err := ICO(32, 16, 32)
	if err != nil {
		t.Fatalf("ICO(32,16,32) 失败: %v", err)
	}
	entries := parseICOHeader(t, data)
	if len(entries) != 2 {
		t.Fatalf("去重后帧数 = %d，应为 2", len(entries))
	}
	if entries[0].width != 16 || entries[1].width != 32 {
		t.Fatalf("帧未按升序排列：%d, %d", entries[0].width, entries[1].width)
	}
}

// TestICOSingleFrameTrayShape 钉住托盘图标那条硬约束：
//
//	energye/systray 把字节写进一个**无扩展名**的临时文件，再 LoadImageW(
//	IMAGE_ICON, LR_LOADFROMFILE)。LoadImage 只认 ICO 内容，喂 PNG 会静默失败
//	（托盘没有图标，且不报错）。所以 ICO(32) 必须是标准的单帧 ICO：
//	ICONDIR(type=1) + 1 条 ICONDIRENTRY(32×32, 32bpp) + DIB 帧。
func TestICOSingleFrameTrayShape(t *testing.T) {
	data, err := ICO(32)
	if err != nil {
		t.Fatalf("ICO(32) 失败: %v", err)
	}
	if len(data) < 6 {
		t.Fatalf("ICO(32) 只有 %d 字节", len(data))
	}
	// 00 00 = reserved，01 00 = type(icon)，下面再跟 count
	if got := [4]byte{data[0], data[1], data[2], data[3]}; got != [4]byte{0x00, 0x00, 0x01, 0x00} {
		t.Fatalf("ICONDIR 前 4 字节 = % x，应为 00 00 01 00（reserved=0, type=1）", got)
	}
	if count := binary.LittleEndian.Uint16(data[4:]); count != 1 {
		t.Fatalf("帧数 = %d，应为 1", count)
	}
	if w, h := data[6], data[7]; w != 32 || h != 32 {
		t.Fatalf("ICONDIRENTRY 宽高 = %d×%d，应为 32×32", w, h)
	}
	if bpp := binary.LittleEndian.Uint16(data[6+6:]); bpp != 32 {
		t.Fatalf("ICONDIRENTRY bpp = %d，应为 32", bpp)
	}
	frame := data[22:] // 6 + 16
	if biSize := binary.LittleEndian.Uint32(frame); biSize != 40 {
		t.Fatalf("单帧不是 DIB（biSize=%d）：托盘走 LoadImage，不能用 PNG 帧", biSize)
	}
	if biHeight := int32(binary.LittleEndian.Uint32(frame[8:])); biHeight != 64 {
		t.Fatalf("DIB biHeight = %d，应为 64（32 像素 + 32 行 AND 掩膜）", biHeight)
	}
}

// TestICOSingleFrameAlwaysDIB 单尺寸调用不看尺寸，一律 DIB：64px 在默认多尺寸
// 集合里是 PNG 帧，但单独请求 64 时必须是 DIB，否则同一个尺寸出现两种字节，
// "托盘该拿哪个"就又变成要读文档才知道的事。
func TestICOSingleFrameAlwaysDIB(t *testing.T) {
	data, err := ICO(64)
	if err != nil {
		t.Fatalf("ICO(64) 失败: %v", err)
	}
	frame := data[22:]
	if biSize := binary.LittleEndian.Uint32(frame); biSize != 40 {
		t.Fatalf("ICO(64) 单帧 biSize = %d，应为 40（DIB）", biSize)
	}
	entries := parseICOHeader(t, data)
	if len(entries) != 1 || entries[0].isPNG {
		t.Fatalf("ICO(64) 应为 1 个 DIB 帧，实际 %+v", entries)
	}
}

func TestICORejectsOutOfRangeSizes(t *testing.T) {
	for _, size := range []int{0, -1, 257} {
		if _, err := ICO(size); err == nil {
			t.Errorf("ICO(%d) 应报错：ICO 目录项表达不了这个尺寸", size)
		}
	}
}

func TestICOIsDeterministic(t *testing.T) {
	a, err := ICO(16, 32)
	if err != nil {
		t.Fatalf("第一次 ICO 失败: %v", err)
	}
	b, err := ICO(16, 32)
	if err != nil {
		t.Fatalf("第二次 ICO 失败: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("两次 ICO() 字节不同：构建产物必须可复现（否则每个 tag 的 exe 都不一样）")
	}
}
