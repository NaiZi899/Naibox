<p align="center">
  <img src="assets/icon/app.svg" alt="NaiBox" width="96" height="96">
</p>

<h1 align="center">NaiBox</h1>

<p align="center">
  <b>本地 AI 网关，多个上游账号，一个 OpenAI 兼容接口</b>
</p>

<p align="center">
  <a href="../../releases">下载</a> ·
  <a href="#第一次启动">快速开始</a> ·
  <a href="#配置与重启">配置</a> ·
  <a href="#自定义上游">自定义上游</a>
</p>

## 用途

NaiBox 是一个 Go 编写的本地 AI 网关，Windows 桌面版与 Linux/macOS 服务端共用同一套装配，对外提供一个 OpenAI 兼容的 `base_url` 和 `api_key`。

内置 12 个上游，也能接入任意 OpenAI 兼容端点。客户端只配一个地址和一把 key，选号、冷却、重试、续期、模型目录由网关处理。

适合多账号统一调用和桌面常驻。需要对外多租户计费时，这个项目不提供那层能力。

## 请求路径

```text
客户端 ──Bearer──► NaiBox ──选号──► 上游
                      │
                      └── 冷却 / 熔断 / 换号
```

模型名带前缀时指定上游，裸模型名走默认上游。前缀不认识会直接返回 400，请求不会发出。

账号池按健康状态、在途名额和会话粘性选号。上游报错后，网关按错误类型决定换号、冷却、熔断或原样透传。

## 运行方式

| | 桌面版（Windows） | 服务端 |
|---|---|---|
| 适合 | 个人电脑，双击启动 | 服务器、NAS、长期在线 |
| 数据 | `%APPDATA%\omni2api\` | 当前目录下的 `config.json`、`auths`、`data` |
| 界面 | WebView2 窗口加托盘 | 浏览器打开 `/ui` |
| 升级 | 替换 exe，数据不动 | 替换二进制，重启 |

桌面版行为

- 首次启动生成配置、凭证、数据和日志目录。
- 端口从 7863 起顺延，实际地址写在 `runtime.json`。
- 第二次双击只唤出已有窗口。
- 托盘提供控制台、数据目录、日志、备份和重启入口。
- 关窗可选询问、最小化到托盘、退出。最小化后网关继续运行。
- 缺少 WebView2 时回落到托盘加默认浏览器。

服务端

```bash
./Naibox-server -config config.json
```

Docker

```bash
cp config.example.json config.json
# 填 api_key，并把要用的上游 enabled 改成 true
docker compose up -d --build
```

## 第一次启动

**桌面版**

1. 从 [Releases](../../releases) 下载 `NaiBox-windows-amd64.zip`，用 `SHA256SUMS.txt` 校验。
2. 解压，双击 `NaiBox.exe`。
3. 在「账号池」添加账号，到「仪表盘」复制 `base_url` 与 `api_key`。

**服务端**

```bash
cp config.example.json config.json
# 填 api_key，启用上游
./Naibox-server -config config.json
```

打开 `http://127.0.0.1:7863/ui`。`/healthz` 在账号数为 0 时返回 503，表示网关已启动，但还没有可用账号。

## 接入账号

控制台「账号池」点「添加账号」，或使用批量导入。

| 凭证类型 | 上游 |
|---|---|
| 浏览器授权 | WorkBuddy、WorkBuddy AI、CodeArts、TRAE、LobsterAI、Qoder、Qoder 中国版、Raccoon |
| 设备码轮询 | Cline |
| 手机号验证码 | Loomy |
| API Key 批量导入 | InkStone、Jiyuan |
| OpenAI 兼容端点 | 自定义上游 |

| 上游 | 功能 |
|---|---|
| WorkBuddy（默认） | 对话、模型、额度、签到、保活、成长、旅行 |
| WorkBuddy AI | 对话、模型、额度，无签到与成长 |
| CodeArts | 对话、模型、签到、额度、STS 续期 |
| Loomy | 对话、模型、额度、生图、联网搜索 |
| Cline | 对话、模型、余额、续期 |
| TRAE | 对话、模型、签到、续期、权益额度 |
| Raccoon | 对话、模型、积分、登录奖励 |
| LobsterAI | 对话、模型、签到、积分 |
| Qoder / 中国版 | 对话、模型、额度、签到、WASM 推理 |
| InkStone | 模型目录、墨点额度 |
| Jiyuan | 模型目录、余额 |
| 自定义上游 | 对话、模型目录、探测、多 key、多分片 |

`inkstone` 与 `jiyuan` 缺省启用。没有凭证时只是一个空分组，想关掉就写 `"enabled": false`。

## 控制台

| 页面 | 用途 |
|---|---|
| 仪表盘 | 健康、请求量、成功率、token、TTFB、趋势 |
| 账号池 | 账号、冷却、禁用、额度、在途请求 |
| 请求日志 | 历史分页与 SSE 实时推送 |
| 可用模型 | 合并目录与能力标注 |
| 对话测试 | 选模型直接发一条 |
| 设置 | 调度、超时、提示词、日志保留、显示过滤 |
| 自定义上游 | 新增、编辑、测试、启停、删除 |
| 关于软件 | 项目地址与推荐项目 |

「账号池显示过滤」只影响界面。被隐藏的上游仍然参与选号和签到。

## 调用网关

在 `config.json` 所在目录执行。

```bash
KEY=$(sed -n 's/.*"api_key"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' config.json)
BASE=http://127.0.0.1:7863

curl "$BASE/v1/chat/completions" \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"workbuddy/glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":true}'
```

| 接口 | 用途 |
|---|---|
| `POST /v1/chat/completions` | 对话，支持流式 |
| `GET /v1/models` | 合并模型目录 |
| `POST /v1/images/generations` | OpenAI 兼容生图 |
| `GET /healthz` | 无鉴权探活，账号数为 0 时返回 503 |
| `GET /status` | 需鉴权状态 |
| `/admin/*` | 管理接口，仅本机 |

模型目录里的 `supports_images` 控制输入图片，`capabilities` 里的 `image_generation` 控制输出图片。

Loomy 的 `generate_image` 与 `web_search` 由网关注入，agent 框架直接可用。关闭用 `server.disables_chat_tools: true`。

生图返回裸 URL，签名地址在 `url_signed`。

## 配置与重启

配置在 `config.json`，完整示例见 [config.example.json](config.example.json)。

| 组 | 关键项 |
|---|---|
| 基础 | `listen`、`api_key`、`service_name`、`auth_dir`、`state_file` |
| 界面 | `ui.hidden_providers`、`server.max_body_mb`、`server.disables_chat_tools` |
| 账号池 | `pool.*`、`session_sticky.*`、`schedule.*` |
| 出站 | `upstream.*`、`prompt.*` |
| 存储 | `upstash.*`，不配则纯内存 |

| 改动 | 生效 |
|---|---|
| 自定义上游、提示词、上游超时、账号池、监听地址 | 重启网关 |
| 日志保留、签到与保活开关、显示过滤、API Key 轮换、账号操作 | 立即生效 |

提示词有三种模式。

| 模式 | 行为 |
|---|---|
| `passthrough`（默认） | 客户端 system 原样透传 |
| `custom` | 删除客户端 system，换成网关提示词 |
| `append` | 保留客户端 system，再追加一条网关 system |

`prompt.file` 不可读或为空会让启动失败。`NAIBOX_PROMPT_MODE=passthrough` 是回滚开关。

## 自定义上游

```json
{
  "custom_upstreams": [
    {
      "id": "office",
      "base_url": "https://api.example.com/v1",
      "api_keys": ["sk-key-one", "sk-key-two"],
      "models": ["gpt-4o-mini"],
      "shards": 1
    }
  ]
}
```

模型名写作 `office/gpt-4o-mini`。

- 密钥有三种写法，`api_key`、`api_keys`、`api_key_env`。
- 账号数等于 key 数量乘以 `shards`。
- 多 key 时 401/403 换 key，429 换号但不冷却；单 key 时这三类错误原样透传。
- 模型列表可以写死，也可以留空实时读取 `/models`。
- 所有改动都要重启网关。

## 日常运维

| 路径 | 内容 |
|---|---|
| `config.json` | 配置 |
| `auths/` | 各上游凭证 |
| `data/` | 状态、请求日志、API Key |
| `logs/` | 服务端日志 |
| `runtime.json` | 桌面版监听地址与进程信息 |

| 现象 | 先查 | 处理 |
|---|---|---|
| `/healthz` 503 | `total` 是否为 0 | 添加账号或重载 auths |
| 401 | 网关 key 是否正确 | 用 `/v1/models` 验 key；上游 401 则重新登录 |
| 429 | 上游限流 | 等 Retry-After；多 key 自定义上游会自动换 |
| 400 模型前缀 | `GET /v1/models` | 修正前缀或模型名 |
| 界面打不开 | `runtime.json` 与 `logs/` | 桌面版看实际端口；服务端改 `listen` 后重启 |

```bash
tail -f logs/server-*.log
```

## 已知限制

- 自定义上游只支持 OpenAI 兼容的 `chat/completions` 与 `models`，不支持 Anthropic 原生协议。
- `/admin/*` 只接受本机访问，远程管理需要 SSH 隧道或反向代理。
- 没有多租户和计费能力，API Key 只用于本机调用鉴权。
- 桌面版依赖 WebView2，发布 exe 未签名，SmartScreen 会提示未知发布者。
- 大部分配置改动需要重启网关，没有完整的配置热加载。

## 安全

- 默认只绑本机。对外服务时自行加反向代理与 TLS。
- 凭证放在 `auths/`，已排除出版本库；控制台昵称与密钥尾号会打码。
- 自定义上游只填可信端点，网关会按配置向该地址发请求。
- 发布产物用 `SHA256SUMS.txt` 校验，不要凭 SmartScreen 的提示判断。
- 仅用于自有账号的自动化，遵守各上游条款。

## 开发、来源与许可

```bash
go build ./... && go vet ./...
go test ./... -count=1
```

Windows 上 `gofmt -l .` 会因 CRLF 误报，以 CI 的 Linux 结果为准。

桌面版必须用构建脚本，否则 exe 没有图标和 DPI 清单。

```powershell
pwsh scripts/build-desktop.ps1
```

`internal/gateway` 是唯一接缝，`internal/boot` 负责装配，`cmd/server` 与 `cmd/desktop` 共用。新增上游只加一个 `internal/<name>/`、一行注册和两份示例配置。

来源

```text
Sliverkiss/workbuddy2api → Practice019/multi2api → NaiZi899/Naibox
```

相对基线的增量包括 Windows 桌面壳、装配层、实时日志、代码图标、InkStone、Jiyuan 和自定义上游。

[MIT](LICENSE)。仅用于自有账号与协议理解，遵守各上游条款，使用者行为由使用者负责。
