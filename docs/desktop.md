# 桌面版手工验收矩阵

这份文档是 `cmd/desktop`（Windows 桌面壳）的**手工验收脚本**：每一条都能照着做，
每一条都写明"造条件 → 期望现象 → 失败时看哪里"。

它不替代自动化测试。`go test ./internal/desktop/ ./internal/boot/` 已经守住了
最容易写错、也最不能靠人手确认的部分（迁移幂等/源目录保留、端口顺延、防连错服务、
生命周期）。本文件覆盖的是自动化测不了的东西：Windows 版本差异、DPI、WebView2 缺失、
单实例、休眠唤醒、托盘退出后的落盘。

> 判定"通过"的最低要求：**账号不丢**、**数据落在 `%APPDATA%\omni2api`**、
> **退出后 `runtime.json` 消失**。其余条目是体验与可诊断性。

---

## 0. 前置：构建、路径、取证命令

### 0.1 构建两种 exe

```powershell
# 验收用的"看得见控制台"版本：出错信息不会消失
go build -o dist\NaiBox-debug.exe .\cmd\desktop

# 发布形态（无黑框）：验收"双击体验"时必须用这个
go build -trimpath -ldflags "-s -w -H=windowsgui" -o dist\NaiBox.exe .\cmd\desktop
```

两种构建都**必须** `CGO_ENABLED=0`（本仓库无 gcc/windres，见 `cmd/desktop/main.go` 的文件头）。

### 0.2 数据根与文件契约

| 路径（相对数据根） | 谁写 | 验收意义 |
| --- | --- | --- |
| `config.json` | 首启生成 / 用户手改 | 存在即表示"这是唯一数据源"；也是迁移的判据 |
| `auths\`、`auths\<上游>\` | 登录/导入 | **账号本体**，丢了就是"号没了" |
| `data\state.json` | 账号池落盘 | 冷却/配额/禁用状态；只在 dirty 时写 |
| `data\request-log.jsonl` | 请求日志 | 端口/请求取证 |
| `data\checkin-log.json` | 签到历史 | 调度是否在跑 |
| `logs\server-YYYYMMDD.log` | 运行日志 | **所有失败的现场都在这里**（保留 7 天） |
| `backups\omni2api-backup-YYYYMMDD-HHmmss.zip` | 托盘"备份数据"（保留 10 份） | 含 config+auths+data，**不含** logs |
| `runtime.json` | 运行期：`{pid, addr, mode, version, started_at}` | 第二实例靠它找人；退出必须删掉 |
| `window.json` | 运行期：窗口尺寸/位置 | 每 5 秒轮询落盘一次 |
| `webview2\` | WebView2 profile | 占几十 MB，可随时删（会丢登录态之外的东西） |

数据根判定顺序（日志第一行就会打印结论）：

```
-data-dir 显式指定  >  exe 同级存在 portable.flag  >  %APPDATA%\omni2api
```

### 0.3 三条固定取证命令

```powershell
$root = "$env:APPDATA\omni2api"
$log  = Join-Path $root ("logs\server-{0}.log" -f (Get-Date -Format yyyyMMdd))

# ① 看今天的日志尾部
Get-Content $log -Tail 100

# ② 只看验收关心的几类事件
Select-String -Path $log -Pattern '数据根|迁移|单实例|已有实例|内嵌窗口|端口|控制台地址|正在退出|bye|致命错误'

# ③ 看"活着的那个实例"与端口归属
Get-Content (Join-Path $root 'runtime.json')
Get-NetTCPConnection -LocalPort 7863 -State Listen | Select-Object OwningProcess
```

需要一次给开发者收走的材料：`logs\`（当天）、`runtime.json`（如果还在）、
`config.json`（**先删掉 `api_key` 的值**）、`window.json`、以及一份
`Get-Process NaiBox | Format-List *` 的输出。

### 0.4 两个容易误判的语义（先读，能省掉一半假故障）

- **`/healthz` 在 0 账号时返回 503**。它的语义是"现在能不能受理对话"，不是"进程活着"。
  验收时不要用 `状态码==200` 判断"服务起来了"：全新安装必然 503，body 里
  `{"healthy":0,"total":0,"service":"omni2api"}` 才是正常形态。
- **`data\state.json` 不一定存在**。账号池只在 dirty 时落盘；刚装好什么都没做，
  这个文件可能根本没有。它不是启动失败。

---

## 1. 环境矩阵

| 编号 | 条目 | 自动化覆盖 |
| --- | --- | --- |
| E1 | Win10 21H2 x64 | 无（人工） |
| E2 | Win11 23H2 x64 | 无（人工） |
| E3 | 无 WebView2 Runtime | 无（人工） |
| E4 | DPI 100% / 150% / 200% | 无（人工） |
| E5 | 中文用户名 / 中文+空格数据根 | `paths_test.go`（路径解析层） |
| E6 | 双开：第二实例唤出主实例 | 部分（`port_test.go` 的 ProbeService） |
| E7 | `runtime.json` 指向陌生服务 | 部分（同上） |
| E8 | 从旧目录升级，账号不丢 | `migrate_test.go`（核心断言） |
| E9 | 托盘退出后的落盘与 `runtime.json` 清理 | `paths_test.go`（runtime 往返/删除幂等） |
| E10 | 崩溃残留 `runtime.json` 后的重启 | 无（人工） |
| E11 | 备份与恢复 | 无（人工） |
| E12 | portable.flag 绿版 / `-config` 显式指定 | `migrate_test.go`、`paths_test.go` |
| E13 | 休眠唤醒 | 无（人工） |

---

### E1 Win10 21H2 x64（基线）

**造条件**：干净 Win10 21H2（内部版本 19044+），装好 Edge WebView2 Evergreen Runtime，
把 `dist\NaiBox.exe` 放到 `D:\tools\omni2api\`（**不是** Program Files），双击。

**期望现象**
- 无控制台黑框；窗口标题 `omni2api 控制台`；内容是可操作的控制台页面。
- 通知区域出现图标，悬停提示含 `控制台 http://127.0.0.1:7863/ui`。
- 数据根被创建：`%APPDATA%\omni2api\` 下有 `config.json`、`auths\`、`data\`、`logs\`、`backups\`。
- 日志头部四行（顺序固定）：

```
omni2api desktop <版本> 启动
数据根 C:\Users\<用户>\AppData\Roaming\omni2api（portable=false）；配置 ...\config.json
迁移检查：C:\tools\omni2api、<cwd> 都没有旧布局 config.json，按首次启动处理（数据根 ...）
首次启动：已生成默认配置 ...\config.json（api_key 为随机值，可在 /ui 里查看）
```

**失败时看哪里**
- 窗口没出来但浏览器开了页 → 日志有 `内嵌窗口不可用：<原因> → 回落浏览器模式`（见 E3）。
- 没有任何东西出现 → 用 `dist\NaiBox-debug.exe -console` 重跑，
  或看 `%APPDATA%\omni2api\logs\server-*.log` 最后一行；若日志目录都没建出来，
  说明卡在 `准备数据根失败` / `生成默认配置失败`（日志会写，但那时日志文件可能还没建，
  用 `-console` 看 stderr）。
- `%APPDATA%` 下没有 `omni2api` → 数据根落到了别处：核对日志第二行的绝对路径。

---

### E2 Win11 23H2 x64（基线 + 任务栏溢出区）

**造条件**：Win11 23H2，其余同 E1。另外先让通知区域"折叠"：设置 > 个性化 > 任务栏 >
其他系统托盘图标 > 把 omni2api 设为**隐藏**，然后重新双击 exe。

**期望现象**
- 窗口与数据行为同 E1。
- 托盘图标即使在溢出区里，右键菜单仍可打开（`打开控制台/在浏览器中打开/打开数据目录/
  查看今日日志/备份数据/开机自启/重启服务/退出`）。
- 点 `打开控制台` 时窗口被提到前台（Win11 的前台锁更严格，见 platform_windows.go 的
  `showAndFocus`：先 `ShowWindow(SW_RESTORE)` 再抢焦点）。

**失败时看哪里**
- 菜单点了没反应 → 日志是否有 `托盘已就绪`；若只有 `托盘不可用（不影响服务）：<err>`
  说明托盘没起来（图标不可见/ICO 不被 LoadImageW 接受），此时**退出入口只剩任务管理器**，
  日志里应有一条 `托盘不可用：... 当前没有退出入口` 的提示框记录。
- 窗口在后台不前置 → 属已知前台锁限制，任务栏有闪烁即可接受；日志不报错。

---

### E3 无 WebView2 Runtime（必须回落，不能白屏）

**造条件**（三选一，优先用干净虚拟机）：
1. 卸载 `Microsoft Edge WebView2 Runtime`（设置 > 应用）；
2. 干净 Win10/11 上不装任何 Edge 更新，确认注册表检查为空：
   ```powershell
   reg query "HKLM\SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}" /v pv
   reg query "HKCU\Software\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}" /v pv
   ```
   两条都 `找不到` → 符合条件；
3. 造"装了但没装成"：把 `pv` 改成 `0.0.0.0`（实现把这种状态当作**缺失**）。

**期望现象**
- 双击后**先弹消息框**：「内嵌窗口不可用：未检测到 WebView2 运行时…本次改用浏览器模式，
  网关服务照常运行。」然后默认浏览器打开 `http://127.0.0.1:7863/ui`。
- 日志：

```
内嵌窗口不可用：未检测到 WebView2 运行时（注册表 pv 缺失或为 0.0.0.0） → 回落浏览器模式
已在浏览器中打开 http://127.0.0.1:7863/ui
```

- **服务是真的在跑**：浏览器里控制台可操作，`http://127.0.0.1:7863/healthz` 有 JSON 应答。

**失败时看哪里**
- 白屏/空窗口 → 这是最坏的形态（说明检测通过但 WebView2 实际不可用）：
  日志应有 `内嵌窗口创建失败：... → 回落浏览器模式`；若连这条都没有，
  记下 `webview2\` 目录是否被创建、以及 `Get-Content logs\... -Tail 30` 的最后一行。
- 弹框出现但没有浏览器打开 → 日志 `打开浏览器失败（<err>）：请手动访问 ...`；
  手工访问该地址即可继续验收。
- `pv=0.0.0.0` 的情况下却开了窗 → WebView2 检测的 `pv` 判据被改坏（回归）。

---

### E4 DPI 100% / 150% / 200%

**造条件**：设置 > 系统 > 显示 > 缩放，分别设 100%、150%、200%（每档**注销重登**，
让 per-monitor DPI 生效）。每档都做一次"摆窗口 → 等 5 秒 → 托盘退出 → 重启"。

**期望现象**
- 窗口内容（WebView 里的文字）在 150%/200% 下**清晰**，不是被拉糊的位图。
- 200% 下窗口不超出屏幕可用区域；首次启动（无 `window.json`）落 1180×820
  （逻辑像素），Centered。
- 摆好窗口后等 ≥5 秒，`window.json` 出现/更新，例如：

```json
{ "width": 1420, "height": 980, "x": 120, "y": 60, "has_pos": true }
```

- 同一 DPI 下重启：窗口回到原位置与原尺寸（`has_pos:true` 时不居中）。
- 换 DPI 再启动：窗口仍可见可拖；`window.json` 的数值按新 DPI 重算（不会出现
  "窗口跑到屏幕外再也拉不回来"）。

**失败时看哪里**
- 没有 `window.json` → 快照没落盘：日志找 `保存窗口位置失败：<err>`；
  若窗口被拖得小于 480×360，实现会**刻意不保存**（`snapshot` 的下限判断），
  所以"小窗口记不住"是设计行为。
- 窗口跑到屏幕外 → 删掉 `%APPDATA%\omni2api\window.json` 再启动即可回到 1180×820；
  同时记录该次 `window.json` 内容，这是 DPI 与 `GetWindowRect` 坐标口径不一致的证据。
- 文字发糊 → 进程 DPI 感知声明有问题（属实现/清单项，记录 exe 的
  `-H windowsgui` 构建参数、以及是否自带 manifest 的 dpiAware 项）。

---

### E5 中文用户名 / 中文 + 空格数据根

**造条件**：新建本地账户 `张三` 并用它登录（数据根会变成
`C:\Users\张三\AppData\Roaming\omni2api`）；或直接用参数指定带空格与中文的目录：

```powershell
.\NaiBox.exe -data-dir "D:\我的 数据\omni2api"
```

**期望现象**
- 日志第二行是完整的绝对路径，中文不乱码、不截断：
  `数据根 D:\我的 数据\omni2api（portable=false）；配置 D:\我的 数据\omni2api\config.json`
- 该目录下 `config.json` / `auths\` / `data\` / `logs\` / `backups\` 全部齐全。
- 托盘 `打开数据目录` 能直接定位到这个目录；`查看今日日志` 用记事本打开当天的
  `server-YYYYMMDD.log`。
- 账号文件名含中文（如 `auths\workbuddy\账号-甲.json`）时，UI 里账号正常显示。

**失败时看哪里**
- 日志里路径变成 `D:\??\omni2api` 或空 → 编码路径问题，记录 `cmd /c chcp` 与
  `$env:APPDATA` 的实际值。
- 服务起来了但账号列表空、且 `auths\` 是空的 → 可能是账号写到了另一处：
  用 `Get-ChildItem D:\ -Recurse -Filter *.json -Depth 3 | Where-Object Name -like 'workbuddy*'`
  找有没有"另一个 auths"。这属于数据根解析回归。
- 自动化侧对应断言：`TestResolveLayoutExplicitDataDirWinsWithUnicodeAndSpace`。

---

### E6 双开：第二实例把主实例唤出来

**造条件**：实例 1 正常运行（窗口可以最小化到托盘）。再双击一次同目录的 exe。

**期望现象**
- 实例 1 的窗口被提到前台（若在托盘里则恢复显示）。
- 实例 2 进程**干净退出**（退出码 0，任务管理器里不留第二个 `NaiBox.exe`）。
- 实例 1 日志：`收到另一个实例的唤醒请求`。
- 实例 2 日志（同一个日志文件，因为共用数据根）：

```
单实例锁已持有（Global\NaiBox-<12位hash>）      ← 实例1
检测到已有实例（Global\NaiBox-<12位hash>）      ← 实例2
已验证实例 pid=<pid> addr=127.0.0.1:7863，已请求它显示控制台
已有实例在运行，本次启动让位
```

- **反例（合法）**：不同数据根应能并存。用
  `.\NaiBox.exe -data-dir "D:\portable-data"` 再起一个，两个都要能跑，
  各自占不同端口，日志里两把锁的 hash 不同。

**失败时看哪里**
- 实例 2 没让位、起了第二个服务 → 日志里锁名 hash 是否相同：
  hash = `DataDirHash(数据根)`（12 位十六进制，大小写与末尾斜杠不影响）。
- 实例 2 让位但实例 1 窗口没出来 → 日志 `无法唤醒已有实例：<err>（本进程让位退出…）`，
  按 E7 分诊。
- 两个实例互相顶掉（各起各的又都被杀）→ 单实例对象创建报错，日志
  `单实例互斥体创建失败（继续启动，可能出现双实例）：<err>`。

---

### E7 `runtime.json` 指向陌生服务（防连错服务，**安全相关**）

**造条件**：正常运行实例 1 后，手工篡改 `runtime.json`，让 `addr` 指向一个**别的**
HTTP 服务，`pid` 挂到一个确实活着的进程上（例如刚开的一个 `powershell` 的 pid）：

```powershell
# 另开一个"陌生服务"
cd $env:TEMP; python -m http.server 9999        # 或任意 HTTP 服务
# 篡改 runtime.json
notepad "$env:APPDATA\omni2api\runtime.json"    # addr 改 127.0.0.1:9999，pid 改上面 powershell 的 PID
# 再双击一次 exe
```

**期望现象**（三道门，任一不过都不许挥手）
- 实例 2 **不**触发激活事件，日志明确写出原因：

```
端口 127.0.0.1:9999 上不是本程序的服务：/healthz 的 service="<陌生服务返回的>" 与本程序（"omni2api"）不一致
无法唤醒已有实例：... （本进程让位退出，避免两个实例写同一份数据）
```

- 陌生服务**收不到**带 `api_key` 的任何业务请求（只有一次 `GET /healthz`，可在
  `python -m http.server` 的访问日志里看到）。
- 同类变体（都要测）：
  - `pid` 指向已退出进程 → `runtime.json 里的 pid=<x> 已退出（残留文件）`；
  - 端口上无人监听 → `读 runtime.json：...` 或连接错误文案；
  - 端口上是另一个 **omni2api 分支/仿制品**（`service` 名不同）→ 也要拒绝。

**失败时看哪里**
- 若日志出现 `已验证实例 pid=... ... 已请求它显示控制台` 却指向陌生端口 →
  `ProbeService` 的身份判据被绕过，属**高危回归**，立刻记录该 `runtime.json` 原文。
- 自动化侧对应断言：`TestProbeServiceRejectsForeignService`、
  `TestProbeServiceRejectsNonHealthzPayload`、`TestProbeServiceFailsOnDeadPort`。
  注意 `wantService` 传空串时只校验"是 JSON"，生产路径永远传具体名字。

---

### E8 从旧目录升级：账号不能丢（**最高优先级**）

**造条件**：先造一份"改造前的布局"，再让桌面版去救它。

```powershell
# 1) 造旧布局：exe 同级（或当前工作目录）有 config.json + auths + data
mkdir C:\omni2api-legacy; cd C:\omni2api-legacy
#    放一份改过的 config.json（有 api_key / listen）
#    放 auths\workbuddy\账号-甲.json、auths\账号-根.json、data\state.json
# 2) 从那个目录启动桌面版（旧布局的候选就是 exe 同级与 cwd）
& 'D:\tools\omni2api\NaiBox.exe' -console
```

**期望现象**（四件事，缺一条就算不通过）
1. 数据根三样齐全，且与旧目录**逐字节相同**：
   `%APPDATA%\omni2api\config.json`、`auths\**`、`data\state.json`。用
   `Get-FileHash` 或 `fc /b` 对比。
2. **旧目录原封不动**：`C:\omni2api-legacy\` 里所有文件还在（迁移是复制不是移动）。
3. 旧目录多出标记 `.migrated.bak`，内容是 JSON，含 `from` / `to` / `copied` / `note`，
   其中 `to` 必须等于 `%APPDATA%\omni2api`。
4. 再启动一次是**幂等**的：日志出现 `迁移检查：...\config.json 已存在，无需迁移`，
   不重复复制、不改动已迁移文件、不重写标记。

对应的日志四行：

```
发现旧布局：C:\omni2api-legacy（含 config.json）→ 复制到数据根 C:\Users\<用户>\AppData\Roaming\omni2api
迁移完成：复制 <N> 个文件（C:\omni2api-legacy → ...）；源目录保留未动
已在源目录留标记：C:\omni2api-legacy\.migrated.bak
迁移摘要：来自 C:\omni2api-legacy，复制 <N> 个文件，保留 <M> 项
```

- UI 里的账号数必须与旧版一致（打开 `http://127.0.0.1:7863/ui` 看账号列表）。
- `logs\` **不该**被搬过来（日志是诊断材料，不是资产）。

**失败时看哪里**
- UI 账号为 0 → 先看 `%APPDATA%\omni2api\auths\` 有没有文件；没有就看日志是
  `旧布局迁移未完成（服务继续启动）：<err>`（迁移中途失败，已复制的文件**不会**被删）
  还是压根没找到旧布局（`...都没有旧布局 config.json，按首次启动处理`）。
- 旧目录的文件被删了 → **严重回归**（复制写成了移动）。
- 目标已有同名文件时，实现选择保守策略"谁在目标里谁赢"，不会覆盖；
  已知不足：部分冲突时 `MigrationResult.Kept` 为空，迁移摘要只会说"复制 N 个文件"，
  不会点名被跳过的那个——排查时以目标文件的实际内容为准。
- 旧目录不可写（例如装在 `C:\Program Files\` 下）时，数据照样复制成功，只是标记写不进去：
  日志会出现 `迁移完成但标记文件写入失败（不影响数据）：<err>`。这属于**可接受**结果，
  但要在验收记录里写明"该机器没有 `.migrated.bak`"。
- 自动化侧对应：`migrate_test.go` 里的
  `TestMigrateLegacyCopiesByteForByteAndKeepsSourceIntact`（四断言）、
  `TestMigrateLegacySecondRunIsIdempotent`、`TestMigrateLegacyNeverOverwritesExistingTargetFiles`、
  `TestMigrateLegacySkippedWhenExplicitConfig`、`TestMigrateLegacyNoopWhenNoLegacyLayout`。

---

### E9 托盘退出后的落盘与 `runtime.json` 清理

**造条件**：先让账号池变"脏"（这一步很关键，否则"没落盘"与"没东西可落盘"分不开）：
在 UI 里禁用（或删除）一个账号 → 等 3 秒 → 托盘右键 `退出`。

**期望现象**
- 日志尾部：

```
正在退出（先落盘再停机）
bye
```

- `runtime.json` **消失**（`Test-Path` 为 False）。
- `data\state.json` 存在，且其中的账号状态与 UI 上的最后操作一致
  （禁用/删除后**重启不复活**）。
- 进程退出码为 0，任务管理器里没有残留 `NaiBox.exe`。
- 托盘图标消失（`Quit` 已调用）。

**失败时看哪里**
- `runtime.json` 还在 → 退出路径没走 `RemoveRuntime`（下次启动会把死进程当活实例，
  表现为"双击没反应"）。先手工删掉它再继续验收。
- 重启后账号复活 → 落盘与停机的**顺序**被调换（必须先 `p.Flush()` 再 `srv.Shutdown`）。
  证据：`state.json` 的 `LastWriteTime` 早于日志里 `正在退出` 那一行的时间。
- 退出卡住 20 秒以上 → 日志 `等待后台收尾超时（继续退出）`；记录当时是否有
  长连接（SSE）在跑。
- 0 账号、什么都没做就退出时 `state.json` 可能不存在——**这不是失败**（只在 dirty 时落盘）。
- 自动化侧对应：`TestRuntimeFileRoundTripAndRemoveIsIdempotent`（删除幂等）、
  `TestDesktopBootServesHealthzThenShutdownReleasesPort`（Shutdown 幂等 + 端口释放）。

---

### E10 崩溃残留 `runtime.json` 后的重启

**造条件**：正常运行后，用任务管理器**结束进程**（不是托盘退出），确认
`%APPDATA%\omni2api\runtime.json` 仍然存在（pid 已死）。然后正常双击 exe。

**期望现象**
- 新实例照常启动（日志 `数据根 ... 启动`），并覆盖 `runtime.json` 为新的 pid/addr。
- 若此时再双击第二次，实例 2 得到的结论是"pid 已退出（残留文件）"并正常让位/唤醒。
- 端口按 E-端口规则选：7863 若仍被 TIME_WAIT/僵尸占用则顺延，日志
  `端口 7863 被占用，改用 7864`。

**失败时看哪里**
- 一直提示"已有实例在运行"却没人起来 → 残留 pid 正好被别的进程复用（罕见）。
  临时解法：删 `runtime.json`；根本判据是 `processAlive` 是否只看 pid 存活
  而不看 `/healthz`（两道门都要过才对）。
- 端口漂到 7864+ 而客户端写死 7863 → 看日志的 `端口 ... 改用 ...` 行，
  以及 `Get-NetTCPConnection -LocalPort 7863` 的占用者。

---

### E11 备份与恢复

**造条件**：托盘 `备份数据`。

**期望现象**
- `%APPDATA%\omni2api\backups\omni2api-backup-YYYYMMDD-HHmmss.zip` 生成，且资源管理器
  自动打开备份目录。
- zip 内路径用正斜杠，包含 `config.json`、`auths\...`、`data\...`，**不含** `logs`。
- 恢复演练：把 zip 解压到另一台机器的 `%APPDATA%\omni2api\`，启动桌面版 →
  账号齐全、`api_key` 与备份的一致（客户端不用改配置）。
- 连续点 11 次，`backups\` 里最多 10 个 zip（按文件名时间序保留最新 10 份）。

**失败时看哪里**
- 弹框 `omni2api 备份失败` → 日志有具体原因（`创建备份文件: ...` / `备份 auths: ...`）。
- zip 里少了某个上游的账号 → 该上游的 `auth_dir` 被配成了**数据根之外**的绝对路径
  （备份只收 `DataDir\auths` 与 `DataDir\data`）。核对 `config.json` 里各段的 `auth_dir`。

---

### E12 portable.flag 绿版 / `-config` 显式指定

**造条件 A（绿版）**：把 exe 放进 `E:\omni2api-portable\`，在同目录建空文件 `portable.flag`，
双击运行。
**期望**：日志 `数据根 E:\omni2api-portable（portable=true）`；数据全部落在 exe 旁边；
`%APPDATA%\omni2api` **不被创建**；可以与 E-双开 的 `%APPDATA%` 实例**同时运行**
（两把单实例锁 hash 不同）。

**造条件 B（-config）**：`.\NaiBox.exe -config "D:\cfg\我的.json"`。
**期望**：日志 `数据根 <默认根>（portable=false）；配置 D:\cfg\我的.json`，并且
`迁移检查：-config 显式指定（D:\cfg\我的.json），尊重用户选择，不做旧布局迁移`，
旧目录**不会**出现 `.migrated.bak`。

**失败时看哪里**
- portable 版仍写了 `%APPDATA%` → 数据根优先级回归（对应
  `TestResolveLayoutExplicitDataDirWinsWithUnicodeAndSpace` 的同源逻辑）。
- `-config` 却发生了迁移 → 用户会得到两份 auths（"改配置没反应"、"删号又复活"）。
- 两个实例互相顶掉 → 锁名没带 `DataDirHash`（见 E6 的 hash 判据）。

---

### E13 休眠唤醒

**造条件**：起好服务（有至少 1 个账号，便于观察保活），合盖睡眠 ≥2 分钟，唤醒。

**期望现象**
- 窗口内容还能用（必要时刷新一次 `http://127.0.0.1:7863/ui` 立即恢复）。
- 端口不变（`Get-NetTCPConnection -LocalPort 7863 -State Listen` 仍指向本进程）。
- 日志在唤醒后没有 `http: <监听错误>`（那是致命出口，会直接退出）。
- 断网期间的上游请求只产生可读错误（`401/超时` 之类），进程不退出；
  账号不会因为断网被大批标记为失效。
- 唤醒后网络恢复，30 分钟粒度的签到/保活扫描继续（`data\checkin-log.json` 有新记录）。

**失败时看哪里**
- 唤醒后控制台 502/连接被拒、托盘图标还在 → 服务 goroutine 已死但进程活着：
  看日志最后几行有没有 `http: ...`（说明 Serve 循环退出）、
  以及是否触发了 `致命错误：...`（会弹框）。
- 托盘图标消失但进程还在 → `explorer.exe` 重启导致的通知区重建（已知形态）；
  这次不算失败，但必须验证"还能退出"：用 `-console` 或任务管理器结束，
  且结束前确认 `runtime.json` 会被下次启动覆盖（E10）。
- 唤醒后立刻"账号全冷却" → 保活刷新的时间跳变处理有问题（记录日志里
  刷新时间戳与 `state.json` 的冷却字段）。

---

### E14 重启服务（自我替换进程）

**造条件**：服务在跑、窗口开着 → 托盘右键 → `重启服务`。观察 5 秒内的窗口与进程。

**为什么它单独列一条**：这是全项目**唯一**「两个进程同时存在一瞬间」的路径，也是最容易把
顺序写错的地方。实现是自我替换：旧进程用原参数加 `-restart-wait` 拉起自己 → 落盘停机 →
释放单实例锁 → 退出；新进程在锁上**轮询等待**（最多 30s）而不是让位，拿到锁后接管。

**期望现象（日志原文）**
- 旧进程：`已拉起新进程 pid=…，本进程开始收尾并释放单实例锁`
  → `正在退出（先落盘再停机）` → `bye`
- 新进程：`由「重启服务」拉起：等待旧实例释放单实例锁`
  → `检测到旧实例正在退出（…），等待它释放单实例锁（最多 30s）`
  → `旧实例已释放单实例锁，本进程接管` → `omni2api listening on 127.0.0.1:7863`
- 窗口重新出现（新窗口），`runtime.json` 的 `pid` 已换成新进程。
- `data\state.json` 的修改时间早于新进程的启动时间 —— 证明**落盘先于释放锁**。

**失败时看哪里（三种错序各有明确症状）**
- 新进程报「已有实例」后退出 → **旧进程没释放锁就退出了**；结果是服务没了。
  看 `app.go` 的 `restart()` 头注释里写的顺序约束。
- 两个进程都在、其中一方报 `bind: Only one usage of each socket address` →
  **释放锁过早**（新进程抢到了端口，旧进程还在跑）。这条会留下两个进程写同一份
  `state.json`，比服务停摆更危险，出现了必须回滚这次改动。
- 重启后账号池空了 / 刚删的号复活 → `p.Flush()` 没在停机前执行（顺序被调换）。
- 新进程等满 30s 仍未接管 → 旧进程卡在 `srv.Shutdown(5s)` 或写日志阻塞；
  查旧进程是否还有未结束的 SSE 长连接。

---

## 2. 自动化测试与手工条目的对应关系

```powershell
# 每次改动桌面相关代码后先跑这个（无桌面环境也能跑，不联网，不写真实 %APPDATA%）
go test ./internal/desktop/ ./internal/boot/ -count=1

# 本机无 gcc（CGO_ENABLED=0），-race 跑不了，实测输出：
go test -race ./internal/desktop/ ./internal/boot/
#   go: -race requires cgo; enable cgo by setting CGO_ENABLED=1
```

| 自动化用例 | 守的故障 |
| --- | --- |
| `TestResolveLayoutExplicitDataDirWinsWithUnicodeAndSpace` | `-data-dir` 被忽略；中文/空格路径下子目录跑出数据根 |
| `TestResolveLayoutDefaultsUnderUserConfigDir` | 默认数据根落到安装目录/当前工作目录（升级丢账号的根因） |
| `TestResolveLayoutExplicitConfigKeepsDataRoot` | `-config` 顺手搬走数据根，出现两份 auths |
| `TestEnsureDirsCreatesSkeletonButNeverConfig` | 建目录时顺手生成 config，迁移判据失效 → 老账号被挡在门外 |
| `TestEnsureConfigGeneratesUniqueKeyAndNeverOverwrites` | api_key 空/固定；覆盖用户改过的配置（全体 401） |
| `TestDataDirHashCaseInsensitiveAndDirSpecific` | 同一数据根两种写法互不顶掉；不同数据根互相顶掉 |
| `TestRuntimeFileRoundTripAndRemoveIsIdempotent` | runtime.json 往返丢字段；退出时删除非幂等报错 |
| `TestMigrateLegacyCopiesByteForByteAndKeepsSourceIntact` | **迁移内容不一致 / 源目录被删 / 缺标记 / 搬了 logs** |
| `TestMigrateLegacySecondRunIsIdempotent` | 二次迁移覆盖新数据（删掉的号复活） |
| `TestMigrateLegacySkippedWhenExplicitConfig` | `-config` 场景照样搬家 |
| `TestMigrateLegacyNeverOverwritesExistingTargetFiles` | 旧数据砸新数据（新登录的号被过期凭证顶掉） |
| `TestMigrateLegacyNoopWhenNoLegacyLayout` | 空目录里凭空迁移、留下假标记 |
| `TestMigrateLegacyDoesNotCopyIntoItself` | portable 形态下自己迁自己 |
| `TestPickPortKeepsExplicitPort` | 用户显式端口被私自改写 |
| `TestPickPortSkipsOccupiedDefaultPort` | 默认端口被占仍硬用（启动失败） |
| `TestPickPortFallsBackOutsideRangeWhenRangeBusy` | 顺延段全占时返回 0/返回已知不可用端口 |
| `TestProbeServiceAcceptsOwnService` | 自家服务被误拒（双击没反应） |
| `TestProbeServiceAcceptsOwnServiceEvenWhenUnhealthy` | 把 `/healthz` 的 503（0 账号）当成"不是本程序"，导致首启双击无效 |
| `TestProbeServiceRejectsForeignService` | **把请求发给陌生服务** |
| `TestProbeServiceRejectsNonHealthzPayload` | 任意 200（HTML/错误页）被当成服务在线 |
| `TestProbeServiceEmptyWantServiceOnlyChecksJSON` | 固定"唯一不校验身份的入口"的语义 |
| `TestProbeServiceFailsOnDeadPort` | 残留 runtime.json 让第二实例白等 |
| `TestDesktopBootServesHealthzThenShutdownReleasesPort` | Run 返回时未监听；service 字段不对；Shutdown 不幂等/端口不释放 |
| `TestDesktopBootPortCollisionReturnsErrorThroughFatalf` | 端口冲突走了 `log.Fatalf`（双击一闪、无日志）或静默换端口 |
| `TestResolveListenPortOverrideOnlyInDesktopMode` | 服务器形态被 `Port` 悄悄改端口 |

**只能人工的条目**（自动化测不了）：WebView2 真实渲染、托盘菜单与图标、单实例事件唤醒、
DPI、休眠唤醒、Windows 版本差异、离开本机后的浏览器回落体验。

---

## 3. 判定速查（把这份文档收尾成一句话）

一次完整的桌面验收，只要下面六条同时成立，就可以签字：

1. `%APPDATA%\omni2api\` 下 `config.json` + `auths\` + `data\` 齐全，且 `auths\` 里的账号
   与升级前逐字节一致（E8）。
2. 旧目录仍在，且多一个 `.migrated.bak`（E8）。
3. 再启动一次不重复迁移（E8 第 4 条幂等）。
4. 托盘退出后 `runtime.json` 消失，`state.json` 反映了最后的状态变更（E9）。
5. 双击第二次 exe 只把已有实例唤到前台，且端口/pid 与 `runtime.json` 一致（E6）。
6. 篡改 `runtime.json` 指向陌生服务时，第二实例拒绝唤醒并写明原因（E7）。
