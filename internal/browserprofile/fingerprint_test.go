package browserprofile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFingerprintArgsAreStableAndOrdered 参数向量必须逐字节稳定：
// 同一份档案每次启动都要报出同一套指纹（map 遍历在这里是禁忌）。
func TestFingerprintArgsAreStableAndOrdered(t *testing.T) {
	f := Fingerprint{
		CanvasNoise: true, ClientRectsNoise: true,
		GPUVendor: "NVIDIA Corporation", GPURenderer: "ANGLE (NVIDIA GeForce RTX 3060)",
		WebGLVendor: "Google Inc. (NVIDIA)", Platform: "Win32", PlatformVersion: "15.0.0",
		ScreenWidth: 1920, TouchPoints: 0, DoNotTrack: true, Lang: "zh-CN",
	}
	want := []string{
		"--fingerprint-canvas-noise",
		"--fingerprint-client-rects-noise",
		"--fingerprint-gpu-vendor=NVIDIA Corporation",
		"--fingerprint-gpu-renderer=ANGLE (NVIDIA GeForce RTX 3060)",
		"--fingerprint-webgl-vendor=Google Inc. (NVIDIA)",
		"--fingerprint-platform=Win32",
		"--fingerprint-platform-version=15.0.0",
		"--fingerprint-screen-width=1920",
		"--fingerprint-do-not-track",
		"--lang=zh-CN",
		"--accept-lang=zh-CN",
	}
	for round := 0; round < 20; round++ {
		got := f.Args()
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("第 %d 次生成的参数不一致:\n got=%v\nwant=%v", round, got, want)
		}
	}
	if (Fingerprint{}).Args() != nil {
		t.Fatal("零值指纹不该产生任何参数（否则每个档案都会带一堆空开关）")
	}
}

// TestFingerprintNormalize 越界的指纹本身就是异常特征，必须拒。
func TestFingerprintNormalize(t *testing.T) {
	good := Fingerprint{ScreenWidth: 1920, TouchPoints: 5, Lang: "en-US", Platform: "Win32"}
	if err := good.Normalize(); err != nil {
		t.Fatalf("合法值被拒: %v", err)
	}
	cases := []struct {
		name string
		f    Fingerprint
		want string
	}{
		{"屏幕过窄", Fingerprint{ScreenWidth: 400}, "screen_width"},
		{"屏幕过宽", Fingerprint{ScreenWidth: 9000}, "screen_width"},
		{"触点过多", Fingerprint{TouchPoints: 11}, "touch_points"},
		{"触点负数", Fingerprint{TouchPoints: -1}, "touch_points"},
		{"语言含空格", Fingerprint{Lang: "zh CN"}, "lang"},
		{"语言注入", Fingerprint{Lang: "zh-CN\n--proxy-server=evil"}, "lang"},
		{"GPU 名过长", Fingerprint{GPURenderer: strings.Repeat("x", 200)}, "过长"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			err := c.f.Normalize()
			if err == nil {
				t.Fatalf("应当拒绝: %+v", c.f)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误信息应当提到 %q，实际 %q", c.want, err.Error())
			}
		})
	}
	// 换行会被剥掉而不是直接报错（粘贴带换行是常态）
	f := Fingerprint{Platform: "Win32\n"}
	if err := f.Normalize(); err != nil || f.Platform != "Win32" {
		t.Fatalf("换行应当被剥掉: %v %q", err, f.Platform)
	}
}

// TestSupportsFingerprint 二进制里找开关字符串：有 = 变体，没有 = 原版。
func TestSupportsFingerprint(t *testing.T) {
	dir := t.TempDir()

	variant := filepath.Join(dir, "variant.exe")
	if err := os.WriteFile(variant, []byte("MZ...\x00--fingerprint-canvas-noise\x00more"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ok, err := SupportsFingerprint(variant); err != nil || !ok {
		t.Fatalf("含标记的文件应当判为变体: ok=%v err=%v", ok, err)
	}

	plain := filepath.Join(dir, "plain.exe")
	if err := os.WriteFile(plain, []byte("MZ...--enable-features=foo--disable-gpu"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ok, err := SupportsFingerprint(plain); err != nil || ok {
		t.Fatalf("原版应当判为不支持: ok=%v err=%v", ok, err)
	}

	if _, err := SupportsFingerprint(filepath.Join(dir, "nope.exe")); err == nil {
		t.Fatal("文件不存在应当报错（而不是当作不支持）")
	}
	if _, err := SupportsFingerprint(dir); err == nil {
		t.Fatal("目录应当报错")
	}
}

// TestSupportsFingerprintAcrossChunkBoundary 分块扫描必须处理跨块边界。
//
// 1 MiB 分块下把标记放在块尾，是这类扫描器最容易漏的位置。
func TestSupportsFingerprintAcrossChunkBoundary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "boundary.exe")
	marker := []byte("--fingerprint-screen-width")
	// 让标记横跨第一个 chunk 的边界（chunk = 1<<20）
	prefix := make([]byte, (1<<20)-5)
	for i := range prefix {
		prefix[i] = 'A'
	}
	body := append(prefix, marker...)
	body = append(body, []byte("tail")...)
	if err := os.WriteFile(path, body, 0o755); err != nil {
		t.Fatal(err)
	}
	if ok, err := SupportsFingerprint(path); err != nil || !ok {
		t.Fatalf("跨块边界的标记应当被找到: ok=%v err=%v", ok, err)
	}
}

// TestSupportsFingerprintCached 探测有缓存：改内容但不动大小/时间不该重复读，
// 改了大小就该重新读（否则换了浏览器还以为是旧的）。
func TestSupportsFingerprintCached(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.exe")
	if err := os.WriteFile(path, []byte("--fingerprint-platform=Win32"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ok, _ := SupportsFingerprint(path); !ok {
		t.Fatal("第一次应当判为变体")
	}
	// 换成原版内容（大小不同 → 缓存键变化 → 重新探测）
	if err := os.WriteFile(path, []byte("nothing here"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ok, err := SupportsFingerprint(path); err != nil || ok {
		t.Fatalf("内容与大小都变了，应当重新探测为不支持: ok=%v err=%v", ok, err)
	}
}

// TestPlanAppendsFingerprintArgs 指纹参数必须真的进参数向量，且排在附加参数之前。
func TestPlanAppendsFingerprintArgs(t *testing.T) {
	fp := &Fingerprint{CanvasNoise: true, Lang: "ja-JP"}
	cmd, err := Plan("linux", Profile{ID: "p", Fingerprint: fp}, t.TempDir()+"/dir", Options{
		Lookup:    fakeLookup,
		ExtraArgs: []string{"--window-size=1280,900"},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	joined := strings.Join(cmd.Args, " ")
	for _, want := range []string{"--fingerprint-canvas-noise", "--lang=ja-JP", "--window-size=1280,900"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("参数缺 %q：%v", want, cmd.Args)
		}
	}
	// 指纹在前、附加参数在后（后写的有最终决定权）
	if strings.Index(joined, "--fingerprint-canvas-noise") > strings.Index(joined, "--window-size") {
		t.Fatalf("指纹参数应当排在附加参数之前: %v", cmd.Args)
	}
}
