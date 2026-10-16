<!--
  提交 PR 前请先跑本地质量门禁（与 CI 同一组命令，见 CONTRIBUTING.md）：
    go mod tidy && git diff --exit-code go.mod go.sum
    go build ./... && go vet ./...
    gofmt -l .
    go test ./... -count=1
-->

## 这个 PR 做了什么

<!-- 一两句话说明动机与结果。关联的 Issue 用 "closes #123" 形式写。 -->

## 改动类型

- [ ] 修复缺陷
- [ ] 新增上游
- [ ] 新能力 / 行为改进
- [ ] 文档
- [ ] 重构（无行为变更）
- [ ] CI / 构建 / 发布

## 如何验证

<!--
  写下你实际跑过的命令与看到的结果。
  - 不要写"应该没问题"：没跑过就说明没跑过。
  - go test -race 若本机缺 gcc 跑不了，直接说明，不要假装跑过。
-->

```bash

```

## 检查清单

- [ ] 本地五件套已跑过（tidy / build / vet / gofmt / test），结果写在上面的「如何验证」里
- [ ] 新增上游时**没有改核心包**（依赖方向由 `internal/gateway/arch_test.go` 守着）
- [ ] 测试与 fixture 里**没有真实凭据**（uid / 手机号 / token / API Key / 设备指纹一律占位）
- [ ] 行为变更已同步 `CHANGELOG.md`
- [ ] 破坏性变更已在描述里写明，并给出回滚方式（没有就写"无"）

## 破坏性变更与回滚

<!-- "无" 也是一种答案。有的话写清：影响谁、怎么迁移、怎么回退。 -->
