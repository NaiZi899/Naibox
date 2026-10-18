// Package desktop Windows 桌面壳：双击出窗口、托盘常驻、数据固定在 %APPDATA%\omni2api。
//
// # 为什么数据根不在 exe 旁边（本包存在的第一理由）
//
// 改造前所有数据（config.json / auths / data）都相对**当前工作目录**解析。
// 双击 exe 时工作目录是 exe 所在目录，看起来"没问题"；但只要用户换一种
// 启动方式（快捷方式设了起始位置、从命令行在别的目录启动、装进
// Program Files 后目录只读），数据就会落在另一个地方 —— 表现成
// "账号没了"。而"卸载重装/覆盖升级后账号消失"的根因同理：数据住在
// 安装目录里，安装器清理/替换目录就把账号一起带走了。
//
// 所以桌面形态把数据根固定到 %APPDATA%\omni2api：exe 可以随意覆盖，
// 数据不动。旧布局的救回由 migrate.go 负责（复制而不是移动）。
//
// # 分层
//
// 纯路径与纯文件操作（本文件、migrate.go、port.go、logfile.go、
// backup.go、windowstate.go）不触碰任何 Windows API，因此在
// GOOS=linux 下也能编译并跑测试；只有托盘、窗口、单实例、开机自启、
// 打开外部程序这五件事按 build tag 分实现。
package desktop

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "embed"
)

// defaultConfigJSON 内嵌的首次启动模板。
//
// 内容是仓库根 config.example.json 的副本：//go:embed 不允许引用包外目录，
// 所以只能复制一份。两份应当保持一致（生成时会先做 JSON 合法性校验）。
//
//go:embed defaultconfig.example.json
var defaultConfigJSON []byte

const (
	// appDirName %APPDATA% 下的目录名。刻意与 module 名一致，便于用户找。
	appDirName = "omni2api"

	// portableFlagName exe 同级存在该文件时，数据根回退到 exe 目录
	// （绿色版/U 盘场景：用户明确要求"数据跟着 exe 走"）。
	portableFlagName = "portable.flag"
)

// Layout 一次启动解析出的全部绝对路径。
//
// 所有字段都是绝对路径：装配层（internal/boot）拿到绝对路径后不会
// 再做"相对谁"的二次推断，这正是"同一份配置在不同工作目录下指向不同
// 数据"这个老问题的封堵点。
type Layout struct {
	// DataDir 数据根（%APPDATA%\omni2api）。
	DataDir string
	// ConfigPath 配置文件（默认 DataDir\config.json；-config 可指向别处）。
	ConfigPath string
	// AuthDir 账号目录（DataDir\auths）。
	AuthDir string
	// DataSubDir 状态目录（DataDir\data，state.json 在这里）。
	DataSubDir string
	// LogDir 日志目录（DataDir\logs）。
	LogDir string
	// BackupDir 备份目录（DataDir\backups）。
	BackupDir string
	// Portable 数据根是否因为 portable.flag 而落在 exe 目录。
	Portable bool
}

// ResolveLayout 决定数据根与配置文件路径。
//
// 优先级（与 task 要求一致）：
//  1. -data-dir 显式指定
//  2. exe 同级 portable.flag → exe 目录
//  3. %APPDATA%\omni2api
//
// 配置文件不受 1/2 影响时固定为 <DataDir>\config.json；-config 显式指定
// 时用它（此时不迁移旧布局，见 MigrateLegacy）。
func ResolveLayout(explicitDataDir, explicitConfig string) (Layout, error) {
	exeDir := executableDir()

	var (
		root     string
		portable bool
	)
	switch {
	case strings.TrimSpace(explicitDataDir) != "":
		root = strings.TrimSpace(explicitDataDir)
	case fileExists(filepath.Join(exeDir, portableFlagName)):
		root, portable = exeDir, true
	default:
		if base, err := os.UserConfigDir(); err == nil && strings.TrimSpace(base) != "" {
			root = filepath.Join(base, appDirName)
		} else {
			// UserConfigDir 在极端环境（无 HOME/APPDATA）会失败；
			// 退到 exe 目录也比"写到当前工作目录"可预测。
			root, portable = exeDir, true
		}
	}

	abs, err := filepath.Abs(root)
	if err != nil {
		return Layout{}, fmt.Errorf("解析数据根 %q: %w", root, err)
	}

	l := Layout{
		DataDir:    abs,
		ConfigPath: filepath.Join(abs, "config.json"),
		AuthDir:    filepath.Join(abs, "auths"),
		DataSubDir: filepath.Join(abs, "data"),
		LogDir:     filepath.Join(abs, "logs"),
		BackupDir:  filepath.Join(abs, "backups"),
		Portable:   portable,
	}
	if c := strings.TrimSpace(explicitConfig); c != "" {
		absCfg, err := filepath.Abs(c)
		if err != nil {
			return Layout{}, fmt.Errorf("解析 -config %q: %w", c, err)
		}
		l.ConfigPath = absCfg
	}
	return l, nil
}

// EnsureDirs 建好目录骨架（不碰 config.json）。
//
// 与 EnsureConfig 分开是硬要求：旧布局迁移的判据是"目标还没有 config.json"，
// 如果建目录时顺手生成默认配置，迁移就永远看不到"目标缺失"，老用户的
// 账号会被一份全新的默认配置挡在门外。
func (l Layout) EnsureDirs() error {
	for _, dir := range []string{l.DataDir, l.AuthDir, l.DataSubDir, l.LogDir, l.BackupDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("创建目录 %s: %w", dir, err)
		}
	}
	return nil
}

// EnsureConfig 在配置缺失时生成一份带随机 api_key 的默认配置。
// 返回新建的配置路径（已存在时返回空串），供调用方写日志。
func (l Layout) EnsureConfig() (createdConfig string, err error) {
	if fileExists(l.ConfigPath) {
		return "", nil
	}
	if err := writeDefaultConfig(l.ConfigPath); err != nil {
		return "", err
	}
	return l.ConfigPath, nil
}

// writeDefaultConfig 用内嵌模板生成首次启动的 config.json。
//
// 模板是仓库根 config.example.json 的副本（internal/desktop/defaultconfig.example.json）：
// //go:embed 不允许引用包外目录，所以只能复制一份。两份必须保持一致 ——
// 归一到一处没有干净解法，因此这里只做两件防漂移的事：
//   - 生成前先用 JSON 校验模板，坏模板（例如有人手改了逗号）当场报错，
//     而不是让用户拿到一份装配层读不动的配置；
//   - 解码时开 UseNumber，整数字段原样往返（否则 8 会变成 8，
//     但未来某个大整数会因 float64 精度被悄悄改写）。
func writeDefaultConfig(path string) error {
	dec := json.NewDecoder(bytes.NewReader(defaultConfigJSON))
	dec.UseNumber()
	var probe map[string]any
	if err := dec.Decode(&probe); err != nil {
		return fmt.Errorf("内嵌默认配置不是合法 JSON（defaultconfig.example.json）: %w", err)
	}
	key, err := randomAPIKey()
	if err != nil {
		return err
	}
	probe["api_key"] = key

	// 用 map 重新编码会丢字段顺序，但配置是给人看/给程序读的，顺序无意义；
	// 更重要的是**不能丢字段** —— map 往返保留全部键值，不会静默截断。
	body, err := json.MarshalIndent(probe, "", "  ")
	if err != nil {
		return fmt.Errorf("编码默认配置: %w", err)
	}
	body = append(body, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建配置目录 %s: %w", filepath.Dir(path), err)
	}
	// O_EXCL：并发启动（两个 exe 同时首启）时只有一个能写成功，
	// 另一个看到已存在就直接用，不会互相覆盖成两把不同的 key。
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil
		}
		return fmt.Errorf("写入默认配置 %s: %w", path, err)
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		return fmt.Errorf("写入默认配置 %s: %w", path, err)
	}
	return f.Close()
}

// randomAPIKey 生成网关 API key：sk- + 32 位十六进制。
//
// 不用 base64：URL/JSON/命令行里都免转义，用户复制粘贴不会踩坑。
func randomAPIKey() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("生成随机 api_key: %w", err)
	}
	return "sk-" + hex.EncodeToString(buf[:]), nil
}

// DataDirHash 数据根的稳定短哈希（12 位）。
//
// 用途：单实例互斥体名、跨进程激活事件名。同一台机器上只有同一个数据根
// 的实例会互相顶掉；portable 版与 %APPDATA% 版可以并存。
func DataDirHash(dataDir string) string {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		abs = dataDir
	}
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(abs))))
	return hex.EncodeToString(sum[:6])
}

// executableDir 返回 exe 所在目录（拿不到时退当前工作目录）。
func executableDir() string {
	exe, err := os.Executable()
	if err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		return filepath.Dir(exe)
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func dirExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}
