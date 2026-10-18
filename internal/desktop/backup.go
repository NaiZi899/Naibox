package desktop

import (
	"archive/zip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// backupKeep 备份保留份数。备份是"用户手动点"的动作，不设上限的话
// 一个爱点按钮的用户能把 %APPDATA% 撑到几百 MB（data/ 里有 state.json）。
const backupKeep = 10

// Backup 把 config.json + auths\ + data\ 打成一个 zip。
//
// # 为什么不备份 logs
//
// 日志是诊断材料，不是资产；它每天都在长，打进去会让备份体积与"最近
// 出了什么问题"挂钩，而用户真正想保住的是账号与配置。要日志点托盘的
// "打开数据目录"即可。
//
// 返回 zip 的绝对路径。目录不存在/为空不算错误（首次启动就点备份也应有
// 一个可用的空壳备份，而不是一个报错）。
func (l Layout) Backup() (string, error) {
	ts := time.Now().Format("20060102-150405")
	path := filepath.Join(l.BackupDir, "omni2api-backup-"+ts+".zip")
	if err := os.MkdirAll(l.BackupDir, 0o755); err != nil {
		return "", fmt.Errorf("创建备份目录: %w", err)
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("创建备份文件: %w", err)
	}
	zw := zip.NewWriter(f)

	addFile := func(src, name string) error {
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		defer in.Close()
		st, err := in.Stat()
		if err != nil {
			return err
		}
		hdr, err := zip.FileInfoHeader(st)
		if err != nil {
			return err
		}
		// 固定用正斜杠：zip 规范要求，Windows 路径进 zip 会在别的工具里
		// 变成一层名字里带反斜杠的文件。
		hdr.Name = filepath.ToSlash(name)
		hdr.Method = zip.Deflate
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		_, err = io.Copy(w, in)
		return err
	}

	addTree := func(root, prefix string) error {
		if !dirExists(root) {
			return nil
		}
		return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			return addFile(p, filepath.Join(prefix, rel))
		})
	}

	buildErr := func() error {
		if fileExists(l.ConfigPath) {
			if err := addFile(l.ConfigPath, "config.json"); err != nil {
				return fmt.Errorf("备份 config.json: %w", err)
			}
		}
		if err := addTree(l.AuthDir, "auths"); err != nil {
			return fmt.Errorf("备份 auths: %w", err)
		}
		if err := addTree(l.DataSubDir, "data"); err != nil {
			return fmt.Errorf("备份 data: %w", err)
		}
		return nil
	}()
	if buildErr != nil {
		zw.Close()
		f.Close()
		os.Remove(path)
		return "", buildErr
	}
	if err := zw.Close(); err != nil {
		f.Close()
		os.Remove(path)
		return "", fmt.Errorf("写入备份: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("关闭备份文件: %w", err)
	}

	l.pruneBackups()
	return path, nil
}

// pruneBackups 只保留最近 backupKeep 份。
//
// 按文件名排序（文件名里是 yyyyMMdd-HHmmss，字典序 = 时间序），不按 mtime：
// 备份目录被同步盘搬动过之后 mtime 会全变。
func (l Layout) pruneBackups() {
	entries, err := os.ReadDir(l.BackupDir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasPrefix(n, "omni2api-backup-") && strings.HasSuffix(n, ".zip") {
			names = append(names, n)
		}
	}
	if len(names) <= backupKeep {
		return
	}
	sort.Strings(names)
	for _, n := range names[:len(names)-backupKeep] {
		os.Remove(filepath.Join(l.BackupDir, n))
	}
}
