<p align="center">
  <img src="assets/logo.svg" alt="omni2api" width="120" height="120">
</p>

<h1 align="center">omni2api</h1>

<p align="center">
  <sub>开源二创项目 · 基线仓库 <a href="https://github.com/Practice019/multi2api">Practice019/multi2api</a>（MIT）</sub>
</p>

<p align="center">
  <b>把 12 个 AI 上游聚合成 OpenAI 兼容 API 的多账号网关</b><br>
  统一鉴权 · 账号池轮转 · 熔断与冷却 · 会话粘性 · 定时签到与续期 · 管理控制台
</p>

## 这是什么

**omni2api 是一个多上游聚合反代网关**：把多个上游账号聚合成一个 OpenAI 兼容的
`base_url` + `api_key`。你的应用只认 OpenAI 协议，网关负责背后的上游选号、额度、
签到、续期、冷却与排障。

```
你的应用 ──► omni2api ──┬──► WorkBuddy          腾讯 CodeBuddy 桌面端（默认上游）
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

> ⚠ **它不再是 WorkBuddy 专用**。架构上"加一个上游 = 加一个目录 + 实现一组接口 +
> 配置加一段，**核心零改动**"——这是本项目最硬的判据，由
> `internal/gateway/arch_test.go` 的**可执行断言**守着（不是约定）：
>
> - 核心包不得依赖任何具体上游
> - 上游之间不得互相依赖
> - 新增上游只需在 `cmd/server/main.go` 加一行 `registry.Register(...)`
>
> 从 4 个上游扩到 12 个的过程中，核心包**一行没改** —— 这条判据是实践过的，不是口号。

## ✨ 核心能力

| 能力 | 说明 |
|---|---|
| 🔌 **OpenAI 兼容** | `POST /v1/chat/completions`（流式/非流式）、`POST /v1/images/generations`（**生图**）、`GET /v1/models`、`GET /healthz`；任何 OpenAI SDK 直接可用 |
| 🧠 **统一账号池** | 多上游账号混在一个池：健康/冷却/熔断/禁用/在途状态机，按权重自动选号，坏号自动摘除 |
| 🎨 **生图** | 生图端点 + **对话工具自动注入**：拿任一对话题一句"画一张…"就自动调生图模型。返回**可预览的裸 URL**（见下方说明） |
| 🛠️ **对话工具注入** | 上游自报工具（Loomy 的生图 / 联网搜索）。**agent 框架零配置可用** —— 客户端自带的工具原样透传，网关注入的自己执行 |
| 🔄 **自动调度** | 定时签到、保活、旅行领奖、额度刷新、凭证续期——全部由各上游**自报**的清单生成，加新上游核心零改动 |
| ❤️‍🩹 **自愈机制** | **主动健康检查**（每 10 分钟探测冷却/熔断中的账号并提前恢复）、熔断错误按时间窗衰减（零散错误不攒坏账号）、凭证续期连续失败自动禁用 |
| 🛡️ **错误治理** | 硬冷却（额度耗尽）/软限流/会话失效/内容拦截分类处置：该换号换号、该禁用禁用。上游的**业务失败是 HTTP 200 + body 里的业务码**的，也按业务码处置 |
| 🖥️ **管理控制台** | `/ui` 单页：仪表盘 / 账号池 / 模型目录 / 对话测试 / 请求日志 / 设置 / 上游专属标签页 |
| 🔑 **安全默认** | `api_key` 鉴权、密钥只在本机注入、昵称自动打码、凭证按上游分子目录存放 |

## 📸 界面预览

控制台截图**不随仓库分发** —— 界面上难免出现真实账号的 uid 与手机号尾号，
脱敏图维护成本又高于它的价值。自己跑起来看最直接：

```bash
go build -o omni2api-server ./cmd/server
./omni2api-server -config config.json
# 浏览器打开 http://127.0.0.1:7863/ui
```

单页控制台包含：仪表盘 / 账号池 / 模型目录 / 对话测试 / 请求日志 / 设置，
以及各上游自报的专属标签页（如 Loomy 的新手任务与邀请码）。
外观示意见 `assets/architecture.svg`（架构图，非截图）。

## 🐾 已接入上游

| 上游 | 账号形态 | 控制台能力 |
|---|---|---|
| **WorkBuddy**（默认） | OAuth 设备码登录 | 签到 / 保活 / 成长计划 / 猫猫旅行 / 任务一键完成 |
| **WorkBuddy AI**（海外版） | OAuth 设备码登录（www.workbuddy.ai） | 对话 / 模型 / 额度查询（**海外版无签到/成长/旅行**，显式禁用）。按凭证的 `channel` 字段自动选域，国内与海外账号可同池共存 |
| **CodeArts** | OAuth + DPoP（约 2 小时 STS，自动续期） | 签到（福利领取）/ 额度探测 |
| **Loomy** | `session`（无 TTL） | 手机号验证码登录 / 新手任务一键完成 / 邀请码绑定 / 批量粘贴导入 / 额度实时查询 / **生图 + 联网搜索** |
| **Cline** | WorkOS 设备码轮询 | 余额查询 / 后台续期（**不起本地端口**，服务器部署天然可用） |
| **TRAE** | JWT + 消费型 refreshToken | 页内添加账号（浏览器授权） / 每日自动签到 / token 自动续期 / 权益包额度 |
| **Raccoon Work** | **浏览器授权（微信扫码 / 手机号）** | 积分余额 / 登录奖励 / onboarding 状态 |
| **LobsterAI** | 浏览器 OAuth 回跳 | 三步式签到领取积分 / 定时自动签到 |
| **Qoder / Qoder 中国版** | PKCE 设备码 | 积分余额 / 每日签到 / WASM 加密推理桥 |
| **InkStone**（书生·端砚 TokenPlan） | API Key（`sk-…`，默认 6 个月，无 refresh） | 模型目录 / 额度（墨点）查询。Key 只能在 SSO 后的 TokenPlan 页创建、网关拿不到，故只提供**批量导入** |
| **Jiyuan**（基元律动 / TokenRhythm） | API Key（`sk-…`） | 模型目录（实时拉取，失败回落内置静态表）/ 批量导入。上游不提供页内登录，同样只走批量导入 |

## 🚀 快速开始

### A. 下载预编译二进制

到 [Releases](../../releases) 下载对应平台包（用 `SHA256SUMS.txt` 校验）：

- Windows：`omni2api-server-windows-amd64.zip`（exe + `config.example.json` + `start.bat`）
- Linux：`omni2api-server-linux-amd64.tar.gz` / `omni2api-server-linux-arm64.tar.gz`
- macOS：`omni2api-server-darwin-amd64.tar.gz` / `omni2api-server-darwin-arm64.tar.gz`

**全部五个平台由 CI 构建**（本机不手工上传产物），命名统一为
`omni2api-server-<goos>-<goarch>.<zip|tar.gz>`。

```bash
# Windows
unzip omni2api-server-windows-amd64.zip
cp config.example.json config.json     # 编辑：开上游、填 api_key
./omni2api-server -config config.json
```

### B. 从源码构建

```bash
go build -o omni2api-server ./cmd/server    # Go ≥ 1.22（CI 用 1.22.5）
./omni2api-server -config config.json
```

### 添加账号

控制台「账号池」分组行点「＋ 添加账号」，各上游走自己的流程：

- **WorkBuddy / CodeArts / TRAE / LobsterAI / Qoder / Raccoon**：点「添加账号」后
  **由网关用无痕窗口自动打开授权页**（见下方 `login.*`）。
  授权页里有各自的登录方式（扫码 / 设备码 / 手机号，取决于上游）。
  - **Raccoon**：打开的是**官方登录页**，页内有**微信扫码**与**手机号短信**两个 Tab。
    过完滑块点授权后会回调到本机端口，网关当场换取凭证。
  - 窗口被关掉时可点「重新无痕打开」，或点「复制链接」粘贴到无痕/隐私窗口。
- **Cline**：WorkOS 设备码轮询，**不起本地端口** —— 服务器部署也能用。
- **Loomy**：点「＋ 添加账号」可**输手机号 + 验证码登录**；或点「批量导入」直接粘贴
  JSON（`[{"phone":"...","userid":"...","session":"..."}]`，支持多条）；或把凭证放进
  `auths/loomy/loomy-<uid>.json` 后点「重载 auths」。

> ⚠ **关于无痕窗口**：默认给浏览器一份**独立 profile**（临时目录），
> 这次授权从零 cookie 开始 —— 避免浏览器里已登录的账号把会话串到别的账号上。
> 代价是需重新输入账号密码。

### 生图怎么用

两种方式，任选：

```bash
# ① 对话里说一句（网关自动调生图模型）
curl http://127.0.0.1:7863/v1/chat/completions \
  -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
  -d '{"model":"loomy/deepseek-v4-flash-0731","messages":[{"role":"user","content":"画一张：赛博朋克城市夜景"}]}'

# ② 直接调生图端点（OpenAI 兼容）
curl http://127.0.0.1:7863/v1/images/generations \
  -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
  -d '{"model":"loomy/doubao-seedream-5-lite","prompt":"赛博朋克城市夜景","n":1,"size":"1024x1024"}'
```

生图模型会出现在 `GET /v1/models` 里，带 `capabilities: ["image_generation"]` ——
调用方据此知道该用 `/v1/images/generations` 而不是 `/chat/completions`。

> ⚠ **返回的图片 URL 是"裸 URL"**（已剥掉 COS 签名）。这不是偷懒：上游给的是签名
> URL，而它对 **HEAD 请求必然 403**（COS 把 HTTP method 也算进签名，URL 是按 GET 签的），
> 而预览器 / 取图器 / 文件发送队列**通常先发 HEAD** 探测 —— 一探测就失败，
> 表现为"图片无法预览 / 拿不到文件实体"。
>
> 原始签名 URL 保留在同级的 `url_signed` 字段里作兜底（bucket 若改成私有读，
> 会自动退回它）。

### `agent` 框架里用（零配置）

网关把上游工具**注入**给模型，并在网关侧执行 —— 所以 **agent 框架（DSH /
Claude Code / Cursor 等）不需要在配置里声明任何工具**：

| 模型调谁 | 谁执行 | agent 框架看到什么 |
|---|---|---|
| 客户端自己的（`bash` / `read` / …） | **客户端** | 正常的 `tool_calls` |
| 网关注入的（`generate_image` / `web_search`） | **网关** | 只见模型"直接回了张带图的 markdown" |

即两边**按工具名分工**，互不打架。想关掉注入：`server.disables_chat_tools: true`
（⚠ 只影响**注入**；客户端自己带 `tools` 时本就按名分工，与此开关无关）。

## ⚙️ 配置说明

全部配置在 `config.json`（参考 `config.example.json`）。核心项：

| 项 | 默认 | 说明 |
|---|---|---|
| `listen` | `127.0.0.1:7863` | 监听地址（默认只绑本机） |
| `api_key` | — | 调用方 Bearer 鉴权；为空则不鉴权（不建议） |
| `auth_dir` | `./auths` | 凭证根目录，各上游分子目录存放 |
| `server.*` | — | `max_body_mb`（请求体上限）、`disables_chat_tools`（关工具注入） |
| `pool.*` | — | 账号池：熔断阈值、在途上限、冷却时长、`health_check_interval_seconds` |
| `schedule.*` | — | 签到 / 保活的间隔与开关（**统一 30 分钟被动扫描**） |
| `session_sticky.*` | `enabled=true` / `ttl=30m` / `gc_interval=5m` | 会话粘性：同一会话固定用同一账号 |
| `upstream.*` | 内嵌默认 | 出站身份（UA / 客户端版本 / 设备 token / IP 透传） |
| `prompt.*` | 内嵌默认 | 系统提示词（`mode` = `custom` / `passthrough`，空值按 `custom`；`file` 可指向自定义文件） |
| `upstash.*` | — | 可选 Redis/Upstash 后端；不配则纯内存 |

各上游的配置段（都支持 `enabled` / `auth_dir`，其余见下）：

`workbuddy` · `workbuddy_intl` · `codearts` · `loomy` · `trae` · `cline` ·
`raccoon` · `lobsterai` · `qoder`

**「添加账号」的浏览器行为（`login.*`）**：

| 项 | 默认 | 说明 |
|---|---|---|
| `login.open_browser` | `true` | 点「＋ 添加账号」时由网关自动用**无痕窗口**打开授权页；`false` = 只回授权链接 |
| `login.isolated` | `true` | 给浏览器一份**独立 profile**（临时目录，48h 后自动清理）：这次授权从零 cookie 开始，代价是需重新输入账号密码；`false` = 复用当前 profile，仅开无痕窗口 |
| `login.browser` | `""`（自动探测） | 显式指定浏览器可执行文件路径；探测顺序 Edge → Chrome → Brave → Chromium（macOS 走 `open -n -a`） |

本机没有任何受支持的浏览器、或运行在无 GUI 环境时，启动日志会说明原因，
「添加账号」自动退回「复制链接 → 粘贴到无痕窗口」的老路径（功能不受影响）。

**Loomy 特有配置段**：

| 项 | 默认 | 说明 |
|---|---|---|
| `loomy.enabled` | `false` | 显式启用（缺省不启用，向后兼容） |
| `loomy.client_data_dir` | `""`（自动探测） | 本机 Loomy 客户端 Local Storage 目录（决定本机拾取/额度回退） |
| `loomy.login_mode` | `auto` | `auto` 先本机拾取 / `sms` 只走手机号验证码 / `local` 只本机拾取 |
| `loomy.sms_*` | 内嵌默认 | 讯飞账号网关（base_url / app_id / access key），一般不用改 |

**TRAE 特有配置段**：

| 项 | 默认 | 说明 |
|---|---|---|
| `trae.auth_dir` | `<auth_dir>/trae` | 凭证目录（`trae-*.json`，嵌套或扁平两种形态都认） |
| `trae.refresh_interval_seconds` | `1800` | 后台 token 自动续期扫描间隔（`<=0` 关闭） |
| `trae.checkin_enabled` | `true` | 每日自动签到（30 分钟扫一次，幂等） |
| `trae.agent_base_url` / `ug_base_url` / `oauth_base_url` | 内嵌默认 | 三个 host 覆盖（一般不用改） |

## 🖥️ 管理控制台

访问 `http://127.0.0.1:7863/ui`（本机访问自动注入 Key，顶栏可复制 curl 示例）。

- **仪表盘**：网关状态（账号总数 / 健康）+ 调用统计（总请求 / 成功率 / 消耗 token /
  平均 TTFB 等）+ API 接入信息。
- **账号池**：每个上游一张表（列集由上游**自报**，首列统一为「上游」；昵称自动打码），
  行内动作（签到 / 额度 / 保活 / 禁用 / 移除）按上游自报渲染。
- **Loomy 专属标签页**：新手任务（一键完成单账号 / 全部账号）、邀请码（绑定别人的码、
  查看自己生成的码，含 active/exhausted 状态）。
- **模型 / 对话测试 / 请求日志 / 设置**：日常排障所需都在页面上。

## 🔌 API

```bash
curl http://127.0.0.1:7863/v1/chat/completions \
  -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
  -d '{"model":"loomy/deepseek-v4-flash-0731","messages":[{"role":"user","content":"hi"}],"stream":true}'
```

- 模型名带上游前缀：`loomy/xxx`、`workbuddy/xxx`、`codearts/xxx`…；裸模型名走默认上游。
- `GET /v1/models`：各上游目录合并（含生图模型，带 `capabilities` 标注）。
- `POST /v1/images/generations`：OpenAI 兼容生图。
- 健康检查：`GET /healthz`（**无鉴权**，负载均衡友好）；`GET /status`（需鉴权，带详情）。
- 管理 API：`/admin/*`（默认只建议本机使用）。

### `/v1/models` 每条记录带的字段

```json
{"id":"workbuddy/deepseek-v4-pro","object":"model","owned_by":"workbuddy",
 "context_length":1000000,"max_output_tokens":128000,"supports_images":true}
```

| 字段 | 含义 | 缺失时调用方该怎么办 |
| --- | --- | --- |
| `context_length` | 上下文窗口（token） | 逐级回落：上游没给时网关填 131072 兜底 |
| `max_output_tokens` | 单次输出上限（token） | `0` 表示上游没给，按自己的默认值 |
| `supports_images` | **接受图片作为输入** | **键不存在 = 上游没声明支持**，按纯文本处理 |
| `capabilities` | 需要换端点/换面板的能力（目前只有 `image_generation`） | 键不存在 = 走 `/chat/completions` |
| `name` | 展示名（只有生图条目会给） | 用 `id` |

两个容易混的判据：

- `supports_images` 与 `capabilities:["image_generation"]` 是**两个轴**：
  前者是"能不能把图**发进去**"，后者是"能不能让上游**生图出来**"。
  `loomy/Hy-Image-3.5-preview` 两条都占（收图做编辑、输出也是图），
  `loomy/doubao-seedream-5-lite` 只占后者。
- `supports_images` 是**逐上游**的事实，不是模型固有属性：同一个 `glm-5.2`
  在 `workbuddy/` 上是 `true`，在 `trae/` 上是 `false` —— 两边都对，
  说的是各自网关放不放图片进来。所以它由上游如实上报，核心不按模型名推断。

`supports_images` 的取值来源（每个上游字段名不同，覆盖 5 个在册上游）：

| 上游 | 上游字段 |
| --- | --- |
| `workbuddy` | `supportsImages`（`/console/enterprises/personal/models`） |
| `trae` | `display_config.multimodal`（`get_detail_param`） |
| `loomy` | `capabilities.vision`（`/models`） |
| `inkstone` | `input_modalities[].type` 含 `"image"`（`/v1/models`） |
| `jiyuan` | `supports_vision`（`/v1/models`） |

## 🛡️ 安全与合规

- 网关默认只绑本机；对外暴露请自行加反向代理与更严格的鉴权。
- 凭证按上游分目录存于 `auths/`（已 gitignore）；控制台昵称自动打码。
- Loomy 短信登录的签名 key 随官方安装包分发（官方注释自述"混淆而非加密"），可通过
  `loomy.sms_*` 覆盖。
- 本项目面向**自有账号**的自动化管理与协议理解；请遵守各上游的服务条款与额度限制，
  控制并发、避免滥用。

## 🛠️ 开发

```bash
go build ./... && go vet ./...        # 构建 + 静态检查
go test ./... -count=1                # 全量测试（含假上游契约测试，CI 中不跳过）
gofmt -l .                            # 格式检查
```

> ⚠ **Windows 上 `gofmt -l .` 会误报**（CRLF 被当成格式问题）。本地判真实情况要先
> LF 归一化再查；CI（Linux）不受影响。
>
> ⚠ `go test -race` 在 CI 里是 **advisory**（`continue-on-error: true`）。
> 本机若无 gcc 则跑不了 —— **不要假装跑过**。

- **架构**：`internal/gateway` 是唯一接缝 —— `Provider` 4 方法 + 若干可选扩展点
  （登录流程 / 凭证加载 / 额度探测 / 每日动作 / 列集自报 / 凭证寿命 / 后台任务 /
  生图 / 对话工具 / 诊断端点……）。加新上游不改核心；
  `internal/gateway/arch_test.go` 自动约束包依赖方向（判据是**包级依赖图**，
  不是源码文本匹配）。
- **CI**（`.github/workflows/ci.yml`）：`go mod tidy` 检查 / build / vet / gofmt /
  全量测试（五项门禁，缺一不可合）。
- **发布**（`.github/workflows/release.yml`）：打 `v*` 标签自动构建
  **五个平台**（windows-amd64 / linux-amd64 / linux-arm64 / darwin-amd64 /
  darwin-arm64）、生成 `SHA256SUMS.txt` 并挂到 GitHub Release。
  发布 Release 的**说明由人写**（含回滚方案），workflow 只负责产物。

## 🤝 贡献

欢迎 Issue 与 PR。提 PR 前请先跑一遍本地质量门禁（与 CI 完全同一组命令）：

```bash
go mod tidy && git diff --exit-code go.mod go.sum
go build ./... && go vet ./...
gofmt -l .
go test ./... -count=1
```

约定：

- **新上游不改核心**：只加一个 `internal/<name>/` 包 + 一段配置 + `cmd/server/main.go`
  里一行 `registry.Register(...)`；`internal/gateway/arch_test.go` 会验证包依赖方向
  没被破坏。
- **提交信息**：`<type>(<scope>): <摘要>`，type 取 `feat` / `fix` / `docs` / `refactor` /
  `test` / `chore`。
- **不要把真实凭据提交进仓库**：测试里的 uid / 手机号 / token / API Key 一律用占位值。
  `.gitignore` 只挡得住 `auths/`、`data/`、`config.json`，**测试文件不在忽略范围内** ——
  这一条只能靠提交前自己看一眼。
- 行为变更请同步 `CHANGELOG.md`。

安全问题的报告方式见 [SECURITY.md](SECURITY.md)。

## 🧬 来源与二次开发

本项目是**开源项目基础上的二次开发（二创）**。来源链条：

```
Sliverkiss/workbuddy2api  ──►  Practice019/multi2api  ──►  NaiZi899/omni2api
    WorkBuddy 专用反代            多上游聚合网关（10 个上游）        本仓库：12 个上游 + 优化
```

| 层级 | 仓库 | 说明 |
| --- | --- | --- |
| 原始项目 | [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) | WorkBuddy 专用反代，MIT |
| **二创基线** | [Practice019/multi2api](https://github.com/Practice019/multi2api) | 扩展为多上游聚合网关，**本项目的全部改动都在它之上进行** |
| 本仓库 | [NaiZi899/omni2api](https://github.com/NaiZi899/omni2api) | 在 multi2api 基础上继续二创，沿用 MIT |

三层的 MIT 版权声明都保留在 [LICENSE](LICENSE) 里。

### 相对基线做的主要改动

**页面渲染优化**

- 控制台操作后就地刷新快照：此前点一次操作要等约 7 秒全量回源，现改为读缓存（约 1ms）
- 按钮点击立即进入「进行中」状态：此前约 6.9 秒页面完全静止，看起来像没响应
- 模型目录改为非阻塞加载 + 缓存，修掉「状态判定与取数互相依赖」导致的冷启动自锁
- 账号探测并发化（上限 5）：3 个账号强制刷新由 7.2s 降到 2.5s
- 前端渲染层重构，并新增静态探测（`tests/frontend/`）拦截渲染期脚本错误；
  修掉裸插值把脏数据渲染成 `[object Object]` 的问题
- 控制台模型目录的加载不再阻塞首屏（见 `internal/server/models_catalog*.go`）

**跳转优化**

- TRAE 页内添加账号：浏览器 OAuth 授权后由本机回调端口自动接收登录回跳
  （端口可配 `trae.oauth_callback_port`）
- Raccoon「添加账号」不再落到微信落地页：改走本机回调，并把防串号标识放进
  `redirect` 的 query（`nonce`）而不是 `state`
- LobsterAI 浏览器 OAuth 回跳链路收口
- 各上游的深链 / 回调形态差异统一由适配层吸收，核心不感知

**新增模块**

- **InkStone**（书生·端砚 TokenPlan）上游适配
- **Jiyuan**（基元律动 TokenRhythm）上游适配
- 配套：账号导入按类型分发、错误解释扩展（把上游业务码翻译成可读原因）、
  额度单位与上限按上游自报

上游从 10 个扩到 12 个，核心包依然零改动 —— 这条由
`internal/gateway/arch_test.go` 的可执行断言守着。

> 上游自身的版本变化请见对应原仓库；本仓库的逐条变更记录见 [CHANGELOG.md](CHANGELOG.md)。

## 📄 许可证

[MIT](LICENSE)。本项目二创自 [Practice019/multi2api](https://github.com/Practice019/multi2api)
（同为 MIT，其本身 fork 自 [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api)），
沿用该许可证；三层版权与本仓库增量的声明都在 `LICENSE` 文件里。

## 免责声明

本项目仅用于对自有账号/自有软件协议的理解与自动化，请遵守各相关平台的服务条款。
