package desktop

import (
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// logFilePrefix 日志文件名前缀。日期后缀保证"按日滚动"，前缀保证清扫时
// 只删自家的文件（不会误删用户丢进 logs 目录里的其它东西）。
const logFilePrefix = "server-"

// retainedLogDays 保留天数（含当天）。
const retainedLogDays = 7

// DailyLogWriter 按日滚动的文件日志，同时 fan-out 一份到控制台。
//
// # 为什么要自己写这 80 行
//
// 装配层的 17 处 log.Fatalf / 大量 log.Printf 都往标准库默认 logger 写。
// 桌面形态没有可见控制台（-H windowsgui 或用户从资源管理器双击），
// 日志如果只进 stderr 就等于**全部丢失** —— 出问题时用户手里没有任何
// 可提供的信息。所以 stdout 必须接一个"写到 %APPDATA%\NaiBox\logs"的
// 出口，并且保留"控制台看得见"这个能力（-console 调试时用）。
//
// 自己写而不是引第三方滚动库：需求只有"按日切 + 保留 N 天 + tee"，
// 依赖成本远高于代码成本。
//
// 并发安全：Write 在互斥锁内完成"判断日期 → 必要时换文件 → 写"，
// 多个 goroutine 同时打日志不会写串行或写进昨天的文件。
type DailyLogWriter struct {
	dir     string
	console io.Writer
	keep    int

	mu   sync.Mutex
	day  string
	file *os.File
	err  error
}

// NewDailyLogWriter 建一个写入 dir 的按日滚动日志；console 通常是 os.Stderr
// （nil = 不 fan-out）。
func NewDailyLogWriter(dir string, console io.Writer) *DailyLogWriter {
	return &DailyLogWriter{dir: dir, console: console, keep: retainedLogDays, day: time.Now().Format("20060102")}
}

// Write 实现 io.Writer。
func (w *DailyLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	day := time.Now().Format("20060102")
	if day != w.day {
		// 跨日：先关旧文件再换。旧文件句柄不关的话在 Windows 上会被
		// 备份/清理工具判为占用（"文件正在使用中"）。
		w.closeLocked()
		w.day = day
	}
	if w.file == nil {
		if err := w.openLocked(); err != nil {
			// 打不开文件不能吞掉日志：退回控制台，并记住错误供调用方查询。
			w.err = err
			if w.console != nil {
				w.console.Write(p)
			}
			return len(p), nil
		}
	}
	if _, err := w.file.Write(p); err != nil {
		w.err = err
	}
	if w.console != nil {
		w.console.Write(p)
	}
	return len(p), nil
}

// Path 当前日志文件路径（例如给托盘的"查看今日日志"用）。
func (w *DailyLogWriter) Path() string { return filepath.Join(w.dir, logFilePrefix+w.day+".log") }

// Err 最近一次文件操作错误（nil = 正常）。供启动日志自检用。
func (w *DailyLogWriter) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// Close 关闭当前文件。退出路径调用；幂等。
func (w *DailyLogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closeLocked()
}

func (w *DailyLogWriter) openLocked() error {
	if err := os.MkdirAll(w.dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(w.Path(), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	w.file = f
	w.pruneLocked()
	return nil
}

func (w *DailyLogWriter) closeLocked() error {
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// pruneLocked 删除超过保留天数的日志。
//
// 判定依据是**文件名里的日期**而不是 mtime：用户可能整体复制过 logs 目录
// （mtime 全变成复制时刻），按 mtime 会一次删光全部历史。
func (w *DailyLogWriter) pruneLocked() {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -(w.keep - 1)).Format("20060102")
	var stale []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, logFilePrefix) || !strings.HasSuffix(name, ".log") {
			continue
		}
		day := strings.TrimSuffix(strings.TrimPrefix(name, logFilePrefix), ".log")
		if len(day) != 8 {
			continue
		}
		if day < cutoff {
			stale = append(stale, filepath.Join(w.dir, name))
		}
	}
	sort.Strings(stale)
	for _, path := range stale {
		os.Remove(path)
	}
}
