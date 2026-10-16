# 贡献指南（Contributing）

感谢你愿意花时间改进 omni2api。这份文件只写"怎么改才不会被退回"，其余交给常识。

## 开发环境

- Go **1.22 及以上**（`go.mod` 声明 1.22.5，CI 用 1.22.x）
- 可选：Docker（跑容器化验证）、Node.js（`tests/frontend/` 下的控制台冒烟脚本）
- Windows 上开发完全可以；CI 跑在 Linux 上

```bash
git clone https://github.com/NaiZi899/omni2api.git
cd omni2api
cp config.example.json config.json     # 填自己的 api_key；config.json 已被 .gitignore 忽略
go build -o omni2api-server ./cmd/server
```

## 提 PR 之前：本地质量门禁

CI 跑的就是下面这几条，本地先跑一遍能省掉一轮来回：

```bash
go mod tidy && git diff --exit-code go.mod go.sum   # 依赖清单必须已同步
go build ./...
go vet ./...
gofmt -l .                                          # 不应有输出
go test ./... -count=1
```

`go test ./... -race` 在 CI 里是 **advisory**（`continue-on-error: true`）：
本机没有 gcc 时跑不起来，因此不设为合入门槛。如果你的环境跑通了，欢迎在 PR 里
附上输出 —— 有人验证过之后这条才可能升级为门禁。

> Windows 提示：`gofmt -l .` 会把 CRLF 工作树里的文件全部报成未格式化（假阳性）。
> 判真实情况要先归一到 LF 再看，或者直接在 WSL/CI 里跑。

## 加一个新上游

这是本项目最常见的贡献形态，也是设计上刻意做薄的一条路径：

1. 新建 `internal/<name>/`，实现 `gateway.Provider`（`Chat` / `Models` / …），
   再按需要实现可选扩展点：登录流程 / 凭证加载 / 额度探测 / 每日动作 / 列集自报 /
   凭证寿命 / 后台任务 / 生图 / 对话工具 / 诊断端点。
2. `cmd/server/config.go` 加一个配置段（`enabled` + `auth_dir` 起步）。
3. `cmd/server/main.go` 加一行 `registry.Register(...)`。
4. **核心包一行都不要改。** 这是判据，不是口号 ——
   `internal/gateway/arch_test.go` 会检查包级依赖图：
   核心不得依赖具体上游，上游之间不得互相依赖。它红了就是设计被破坏了。
5. 配套测试：契约测试用 `httptest` 起假上游，不要依赖真实网络。

## 提交规范

- 提交信息：`<type>(<scope>): <摘要>`，type 取 `feat` / `fix` / `docs` / `refactor` /
  `test` / `chore`。
- 一个 PR 做一件事。顺手重构请在描述里说明，别混在功能提交里。
- 行为变更（配置项、接口语义、默认值）请同步更新 `CHANGELOG.md`，并在 PR 描述里
  写清**破坏性变更**与回滚方式。

## 绝不提交这些东西

`.gitignore` 挡得住 `auths/`、`data/`、`config.json`、`*.key`、`*.pem`、编译产物，
但**挡不住你写进源码和测试里的凭据**。请务必：

- 测试里的 uid、手机号、邮箱、token、API Key、设备指纹一律用占位值
  （如 `13800138000`、`sk-test0000000000000000`、`00000000-0000-4000-8000-000000000000`）。
- 粘贴上游"实测回执"做回归 fixture 时，**先脱敏再粘** —— 上游回执里通常带真实
  uid / 手机号尾号 / key 前缀 / traceId。
- 提交前自查：

```bash
git diff --cached | grep -nE '1[3-9][0-9]{9}|sk-[A-Za-z0-9]{20,}|eyJ[A-Za-z0-9_-]{20,}'
```

不确定某个值算不算敏感时，按敏感处理。

## 安全问题的报告方式

**不要开公开 Issue。** 走 [SECURITY.md](SECURITY.md) 里写的私密通道。

## 许可证

提交即表示你同意以本仓库的 MIT 许可证发布你的贡献（见 [LICENSE](LICENSE)）。
