# Contributing to gvos

感谢关注 gvos。Issue 与 PR 均欢迎。

## 开发环境

- Go 1.21+（标准库优先，新增依赖需说明理由）
- 可选：Docker（PG/NATS 集成测试）、docker compose（全栈演示）

```bash
go test ./...          # 全部单元测试（无外部依赖）
go build ./... && go vet ./...
# 真依赖集成测试（跳过条件见 internal/store/pg_test.go 头注释）
docker compose -f deploy/docker-compose.yml up -d postgres nats
```

## 提交纪律

- **计费语义变更必须先有失败测试**：步长、精度（7 位小数第 8 位进一）、锁定/回滚不变量是本项目的核心契约，回归即事故
- 对账不变量（CDR 合计 = 账本合计）不得破坏；涉及持久化的改动请在真实 PG 上跑对账检查
- 事件 payload 只携带业务字段：凭据（HA1/密码）与密钥永进总线
- 新增管理 API 必须写审计（`s.audit`）并考虑多副本广播语义

## 分支与提交

- 从 `main` 拉 feature 分支，PR 合入
- Commit message 用英文祈使句，说明"为什么"而不只是"改了什么"
