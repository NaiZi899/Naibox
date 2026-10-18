package desktop

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// migratedMarkerName 迁移完成后写在**源目录**里的标记文件。
//
// 它不参与"要不要迁移"的判断（判断只看目标有没有 config.json），存在的
// 意义是给人看：用户翻出老目录时能立刻明白"这里的数据已经搬到 %APPDATA%，
// 别在错误的目录里找账号"。
const migratedMarkerName = ".migrated.bak"

// MigrationResult 迁移结果。字段全部可日志化。
type MigrationResult struct {
	// Attempted 是否进入过迁移流程（false = 目标已有配置/被 -config 顶掉/没找到旧布局）。
	Attempted bool
	// Migrated 是否真的复制过东西。
	Migrated bool
	// Source 旧布局来源目录（Attempted 且非 Migrated 时也可能有值）。
	Source string
	// Copied 复制成功的相对路径（写进日志与 .migrated.bak，是"确实搬了哪些"的账本）。
	Copied []string
	// Kept 目标已存在、按保守策略**没有覆盖**的相对路径（逐文件，不聚合）。
	//
	// 为什么必须逐文件：用户升级后发现"少了一个号"时，日志要能直接点名
	// 是哪个文件因为目标已存在而没覆盖。聚合式的"auths/ 未覆盖"等于没说 ——
	// 用户无从判断丢的是哪一个账号。
	Kept []string
	// Marker 标记文件路径。
	Marker string
}

// MigrateLegacy 把旧布局（exe 同级或当前工作目录下的 config.json + auths + data）
// 复制到数据根。
//
// # 四个硬要求（顺序即决策顺序）
//
//  1. **-config 显式指定 → 不迁移。** 用户说"配置在这里"，那数据根也该
//     由用户负责；替他搬家只会把两份数据搞成双头。
//  2. **目标已有 config.json → 不迁移。** 这是幂等的关键：第二次启动时
//     目标已经有配置，直接返回，绝不二次复制、绝不覆盖用户改过的配置。
//  3. **只复制，不移动。** 旧目录原封不动保留（用户可能还要用
//     cmd/server 形态跑同一份 auths），源目录只多一个 .migrated.bak 标记。
//  4. **目标已有同名文件 → 跳过并记录。** 覆盖等于用旧数据砸新数据；
//     "复制时保留更新的那份"没法自动判断，于是选择保守：谁在目标里，谁赢。
//
// 失败语义：中途失败返回 error（日志里已写明到哪一步），**不删除任何
// 已复制文件**——删除会把"迁移失败"升级成"数据丢失"。
func MigrateLegacy(l Layout, explicitConfig bool, logger *log.Logger) (MigrationResult, error) {
	res := MigrationResult{}
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}

	if explicitConfig {
		logger.Printf("迁移检查：-config 显式指定（%s），尊重用户选择，不做旧布局迁移", l.ConfigPath)
		return res, nil
	}
	if fileExists(l.ConfigPath) {
		logger.Printf("迁移检查：%s 已存在，无需迁移", l.ConfigPath)
		return res, nil
	}

	src := findLegacyRoot(l.DataDir)
	if src == "" {
		logger.Printf("迁移检查：%s、%s 都没有旧布局 config.json，按首次启动处理（数据根 %s）",
			executableDir(), cwdOr("."), l.DataDir)
		return res, nil
	}

	res.Attempted = true
	res.Source = src
	logger.Printf("发现旧布局：%s（含 config.json）→ 复制到数据根 %s", src, l.DataDir)

	// 1) 配置
	copied, err := copyFileIfMissing(filepath.Join(src, "config.json"), l.ConfigPath)
	if err != nil {
		return res, fmt.Errorf("迁移 config.json: %w", err)
	}
	if copied {
		res.Copied = append(res.Copied, "config.json")
	} else {
		res.Kept = append(res.Kept, "config.json")
	}

	// 2) 账号与状态。目录不存在不算失败（老版本可能还没登录过）。
	for _, pair := range []struct{ name, dst string }{
		{"auths", l.AuthDir},
		{"data", l.DataSubDir},
	} {
		srcDir := filepath.Join(src, pair.name)
		if !dirExists(srcDir) {
			continue
		}
		c, skipped, err := copyTreeIfMissing(srcDir, pair.dst)
		if err != nil {
			return res, fmt.Errorf("迁移 %s: %w", pair.name, err)
		}
		for _, rel := range c {
			res.Copied = append(res.Copied, filepath.Join(pair.name, rel))
		}
		for _, rel := range skipped {
			res.Kept = append(res.Kept, filepath.Join(pair.name, rel))
		}
	}

	res.Migrated = true
	if len(res.Copied) > 0 {
		logger.Printf("迁移完成：复制 %d 个文件（%s → %s）；源目录保留未动",
			len(res.Copied), src, l.DataDir)
	} else {
		logger.Printf("迁移完成：目标已包含全部文件，本次未复制任何内容（%s → %s）", src, l.DataDir)
	}
	// 逐条打印被跳过的文件：这是"升级后少了一个号"时唯一的审计线索。
	if len(res.Kept) > 0 {
		logger.Printf("以下 %d 个文件在目标里已存在，按保守策略未覆盖（保留目标版本，旧版本仍在源目录）：", len(res.Kept))
		for _, rel := range res.Kept {
			logger.Printf("  未覆盖（目标已存在）：%s", rel)
		}
	}

	marker, err := writeMigrationMarker(src, l.DataDir, res)
	if err != nil {
		// 标记失败不影响迁移结果：数据已经复制好了，只是少一行说明。
		logger.Printf("迁移完成但标记文件写入失败（不影响数据）：%v", err)
	} else {
		res.Marker = marker
		logger.Printf("已在源目录留标记：%s", marker)
	}
	return res, nil
}

// findLegacyRoot 按 (b) exe 同级 → (c) 当前工作目录 的顺序找旧布局。
//
// 跳过数据根本身：portable 形态下 exe 目录**就是**数据根，不能自己迁自己。
func findLegacyRoot(dataDir string) string {
	candidates := []string{executableDir(), cwdOr(".")}
	seen := map[string]bool{}
	for _, c := range candidates {
		abs, err := filepath.Abs(c)
		if err != nil {
			continue
		}
		abs = filepath.Clean(abs)
		key := strings.ToLower(abs)
		if seen[key] {
			continue
		}
		seen[key] = true
		if samePath(abs, dataDir) {
			continue
		}
		if fileExists(filepath.Join(abs, "config.json")) {
			return abs
		}
	}
	return ""
}

// writeMigrationMarker 在源目录写 .migrated.bak。
//
// 覆盖写而不是追加：重复迁移时文件内容应该是"最近一次"，否则这个标记
// 会变成一份越滚越长的流水账。
func writeMigrationMarker(srcDir, dstDir string, res MigrationResult) (string, error) {
	path := filepath.Join(srcDir, migratedMarkerName)
	body, err := json.MarshalIndent(map[string]any{
		"migrated_at": time.Now().Format(time.RFC3339),
		"from":        srcDir,
		"to":          dstDir,
		"copied":      res.Copied,
		"kept":        res.Kept,
		"note": "本目录的数据已复制到上面的 to；原文件全部保留。" +
			"桌面版从此只读写 to，改这里的配置不会再生效。",
	}, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// copyTreeIfMissing 复制目录树，目标已存在的文件一律跳过。
//
// 返回两个**相对路径**列表：复制成功的、以及因为目标已存在而跳过的。
// 跳过列表是审计账本的一部分（见 MigrationResult.Kept 的注释），
// 所以这里必须逐文件返回，不能只回一个计数。
func copyTreeIfMissing(srcDir, dstDir string) (copied, skipped []string, err error) {
	err = filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dstDir, 0o755)
		}
		dst := filepath.Join(dstDir, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(dst, 0o755)
		case d.Type()&fs.ModeSymlink != 0:
			// 符号链接不跟：跟出去可能复制到数据根外面
			// （Windows 上还可能是目录联接，WalkDir 会绕圈）。
			return nil
		case !d.Type().IsRegular():
			// 设备文件/管道之类：跳过。
			return nil
		}
		ok, err := copyFileIfMissing(path, dst)
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		if ok {
			copied = append(copied, rel)
		} else {
			skipped = append(skipped, rel)
		}
		return nil
	})
	if err != nil {
		return copied, skipped, err
	}
	return copied, skipped, nil
}

// copyFileIfMissing 复制单个文件；目标存在则什么都不做，返回 false。
//
// 先写 .tmp 再 rename：中途断电/崩溃不会留下半个 config.json（那种文件
// 会被装配层读成"配置损坏"，比没有配置更难判断）。
func copyFileIfMissing(src, dst string) (bool, error) {
	if fileExists(dst) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, err
	}
	in, err := os.Open(src)
	if err != nil {
		return false, err
	}
	defer in.Close()

	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return false, err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return false, err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return false, err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return false, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func cwdOr(fallback string) string {
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return fallback
}

// samePath 比较两个已 abs 的路径是否指向同一目录（Windows 大小写不敏感）。
func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}
