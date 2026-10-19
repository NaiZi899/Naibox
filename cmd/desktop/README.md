# cmd/desktop — Windows 桌面壳

双击出一个 WebView2 窗口 + 任务栏托盘，网关（装配逻辑在 `internal/boot`）照常监听。
`cmd/server` 与它**共用同一套装配**，不存在第二份 `main`。

## 构建

```bash
# 日常验收（控制台子系统，从资源管理器双击会带一个控制台窗口）
GOOS=windows CGO_ENABLED=0 go build -o dist/NaiBox.exe ./cmd/desktop

# 发布构建（GUI 子系统：双击不闪黑框；rsrc.syso 由构建脚本生成到本目录）
GOOS=windows CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -H=windowsgui -X github.com/NaiZi899/Naibox/internal/desktop.Version=0.1.0" \
  -o dist/NaiBox.exe ./cmd/desktop
```

`rsrc.syso`（exe 图标 + DPI manifest）由构建脚本生成后放在本目录，**不进版本库**；
链接器会自动识别同目录的 `.syso`，代码里不需要任何引用。

## 参数

| flag | 默认 | 说明 |
| --- | --- | --- |
| `-config` | 空 | 配置文件路径。空 = `<数据根>\config.json`；显式指定时**不**迁移旧布局 |
| `-data-dir` | 空 | 数据根。空 = `%APPDATA%\omni2api`（exe 同级有 `portable.flag` 时用 exe 目录） |
| `-mode` | `window` | `window` 内嵌窗口 / `browser` 只用系统浏览器 |
| `-port` | `0` | `0` = 7863 起顺延到 7873，仍占用则由系统分配 |
| `-console` | `false` | 保留控制台，日志同时打到 stderr |
| `-autostart` | `false` | 由开机自启项拉起（窗口不抢前台），内部使用 |
| `-version` | — | 打印版本后退出 |

## 数据布局

```
%APPDATA%\omni2api\
├─ config.json         首次启动生成（api_key 随机），也可由旧布局迁移而来
├─ auths\              账号凭证
├─ data\               state.json / apikeys.json / request-log.jsonl
├─ logs\               server-YYYYMMDD.log（保留 7 天）
├─ backups\            托盘「备份数据」的 zip（保留 10 份）
├─ runtime.json        运行中实例的 {pid, addr, mode, version, started_at}，退出时删除
├─ window.json         窗口尺寸与位置
└─ webview2\           WebView2 profile（cookie/缓存，可删）
```

## 旧布局迁移（升级不丢账号）

启动时按顺序探测：

1. `-config` 显式指定 → 尊重用户，不迁移
2. exe 同级 `config.json` → 迁移来源
3. 当前工作目录 `config.json` → 迁移来源
4. 都没有 → 生成默认配置

命中 2/3 时把 `config.json` + `auths\` + `data\` **复制**到数据根（绝不移动），
源目录完整保留并写入 `.migrated.bak`（记录来源、去向、复制清单）。
目标已有同名文件时跳过、不覆盖 → 重复启动安全（幂等）。

## 回落与退出

- WebView2 运行时缺失（注册表 `EdgeUpdate\Clients\{F3017226-…}\pv` 为空或 `0.0.0.0`），
  或当前会话没有可见桌面（服务会话/无头会话）→ 弹提示后自动回落 `-mode=browser`，不白屏。
- 退出统一走「cancel ctx → `Runtime.Shutdown()`（内部先落盘再停机）」：
  关窗口、托盘「退出」、Ctrl+C 三条路同一条收尾，最后删 `runtime.json`。
- 第二实例：命名互斥体让位 → 读 `runtime.json` → `/healthz` 校验 `service` 是自家
  → 触发命名事件让主实例把窗口提到前台 → 本进程退出（`os.Exit` 在 `internal/desktop` 内不存在）。
