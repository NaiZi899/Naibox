# 变更日志（Changelog）

本文件记录**本仓库相对于上游 `Sliverkiss/workbuddy2api` 的增量**。
上游版本自身的变化请关注原仓库的 Release / commit 历史；本仓库的主要功能版本
仍会跟上游对齐，只是本表会列出本仓库的额外提交与里程碑。

格式参考 [Keep a Changelog](https://keepachangelog.com/)，版本号不在本表里维护
（跟着上游走），日期格式 `YYYY-MM-DD`。

> 📌 **关于命名与路径（读历史条目必看）**
>
> 本表是**历史记录**，早期条目里的旧命名一律保留原样，不做事后改写：
>
> | 历史写法 | 现在的对应物 |
> |---|---|
> | `workbuddy2api` / `multi2api` | 项目名 → **omni2api** |
> | `wb2api-server` / `wb2api-server-*`（产物名） | **omni2api-server** / `omni2api-server-<goos>-<goarch>.<zip\|tar.gz>` |
> | `deploy-config/`、`deploy-*.sh`、`restart-7863.sh` 里的绝对路径 | 私有部署资产，**不随公开仓库分发**（部署方式见 README） |
>
> 也就是说：历史条目描述的事都发生过，但里面的路径/产物名要按上表换算，
> 不要当成"现在仓库里还有这些文件"。

## 未发布 — 额度刷新闭环（基元律动）

> 基元律动的余额从"只能靠 curl"变成"控制台有按钮、后台有周期刷新、充值后自动解冻"；
> 额度刷新的耗时从"账号数 × 上游往返"降到 1~2 个往返。

### 新增 — 基元律动的额度入口

能力位补上 `quota-probe`（此前刻意不声明，理由是"没有可用的额度接口" —— 那个理由在
`quota_ext.go` 落地后已不成立）。账号行与分组头的「额度」按钮因此开始渲染。

新增 `internal/jiyuan/adminroute.go`：`POST /admin/jiyuan/quota-probe`，支持 `?uid=`
单号查询，返回 `updated / unknown / skipped` 三态与原因。`Hidden: true`（诊断入口，
不占左侧导航），回执不含 key 与 session。

一句话说清它与核心那条的区别：核心的 `/admin/accounts/quota/refresh` 会**写回池**，
这个端点只探测、不改变任何状态 —— 排障时的一次随手调用不该覆盖池里的额度快照。

### 新增 — `jiyuan.mgmt_base`

管理面（`/api/*`，余额所在）此前硬编码为 `https://tokenrhythm.studio`。走反代时
聊天走代理、余额直连官方域名，于是余额恒为 `—` 且**没有任何报错**。

现在与 `base_url`（推理面）分开配置，因为很多反代只转发 `/v1`。归一与校验在装配层：
必须 `http/https` + 主机名，去尾斜杠；**形态错误启动即失败**。留空 = 官方域名，
行为逐字节不变。

### 新增 — 周期额度刷新（core 任务）

配置 `pool.quota_refresh_interval_seconds`，默认 900 秒，`0` = 关闭，负数启动失败。
调度器语义是"从未跑过则立即跑"，所以**启动即刷一轮**，之后按间隔推进。

新导出入口 `admin.Handler.RefreshAllQuotas()`，与 HTTP 端点共用同一套作用域/跳过/并发
逻辑。定时任务额外**预过滤**：禁用账号、以及没实现 `QuotaExt` 的上游不参与 —— 否则
每轮都刷一行 `skipped` 日志，一天 96 条，真故障会被周期性噪音埋掉。

### 新增 — 充值后自动解冻（只针对余额耗尽型硬冷却）

新增可选扩展点 `gateway.QuotaUsabilityExt`：上游自报"余额是否直接决定可用性"。
基元律动返回 `true`（静态 key + 余额唯一门禁）；`workbuddy` 这类凭证有独立生命周期的
上游不实现，行为完全不变。

核心新增 `pool.ReviveHardCreditIfUsable`：额度刷新成功后，若余额 > 0 且账号处于
`CoolHard`，解冻。

**刻意不碰软限流与熔断。** 周期刷新若把正在吃 429 指数退避的号每 15 分钟放回池子，
会形成稳定的违规重试。余额只能证明"钱够"，不能证明"限流窗口过了"。所以它只清
`until / coolKind / reason` 三个字段，而不是复用 `ClearCooldown`（后者会连熔断退避、
软限流 streak、会话失效进度一起清，那是"人工宣布这个号没问题"的语义）。

### 优化 — 额度刷新并发化

`refreshQuotas` 由串行改为三段式：串行解析"每个账号问哪个上游"（纯内存）→ 4 并发打
上游 → 串行写池并解冻。锁只在第一段和第三段出现，并发段里没有任何共享状态。

### 修复 — 导入弹窗文案

- 三个标签改名：`API Key` / `登录凭证（给已有 Key 补余额）` / `登录凭证（新建 Key 并入库）`。
- 去掉界面里原样显示的 Markdown 星号（前端只做 esc，不渲染 markdown，所以 `**实时余额**`
  显示的就是一对裸星号）。
- 修掉一句**与事实相反**的橙色警告："登录凭证不会被存进网关"。这句话对「新建 Key」栏成立，
  对「补余额」栏相反 —— 那一栏必须把登录态写进凭证文件，否则周期刷新读不到余额。现改为
  中性提示，是否落盘由各栏说明。
- 说明区加 `white-space:pre-wrap`，多行提示不再被压成一整段。

### ⚠ 已知限制

基元律动"加账号"与"读余额"仍是两步：新建 Key 那一栏只保存 Key、不保存登录凭证，
所以导入后额度为空，需要在「给已有 Key 补余额」再贴一次同一个 `sess_`。该行为本次
**刻意不改**，只把文案写清楚。

## v1.1 — 2026-10-19

> 本版主题：**把"统计不准"和"界面误导"这两类看不见的错误修掉**，外加一个
> 只在二次启动才复现的桌面壳缺陷。
>
> 完整发布说明（含下载对照与已知限制）见
> <https://github.com/NaiZi899/Naibox/releases/tag/v1.1>；本节只记改动要点。
>
> ⚠ **升级后「消耗 token」会明显变大** —— 那是修掉了一处口径错误，不是流量涨了。

### 修复 — 用量统计补上输入侧口径

面板「消耗 token」此前只统计 `completion + reasoning`，两处都错：

- 输入侧 token **从来没有入库**（全仓库没有代码读取 `usage.prompt_tokens`）。
  实测每请求平均 prompt ≈ 9 万、completion ≈ 962 —— 只算输出，面板只剩
  真实量的 **1.65%**。
- reasoning 是 completion 的**子集**（`completion_tokens_details.reasoning_tokens`），
  旧口径把它加了第二遍，实测整体**高报 58%**。

现在 `prompt_tokens` / `tokens` / `total_tokens` 三口径分离，卡片为
输入 / 输出 / 消耗（输入+输出）。历史行没有输入侧数据，从 cache 明细反推
并在界面标注「输入侧覆盖 N 条（其中 M 条由缓存明细反推）」。

缓存命中的兜底链补上 OpenAI 官方**嵌套**形态
`prompt_tokens_details.cached_tokens`（此前只认顶层 `cached_tokens`，
照抄该结构的上游命中量恒为 0）。判据是"键是否存在"，不是"值是否为 0"。

### 修复 — 流式日志把「没有数据」记成「数据是 0」

流式路径曾写 `st.toks, _ = stats.Tokens()`，忽略 ok 标志。末帧无 usage 时
`Tokens()` 返回 `(0,false)`，于是 `0` 被写进日志、`-1` 哨兵被抹掉。
对聚合而言 `-1` 被排除而 `0` 会进平均值与 tok/s —— 静默把「上游没给」
当成「真的用了 0」。

### 修复 — 账号状态列不再把旧错误当成当前状态

`pool.NoteErrorExplain` 刻意不在成功时清空 `last_error`，代价是「6 分钟前
503、之后连续 200」与「现在就坏着」同形。现在 `last_success` 晚于错误时刻
即显示「正常」、错误降级进 tooltip；无成功证据时标签自带「· N 分钟前」。
顺带修好 `titleOf` 把 `extra`（「冷却中」「已恢复」）整个丢掉的问题。

### 修复 — 基元律动凭证导入三处

- 挂 session 时**不刷新昵称**：注释声称会刷，实现里没有那一行。现在只覆盖
  自动派生的昵称（用户手打的标签不动）。
- **上游 Key 列表读失败被吞**（`entries, _ :=`）：「读不到」与「对不上」落到
  同一句说明，把用户引去补 key —— 实际该做的是等一会儿重试。
- **说明性结论被当成失败**：「登录凭证有效，但这一栏不新增账号」回 400、
  前端打红。改为 200 + `notice`，渲染成中性信息弹窗。

### 修复 — 桌面壳启动后缩在左上角只剩一条标题栏

`moveWindow` 少了 `SWP_NOSIZE`。`SetWindowPos` 传 `cx=cy=0` **只有带
`SWP_NOSIZE` 时**才表示"不动尺寸"，否则 0 就是一个真实的尺寸请求 ——
Windows 把窗口压到最小跟踪尺寸（实测 135×37）。

只在**存在 `window.json` 时**复现：首次启动 `HasPos=false` 不调该函数，
所以"重装一次就好了"是假象。同时补记「上次是否最大化」—— 最大化窗口的
矩形是铺满屏幕 + 边框，只存几何不存标志会把下次启动的窗口建得比屏幕还大。

### ⚠ 破坏性变更 — 仓库改名

仓库 `omni2api` → **`Naibox`**，Go 模块路径随之改为
`github.com/NaiZi899/Naibox`。GitHub 的仓库重命名会给网页与 git 传输做
301 重定向，但 **Go 模块路径不走那套重定向**：`go get` 旧路径会落到模块
代理的旧缓存或直接 path mismatch。**有外部代码 import 本项目的需改 import。**
CI 产物命名同步改为 `Naibox-server-<os>-<arch>.<zip|tar.gz>`。

### 工程 — 发版流程

`release.yml` 此前把 Release 说明硬编码成一句"自动构建产物…详见 README"，
对下载者零信息量。现在改为 `--notes-from-tag`，说明取自 annotated tag 的
message；并把 release job 的 checkout 补成 `fetch-depth: 0`（浅克隆拿不到
tag 对象，`--notes-from-tag` 会读不到 message），另加一条"tag message 为空
就失败"的自证。

桌面壳版本改为从 tag 注入 `-X ...desktop.Version`，并在 CI 当场交叉编译
一个同参数的 linux 版跑 `-version` 校验（`-X` 路径写错时 `go build` 不报错，
只是静默不生效）。

## v1.0 — 2026-10-18（Windows 桌面版 + 控制台现代化）

> 本版主题有两块，互不耦合：**把"能跑的 exe"变成"像应用的 exe"**（桌面壳 /
> 装配层抽取 / 图标纯代码生成），以及**把控制台的"轮询"换成"推送"**（SSE 日志流 /
> 统计轮询 gating / 视觉令牌层）。
>
> **核心包与 HTTP 契约零改动**：`/v1/*`、`/admin/*` 的既有响应逐字节不变
>（只新增了一条 SSE 端点），`webui.html` 的 DOM id / class / 文案 / `data-act` 一个没动 ——
> 它们是与后端和 46 个前端脚本的字面契约。

### 新增 — Windows 桌面版（`omni2api-desktop.exe`）

双击即用，无控制台黑框（`-H windowsgui`），托盘常驻。菜单：打开控制台 /
在浏览器中打开 / 打开数据目录 / 查看今日日志 / 备份数据（zip，留 10 份）/
开机自启（`HKCU\...\Run`）/ 重启服务 / 退出。

**数据与程序分离（本版最重要的行为变化）**：数据根从"当前工作目录"改为
`%APPDATA%\omni2api\`。改这一条的原因是实测后果：旧布局下"删目录重装"
= `auths/` 与 `data/` 一起消失 = **每个账号都要重新登录授权**。改造后
程序目录可以随便删、随便换版本，数据零迁移。

**旧布局自动迁移**：首次启动按 `-config` 显式指定 → exe 同级 `config.json`
→ cwd `config.json` → 无（生成默认）的顺序探测。命中旧布局时**复制不移动**，
源目录保留并写 `.migrated.bak`（含 from / to / copied / note），日志记明
来源与去向。重复启动幂等；目标已有同名文件时**跳过而不覆盖**
（新登录的凭证不能被过期文件顶掉）。

**端口**：7863 起顺延到 7873，全占则请系统分配；实际地址写
`runtime.json`（`{pid, addr, mode, version, started_at}`），退出时删除。
第二实例启动时读它 → 过三道门（pid 存活 → `/healthz` 的 `service` 字段
是自家 → 才发唤醒事件）→ 唤醒已有窗口后退出。
**只凭"端口上有人"就发请求是危险的** —— 那可能连到别人的程序。

**安全默认**：桌面形态下 `listen` 的空 host 一律收敛到 `127.0.0.1`。
`":7863"` 在 Go 里等于"监听所有网卡"，而 `/ui` 对**回环来源**会自动注入
api_key（`server.webui` 的 `isLoopback` 分支）——绑通配等于同网段任何人
打开 `http://<内网IP>:7863/ui` 就能进控制台。用户**显式**写 `0.0.0.0`
或具体网卡地址时原样尊重（那是明确意图），只在日志里提示风险。
**服务形态（`cmd/server`）不受影响**，仍按配置原样绑定 —— Docker 的端口
映射依赖通配。

**兜底路径**：`-mode=browser` = 常驻托盘 + 系统浏览器，零 WebView 依赖。
WebView2 Runtime 缺失、无可见桌面会话、窗口异常时都回落到它。

**Windows 特有的两个坑（实测踩到并修掉）**：

1. 托盘 map 未初始化导致 `assignment to entry in nil map`，**把整个进程
   带走**（浏览器模式也死）。托盘 goroutine 与所有回调现在带 `recover`：
   界面坏了不能杀服务。
2. 端口探测在 Windows 上会误判空闲：一个进程已占通配 `:7863` 时，
   `bind 127.0.0.1:7863` **仍然成功**（Windows 按元组判冲突，不像 Unix）。
   于是第二个实例选中 7863，装配层绑通配时才失败退出。
   修法：探测必须同时能绑 `:p` 与 `127.0.0.1:p`。

### 重构 — 装配体迁入 `internal/boot`

`cmd/server/main.go` 原来是 1428 行 `package main`，装配逻辑无法被第二个
入口复用（`package main` 不能被 import），而复制一份等于两个装配源 ——
"加一个上游要改两处"正是判据 1 要避免的。

现在装配体在 `internal/boot`（`Run` / `Runtime.Shutdown` / `Runtime.Wait`），
`cmd/server/main.go` 37 行，`cmd/desktop` 共用同一套装配。

- 退出顺序**逐字保持**：先 `p.Flush()` 落盘，再 `srv.Shutdown(5s)`，最后
  按注册逆序跑清理链（等价于原来的 `defer` 链）。**先停机再落盘**会留下
  "已经不再接受请求、状态还是旧的"进程，表现就是删掉的账号重启后复活。
- 启动期致命错误仍是 `log.Fatalf` 的语义，只是改成**注入式**
  （`Options.Fatalf`）：桌面版可以接管成"写日志 + 弹框说明"，而不是
  双击后窗口一闪什么都没留下。
- 架构约束测试新增第三类 `assemblyPackages`（`boot` / `desktop`）：装配层
  **允许**依赖上游，但**不得被上游依赖**（新断言
  `TestUpstreamsDoNotDependOnAssembly`）。同时保留"登记必须真实存在"与
  "不得与 core / exempt 表语义串台"的守卫 —— 后两条防的是"分类表本身
  写错导致判据失效"。
- 已知行为差异：`-config ""` 现在按"未指定"回落 `config.json`
  （改造前是"不读文件、纯默认 + env"）。只影响显式传空串这种病态用法。

### 变更 — 图标改为纯 Go 代码生成

仓库不再保留任何二进制媒体文件。`assets/icon/` 只剩三件文本：`app.svg`
（矢量规格）、`app.manifest`（PerMonitorV2 / longPathAware / Win10-11 兼容）、
`README.md`。

- `internal/icon`（纯标准库）：`Draw(size)` / `PNG(size)` / `ICO(sizes...)`。
  形状用代码画（圆角渐变底板 + 对话气泡 + 四角星），构建期与运行时**共用
  同一份图形代码**，不会分叉成两套标。
- `tools/genicon`：`go run ./tools/genicon -out .build/icon/app.ico` 生成
  16/32/48（BMP 帧）+ 64/128/256（PNG 帧）六帧 ICO；`-verify` 直接读 PE 头
  断言 GUI 子系统与资源目录非空。
- **托盘图标必须给 ICO 内容，不能给 PNG**：`energye/systray` 的 Windows 分支
  把字节写到无扩展名临时文件后调 `LoadImageW(..., IMAGE_ICON, ...)`，而它只
  按 ICO/CUR 内容解析。喂 PNG 的结果是内部打一行
  `systray error: unable to set icon` 就返回 —— **不 panic、不报错、托盘直接
  没有图标**。所以单尺寸调用一律输出 DIB 帧（`icon.ICO(32)`），多尺寸才用
  PNG 帧；两条测试钉住这个形状。
- 构建期派生物（`.build/`、`cmd/desktop/rsrc.syso`、`dist/`）全部 gitignore，
  `go build` 与 `go test` 在干净 clone 上仍然只需 Go 一条命令。

### 修复 — 示例配置缺两个上游段，导致成品少两个上游

**现象**：下载 Release 包解开就是一整套现成配置，双击启动后顶栏显示「上游 10」，
而 README 承诺 12 个。用户的第一反应是"基元（jiyuan）被打包丢了"，
并且因为 `workbuddy_intl` 在示例里是 `enabled: true`，现象看起来更像
"基元被 workbuddy 国际版顶替了"。

**实际根因**：`internal/jiyuan` 与 `internal/inkstone` 的代码、`config.go` 的段定义、
`boot.go` 的 `registry.Register` 三者都齐全，**但两份示例配置里从来没有这两个段**。
而本项目的配置语义是「段缺席 = 不加载」：

```go
c.JiyuanEnabled = c.Jiyuan.Enabled   // 缺省 false
```

所以这不是打包丢了模块，是**示例配置从来没告诉用户有这两个上游**。
`workbuddy_intl` 一直存在（它本来就是 12 个之一），两个现象同时出现只是巧合。

**这类缺口是最难发现的一种**：编译不报错、测试不报错、启动不报错，
只在用户拿到成品后以"少一个功能"的形态暴露，而且看起来像构建问题。

**修复**：`config.example.json` 与 `internal/desktop/defaultconfig.example.json`
都补上 `inkstone` 与 `jiyuan` 段（`enabled: true`、`auth_dir` 指向
`./auths/<name>`、`pool_accounts: true`），与其余上游同律。

**守卫**（新增 `internal/desktop/config_drift_test.go`）：

- `TestExampleConfigsListEveryUpstream`：两份示例都必须列出全部上游段且默认启用。
  判据取"README 承诺给用户的清单"，不是"代码里注册了几个" —— 后者会把这个测试
  变成实现的镜像，实现漏了它就跟着漏。
- `TestExampleConfigsStayInSync`：桌面版那份是根示例的**副本**（`//go:embed`
  不能跨目录），复制就会漂移。只比顶层键集合、不比内容（`api_key` 刻意不同）。

两条都做了变异验证：把 `jiyuan` 段从根示例删掉 → 两条同时红
（"缺少 jiyuan 段"+"两份不同步"），恢复后绿。守卫不是永远绿灯的摆设。

### 修复 — 前端把「只有批量导入/凭证重载」的上游整组隐藏

**现象**：账号池只显示 10 个分组，缺 jiyuan（基元律动）与 inkstone（书生·端砚），
而后端 `/admin/providers` 返回 12 个。看起来像"基元被打包丢了"。

**根因在前端的显示门槛**（`upstreamGroups()`）：

```js
p.account_count > 0 || hasAnyPanel(p.id)
```

`hasAnyPanel` 判的是"有没有**自注册**管理端点"。而 jiyuan 的批量导入走的是
**通用**端点 `/admin/accounts/import`、inkstone 只有 `CredentialLoader`（放凭证文件 +
重载 auths）—— 两者都没有 `AdminExt` 路由，0 账号时整组从导航消失。

**修复**：manifest 每个 provider 增加 `entry` 字段，语义是"网关这边能不能收号"
（与 `capabilities` 的"上游能做什么"是两个轴）：

    LoginFlow 且 Configured()  ∨  import_kinds 非空  ∨  实现 CredentialLoader

前端门槛改为 `account_count > 0 || hasAnyPanel(id) || entry`。
`/admin/providers` 与 `/admin/ui/manifest` 两个构造点共用同一个 `hasEntryPoint`，
避免两套判据漂移（这个仓库已经栽过四次同类坑）。

**守卫**：`TestUIManifestEntryPoint` 不只断言两个 true，还带两类反例
（三入口全无必须 false、实现了 LoginFlow 但 `Configured()=false` 必须 false），
并自检两个测试桩各自只命中一条判据 —— 否则"任一成立即 true"会退化成同义反复。

用**真实 manifest** 回放新旧门槛：旧门槛 4 组 → 新门槛 12 组，
且 jiyuan/inkstone 都是 `acct=0 hasAnyPanel=false entry=true`，与事故现场完全一致。

### 新增 — 桌面版关窗行为（询问 / 最小化到托盘 / 退出）

`window.json` 增加 `close_behavior`（`ask`|`minimize`|`quit`，默认 `ask`）与
`tray_hint_shown`。入口在托盘菜单的三个单选勾选项（不占 webui.html）。

`minimize` 是**隐藏 HWND**，进程与 WebView2 继续跑 —— 这就是"后台保活"。

实现要点，每条都是踩出来的：

- **子类化必须保存原过程**（`SetWindowLongPtr` 的返回值）并在未处理消息上
  `CallWindowProc`，否则窗口的拖动 / 缩放 / DPI 变更全坏；`WM_NCDESTROY` 里还原。
  回调用全局单例（`syscall.NewCallback` 只有 2000 个槽）。
- **`WM_QUERYENDSESSION` 恒返回 TRUE**。拦截关机会让用户关机被卡住，Windows 随即
  强杀进程，`state.json` 来不及落盘 —— 这是本功能最容易犯的错。
  `WM_ENDSESSION(wParam=1)` 才抢时间落盘。
- **隐藏窗口不 Destroy WebView**：WebView2 进程与窗口生命周期解耦
  （MicrosoftEdge/WebView2Feedback#1424），隐藏正好利用它保活；
  真退出时才走 `Terminate`/`Destroy`。
- **实测：WebView2 的 `SW_HIDE` 不会让 `document.visibilityState` 变成 hidden**
  （探针连跑 3 次，全部仍报 visible）。所以"后台"必须由 desktop 侧主动补：
  隐藏时遮蔽 `document.hidden`/`visibilityState` 两个 getter 再派发
  `visibilitychange`，恢复时 `delete` 掉（可逆，未改 webui.html 任何函数）。
- 首次隐藏弹**一次性**托盘气泡（`NIF_INFO`）。不提示的话用户会以为程序被关了、
  再也找不回来。
- 连带修掉两个既有缺陷：几何每 5 秒落盘时会把 `close_behavior` 清空；
  `LazyProc.Call` 在 errno=0 时也返回非 nil error，导致 `ShowWindow`
  成功却被报成失败。

`quit` 与询问选「否」都**交回原过程**（DestroyWindow → PostQuitMessage →
`app.shutdown()`），不绕过 `boot.Runtime.Shutdown` 的「先落盘再停机」。

### 新增 — 仪表盘用量统计支持自定义时间区间

`/admin/stats/series` 增加 `from`/`to`（Unix 秒，与 `range` 互斥，from/to 优先）。
桶宽自动选档（≤2h → 5m，≤48h → 1h，其余 1d），目标是把桶数控制在几十个。

三条保护：跨度 ≥ 1 分钟、≤ 90 天、桶数 ≤ 2000。参数不成立时**回落 24h 而不是报错**
—— 这个端点的调用方是前端图表，它更需要"一张默认图"而不是 400 加空白面板。

两种模式**裁尾行为刻意不同**：`range` 裁掉尾部空桶（"最后一小时还没数据"是常态，
留着折线会莫名掉 0）；自定义区间**不裁**（用户指定的范围就是他要看的，
裁掉会把"这段确实没请求"表现成"这段不存在"）。

前端在「调用统计」标题旁加下拉（近 24 小时 / 近 7 天 / 自定义…）+ 两个
`datetime-local`。自定义区间看的是历史，30s 轮询与窗口隐藏时都不再重复拉取。

顺带修掉一个静默忽略：`range` 模式的 `bucket` 只认 `day`，传 `hour` 会被无声吃掉。

### 变更 — 应用改名 NaiBox，图标改为浅蓝底 + 字母 N

- 产物：`dist/NaiBox.exe`、发布包 `NaiBox-windows-amd64.zip`；
  窗口标题「NaiBox 控制台」，托盘 tooltip、单实例互斥体（`NaiBox-<hash>`）、
  开机自启注册表值名、错误框标题一并改。
- **向后兼容**：托盘图标外部覆盖认 `NaiBox.ico` 与 `omni2api.ico`（老用户的文件仍生效）；
  开机自启认新旧两个注册表值名，关闭时两个都删（否则老用户会显示"没开"、
  再点一次就多一条启动项）。
- **刻意不改**：`service_name`（`/healthz` 的协议字段，外部监控依赖它）、
  module path、数据根 `%APPDATA%\omni2api`（改了等于丢账号）、
  备份文件名前缀（清理逻辑按前缀匹配，改了会让老备份永不清理）。
- 图标：浅蓝渐变圆角底板（`#a8dcf7 → #6fbfef`）+ 深蓝字母 N（`#0d4a75`）。
  **N 不能用白色**：白字压在浅蓝底上对比度只有约 1.3:1（两者亮度太接近），
  16px 的托盘图标里会糊成一片白斑；深蓝约 6.5:1，小尺寸仍一眼可辨。
  `internal/icon` 与 `assets/icon/app.svg` 同步（Go 绘制是唯一实现，SVG 是规格），
  构建期由 `tools/genicon` 生成 6 帧 ICO，运行时托盘仍走 `icon.ICO(32)`
  （单尺寸 DIB 帧的契约不变 —— `LoadImageW` 只认 ICO 内容）。

### 修复 — 控制台顶栏仍是旧品牌与旧图标；基元/端砚在存量配置上不注册

用户报的两张图，是两个不同的漏项。

**① 顶栏品牌（`favicon.svg` + 页面文字）**

上一轮把应用改名只做到了**桌面壳那一层**（exe 名、窗口标题、托盘、互斥体），
控制台页面内部没动：标签页与顶栏用的是 `internal/server/favicon.svg`
（旧的紫蓝渐变气泡），h1 的文本来自 `MANIFEST.service`，而它在
`config.json` 里是 `"omni2api"`。

修法是把**品牌与协议标识分开**：

- `uiManifest` 新增 `brand` 字段（固定 `NaiBox`），前端 `serviceName()` 改为
  `brand || service || 'gateway'`。
- `service_name` **不改**：它同时出现在 `/healthz` 与 `/status` 的 `service` 里，
  桌面版的单实例校验靠它判断"端口上那个是不是自己"，外部监控也可能按它筛选。
  品牌随产品走，协议标识保持稳定。
- `favicon.svg` 换成与 `internal/icon` 同几何的 N 图标（浅蓝底 + 深蓝 N）。

⚠ 踩到一个容易复发的坑：`applyManifest()` 是**按白名单重建** MANIFEST 对象的，
新加的后端字段不显式拷贝就永远到不了前端——表现为"接口明明返回了 brand，
界面却没变"。已在拷贝处注明。

**② 基元/端砚在存量部署上根本不注册**

上一轮修的是"已注册但被前端过滤"（`entry` 门槛）。但用户的实际
`%APPDATA%\omni2api\config.json` 是升级前生成的，里面**从来没有这两段**，
而配置语义是"段缺席 = 不加载" —— 所以上游压根没注册，前端无从显示。

修法：`inkstone` 与 `jiyuan` 的 `enabled` 改成 `*bool` 三态，
**没写 = 启用，写 `false` = 关闭**。

这与其它上游的 opt-in 语义**刻意相反**，理由写在
`config_provider_sections_test.go` 的 `defaultEnabledSections` 里：

- 其余上游保持 opt-in 是对的（避免升级后凭空多出上游、自动向新平台发请求）；
- 但这两个上游的 Caps 只有 `chat|models|import`，**没有签到/保活**，
  "默认启用但没凭证"不产生任何周期出站请求，只是界面多一个空分组；
- 而"少两个上游"是用户实报的 bug，README 还承诺 12 个。

原测试 `TestAbsentProviderSectionResolvesDisabled` 的注释**精确预言**了
这个改动（"若写成默认 true，段缺席会凭空多出一个上游"），所以改测试时
把那两个上游单独列为例外，并把新语义钉死；其余上游仍受"段缺席必须禁用"约束。

### 修复 — 应用内图标与窗口图标都没有换新

两处是**独立原因**，只修一处会以为"改了没用"。

**① 应用内（标签页 / 顶栏 logo）**：`favicon.svg` 确实换了，但
`webui.go` 给它设了 `Cache-Control: public, max-age=86400`。于是**只有
浏览器里存着缓存的人看不到**——`curl` 与 headless（每次新 profile）拿到的
一定是新图，用户浏览器里还是 24 小时前那个气泡。

修法：页面三处引用（`link icon` / `apple-touch-icon` / 顶栏 `img`）都带版本串
`?v=nb2`，并在 favicon handler 的注释里写明"改图标要同步改这个值"。
这类"改了没生效"极难自查，因为**验证工具（新 profile）永远看不到缓存问题**。

**② 窗口图标（标题栏 / 任务栏 / Alt-Tab）**：从来没设过。

exe 的 PE 资源里有图标，但那是**文件图标**——资源管理器读它；
标题栏 / 任务栏 / Alt-Tab 读的是**窗口图标**（`WM_SETICON`）。两套机制。
go-webview2 建的是普通 Win32 窗口，不会自动设，Windows 于是给默认空白图标。

修法：`LoadImage` 把资源读成 `HICON` 再 `SendMessage(WM_SETICON)`，
并**分两个尺寸**（16 给标题栏、32 给任务栏）——用 `cx=cy=0` 取到的是第一帧
（16×16），把它设给 `ICON_BIG` 会让任务栏里出现一张放糊的小图。

⚠ 踩的坑：**`RT_GROUP_ICON` 的 ID 是 2，不是 1**。rsrc 把 manifest 放在 ID 1、
图标组放在 ID 2。我按"常见约定 = 1"写，`LoadImage` 返回 0，窗口保持默认空白图标，
**不报任何错**（只有日志里那句"没找到 RT_GROUP_ICON"能反推）。

查证方式比猜快得多——直接用 Windows 自己的枚举：

```powershell
# EnumResourceNamesW(hModule, 14, ...)
RT_GROUP_ICON(14): ID=2      RT_MANIFEST(24): ID=1      RT_ICON(3): 6 帧
```

现在代码里先按 ID=2 取，取不到再扫 1..16 兜底，并在启动日志里报实际用的 ID
（换资源生成器时不会静默失效）。

### 改进 — 控制台：实时日志推送 + 视觉令牌层

先给基线（否则"更快了"只是体感）。`tests/frontend/perf_probe.js` 是本版新增的
零依赖探针（Node 22 内置 WebSocket 走 CDP，不需要 `ws`/`npm install`），
12 秒采样、Chrome 155：

| 指标 | 改造前 | 改造后 |
|---|---|---|
| 停在请求日志面板 12s 发出的请求数 | **3** | **0** |
| ↳ 那 3 个是什么 | `/admin/stats`、`/admin/stats/series?range=24h`、`/admin/logs/history` | 长连接已建立，无周期请求 |
| 切到日志面板的渲染成本 | 0.069 s | 0.031 s |
| 默认面板空闲 12s 的 CPU | 0.02% | 0.02% |
| 首屏 taskSec | 0.106 s | 0.153 s（CSS 从 452 行增到约 1100 行） |

**新增 `GET /admin/logs/stream`（SSE）**：只推**连上之后**的新行，历史仍由
`/admin/logs/history` 分页提供（职责不重叠）。复用 `logbuf.Ring.Snapshot(sinceSeq)`
的增量游标，服务端 300ms 一 tick、一 tick 攒一个 buffer 一次 Flush；
心跳 `: keep-alive`；非 Flusher 回 501、未接线回 503、远端断开即 return。
鉴权走 `/admin/*` 的统一入口 —— 这条端点会把完整请求日志（uid/model/消耗）
持续推给客户端，单独开洞等于把账号池活动直播出去。

**前端接线**：可见才连（`visibilityState` + 日志面板可见），切走就 close；
收到新行走既有 `renderLogs` 路径（列定义与排序契约仍只有一处实现），
80ms 合并批量重绘；不可插行时（第 2+ 页 / 已清屏）只累计「N 条新日志」按钮。
断线**不用** `EventSource` 原生重连（它复用旧 URL，也就是旧 `since`），改成
close + 1/2/4/8s 退避重开并带上最后收到的 seq；连败 5 次停手，切走再切回重置。
`logStreamLive` 为真时跳过 10s 轮询，为假自动接回 —— 兜底路径始终在。

**统计轮询加可见性 gating**：`/admin/stats` 与 `/admin/stats/series` 只在仪表盘
可见时跑。原先用户停在日志面板看日志，界面每 10 秒还在拉统计，而那两块数据
只有仪表盘用得上。这一条与 SSE 一起，才把「停日志面板 12s 的 3 个请求」降到 0。

**视觉**：建立令牌层，但**既有 8 个语义色一个都没改名** —— `p4_badge_check.js`
在运行期读 `--acc` 的实算值，十余条 CSS 规则引用 `--bg/--panel/--fg…`，
改名不会让任何东西编译失败，只会让守卫**静默失去着力点**。新增的是别名
（surface/border/text/accent/bad）、派生变体（`color-mix(… var(--fg))` 现算，
两套主题共用同组令牌名、切主题自动跟随）、字号 6 档、间距 8 档（4px 基准）、
圆角 5、阴影 3、动效 3 档。

七态补齐，其中两条是真实可用性缺陷：
- **`focus-visible` 原先被 `outline:none` 删掉了**，键盘操作基本看不见焦点在哪；
  现在补 2px 焦点环，`.pager .pagesize` 单独做同权重覆盖（它是全页唯一会漏环的控件）。
- **`disabled` 原先只降 opacity**，深色主题下几乎不可辨，用户会反复点。
  现在除褪色外还压掉背景与边框。

另有：斑马纹 + 整行 hover（用 `:not([colspan])` 排占位行、`:not(.grouprow):not(.errrow)`
排自带底色行，否则会染分组标题）、按钮 loading 锁 min-width 不塌陷、
`prefers-reduced-motion` 覆盖 transition/animation/scroll-behavior 三项、
滚动条改标准 `scrollbar-color/width`、长 uid 省略 + title 全值。

**本版明确不做**（有实测依据，不是偷懒）：
- **虚拟滚动**：切日志面板只需 69ms、DOM 约 5600 节点、零 longTask。
  虚拟滚动要引入窗口化、行高测量、滚动锚定，改错一处就是"翻页错位/空行"，
  收益为零。
- **渲染层重写**：默认面板空闲 CPU 已经是 0.02%，作者此前那轮"面板可见才拉"
  的 gating 是有效的。动一个已经优化过的渲染层，风险远大于收益。

**探针抓出并修掉的三个真实缺陷**：`.livebadge` 在浅色主题下实测 4.38:1
（不达 AA，改 `color-mix(--ok 75%, --fg)` 后 5.83/7.62）；`onerror` 只置了
连接标志却没重画角标（**掉线时角标仍写「实时」**，重连通常几百毫秒成功所以
平时看不见，服务端长时间不在就是持续的谎）；重试计数只在 `open` 时清零
（一次"服务端还没起来"会变成永久放弃，切走再切回也救不回来）。

### 发布

`release.yml` 新增 `desktop` job：生成图标 → 生成 `.syso` → 构建 →
`-verify` 自检 → 打包 `omni2api-desktop-windows-amd64.zip`
（exe + `config.example.json` + README），与 `omni2api-server-*` 并存。

**`Prune stale assets` 的白名单同步加了新产物名** —— 不加的话 CI 会把自己
刚上传的桌面包当"不符合命名规范的残留"删掉。该步骤注释里记着 v1.7.0 的
七资产事故，属于同一类坑。

### 文档

- 新增 `docs/desktop.md`：13 条环境验收矩阵（Win10 / Win11、无 WebView2、
  DPI 100/150/200%、中文路径、双开、休眠唤醒、旧目录升级……），每条给
  「造条件 / 期望现象（含日志原文）/ 失败时看哪里」。
- `README.md` / `CONTRIBUTING.md` / `docs/chat-tools.md` /
  `docs/image-generation.md` 里指向 `cmd/server/*.go` 的路径全部更新为
  `internal/boot/*.go`（搬迁后的真实位置）—— 否则"加新上游"的指引会指向
  不存在的文件。

## v1.7.3 — 2026-10-05

> 本版主题：**修两个用户报障**（TRAE 签到"假成功" / raccoon「添加账号」打不开可用页面）
> \+ **一个跨全部上游的续期缺陷**。全部是修复，无新增上游、无配置项变更、
> 无破坏性变更。
>
> 三处缺陷有共同形态：**上游的业务失败是 HTTP 200 + body 里的业务码**，
> 而旧代码只看 HTTP 状态码 —— 于是失败被记成成功，界面显示绿色、历史表写 `ok`，
> 而实际什么也没发生。失败至少能被发现，假成功会一直骗到用户自己去核对。

### 修复 — TRAE 签到「签到不了」（closes #1）

用户报障（#1）：*trae的签到没生效*。实测复现，**两个缺陷叠加**：

**① `X-Device-Id` 发错了值**（每次都被上游拒绝）

签到接口要的"设备"是**账号 uid**，而本包一直发登录用的 32 位 hex 指纹
（`credential.DeviceID`，是 `login.go` 里 `randomHex32` 生成、参与登录 URL 的那个）。
端点、头名、body 全对，唯独这个**值**错了。真实上游三对照：

    X-Device-Id = uid(3900000000000000)   → {"code":0}      到手
    X-Device-Id = 32hex(ca0000…)          → {"code":9074}   拒绝
    不带该头                               → {"code":9004}   拒绝

设备维度的回执同样印证（`did_checked_in` 是设备级、`checked_in` 是账号级）：

    uid       → did_checked_in=true    ← 上游承认这台"设备"
    32hex     → did_checked_in=false   ← 不承认
    随机 16 位 → did_checked_in=false   ← 不承认

**② 业务失败被记成成功**

TRAE 签到的业务拒绝是 **HTTP 200 + `code != 0`**，而 `doJSON` 只在
`StatusCode >= 400` 时报错。于是：

    上游回 {"code":9074,"message":"当前参与用户太多"} → err == nil
    → 记 StatusOK → 界面"签到成功"、历史 `ok` → **积分一分没到账**

实测 9074 会**持续数分钟**（8/8 次、间隔 30 秒都不恢复），所以"重试一下就好"不成立。

改法：解析业务 `code`（9074 限流 / 9095 设备今日已签 / 9004 缺设备 / 1005 权益不足
分别可分类）；claim 后用**复查 status 的 `checked_in`** 判定成败（存在 `code=0`
但没真到账的情形）；补上 `enable` 守卫；`credits + extra_credits` 一并统计
（**额外奖励此前完全没统计**）。同一 bug 在后台定时任务里也有第四处，一并修。

实测（走真实网关）：`{"credits":200,"ok":true,"status":"already"}`。

### 修复 — raccoon「添加账号」打开的是**微信落地页**

用户报障：点「添加账号」后浏览器打开是空白/无用页。
（用户明确指出扫码与手机号都是正常路径，要修浏览器那条。）

根因：旧实现返回 `https://xiaohuanxiong.com/login/mp?code=<32hex>` ——
那是**二维码内容**，只能在微信里打开：

    官网主 bundle 里 `login/mp` 出现 **0 次**（只被当作二维码内容拼出来）
    微信 UA 与桌面 UA 请求它 → 返回**同一份 SPA 外壳**
    （3884 字节，路由表里没有 `mp`）

即"在电脑浏览器里打开它什么都没有"是**上游设计如此**，不是我们拼错了。

改法：改用商汤**官方 VS Code 扩展**自己的浏览器登录机制
（`Raccoon-VSCode/src/raccoonClient/raccoonClinet.ts` 的 getAuthUrl）：

    /login?appname=…&redirect=http://127.0.0.1:<端口>/raccoon/callback?nonce=…

官方页面上有**微信扫码**与**手机号短信**两个 Tab，过完阿里云滑块点授权后
把 `authorization_code` 回调到本机端口 —— 我们当场换凭证。
**滑块由官方页面与用户完成，我们不碰。**

实测（真实账号，端到端跑通）：

    回调收到  authorization_code=ac_TBFJ_…
    换凭证    HTTP 200 {"code":0,"message":"success","data":{"access_token":"eyJ…"}}
    user_info HTTP 200 {"code":0,"name":"RaccoonSophia","id":"7497524"}

`redirect` **没有白名单限制**。

⚠ 两个必须遵守的细节（都有实证理由）：

  - **绝不能带 `login_source=desktop`** —— 官网用它判断是否走
    `office-raccoon://` 深链；带了就**完全忽略 redirect**，永远等不到回调。
  - **防串号标识塞进 redirect 的 query（nonce），不能用 `state`** ——
    官网只转发 `authorization_code`，不复制其它参数。

⚠ 移除了 `QRLoginExt`：新 URL 编成二维码是**坏码**（手机扫会跳到
**手机自己的** localhost）。扫码能力没丢 —— 官方登录页自己就有扫码 Tab。

顺带说明：短信路径此前被认为"不可程序化"，准确说法是**"不能绕过滑块"** ——
`EncryptPhone`（AES-128-CFB）早已实现，实测加密后能通过，只差滑块；
现在滑块在官方页面里过，所以手机号登录可用。

### 修复 — 每个请求都续期（跨全部 10 个上游）

出口层判"该不该续期"读的是**池投影**的过期时刻，而各上游的池投影都**刻意不抄**
`ExpiresAt`（投影纪律：它是启动快照、活 secret 续期后不会跟新）。
实测 `/admin/accounts` 里 `expires_at` 键**一个上游都没有** ⇒ 恒为 0。

而 `auth.NeedsRefresh` 对 `ExpiresAt <= 0` **恒返回 true** ⇒
「该不该刷」对每个上游都恒真 ⇒ **每个 chat 请求先做一次续期往返**。
危害按 `refresh_token` 形态分两档：

    可重复使用（cline 等）→ 白费一次往返（实测：发一次对话后 token 被换了）
    **一次性**（raccoon / codearts）→ **每次请求烧掉一个 token**

改法：判据优先用**活凭证**的过期时刻（装配层已用 `CredentialExpiryExt`
从活 secret 补齐它，8 个上游全都实现了该扩展点）。

顺带修：`ParseJWTTimes` 缺 `iat` 时回落到 `nbf` —— raccoon 的 token
**只有 nbf 没有 iat**（抽样全部上游，只有它是这样），于是它的比例窗口
**整个空转**（`RefreshSkewExt` 形同虚设，注释却写着"iat + exp 都在"）。

实测：修前发一次 cline 对话请求 token 被换，修后不再被换。

### 已知限制（与上一版相同，未变）

- 只接了 Loomy 生图。其它上游要生图需各自实现 `ImageGenExt`。
- **图生图未实测**（上游支持 `images` 参数，本次只实测文生图）。
- 上游忽略 `response_format=b64_json` 与 `n>1`，网关如实透传、不替它纠正。
- 工具循环轮次上限 3；到顶会去掉工具定义强制收尾。
- 没有 WebUI 生图入口（只有 API）。
- 裸图片 URL 依赖"对象存储桶是公开读"这个**上游部署事实**；
  上游若改成私有读，会自动退回签名 URL（HEAD 会 403，预览器可能仍不工作）。
- raccoon 的登录依赖**上游 `/login?redirect=` 机制不做白名单校验**（本版实测可行）；
  若上游将来收紧，登录会退回"回调收不到 code"并如实报错（不静默超时）。

---
## v1.7.2 — 2026-10-05

> 本版主题：**修三个用户实际报障的缺陷**（agent 框架里生不了图 / 图片取不到 /
> raccoon token 静静过期）+ **一轮稳定性加固**。全部是修复，无新增上游、
> 无破坏性变更。

### 修复 — agent 框架（DSH / Claude Code / Cursor）里生不了图

用户报障："DSH 里生不了图"。根因不是 DSH 判断模型不支持生图，而是上一版
的一条判据**主动不注入工具**：

    客户端自带 tools → 网关一个工具都不注入

那条判据写在"普通聊天客户端"的语境里，初衷是对的（客户端带 tools 说明它
懂协议、要自己驱动工具循环）。但 **agent 框架每轮都带自己的工具**
（bash / read / write …），于是永远命中它 → 模型没有 `generate_image`
可调 → 只能吐一段 SVG 源码或说"我画不了"。

改法：边界从"整个不接管"改成**按工具名分工** ——

| 模型调谁 | 谁执行 | agent 框架看到什么 |
|---|---|---|
| 客户端自己的（`bash`/`read`/…） | **客户端** | 正常的 `tool_calls` |
| 网关注入的（`generate_image`/`web_search`） | **网关** | 只见模型"直接回了张带图的 markdown" |

于是 agent 框架侧**零配置** —— 不必在它的配置里声明生图工具。

⚠ 顺带修掉一个连带 bug：`injectTools` 原来是 `obj["tools"] = wireTools`
（**赋值 = 替换**）。在旧判据下它不出问题，判据一删就会**抹掉 agent 框架的
全部工具** —— 模型再也执行不了任何命令，而现象是"DSH 突然变笨了"，
与本功能毫无表面关联。现在改成合并 + 重名去重（以客户端声明为准，
避免工具列表重名被上游 400）。

### 修复 — 工具循环破坏了流式（"文字不是流式的了"）

上一版工具循环只有非流式一条路（内部跑完整轮再整块发出），而它对
**每个** loomy 请求都成立 —— 于是所有对话都被缓冲成一整块。
实测复现：12.6 秒的内容挤在 2 帧里。

那个设计基于一个**错误假设**（"必须先知道要不要调工具才知道给客户端什么"）。
实测推翻它（真实上游 25 帧的流）：

    第一个含 content 的帧    : 不存在（-1）
    第一个含 tool_calls 的帧 : 第 8 帧

即：模型要调工具时**正文一个字都不先发**；纯文字回复**从头到尾没有
tool_calls 帧**。所以改成边流边判断，普通文字 100% 保持逐字流式，
只在真要调工具的那一轮拦截。

修后实测：纯文字 31-88 个内容帧；流式生图 86 帧；流式搜索 569 帧。

### 修复 — 图片"无法预览" / "拿不到文件实体"

用户报障："图片无法预览"、"file unavailable"，并推断是
"上游出图→传云端的管道断了"。**那个推断是错的** —— 实测对象实体一直都在：

    HEAD → 200    GET → 200/330444
    HEAD → 200    GET → 200/217678
    HEAD → 200    GET → 200/1363756

真因是**签名 URL 对 HEAD 必然 403**（COS 把 HTTP method 也算进签名，
而 URL 是按 GET 签的）。而预览器 / 取图器 / 文件发送队列**通常先发 HEAD**
探测类型与大小 —— 一探测就 403，于是"无法预览"、"file unavailable"。
现象像管道断了，真因只是 method 不匹配。

修法：探测裸形式（去掉签名）是否可用，可用就用它（HEAD/GET 都通、
**不过期**、markdown 里也没有 `&` 与 `;`），不可用则退回签名 URL。
不硬编码"bucket 公开读"这个假设 —— 那是上游的部署事实，上游改私有后
探测会失败并自动退回。

⚠ 另有第二个根因：提示词里的 `]` 会**截断** markdown 链接
（`alt` 是用户输入，而 CommonMark 在第一个未转义的 `]` 处结束）。
用户看到的那段"图片无法预览 · 电商产品主图宣传海报…"就是截断后的 alt。
已转义 `\ [ ] ( ) < > * _ ` ~ | #`。

### 修复 — raccoon 的 Token 一直显示"已过期"（续期从不触发）

用户报障：管理台同时显示"正常"与"已过期"。实测那份凭证：

    access_token   寿命约 3 小时，**已过期 532 分钟**
    refresh_token  寿命 30 天，**还剩 29.5 天**（完全可续期）

根因：核心的续期只有两条触发路径 ——

    ① 出站请求时   handler 判断"该刷了"才刷
    ② 后台定时任务 JobExt.Jobs()

raccoon `RefreshCredential` 与 `RefreshSkew` 都有，**但没有 `Jobs()`**，
于是只有"有人拿它发对话请求"时才顺手续期。横向核对：workbuddy /
codearts / cline / lobsterai / qoder / trae 都有，**只有 raccoon 没有**。

已补 `internal/raccoon/jobs.go`（10 分钟一轮，判据复用 RefreshSkew 的
寿命比例，不另写一套）。实测：无人调用对话接口的情况下，42 秒内自动续期成功。

⚠ 这与 cline 此前那次报障（"Token 已过期，怎么不会自动刷新"）**逐字同一种病**。
教训：「具备续期能力」与「会去续期」是两件事 —— 前者是方法签名，
后者是**注册了后台任务**。缺后者时没有任何报错，只是悄悄过期。

### 稳定性加固

- **流式响应可能被写两遍**（响应损坏）。`consumeRound` 三条错误路径里
  只有两条检查了"是否已写过响应"，`status >= 400` 那条漏了 → 已写出内容后
  仍换号重试 → 往**同一个** ResponseWriter 再写一套 SSE → 客户端收到
  两段交错的流。改用统一出口 `finalize(sw)` 刷新 `committed`，
  并把"已写过"包成独立错误类型让 handler 不再重试。
- **工具执行不跟请求取消**（白扣积分）。三处都用 `context.Background()`，
  而 handler 手上明明有 `r.Context()` —— 后果是客户端断开后生图照跑满
  5 分钟、**积分照扣**。已接请求 ctx。
- **panic 有了可诊断的收场**。net/http 会 recover，但只关连接 + 打 stack：
  未写过 → 客户端看到"连接被重置"然后重试（再触发同一个 panic）；
  已写过 → 半截 SSE 挂到超时。新增 `panicRecovery` 中间件（挂在 `withAuth`，
  即 `/v1` 全部端点的唯一共同入口）。
- **服务器补了 IdleTimeout / MaxHeaderBytes**，但**刻意不设 `WriteTimeout`** ——
  它会掐断正常的长 SSE（实测搜索 35.4s、工具循环最长 15 分钟）。
  逐请求超时由 handler 自己的 ctx 管。
- **日志 TTFB / tok / tok/s 三列不再为空**。工具循环是另一条读取路径，
  绕过了 `chatStatsReader` —— 功能完全正常，只有日志悄悄空了。
  改成让每一轮的流**穿过**统计 reader（不重写一套 usage 解析）。

### 实测（真实上游）

    逐字流式      31-88 个内容帧
    流式生图      86 帧，含保活注释帧 + 图片 markdown + [DONE]
    流式搜索      569 帧
    两条生图路径  字段 created/data/points_consumed 完整，points=110，HEAD 200
    raccoon 续期  无人调用对话接口，42 秒内自动续期成功
    日志三列      TTFB=859ms tok=357 69.3tok/s
    MaxHeaderBytes 2MB header → 431

### 已知限制（与上一版相同，未变）

- 只接了 Loomy 生图。其它上游要生图需各自实现 `ImageGenExt`。
- **图生图未实测**（上游支持 `images` 参数，本次只实测文生图）。
- 上游忽略 `response_format=b64_json` 与 `n>1`，网关如实透传、不替它纠正。
- 工具循环轮次上限 3；到顶会去掉工具定义强制收尾。
- 没有 WebUI 生图入口（只有 API）。

---

## v1.7.1 — 2026-10-04

> 本版主题：**CI 发布流水线改为五平台全自动**，消除产物重复。
> 无任何功能变更。

### 修复 — Release 产物重复

v1.7.0 的 Release 上有 **7 个**资产但实际只有 5 个平台 —— 因为两套命名并存：

    CI 编的：  wb2api-server-windows-amd64.zip
               wb2api-server-linux-amd64.tar.gz
    本机编的： wb2api-server-v1.7.0-{windows,linux,darwin}-{amd64,arm64}.zip

其中 windows-amd64 与 linux-amd64 **各重复了一次**，命名风格还不一致
（一个带版本号、一个不带；一个 tar.gz、一个 zip）。下载的人分不清该拿哪个，
两个文件的内容也可能来自不同 commit。

### 变更 — CI 覆盖全部五个平台

    windows-amd64 / linux-amd64 / linux-arm64 / darwin-amd64 / darwin-arm64

命名统一为 `wb2api-server-<goos>-<goarch>.<zip|tar.gz>`（不带版本号 ——
版本在 Release tag 与二进制里，文件名带版本号只会让"同一 tag 重发"
产生两个名字）。归档格式按平台分两种（Windows 用 zip，其余 tar.gz）。
新增质量门禁 job（build/vet/test/tidy/gofmt）与残留资产清理步骤。

**本机不再手工上传任何产物。**

---
## v1.7.0 — 2026-09-30

> 本版主题：**上游清单从 4 个扩到 10 个** + **登录、签到、额度、续期四条链路的
> 系统性补齐**。
>
> 上游净变化（以**已发布的 v1.6.2 tag** 为基准）：
>
>     v1.6.2 注册 4 个：workbuddy / codearts / loomy / trae
>     本版  注册 10 个：+ workbuddy-intl / cline / raccoon / lobsterai /
>                        qoder / qodercn
>
> ⚠ 所以本版**只增不删** —— 新增 6 个，没有删除任何已发布过的上游。
> 下面「移除」一节里的 `mimo` / `buddy` 都**从未发布过**（它们在
> v1.6.2 之后的未发布提交里出现），对用户无影响，列出只为说明代码来龙去脉。
>
> ⚠ 但本版仍是**破坏性**的：多条链路的实现被重写（见各条标注），
> 且新增了 `dailycheckin` / `qrcode` 两个包与一批扩展点。

### 新增上游（+6）

- **Cline**（Cline 桌面端 / Cline API）：WorkOS 设备码轮询登录（**不起本地端口**，
  服务器部署天然可用）+ 余额查询 + 后台续期。
- **Raccoon Work**（商汤小浣熊）：**微信扫码**登录 + 积分余额 + 登录奖励 +
  onboarding 状态查询。
- **LobsterAI**（有道龙虾）：三步式签到领取积分 + 30 分钟自动签到。
- **Qoder / Qoder 中国版**（阿里系，两个实例）：PKCE 设备码登录 +
  积分余额 + 每日签到 + **WASM 加密推理桥**。
- **WorkBuddy 海外版**（`workbuddy-intl`）：双渠道账号池，按凭证 `channel` 字段
  自动选择 chat/billing/web 三个域的 base 与 Origin/Referer 头。

### 新增 — 统一的「全部签到」（两级）

- 顶部**一个**「全部签到」触发**所有**上游的全量签到（`POST /admin/accounts/checkin/all`，
  后台任务 + 进度回执）；各上游卡片头另有自己的「全部签到」（只签本上游）。
- 新增 `gateway.DailyCheckinExt`：有签到的上游都实现它，顶部那个据此发现它们。
- 为 codearts 补上全量端点 `/admin/welfare/claim/all`（此前只有单账号）。

### 新增 — 额度三层作用域 + 原始值显示

- **行内「额度」**只刷该账号；**卡片头「刷新本上游额度」**只刷该上游；
  **顶部「刷新全部额度」**刷所有账号。三者共用同一个文案渲染函数，措辞不会漂移。
- 四个上游实现 `gateway.QuotaExt`，额度列显示**上游自己的记账单位**（不做换算假设）。
- ⚠ 修 `qoder` 额度恒为 0/空：`QuotaExt` 只在**手动**刷新时被调用，全仓没有定时
  任务扫它 → 重启后额度列一直是空的。现由续期任务每轮顺带写回（新增 `QuotaSink`）。

### 新增 — Token 列 / 续期链路

- 统一「寿命 50% 续期」：各上游经 `gateway.RefreshSkewExt` 自报提前续期窗口，
  且 `RefreshSkewAtRatio` 按 **token 自己的 iat/exp** 算（不再依赖账号池投影）。
- 补齐 `gateway.CredentialExpiryExt` / `CredentialTokenExt` 的接线 ——
  「Token 到期」列此前恒为 `—`，根因是投影里没有 `ExpiresAt`（见 `HasToken` 修复）。
- ⚠ 修 `qoder` / `qodercn`「没有 Token」：续期响应字段名全错
  （读 `access_token` + `expires_in`，上游实际返回 `device_token` + `expires_at`
  ISO 字符串）→ 续期永远报"缺少访问令牌"、过期时刻从未落盘。
- ⚠ 修 `cline` 从不自动续期：`CredentialRefresher` 实现了，但**没有 `Jobs()`** ——
  核心两条续期路径（出站请求 / 后台任务）一条都不占。
- ⚠ 修 `qoder` 两个实例注册**同名**后台任务 → 调度器
  `任务名 qoder-refresh 重复（上游 qoder 与 qodercn），已跳过后者`
  → **中国版永不续期**。任务名改为 `<productID>-refresh`。

### 新增 — 批量导入统一到**全部**上游

- 新增 `gateway.AccountImportExt` + 能力位 `CapImport`，核心提供**一个**通用端点
  `POST /admin/accounts/import {provider,data}`；8 个上游各实现 `ImportCredentials`。
- 改造前只有 2 个上游有（workbuddy / loomy 各写一份）。
- 「导入」就是各上游**已有**的 `ParseCredential` + `MarshalAuthFile` 的复合，
  所以每个上游只需极少的搬运代码，不手写字段映射。

### 新增 — 机器人与登录

- **`runtime-info.exe` 实时生成设备身份**（qoder）：此前的"读磁盘缓存"路径
  在本机**文件根本不存在**（纯登录流程不产生它）→ 两个必需头从未发出 →
  每日签到**永远失败**。
- **扫码登录的二维码渲染**：新增 `internal/qrcode`（自实现二维码，零依赖，
  byte 模式 + 纠错 M + 版本 1–10）+ `gateway.QRLoginExt`。此前 raccoon 的
  「添加账号」给出一个**扫不了的链接**。
- 「添加账号」默认用**无痕窗口**打开授权页（网关自己启动浏览器 + 独立 profile）。
- 登录三步补齐**产品身份 UA**（此前全用 CLI 形态）。

### 控制台 UI

- **卡片头按钮顺序统一**（用户指定）：
  `全部签到 → 添加账号 → 批量导入 → 刷新本上游额度 → 重载 auths`。
  「添加账号」去掉 `＋` 前缀（右端多项对齐时不一致）。
- 账号池额度/状态卡**无感轮询**：只更新数值不整页重绘。
- 扫码类上游的引导语按**登录形态**分叉（qr / local / browser）——
  此前会同时显示「去浏览器登录」与「用手机微信扫码」两句矛盾的话。

### 安全 / 正确性修复

- ⚠ **导入不再丢失 `deviceToken`**：`SaveAtomic` 会把 `account.deviceToken`
  一起写回，导入不认它 = **用空串覆盖用户已有的设备令牌**（每账号一个的风控头
  丢了，表现为"导入后这个号开始被风控"）。两条导入路径各有一处同样的漏，都补了。
- ⚠ **`qoder` 中国版导入误拒**：`normalize()` 用**包级常量** `"qoder"` 兜底，
  拿不到实例产品 → CN 实例**必然误拒**没写 `product_id` 的凭证，
  且错误信息指向用户没写过的东西。判据改为读**粘贴原文**。
- ⚠ **`lobsterai` 签到永远失败**：上游返回的 `actions` 是**裸字符串数组**
  `["check_in"]`，我们按对象数组解析 → 每次签到都在第 3 步失败。
  契约测试的假上游当时也用了错形状，**把缺陷验证成了正确行为**。
- ⚠ **`refreshAccounts is not defined`**：分组行「刷新本上游额度」调用了一个
  从未定义的函数 → 点下去必抛。静态断言抓不到（文本里有这个名字）。
- ⚠ **`quotaScopeToast` 返回对象却被当字符串**：界面显示 `[object Object]`。
- ⚠ **瞬时 503 被当成永久失败**（qoder campaigns）：实测上游约 25% 请求回
  `DEPENDENCY_UNAVAILABLE`（与请求头无关，`/usage` 同时刻恒 200），
  而旧实现不重试 → 签到每 30 分钟扫一次，撞上就记一次 `fail`。现加 5xx 退避重试。

### 移除（两个都**从未发布过**，对用户无影响）

- **`mimo`（小米 MiMo）上游删除** —— 用户要求整体移除（"小米这个有问题"）。
  连带删掉 `internal/mimo/`（16 文件）、`cmd/server/mimocreds.go`、
  `scripts/mimo/`（5 脚本）、配置段与 17 个扁平字段。
  ⚠ 它是 v1.6.2 之后加进来的（commit `a796690`），**从未进过任何 Release** ——
  所以升级的用户不用担心这个功能消失，他们本来就没见过。
  ⚠ `internal/{cline,loomy}/models.go` 里的 `mimo` 是**模型名**（cline 代理的
  `xiaomi/mimo-v2.6-pro` 等），**不是**那个上游 —— 保留。
- **`buddy`（腾讯 CodeBuddy 中国版）也从未发布过**：它在**本批未发布的
  提交里加进来过**（commit `12c335d`），随后被删除（与 workbuddy 同一后端
  `copilot.tencent.com`，仅出站身份不同，用户判定重复）。
  `cmd/server/buddy_identity_test.go` / `config_buddy_test.go` 随之一并删除。
- ⚠ 配置里残留的 `mimo` 段会被**静默忽略**（JSON 未知字段不报错），
  但建议删掉以免误导。

### 破坏性变更有哪些（尽量列全）

| 变更 | 影响 |
|---|---|
| 新增 6 个上游 | 无（都在 `enabled=false` 或缺省关闭时不改变既有行为） |
| `mimo` 段失效 | 若你的 config 里有它：被静默忽略，建议删除 |
| 签到/额度/续期链路重写 | 行为变化（修的都是"本来就不工作"的路径） |
| 卡片头按钮顺序 + 去掉 `＋` | 纯 UI |
| 新增 `internal/dailycheckin`、`internal/qrcode` | 无（内部包） |

**没有任何配置字段被改名或删除** —— 旧 config.json 可直接用。

### 文档 / 工程

- `config.example.json` **补齐 4 个新上游段**（cline / raccoon / lobsterai / qoder）
  —— 它是 Release 压缩包里的配置模板，此前缺项，用户下载后看不到怎么启用它们。
  已用这份模板原样启动验证：10 个上游全部注册成功。
- `gateway` 契约测试新增：`CapImport` 声明必须实现 `AccountImportExt`
  （防"点了回 501 的假按钮"）。
- 前端探针：新增两个**真浏览器 e2e**（`verify_checkin_ui_e2e.js` /
  `verify_all_buttons_e2e.js`），验的是**行为**（点一下发几个请求、发的是哪个 URL、
  刷新后委托是否累积）—— 变异验证过（把"只绑一次"守卫去掉 → 刷新 5 次后点一次发
  **7 个**请求，而所有 DOM 静态断言全绿）。
- `internal/qrcode` 的正确性由**交叉验证**守住：真的调用 Python `qrcode` 库
  逐位比矩阵（结构断言全绿≠码能扫 —— 实测把 zigzag 方向改错时只有交叉验证会红）。

## 未发布 — 海外版 WorkBuddy AI 渠道（workbuddy-intl）

> 本版主题：**国内版 + 海外版（www.workbuddy.ai）双渠道账号池**。

### 新增

- **海外版渠道实例 `workbuddy-intl`**：同一个 workbuddy.Provider 实现注册第二个实例，
  凭证目录 `auths/workbuddy-intl/`，OAuth 站点 `https://www.workbuddy.ai`；
  配置 `workbuddy_intl.enabled=true` 显式启用（缺省关闭，老配置零影响）。
- **能力裁剪（海外版实测依据）**：海外版 product.json 显式禁用签到/成长/旅行
  （DisableCheckin=true、UserGrowth=false、DisableActivityBanner=true），
  因此 workbuddy-intl 实例**只声明对话/模型/额度探测**，不注册玩法后台任务、
  不挂玩法管理端点（DisableGrowthTravel 开关，默认关，国内版行为不变）。
- **多实例隔离**：管理端点路径按实例 ID 加 `/<id>` 前缀、后台任务名加 `<id>-` 前缀，
  同一实现注册多个实例不再互相冲突（默认实例路径/任务名逐字节不变）。
- **凭证级渠道路由**：`auth` 块新增可选 `channel` 字段（`cn`/`intl`），
  旧凭证无该字段时按 `domain` 自动推导（含 `workbuddy.ai` → intl）；
  chat/billing/web 三个域与 Origin/Referer 头随账号渠道自动选择
  （CN：copilot.tencent.com / www.codebuddy.cn / workbuddy.cn；Intl：www.workbuddy.ai）。
- **登录**：`./login.sh -intl` / `cmd/login -intl` 走海外版设备授权；
  WebUI「＋ 添加账号」按上游分派（workbuddy-intl 行内按钮用海外授权站点）。
- **本机客户端登录切换支持海外版**：按目标账号渠道写
  `workbuddy-desktop-ai.info` + `~/.workbuddy-ai`（CN 仍为
  `workbuddy-desktop.info` + `~/.workbuddy`）；进程检测覆盖 `WorkBuddyAI.exe`；
  备份/回滚带渠道元数据（last.meta.json）。
- **集成验证**：临时实例实测两个 provider（workbuddy / workbuddy-intl）账号
  正确并入同一账号池并各自路由。

### 文档 / 工程

- README / config.example.json：海外版渠道章节与配置示例。
- 凭证格式不变（account+auth 两块），channel 字段可选，旧文件零迁移。

### 新增 — 「添加账号」默认用**无痕窗口**打开授权页

- **默认无痕（`login.open_browser`，缺省 true）**：点「＋ 添加账号」时由**网关自己
  启动浏览器**打开授权页，不再把"用哪个窗口"交给用户当前浏览器状态 ——
  否则 OAuth 会拿浏览器里**已登录的账号**完成授权（"加新号"变"加老号"，且无任何报错）。
- **独立 profile（`login.isolated`，缺省 true）**：给浏览器一份临时
  `--user-data-dir`（`%TEMP%/wb2api-login-*`，48h 后自动清理），
  让这次授权从零 cookie 开始；只给无痕参数时，走 SSO 的站点仍可直接用已登录账号。
  代价是需重新输入账号密码（`isolated=false` 可关，仅开无痕窗口）。
- **新包 `internal/browseropen`**：`Plan` 是纯决策函数（平台/浏览器/参数向量
  全部可断言，测试不弹窗口），`Open` = Plan + 启动且**不等待**浏览器退出。
  候选顺序 Edge → Chrome → Brave → Chromium（Windows 上 Edge 一定存在，
  避免"点了没反应"）；macOS 走 `open -n -a <App> --args`；`login.browser` 可显式指定路径。
- **新端点 `POST /admin/login/open`**：用无痕窗口**重新**打开刚签发的授权链接
  （手滑关掉窗口时用，不会作废当前 state）。**只认本进程签发过的链接** ——
  入参是 URL 而它会被交给本机浏览器打开，不校验就等于"用本机浏览器访问任意地址"。
- **失败不拖垮主流程**：开不起来（无 GUI / 策略限制）时授权链接照常返回，
  响应带 `browser_error` 供界面显示「未能自动打开，请复制链接」；
  未接线 opener 的部署行为与改造前逐字节一致（不出现 browser 字段）。
- 前端：弹窗主状态改为「已用 X（无痕）打开」，新增「重新无痕打开」按钮，
  原「打开授权页面」降级为「普通窗口打开」并标注风险。
- 配置段 `login.{open_browser,browser,isolated}`。

## v1.6.0 — 2026-09-15

> 本版为**未发布**的本地里程碑（已 commit、未 push 远端）。主题：**新增第四个上游 TRAE** +
> **稳定性机制升级（移植自 AIClient2API）**。

### 稳定性升级（移植 AIClient2API 的成熟机制，保证账号持久稳定运行）

- **主动健康检查轮询**（新扩展点 `gateway.HealthProbeExt` + 核心任务 `pool-health-check`）：
  每 10 分钟（`pool.health_check_interval_seconds` 可配）探测冷却/熔断中的账号
  —— 用各上游的**便宜端点**（workbuddy/codearts/loomy/trae 各实现了自己的探测），
  成功即 `ClearCooldown` 提前恢复。额度"提前回血"的账号不再干等冷却到期；
  刚报错（2 分钟内）与禁用的账号不探测。
- **熔断错误计数时间窗衰减**：只有 60 秒窗口内的**突发**失败才累计到熔断阈值，
  零散错误不再慢慢攒坏一个号（此前只有成功才能清零）。
- **刷新失败上限 → 禁用**：凭证续期连续失败 3 次直接禁用（refresh token 已死，
  不再每次请求白试一次失败往返），成功清零。

### 新增（TRAE 上游，`internal/trae/`）

- **TRAE SOLO 对话通道**：`llm_utils_chat`（`function=solo_work_lite`），自定义 SOLO SSE
  实时转成 OpenAI SSE（`internal/trae/sse.go`），流式/非流式/工具调用/思考链均可用。
- **多账号 + 自动续期**：JWT + 消费型 refreshToken（与 codearts 同构），
  后台按 `refresh_interval_seconds` 扫描续期 + 请求路径惰性续期；`CredentialExpiryExt`
  提供「Token 到期」列。
- **每日自动签到**：30 分钟扫一次，查状态、未签则领（幂等、跨重启安全）；
  `DailyActionExt` 提供行内「签到」与顶部「全部签到」。
- **权益包额度**：`ide_user_ent_usage` 的 `credits_limit` 求和 → 账号池「额度」列。
- **模型目录**：实时拉 `get_detail_param`（config_name 即模型 ID），失败回落
  13+1 个已知 SOLO 免费模型的静态快照（trae-solo-unlock 实测清单 + glm-5.2）。
- **页内添加账号**：TRAE 浏览器 OAuth（`www.trae.cn/authorization` + 127.0.0.1 回调）
  完整搬进控制台 —— 「＋ 添加账号」返回**真实 TRAE 登录页**，网关在
  `127.0.0.1:18080/authorize`（可配 `trae.oauth_callback_port`）**自动接收登录回跳**，
  全程零手动步骤（无需复制链接/粘贴回调）；每次登录换新 machine/device id。
- **排队检测 + 模型分档自动降级**：监听上游 `request_wait_in_queue` 事件，
  排队位置超过阈值（默认 300）且尚未出内容时，自动换同档其它模型 → 下一档 → 兜底
  `glm-5`（`trae.fallback_enabled / queue_threshold / max_attempts` 可配）；
  已出内容后不再切换（避免客户端看到两段不连续回复）。
- **签到写历史**：「今日签到」列修复 —— trae 签到（手动/定时/批量）现在写
  checkinlog，账号表正确显示「已签到/失败/跳过」。
- 配置段 `trae.{enabled,auth_dir,refresh_interval_seconds,checkin_enabled,pool_accounts,oauth_callback_port,fallback_enabled,queue_threshold,max_attempts}`。

### 说明

- 协议来自对多个 trae 反代项目的复现研究（traework2api / trae-api / trae-local-api /
  trae-solo-unlock），核心事实已固化进 `client.go` 的常量与测试。
- 凭证两种形态都认（嵌套 `{auth,account}` / 扁平）；`trae-<uid>.json`。
- 契约测试用假上游全程执行（CI 不跳过），SSE 转换/分类/续期/额度/登录流程均有单测。

## v1.5.0 — 2026-09-15

> 本版为**自 v1.2.0 以来的累积发布**（此前 v1.3.0 / v1.4.0 两条只是里程碑记录，
> 从未发版；其内容已全部包含在本版）。主题：**Loomy 上游从"能转发"变成"能自助运营"，
> 控制台从"堆满数字"变成"一眼看清"**。仓库同步更名为 `multi2api` —— 本网关是
> 多上游聚合反代，不再是 workbuddy 专属。

### 新增（Loomy 上游完整化）

- **手机号 + 验证码登录**（`internal/loomy/smslogin.go`）：对接讯飞账号网关
  `account.xfinfr.com`，完整实现 HMAC-SHA1 请求签名；「＋ 添加账号」现在支持
  直接输手机号发短信验码登录，不再依赖本机客户端。
- **新手任务一键完成**：新标签页「新手任务 · Loomy」—— 8 个 onboarding 任务
  （共 10000 分）逐账号显示进度，支持「一键完成（N）」与「一键完成全部账号」。
- **邀请码**：新标签页「邀请码 · Loomy」—— 激活状态 / 余额 / **我生成的码**（含
  active/exhausted 状态）/ 绑定别人的码；绑定前置校验"不能用自己账号的码"，
  失败提示翻译成人话；**首登初始化自动补**（导入的号跳过客户端 first-login 时，
  邀请码与注册奖励会自动补齐，服务端幂等）。
- **批量粘贴导入**：账号池分组行「批量导入」按钮 —— 粘贴 JSON（单条/数组），
  逐条落盘 + 自动重载账号池；失败项逐条列出。
- **额度改走积分网关**：`GET /api/v1/points/records` 按 session 逐账号查
  **可用总额度（availableBalance = 总余额 + 当日剩余）** —— 修掉"没加上送的额度"、
  "只有本机那个号有值"两个问题；积分网关失败才回落到本机缓存。

### 变更（控制台 UI）

- 仪表盘：网关状态只留「账号总数 / 健康」两张卡；调用统计精简为 8 张核心卡
  （总请求 / 成功 / 失败 / 成功率 / **消耗 token** / 输出 token / 平均 TTFB /
  平均总耗时），**累计消耗从积分改为 token**（跨上游统一单位）；卡片宽度随内容
  自适应、左对齐、自动换行。
- 删除「任务历史」标签页（用户决定不需要）。
- 账号池：删除「熔断」「在途」两列（workbuddy 默认列集与 loomy 自报列集同步）；
  **所有昵称打码**（首 3 尾 2 保留，中间打 \*，已掩码的保持原样）；顶部「全部签到」
  按钮移除；codearts 按钮文案「领取福利」→「签到」（ID 与端点不变）；
  workbuddy「签到」与「额度」按钮互换位置（额度排最前）。

### 修复

- `/admin/login/poll` 回执不再把 `ExpiresAt` 零值（`0001-01-01T00:00:00Z`）下发 ——
  之前会让"添加账号"弹窗显示「已过期」，而账号表里写着「永久」，两处自相矛盾。
- 页面周期性刷新不再拆建面板、不再重复拉取已加载的面板；输入框重绘后保留已输内容。

### 说明

- 短信登录的 access key 来自 Loomy 安装包内嵌的 `.env.prod`（混淆而非真保密，
  见 `internal/loomy/smslogin.go` 的注释），可配 `loomy.sms_*` 覆盖。
- 邀请码每个 `maxUses=1`，绑定是一次性的、无法撤销；界面在绑定前强制确认。

## v1.4.0 — 2026-09-15

> 本版主题：**把 loomy 那三格空白补上，并把「上游」列统一到所有账号表**。
>
> 用户报的原文是两组现象：loomy 分组只有 2 项能力、「未声明页内登录流程」，
> 而 **额度 / Token 到期 / 今日签到三格都是空的**（"探究一下怎回事"）；
> 另外 codearts 的表格**第一列不是「上游」**，与其他上游的表格对不齐。
>
> 探究出来的根因是一个**共享的错误前提**：上一轮把"上游没有 HTTP 端点"
> 当成了"没有数据"。而实测证明 —— 登录态与积分摘要都**明文缓存在本机**
> Local Storage 里（`internal/loomy/clientstore.go`）。前提消失，能力随之补上。

### 新增

- **`gateway.CredentialLifetimeExt`（第 9 个可选扩展点）** —— 表达凭证过期信息的
  **第三态**：不是"某个到期时刻"，也不是"我们不知道"，而是**设计上就不会过期**。
  - 原来的两个指针（`token_expire_sec` + 字段缺失）表达不了它：loomy 的 session
    是服务端持久化登录态，落进"不知道"于是显示 `—`，用户读成"这功能没做"。
  - 单开接口而不是给 `TokenExpiry` 加返回值：那会与现有契约
    （"`ok=true, at<=0` 与 `ok=false` 同义"）**相反**，并强制全部实现者理解新哨兵 ——
    漏改一处就把"未知"说成"永久"，而"永久"是个**强断言**。
  - `AccountView.TokenNeverExpires`（`token_never_expires`，`omitempty`）。
  - 顺序契约：**先问时刻、拿不到才问永久**；有到期时刻时本字段恒为 false
    （反了会把 codearts 那 2 小时的 STS 渲染成「永久」）。
  - 不实现该扩展点的上游行为**逐字节不变**（未知仍渲染 `—`）。
- **loomy「＋ 添加账号」**（`gateway.LoginFlow`）：读本机客户端已登录的 session，
  **不需要浏览器**。用 `auth_url` 为空串表达"这次不用打开任何页面"——
  前端按**数据**分叉（不是按上游名），文案改成"正在从本机读取登录态…"。
- **loomy「额度」**（`gateway.QuotaExt` + `CapQuotaProbe`）：读本机缓存的
  `loomy-points-summary`，展示 `balance`（总余额，**不随自然日重置**）。
  - 为什么不是打接口：实测 17 条候选路径（`/points`、`/user/points`、`/balance`、
    `/quota`…）**全 404** —— 积分由客户端渲染层通过 IPC 问 Electron 主进程要，
    HTTP 细节在主进程 bundle 里，而结果被明文缓存在本机。
  - **uid 不匹配一律回答未知**：缓存属于本机登录的那一个账号，
    贴到别的号上会得到一个"看起来完全合理的错数"。
- **loomy 诊断端点** `GET /admin/loomy/client-store`（**Hidden**，无面板入口）：
  本机数据目录、读到的账号（脱敏）、两个账本、缓存是否今天的、以及读取失败原因。
  它同时是 `CapQuotaProbe` 的**契约要求**（`unverifiableCaps` 要求声明者实现
  `AdminExt`）—— 不是为过检查造的空壳，它回答的正是用户那句"探究一下怎回事"。
- **loomy `AccountColumnsExt`**：自报一套列，去掉两列不适用的
  （`token`：它的 `accessToken` 恒空；`checkin`：它没有签到端点），
  并把 `token_expiry` 的答案从 `—` 换成「永久」。
- **`loomy.client_data_dir` 配置**：留空 = 自动探测
  （`os.UserConfigDir()` 在 Windows/macOS/Linux 上都能拼对）。探测不到**不是错误**；
  此时按钮**不出现**（不放假按钮），启动日志会说明是哪一种情况。

### 变更

- **账号表首列统一为「上游」**：codearts 的列集补上 `provider`（用户明确要求）。
  - 原先那里是**刻意不含**的，理由是"分组后同组同名，冗余"。那条理由只看本组时成立，
    但它忽略了**同一列在不同上游的表里位置不同** —— workbuddy 走默认列集（首列就是
    「上游」），codearts 从「昵称」开头，两张表横向看过去对不齐。
  - 新增跨上游断言：两份列集的**首列必须相同**（只断言 codearts 自己的列集
    挡不住有人把 provider 从默认列集里删掉）。
- **`Caps()` 增加 `CapQuotaProbe`**（loomy 现在真的能主动拿到额度）。
- `loomy` 的启动日志新增一行客户端数据目录的结论（三种情况分别说明）。

### 修复 / 澄清（用户点名的那三格）

| 那格 | 探究结论 | 处置 |
|---|---|---|
| **额度** | 数据在，只是不在模型代理端点上 | 实现 `QuotaExt`（读本机缓存） |
| **Token 到期** | **它压根没有到期时间**（session 不绑时间、无 refresh token） | 如实渲染「永久」，不是补一个读数 |
| **今日签到** | **它没有签到动作** —— 日额度由服务端按自然日自动重置 | 从列集里去掉那一列 |

### 测试 / 守卫

- 新增 `internal/loomy/clientstore_test.go`（本机存储解析）、`login_test.go`、
  `quota_ext_test.go`、`accountview_test.go`、`adminroute_test.go`，
  `internal/admin/token_lifetime_test.go`，`internal/server/webui_token_lifetime_test.go`。
- **解析器的三条真实形状**各有断言：键值之间隔 `\x01` 等二进制（用 `\s*` 会一个都读不出来）、
  JSON 字符串里带 `}`（朴素正则会提前截断）、半截记录（leveldb 追加写的常态）。
- **积分摘要是部分更新**：最新的那条可能只有 `balance`，若"取最新一条直接解"
  会把 `dailyBalance` 覆盖成 **0** —— 而 0 的展示语义是"今日额度已用完"。
  叠加式折叠是正确行为，有专门断言。
- **凭证泄漏守卫**：诊断端点的回执按**字节**断言不含 session 的任何 8 字符连续片段，
  也不含未脱敏手机号（`Auth.Nickname` 在掩码缺失时会回落成完整号码）。
- **变异验证（8 条，全部实测变红）**：脚本 `mutate_verify_loomy_ui.ps1`。
  逐一改坏实现并断言对应测试变红 —— 包括"把 uid 校验反转"（额度贴错账号）、
  "把永久判据改成无条件"（顺序反了）、"让未知分支排到永久之前"（永久成死代码）、
  "把积分折叠退化成取最新一条"、"删掉 codearts 的 provider 首列"、
  "给一个假的授权 URL"、"把 session 原样回出去"、"把空 authURL 分支改成恒真"。
  - ⚠ 第一版脚本**自己 fail-open**：没切到模块根，每条 `go test` 都以
    "does not contain main module" 退出，而输出里没有 `FAIL` 字面量 →
    八条变异全被判成"仍然绿"。改成看**退出码**并区分"命令失败"与"断言失败"后才可信。

## v1.3.0 — 2026-09-14

> 本版主题：**新增第三个上游 Loomy**（讯飞桌面 AI 助手的模型服务）——
> 判据 1（"加一个上游 = 加一个目录 + 实现接口 + 配置加一段，核心零改动"）
> 的第二次实测，这次测的是**轻上游**形态。

### 新增

- **第三个上游：Loomy**（`internal/loomy/`）。`https://loomyad.xunfei.cn/api/v1`
  是标准 OpenAI 兼容端点，凭证就是客户端登录后写在 Local Storage 里的
  `session`（32 位 hex）。实测：12 个模型、流式 / function_calling / 视觉 / 推理全通，
  简单问答消耗 1 积分，日额度 5000 由服务端按自然日自动重置。
- **双轨鉴权的正反两向测试**。这个上游两个端点认不同的 Header
  （`GET /models` 用 `token:`，`POST /chat/completions` 用 `Authorization: Bearer`），
  而用错的那个返回 **HTTP 200** + `缺少 token` —— "看状态码判断鉴权"这条路不通。
  仓库把它写成两个各自命名的函数并配测试，改错任一方向都会红。
- **凭证两种形态都认**：直接抄客户端登录态，或用网关写盘的形态。
  UID 优先取 `userid`、其次文件里的 `uid`、最后按 session 派生（确定性）。
- **实时目录 + 静态快照兜底**：`GET /models` 拿得到就用实时的，
  拿不到回落内置快照 —— 保证目录不会因为一次抖动/session 失效而整片消失。
- **过滤上游已下架的模型**：`doubao-seedream-5-lite` / `qwen-image-3.0-pro`
  上游仍列在 `/models` 里但一用就 404「该模型暂未开放」，现在不进目录。
- **按模型裁剪超限的 `max_tokens` / `max_completion_tokens`**
  （最大的是 MiniMax-M3 的 512000），并保证 JSON 数字字面量不被改写成科学计数法。
- 配置段 `loomy.{enabled,auth_dir,base_url,pool_accounts}`；
  `auths/loomy/loomy-<uid>.json` 按上游分子目录。

### 说明（与 codearts 的关键区别）

- **没有 `CredentialRefresher`**：Loomy 的 session 无 TTL、无 refresh token
  （客户端重启 4 次未轮换、源码无续期判断），**没有可刷的东西**。
  实现一个空刷新只会让核心误判"这个号能自动恢复"。
- **session 失效 → 永久禁用**：`登录已失效，请重新登录` 只能人工重登。
  codearts 的同类错误映到 `ErrKindAuth`（换号不罚）是因为它**能**自动续期 ——
  两者不是同一件事，所以走的分类也不同。
- **刻意不实现的扩展点**：`AdminExt`（没有可用管理端点）、`JobExt`（无定时事务）、
  `LoginFlow`（登录在桌面客户端里）、`RefreshSkewExt`（无续期）、
  `SoftRateExt`（上游未给限流解除时刻）。有测试钉住这一点。
  > ⚠ **v1.4.0 订正**：`AdminExt` / `LoginFlow` 两条的理由共享一个错误前提 ——
  > 把"上游没有 HTTP 端点"当成了"没有数据"。实测证明登录态与积分摘要都明文缓存在
  > **本机 Local Storage**，于是这两条不成立、能力已补上（`JobExt` 那三条仍然成立）。
  > 详见 v1.4.0 一节与 `internal/loomy/clientstore.go` 的说明。
- 因此本包只有 ~200 行，而核心包（gateway/pool/admin/server/scheduler）**一行未改**；
  `arch_test.go` 自动把它纳入架构约束（实测发现结果 `[codearts loomy workbuddy]`）。

### 测试 / 守卫

- 契约测试（判据 2）跑**假上游**，无网络无凭证也全过程执行，CI 里不会跳过；
  假上游严格校验双轨鉴权，并有一段"负向对照"证明它真的在拦
  （否则正向断言是 fail-open）。
- 反向验证：把 `token:` 头改错 → `TestDualTrackAuth` 红；
  把分类改成"先看状态码" → `TestClassify` 在三条 200-带正文判据上红。
- `TestClassifyDoesNotBorrowOtherUpstreamsMarkers` 断言 loomy **不认**别家上游的
  特征串（workbuddy 的 `12153`、codearts 的 `insufficient quota`）——
  这正是 `gateway.ErrorClassifier` 要防的"用 A 的事实回答 B 的问题"。

### 修复（本轮顺带）

- **日志"已注册上游"打在最后一个上游注册之前**：loomy 启用时日志显示
  `已注册上游: [workbuddy]`，与事实不符 —— 一行"看起来权威"的日志会让人
  顺着它去查一个不存在的注册失败。已挪到所有注册之后。
- **测试夹具里混进了真实凭证**：Loomy 手册把作者的真实 session / 手机号 /
  userid 明文印了出来，第一版 fixture 直接照抄 —— 那等于把可用凭证提交进公开仓库
  （本仓库上一个提交正是"清掉两处真实 api_key 硬编码"）。已全部换成合成值，
  并把这条教训写进 fixture 的注释。

## v1.2.0 — 2026-09-14

> 本版主题：**移植并接通「客户端行为遥测伪造层」** —— 让网关能把一批上游
> 成长任务直接做完；同时补上**内容误报治理**（提示词体系 + 降级重试）
> 与出站身份的一致性。

### 新增

- **伪造客户端行为遥测层**：按官方桌面端 / 小程序 / Web 三个域构造并上报
  行为事件（`copilot.tencent.com` / `www.codebuddy.cn` / `www.workbuddy.cn`）。
  覆盖成长中心 17 项可自动化任务、开学季闭环、夜猫子补足。
  - **两类任务代价不同**：多数是**纯伪造**（只发事件链，无真实动作）；
    `expert_5` / `Expert_team_use_3` / `Expert_lighthouse` / `skill_1` /
    `Model_chat_GLM5.2` / `RichMeow_Chat` 必须**真发一次 chat** 拿上游签发的
    `requestId` —— 实测上游会校验专家 id 与 requestId，自造的**不计数**。
- **任务中心一键完成接上控制台**：任务明细行内「一键完成」、
  工具条「一键完成待办」（整账号）与「开学季一键完成」三个入口。
  按钮按后端能力表（`GET /admin/growth/auto/actions`）**动态出现**，
  不在前端写死；不在表里的任务（如需微信真实认证的 `Expert_Philanthropy`）
  仍显示「去客户端做」。
- **管理台新端点 7 条**（仅本机）：`/admin/growth/auto`、`/admin/growth/auto-all`、
  `/admin/growth/auto/actions`、`/admin/growth/scan`、`/admin/school`、
  `/admin/school/run`、`/admin/blackcat/run`。
- **系统提示词体系**：出站前用网关自有提示词**替换**客户端 system/developer
  （`prompt.mode=custom`，内置 2086 字节，可用 `prompt.file` 整体覆盖），
  或 `passthrough` 透传。被上游内容策略拦截时**不罚账号、不换号**，
  同请求内换极简中性提示词重试一次，并记忆到次日 00:00 CST。
- **出站身份一致性**：三段式 UA（`WorkBuddy/X WorkBuddy/X CLI/Y`）、
  设备令牌（`upstream.device_token` / `device_token_file`，5 分钟缓存）、
  客户端 IP 透传（`upstream.passthrough_ip`）、attribution 与 billing UA。
- **软限流（`code 6004`）按模型冷却**：解析上游重置时刻，指数退避、
  上限 `cooldown.soft_rate_max`（默认 2h），不牵连同账号的其它模型。
- **会话死亡计数**：同一账号连续 3 次会话失效才禁用，替代"一票否决"。
- **内容拦截成为一等错误类**（`ErrKindContentBlocked`）：与"账号故障"分开，
  避免把内容问题误报成账号池故障。
- **请求体硬上限**：`server.max_body_mb`（默认 8 MiB），超限返回 413。
- **DeepSeek thinking 注入**：按模型注入 `thinking` / `reasoning_effort=high`，
  并回填 `reasoning_content`。
- **对话活跃上报**（`schedule.activity_hours`，**空 = 关闭**）：按自然日计分，
  默认不发，避免存量部署升级后凭空产生上游请求。
- **领养前置修复**：`travel` 派猫前先补 `ensureAdoptPrereq`，解决恒失败。

### 修复

- **>8 MiB 请求体被静默截断后仍转发成功（HTTP 200）** → 改为
  `http.MaxBytesReader` + 413。原先调用方以为成功，实际上游收到的是残body。
- **`thinking` 未列入 `supportedFields`** → 注入被整体剥离，配置形同无效。
- **`sanitizeMessages` 对 `content` 键缺失的消息 `continue`**，整条消息
  （含 `tool_calls`）被跳过。
- **`resetDeviceTokenFileCache` 在持锁时替换整个结构体** →
  `fatal error: sync: unlock of unlocked mutex`（必崩）。
- **`6004` 正则会把 `"code":60040` 误判为软限流**（RE2 无环视，改用数字边界）。
- **`upstreamToGateway` 漏映射内容拦截** → 单上游部署下返回 503
  "无可用账号"，掩盖真实原因。
- **单账号部署的内容拦截降级重试无法重新选号**（唯一账号被 `tried` 挡住）。
- 任务中心四个缺陷（均为"不报错但用户会以为坏了"）：
  已领取的任务也长出按钮；`/admin/task` 完成摘要**恒报成功 0**；
  一键完成后**缓存快照不刷新**（跑完 186 秒界面仍显示原样待办）；
  toast 把奖励播报两遍。

### 测试 / 守卫

- 新增源码级守卫：`upstream_kind_mirror`（错误类镜像一致性 ——
  代码注释里声称存在的那条测试此前**并不存在**）、`maxbody`、`degrade`、
  `prompt_wire`、`desktop`、`school`、`autotask`、`taskslot_summary`、
  `webui_task_auto`、`autotask_ui`。
- 每条守卫都做了**反向验证**（把实现改回去必须变红），其中"已完成任务不得
  长按钮"用**单点回退真实文件**的方式，断言恰好只报 1 条并点名根因。

### 文档 / 工程

- README 新增：任务中心与 7 条端点表、提示词体系与降级语义、
  出站身份与设备令牌、软限流、内容拦截错误分类、对话活跃上报、
  领养前置说明、完整配置表与目录树。
- 修正 README 三处**与实现不符**的描述：内置提示词体积（1.6 KB →
  实测 2086 字节）、`passthrough` 的语义（降级期内**不再**透传）、
  降级重试的适用范围（**两种模式都生效**，不只 `passthrough`）。
- 仍然**零新增第三方依赖**（`go.mod` 只有 `go-redis`）。

## v1.1.0 — 2026-09-11

> 本版主题：**让成本可见 + 修一个统计口径的数据正确性 bug**。

### 新增

- **成本倍率**：从上游 `/v3/config` 拉取每个模型的官方积分倍率并展示
  （实测 29 个模型，最贵 `x5.00`、最便宜 `x0.00` —— 同样 token 量差 **17 倍**，
  此前完全不可见）。可用模型与对话下拉框都标上倍率，不再显示无用的 ctx 数字。
- **调用统计新增维度**：累计消耗积分、缓存命中率、缓存 token、推理 token。
  数据来自上游 usage 里此前被直接丢弃的字段（`credit` / 缓存命中未命中 / 推理 token）。
- **成长计划「到期」列**：上游部分任务带有效期（实测 18 个里 4 个），
  现按「远期（灰）/ 7 天内（黄）/ 已过期（红）」三档显示。
- **账号存活探测**：`GET /v2/plugin/accounts`。
- **额度告警接入故障诊断**：余额查询失败时附上上游的中文告警原因。

### 修复

- **调用统计被静默截断**（数据正确性）：统计取数走了分页接口，被单页上限夹到 300 条，
  一份 2000+ 行的日志只聚合了最近 300 条，**成功率 / 平均 TTFB 等全部失真**。
  改为不受分页上界约束的内部取全量，并新增 `aggregated` 字段自检窗口完整性。
- **模型目录无法自举**（功能完全失效）：状态判定与取数互相依赖形成自锁，
  冷启动时目录永远拉不起来，倍率恒为空。
- **写操作后就地刷新快照**：以前点一次操作要等 7 秒全量回源，现在读缓存（~1ms）。
- **界面操作反馈**：点按钮后立刻显示进行中状态（此前 6.9 秒页面完全静止，用户以为没反应）。
- **UTF-8 截断**：错误文案按字节截断会把中文切碎成乱码（`\ufffd`），改为按字符边界。
- 前端统计窗口文案的裸插值会让脏数据渲染成 `[object Object]`。
- 用户可见文案统一：成长计划的「信用分」改为「积分」。

### 性能

- 账号间探测并发化（上限 5）：3 账号强制刷新 **7.2s → 2.5s**。
- 单账号内 4 个上游调用并发化：探针 2402ms → 约 1600ms。
- 两层并发共享同一预算，避免"各限各的"相乘成 20 并发冲破上游风控。

### 文档 / 工程

- 设计文档：开源自审 — SECURITY.md、CI、徽章、README 配图（仪表盘 / 账号池 / 请求日志 / 设置）
- README 改为自托管 logo（`assets/logo.svg`），新增静态架构图（`assets/architecture.svg`）
- 新增 `start.bat`，修正 Windows 双击 `bin\wb2api-server.exe` 时工作目录错位
- `.github/workflows/ci.yml` —— PR 自动跑 `go mod tidy` / `build` / `vet` / `gofmt` / `test`
- `.github/workflows/release.yml` —— 打 tag 自动交叉编译 Windows/Linux 并挂 Release
- `.gitignore` 补 `logs/` 与 `tasks/`
- 隐私：移除 README 与测试注释里的真实账号标识（历史提交里仍有，见该提交说明）

### 历史遗留（未变）

- 依旧**零新增第三方依赖**：`go.mod` 只有 `go-redis` 一个直接依赖，
  产物是 `CGO_ENABLED=0` 的单文件静态二进制，下载即用。

## 历史里程碑（按时间倒序，节选）

> 完整提交历史见 `git log --oneline`。下面这些是**本仓库独有、有外部读者需要看**
> 的提交；其他纯内部重构、测试、注释类提交未列入。

### 2026-09 — 开源前准备

- `feat(ui): 设置面板组内复选框与输入框分离，不再交叉` —— 一个 fieldset 内部不再混排两类控件
- `fix(ui): 每页条数持久化到 localStorage，刷新页面不再回到 30` —— 顺手补了 TDZ 修复
- `feat(ui): 每页条数改为可手动输入，并在服务端夹住单页上限` —— 落盘上限 = 客户端上限 = 300
- `feat(ui): 调用统计的落盘占用只显示占用量；窗口文案只报条数`
- `feat(ui): 网关状态/调用统计/API 接入信息 合并为单一「仪表盘」面板`
- `feat(ui): 移除请求日志「实时」视图；成长计划刷新时一并更新额度`
- `feat(ui): 请求日志新增 tok/s 列；猫猫旅行说明自动派送的绑定关系`
- `feat(log): 统计聚合全量历史 + 前端统一排序/分页 + 旅行今日派送 + 移除调度面板`
- `feat(log): 统一分页 —— 任务历史与请求日志共用一个组件与一套语义`
- `fix(logbuf): 请求序号跨重启接续，消除落盘日志重复 seq`

### 更早

- `feat(admin): 成长计划领奖 + 本机客户端登录切换 + 设置页持久化` —— 管理台核心能力
- 初始从 `Sliverkiss/workbuddy2api` fork 并做本地化增量
