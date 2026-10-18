// Command genicon 生成构建期图标资源，并可校验 PE 里到底有没有图标。
//
// 用法：
//
//	go run ./tools/genicon -out .build/icon/app.ico
//	go run ./tools/genicon -out .build/icon/app.ico -png .build/icon
//	go run ./tools/genicon -verify dist/omni2api-desktop.exe
//
// 为什么是一个 Go 命令而不是一段 Python/ImageMagick 脚本：图标的图形代码已经
// 在 internal/icon 里了（构建期与运行时共用同一份），这里只是把它落盘。多一个
// 语言就多一套依赖要装、多一处"本地和 CI 画出来不一样"的可能。
//
// -verify 存在的理由：`go build` 成功并不代表图标进了 exe —— .syso 没被链接器
// 收进去时编译器一声不响，双击得到的还是默认白框。把"有没有图标"变成一条
// 读 PE 头就能回答的断言，CI 与本地都能一眼看到。
package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/NaiZi899/omni2api/internal/icon"
)

func main() {
	var (
		out       = flag.String("out", filepath.Join(".build", "icon", "app.ico"), "ICO 输出路径")
		pngDir    = flag.String("png", "", "可选：同时输出各尺寸 PNG 到这个目录（人眼校验用）")
		sizeList  = flag.String("sizes", "16,32,48,64,128,256", "ICO 里包含的尺寸，逗号分隔")
		verifyExe = flag.String("verify", "", "改为校验：读该 PE 文件的子系统与资源目录，不做生成")
	)
	flag.Parse()

	var err error
	if *verifyExe != "" {
		err = verifyPE(*verifyExe)
	} else {
		err = generate(*out, *pngDir, *sizeList)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "genicon:", err)
		os.Exit(1)
	}
}

func parseSizes(spec string) ([]int, error) {
	fields := strings.Split(spec, ",")
	sizes := make([]int, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		n, err := strconv.Atoi(f)
		if err != nil {
			return nil, fmt.Errorf("尺寸 %q 不是整数", f)
		}
		if n < 1 || n > 256 {
			return nil, fmt.Errorf("尺寸 %d 越界（ICO 目录项只能表达 1..256）", n)
		}
		sizes = append(sizes, n)
	}
	if len(sizes) == 0 {
		return nil, errors.New("-sizes 为空")
	}
	sort.Ints(sizes)
	return sizes, nil
}

func generate(outPath, pngDir, sizeSpec string) error {
	sizes, err := parseSizes(sizeSpec)
	if err != nil {
		return err
	}

	data, err := icon.ICO(sizes...)
	if err != nil {
		return fmt.Errorf("生成 ICO: %w", err)
	}
	if err := writeFile(outPath, data); err != nil {
		return err
	}
	frames, err := describeICO(data)
	if err != nil {
		return fmt.Errorf("自检 ICO: %w", err)
	}
	fmt.Printf("ICO  %s  %d 字节  %d 帧：%s\n", outPath, len(data), len(frames), strings.Join(frames, ", "))

	if pngDir != "" {
		// 1024 不进 ICO（表达不了），但"大图预览 / README / 设计核对"需要它。
		for _, size := range append(append([]int(nil), sizes...), 1024) {
			png, err := icon.PNG(size)
			if err != nil {
				return err
			}
			p := filepath.Join(pngDir, fmt.Sprintf("app-%d.png", size))
			if err := writeFile(p, png); err != nil {
				return err
			}
			fmt.Printf("PNG  %s  %d 字节\n", p, len(png))
		}
	}
	return nil
}

func writeFile(path string, data []byte) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("建目录 %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("写 %s: %w", path, err)
	}
	return nil
}

// describeICO 解析刚生成的 ICO 并回报每帧格式。生成完立刻自检，比等 Windows
// 那边"图标怎么没了"要省事得多。
func describeICO(data []byte) ([]string, error) {
	if len(data) < 6 {
		return nil, errors.New("字节不足以构成 ICONDIR")
	}
	if typ := binary.LittleEndian.Uint16(data[2:]); typ != 1 {
		return nil, fmt.Errorf("ICONDIR.type = %d，应为 1", typ)
	}
	count := int(binary.LittleEndian.Uint16(data[4:]))
	out := make([]string, 0, count)
	for i := 0; i < count; i++ {
		off := 6 + 16*i
		if off+16 > len(data) {
			return nil, fmt.Errorf("第 %d 条目录项越界", i)
		}
		w, h := int(data[off]), int(data[off+1])
		if w == 0 {
			w = 256
		}
		if h == 0 {
			h = 256
		}
		frameOff := int(binary.LittleEndian.Uint32(data[off+12:]))
		frameLen := int(binary.LittleEndian.Uint32(data[off+8:]))
		if frameOff+frameLen > len(data) {
			return nil, fmt.Errorf("第 %d 帧数据越界", i)
		}
		format := "bmp"
		if strings.HasPrefix(string(data[frameOff:frameOff+4]), "\x89PNG") {
			format = "png"
		}
		out = append(out, fmt.Sprintf("%d×%d/%s", w, h, format))
	}
	return out, nil
}

// verifyPE 读 PE 头回答两件事：
//
//  1. Subsystem 是不是 2（WINDOWS_GUI）—— 不是就意味着双击会弹控制台窗。
//     写错时编译器不会报错，只有读头才知道。
//  2. 资源目录（DataDirectory[2]）是不是非空 —— 为空说明图标与 manifest
//     根本没进 exe（.syso 没被链接器收进去），双击看到的就是默认白框。
func verifyPE(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读 %s: %w", path, err)
	}
	if len(data) < 0x40 || data[0] != 'M' || data[1] != 'Z' {
		return fmt.Errorf("%s 不是 PE 文件（缺 MZ 头）", path)
	}
	e := int(binary.LittleEndian.Uint32(data[0x3C:]))
	if e+0x18+2 > len(data) || string(data[e:e+4]) != "PE\x00\x00" {
		return fmt.Errorf("%s 的 PE 签名不对", path)
	}
	subsystem := binary.LittleEndian.Uint16(data[e+0x18+0x44:])
	opt := e + 0x18
	magic := binary.LittleEndian.Uint16(data[opt:])
	dd := opt + 96 // PE32
	if magic == 0x20b {
		dd = opt + 112 // PE32+
	}
	rvaImgDir := uint32(0)
	szImgDir := uint32(0)
	if dd+16*3 <= len(data) {
		rvaImgDir = binary.LittleEndian.Uint32(data[dd+2*8:])
		szImgDir = binary.LittleEndian.Uint32(data[dd+2*8+4:])
	}

	fmt.Printf("PE  %s\n", path)
	fmt.Printf("    Subsystem        = %d %s\n", subsystem, subsystemNote(subsystem))
	fmt.Printf("    资源目录         RVA=0x%x size=%d %s\n", rvaImgDir, szImgDir, resourceNote(szImgDir))

	var problems []string
	if subsystem != 2 {
		problems = append(problems, "子系统不是 WINDOWS_GUI(2)：双击会弹控制台窗口")
	}
	if szImgDir == 0 {
		problems = append(problems, "资源目录为空：图标 / manifest 没进 exe（检查 cmd/desktop/rsrc.syso 是否被链接）")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "；"))
	}
	fmt.Println("    OK：GUI 子系统 + 资源段非空（图标与 DPI 清单都在）")
	return nil
}

func subsystemNote(v uint16) string {
	switch v {
	case 2:
		return "(WINDOWS_GUI)"
	case 3:
		return "(WINDOWS_CUI)"
	default:
		return ""
	}
}

func resourceNote(size uint32) string {
	if size == 0 {
		return "(空)"
	}
	return "(非空)"
}
