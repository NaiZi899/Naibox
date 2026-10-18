<p align="center">
  <img src="assets/icon/app.svg" alt="NaiBox" width="112" height="112">
</p>

<h1 align="center">NaiBox</h1>

<p align="center">
  <b>把 12 个 AI 上游聚合成一个 OpenAI 兼容 API 的多账号网关</b><br>
  统一鉴权 · 账号池轮转 · 熔断与冷却 · 会话粘性 · 定时签到与续期 · 桌面版与控制台
</p>

> **关于名字**：界面与可执行文件的产品名是 **NaiBox**；仓库名、Go 模块路径、
> `/healthz` 的 `service` 字段仍是 `omni2api`——模块路径改名会打断所有既有部署的
> import 与数据目录约定，而 `service` 是桌面版判断"端口上那个是不是自己"的协议标识。

## 这是什么

**NaiBox 是一个多上游聚合反代网关**：把多个上游账号聚合成一个 OpenAI 兼容的
`base_url` + `api_key`。你的应用只认 OpenAI 协议，网关负责背后的上游选号、额度、
签到、续期、冷却与排障。

```
你的应用 ──► NaiBox ──┬──► WorkBuddy          腾讯 CodeBuddy 桌面端（默认上游）
                       ├──► WorkBuddy AI       海外版（www.workbuddy.ai）
                       ├──► CodeArts           华为云 CodeArts
                       ├──► Loomy              讯飞 Loomy 桌面端
                       ├──► Cline              Cline 桌面端
                       ├──► TRAE               字节 TRAE
                       ├──► Raccoon Work       商汤小浣熊
                       ├──► LobsterAI          有道龙虾
                       ├──► Qoder              阿里 Qoder
                       ├──► Qoder（中国版）
                       ├──► InkStone           书生·端砚 TokenPlan
                       └──► Jiyuan             基元律动（TokenRhythm）
```

"加一个上游 = 加一个目录 + 实现一组接口 + 配置加一段，**核心零改动**"——这条判据
由 `internal/gateway/arch_test.go` 的**可执行断言**守着（不是约定）：

- 核心包不得依赖任何具体上游
- 上游之间不得互相依赖
- 装配层（`internal/boot`）允许依赖上游，但**不得被上游反向依赖**
- 判据本身用 `go list -deps` 的包级依赖图，不是源码文本匹配
  （文本判据被 `import` 别名绕过过两次，这条写在测试注释里）

从 4 个上游扩到 12 个的过程中，核心包**一行没改**。

## 两种形态，同一套装配

| | 服务端 | 桌面版（Windows） |
|---|---|---|
| 入口 | `cmd/server` | `cmd/desktop` |
| 装配 | `internal/boot`（两者共用） | 同左 |
| 界面 | 浏览器打开 `127.0.0.1:7863/ui` | 原生窗口（WebView2）+ 托盘 |
| 数据 | 当前工作目录下的 `config.json` / `auths` / `data` | `%APPDATA%\omni2api\` |
| 典型场景 | 服务器 / Docker / 长期在线 | 个人电脑，双击即用 |

桌面版**不是**另一套实现：它复用同一个 `boot.Run`，窗口只是 `Navigate` 到
`127.0.0.1:<port>/ui`。所以两者的账号池、配置、上游行为、HTTP 契约完全一致。

## 🖥 桌面版

**数据与程序分离**：数据根固定在 `%APPDATA%\omni2api\`，程序目录可以随便删、
随便换版本。卸载/覆盖安装不影响账号。

| 项 | 行为 |
|---|---|
| 首次启动 | 生成 `config.json`（随机 `api_key`）、`auths\`、`data\`、`logs\` |
| 旧版升级 | 自动发现 exe 同级或当前目录的旧 `config.json`，**复制**（不移动）到新数据根；源目录保留并写 `.migrated.bak`，重复启动幂等 |
| 端口 | 7863 起自动顺延到 7873，仍占用则请系统分配；实际地址写在 `runtime.json` |
| 单实例 | 命名互斥体 + `runtime.json` + `/healthz` 三重校验，双击第二次只唤出已有窗口 |
| 关窗行为 | 托盘菜单可设「询问 / 最小化到托盘 / 直接退出」。**最小化 = 隐藏窗口、进程与网关继续在后台运行**（这就是保活） |
| 托盘菜单 | 打开控制台 / 在浏览器中打开 / 打开数据目录 / 查看今日日志 / 备份数据 / 开机自启 / 重启服务 / 退出 |
| 兜底 | 系统缺 WebView2 Runtime 时自动回落「托盘 + 默认浏览器」，不会白屏 |

几个刻意为之的细节：

- **关窗不直接退出**是默认行为（首次会问一次），因为网关是"一直在跑"的东西，
  点 X 就把账号池停掉是反直觉的。系统关机（`WM_QUERYENDSESSION`）永远放行——
  拦关机会被强杀，`state.json` 来不及落盘。
- **托盘图标必须给 ICO 内容，不能给 PNG**。`energye/systray` 在 Windows 上走
  `LoadImageW`，它只按 ICO/CUR 内容解析；喂 PNG 的结果是内部打一行
  `systray error: unable to set icon` 就返回——不 panic、不报错、托盘直接没图标。
- **窗口图标 ≠ 文件图标**。资源管理器读 exe 的 PE 资源，标题栏/任务栏/Alt-Tab 读
  窗口图标（`WM_SETICON`）。两者都要设，否则标题栏是 Windows 的默认空白图标。
- **桌面形态下 `listen` 的空 host 会收敛到 `127.0.0.1`**：`":7863"` 在 Go 里等于
  监听所有网卡，而 `/ui` 对回环来源会自动注入 `api_key`——绑通配等于同网段任何人
  都能打开控制台。服务端形态不受影响（Docker 端口映射依赖通配）。

## 🐾 已接入上游

| 上游 | 账号形态 | 控制台能力 |
|---|---|---|
| **WorkBuddy**（默认） | OAuth 设备码登录 | 签到 / 保活 / 成长计划 / 猫猫旅行 / 任务一键完成 |
| **WorkBuddy AI**（海外版） | OAuth 设备码登录（www.workbuddy.ai） | 对话 / 模型 / 额度查询（海外版无签到/成长/旅行，显式禁用）。按凭证的 `channel` 字段自动选域，国内与海外账号可同池共存 |
| **CodeArts** | OAuth + DPoP（约 2 小时 STS，自动续期） | 签到（福利领取）/ 额度探测 |
| **Loomy** | `session`（无 TTL） | 手机号验证码登录 / 新手任务一键完成 / 邀请码绑定 / 批量粘贴导入 / 额度实时查询 / **生图 + 联网搜索** |
| **Cline** | WorkOS 设备码轮询 | 余额查询 / 后台续期（不起本地端口，服务器部署天然可用） |
| **TRAE** | JWT + 消费型 refreshToken | 页内添加账号（浏览器授权）/ 每日自动签到 / token 自动续期 / 权益包额度 |
| **Raccoon Work** | 浏览器授权（微信扫码 / 手机号） | 积分余额 / 登录奖励 / onboarding 状态 |
| **LobsterAI** | 浏览器 OAuth 回跳 | 三步式签到领取积分 / 定时自动签到 |
| **Qoder** 与 **Qoder 中国版** | PKCE 设备码 | 积分余额 / 每日签到 / WASM 加密推理桥。**是两个独立 id**（`qoder` / `qodercn`），下表因此只有 11 行但实际 12 个上游 |
| **InkStone**（书生·端砚 TokenPlan） | API Key（`sk-…`，默认 6 个月） | 模型目录 / 额度（墨点）查询。Key 只能在 SSO 后的 TokenPlan 页创建，网关拿不到，故只有**批量导入**与「重载 auths」 |
| **Jiyuan**（基元律动 / TokenRhythm） | API Key（`sk-…`） | 模型目录（实时拉取，失败回落内置静态表）/ 批量导入（`sk_` 与 `sess_` 两种形态）/ 实时余额 |

> ⚠ `inkstone` 与 `jiyuan` 的配置段**缺省即启用**（与其它的 opt-in 相反）。
> 理由：它们加进代码时没同步进示例配置，而"段缺席 = 不加载"会让存量部署
> 永远看不到这两个上游。它们的 Caps 只有 `chat|models|import`、没有签到/保活，
> 所以"默认启用但没凭证"不产生任何周期请求，只是界面多一个空分组。
> 想关掉就显式写 `"enabled": false`。

## 🚀 快速开始

### A. 下载预编译二进制

到 [Releases](../../releases) 下载（用 `SHA256SUMS.txt` 校验）：

**Windows 桌面版**（推荐个人使用）

```
NaiBox-windows-amd64.zip     →  NaiBox.exe + config.example.json + README.txt
```

解压双击即可，数据在 `%APPDATA%\omni2api\`。

**服务端**（Linux / macOS / 需要对外提供服务时）

```
omni2api-server-windows-amd64.zip
omni2api-server-linux-amd64.tar.gz      omni2api-server-linux-arm64.tar.gz
omni2api-server-darwin-amd64.tar.gz     omni2api-server-darwin-arm64.tar.gz
```

全部产物由 CI 构建（本机不手工上传），命名即上表。

```bash
# Windows 服务端
unzip omni2api-server-windows-amd64.zip
cp config.example.json config.json     # 编辑：开上游、填 api_key
./omni2api-server -config config.json
```

### B. 从源码构建

```bash
go build -o omni2api-server ./cmd/server    # Go ≥ 1.22（CI 用 1.22）
./omni2api-server -config config.json
```

### C. 构建桌面版

```powershell
pwsh scripts/build-desktop.ps1     # 生成图标（纯 Go）→ 生成 rsrc.syso → 构建 -H windowsgui
```

不要直接 `go build ./cmd/desktop`——那样缺图标与 DPI manifest（链接器只收集
参与构建的包目录里的 `.syso`，而它是构建期派生物、不入库）。

## 添加账号

控制台「账号池」分组行点「＋ 添加账号」，各上游走自己的流程：

- **WorkBuddy / CodeArts / TRAE / LobsterAI / Qoder / Raccoon**：点「添加账号」后
  由网关用无痕窗口自动打开授权页（授权页里各有扫码 / 设备码 / 手机号）。
  窗口被关掉时可点「复制链接」粘贴到无痕/隐私窗口。Raccoon 还额外把防串号标识
  放进 `redirect` 的 query（`nonce`）而不是 `state`。
- **Cline**：WorkOS 设备码轮询，不起本地端口——服务器部署也能用。
- **Loomy**：输手机号 + 验证码登录；或「批量导入」粘贴 JSON；或把凭证放进
  `auths/loomy/loomy-<uid>.json` 后点「重载 auths」。
- **InkStone / Jiyuan**：上游不提供可自动化的页内登录，只能**批量导入** Key
  （jiyuan 还支持 `sess_` 登录凭证，用于查实时余额）。

> **关于无痕窗口**：默认给浏览器一份独立 profile（临时目录），本次授权从零 cookie
> 开始——避免浏览器里已登录的账号把会话串到别的账号上。代价是需重新输入账号密码。

## 🖥 管理控制台

访问 `http://127.0.0.1:7863/ui`（本机访问自动注入 Key，顶栏可复制 curl 示例）。

- **仪表盘**：网关状态、调用统计（请求数 / 成功率 / token / TTFB）、
  趋势图（可切「近 24 小时 / 近 7 天 / 自定义区间」）、API 接入信息。
- **账号池**：每个上游一张表（列集由上游自报），行内动作按上游能力渲染；
  顶部可按上游**显示过滤**（见下）。
- **请求日志**：历史分页 + **实时推送**（SSE 长连接，新请求立刻出现，
  延迟从 10s 降到亚秒级；断线自动重连并带上最后一条序号续传）。
- **设置**：调度、上游显示过滤、超时、限流与熔断、日志保留等。

两个容易误解的地方：

- **「账号池显示过滤」只影响显示**。勾选的上游不出现在账号池与左侧导航，
  但它的账号仍在池里参与选号、签到、保活，模型也仍可调用。
  想真正停用，改 `config.json` 里它的 `enabled`。
- **`/healthz` 在 0 账号时返回 503**（不是 200）。这是有意的：它同时是桌面版
  的"就绪判据"，所以**不要**把 200 当成本服务已就绪。

### 生图与对话工具

上游自报的工具（Loomy 的生图 / 联网搜索）由网关注入并在网关侧执行，
**agent 框架零配置可用**：

| 模型调谁 | 谁执行 | agent 框架看到什么 |
|---|---|---|
| 客户端自己的（`bash` / `read` / …） | 客户端 | 正常的 `tool_calls` |
| 网关注入的（`generate_image` / `web_search`） | 网关 | 只见模型"直接回了张带图的 markdown" |

关掉注入：`server.disables_chat_tools: true`（只影响注入，客户端自带 `tools` 不受影响）。

生图两种用法：

```bash
# ① 对话里说一句（网关自动调生图模型）
curl http://127.0.0.1:7863/v1/chat/completions \
  -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
  -d '{"model":"loomy/deepseek-v4-flash-0731","messages":[{"role":"user","content":"画一张：赛博朋克城市夜景"}]}'

# ② 直接调 OpenAI 兼容生图端点
curl http://127.0.0.1:7863/v1/images/generations \
  -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
  -d '{"model":"loomy/doubao-seedream-5-lite","prompt":"赛博朋克城市夜景","n":1,"size":"1024x1024"}'
```

返回的图片是**裸 URL**（已剥掉 COS 签名）。这不是偷懒：签名 URL 对 HEAD 请求
必然 403（COS 把 method 也算进签名），而预览器/取图器通常先发 HEAD 探测，
一探测就失败。原始签名 URL 保留在同级 `url_signed` 字段里作兜底。

## ⚙️ 配置说明

全部配置在 `config.json`（参考 `config.example.json`）。

| 项 | 默认 | 说明 |
|---|---|---|
| `listen` | `:7863` | 监听地址。**桌面版**把空 host 收敛到 `127.0.0.1`；服务端按配置原样绑定（Docker 需要通配）。显式写 `0.0.0.0` 时两者都尊重 |
| `api_key` | — | 调用方 Bearer 鉴权；为空则不鉴权（不建议） |
| `service_name` | `omni2api` | `/healthz` 与 `/status` 的协议标识（不改名，见开头说明） |
| `auth_dir` | `./auths` | 凭证根目录，各上游分子目录 |
| `state_file` | `./data/state.json` | 账号池状态（额度/冷却/禁用/成功率） |
| `ui.hidden_providers` | `[]` | 账号池与导航里**不显示**的上游 id（展示过滤，不是停用） |
| `server.max_body_mb` | `8` | 请求体上限 |
| `server.disables_chat_tools` | `false` | 关闭网关注入的对话工具 |
| `login.open_browser` / `isolated` / `browser` | `true` / `true` / `""` | 添加账号时的浏览器行为 |
| `pool.*` | — | 在途上限、熔断阈值与退避、闲置权重、主动健康检查间隔 |
| `schedule.*` | — | 签到 / 保活 / 活跃上报（统一 30 分钟被动扫描） |
| `session_sticky.*` | `enabled=true` `ttl=30m` | 同一会话固定同一账号 |
| `upstream.*` | 内嵌默认 | 出站身份（UA / 客户端版本 / 设备 token / IP 透传）与超时 |
| `prompt.*` | 内嵌默认 | 系统提示词（`custom` / `passthrough`） |
| `upstash.*` | — | 可选 Redis/Upstash 后端；不配则纯内存 |

各上游都有自己的段（都支持 `enabled` / `auth_dir`，多数还有 `pool_accounts`）：

`workbuddy` · `workbuddy_intl` · `codearts` · `loomy` · `trae` · `cline` ·
`raccoon` · `lobsterai` · `qoder` · `inkstone` · `jiyuan`

## 🔌 API

```bash
curl http://127.0.0.1:7863/v1/chat/completions \
  -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
  -d '{"model":"loomy/deepseek-v4-flash-0731","messages":[{"role":"user","content":"hi"}],"stream":true}'
```

- 模型名带上游前缀：`loomy/xxx`、`workbuddy/xxx`…；裸模型名走默认上游。
- `GET /v1/models`：各上游目录合并（含生图模型，带 `capabilities` 标注）。
- `POST /v1/images/generations`：OpenAI 兼容生图。
- `GET /healthz`：**无鉴权**，负载均衡友好（0 账号返回 503）。
- `GET /status`：需鉴权，带详情。
- 管理 API：`/admin/*`（含 `/admin/logs/stream` 实时日志 SSE）。

`/v1/models` 每条记录的字段：

```json
{"id":"workbuddy/deepseek-v4-pro","object":"model","owned_by":"workbuddy",
 "context_length":1000000,"max_output_tokens":128000,"supports_images":true}
```

`supports_images`（能不能把图**发进去**）与 `capabilities:["image_generation"]`
（能不能让上游**生图出来**）是**两个轴**，不是一回事。前者是**逐上游**的事实，
不由模型名推断——同一个 `glm-5.2` 在 `workbuddy` 上可能是 `true`、在 `trae` 上 `false`。

## 🛡 安全

- 默认只绑本机；对外暴露请自行加反向代理与更严格的鉴权。
- 凭证按上游分目录存于 `auths/`（已 gitignore）；控制台昵称自动打码。
- `auths/`、`data/`、`config.json`、`logs/`、`.build/`、`dist/` 全部不入库。
- 发布产物未做代码签名，双击时 SmartScreen 会提示"未知发布者"——这是未签名
  exe 的必然现象，不是中毒。校验请用 `SHA256SUMS.txt`。
- 本项目面向**自有账号**的自动化管理与协议理解；请遵守各上游的服务条款与额度限制。

## 🛠 开发

```bash
go build ./... && go vet ./...        # 构建 + 静态检查
go test ./... -count=1                # 全量测试（含假上游契约测试）
gofmt -l .                            # 格式检查
```

> ⚠ Windows 上 `gofmt -l .` 会误报（CRLF 被当成格式问题）。本地判真实情况要先做
> LF 归一化；CI（Linux）不受影响。`.gitattributes` 已对 `webui.html` / `favicon.svg` /
> 内嵌的 `.md` 关掉换行转换——它们是被 `//go:embed` 逐字节内嵌的数据，
> 两个平台的字节数不同会让产物不同构。

- **架构**：`internal/gateway` 是唯一接缝（`Provider` 4 方法 + 若干可选扩展点：
  登录流程 / 凭证加载 / 额度探测 / 每日动作 / 列集自报 / 凭证寿命 / 后台任务 /
  生图 / 对话工具 / 诊断端点…）。`internal/boot` 是装配层，`cmd/server` 与
  `cmd/desktop` 共用它。
- **图标**：仓库里没有 `.ico` / `.png`。图形由 `internal/icon`（纯标准库）用代码画，
  构建期 `tools/genicon` 生成 6 帧 ICO，运行时托盘调同一个包——
  `assets/icon/app.svg` 是设计规格，`internal/server/favicon.svg` 是同一套几何的站点图标。
- **CI**（`.github/workflows/ci.yml`）：`go mod tidy` 检查 / build / vet / gofmt /
  全量测试，五项门禁。
- **发布**（`.github/workflows/release.yml`）：打 `v*` 标签自动构建五平台服务端 +
  Windows 桌面版，生成 `SHA256SUMS.txt` 挂到 Release。

## 🤝 贡献

提 PR 前跑一遍本地质量门禁（与 CI 同一组命令）：

```bash
go mod tidy && git diff --exit-code go.mod go.sum
go build ./... && go vet ./...
gofmt -l .
go test ./... -count=1
```

约定：

- **新上游不改核心**：只加一个 `internal/<name>/` 包 + 一段配置 +
  `internal/boot/boot.go` 里一行 `registry.Register(...)`；
  `internal/gateway/arch_test.go` 会验证包依赖方向没被破坏。
- **新增上游时顺手改三处示例**：`config.example.json`、`internal/desktop/defaultconfig.example.json`、
  本 README 的上游表格。漏掉前两处的表现是"代码有、成品里没有"——
  有 `internal/desktop/config_drift_test.go` 守着。
- **提交信息**：`<type>(<scope>): <摘要>`，type 取 `feat` / `fix` / `docs` /
  `refactor` / `test` / `chore`。
- **不要把真实凭据提交进仓库**：测试里的 uid / 手机号 / token / API Key 一律占位值。
- 行为变更请同步 `CHANGELOG.md`。

安全问题见 [SECURITY.md](SECURITY.md)。

## 🧬 来源与二创

本项目是**开源项目基础上的二次开发**：

```
Sliverkiss/workbuddy2api  ──►  Practice019/multi2api  ──►  NaiZi899/omni2api (NaiBox)
    WorkBuddy 专用反代            多上游聚合网关（10 个）        12 个上游 + 桌面版
```

三层的 MIT 版权声明都保留在 [LICENSE](LICENSE) 里。

相对基线的增量（详见 [CHANGELOG.md](CHANGELOG.md)）：

- **桌面版**：Windows 原生窗口 + 托盘 + 数据目录分离 + 旧布局迁移 + 关窗行为
- **装配层抽取**：`cmd/server` 与 `cmd/desktop` 共用 `internal/boot`
- **控制台**：实时日志 SSE、自定义时间区间、上游显示过滤、视觉令牌层与七态
- **图标代码化**：仓库零二进制媒体，图形由代码画、构建期派生
- **两个上游**：InkStone（书生·端砚）与 Jiyuan（基元律动）

## 📄 许可证

[MIT](LICENSE)。二创自 [Practice019/multi2api](https://github.com/Practice019/multi2api)
（同为 MIT，本身 fork 自 [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api)），
沿用该许可证。

## 免责声明

本项目仅用于对自有账号 / 自有软件协议的理解与自动化，请遵守各相关平台的服务条款。
