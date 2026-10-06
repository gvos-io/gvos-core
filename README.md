# G-VOS

面向持证话务商、呼叫中心服务商与 AI 语音平台的**云原生、API-first、内置合规的下一代话务运营系统**（软交换 + 计费 + 路由 + 风控）。定位是 VOS（南京昆石）的替代品，不是复刻。

> 战略与市场依据见 [vos-competitive-analysis.md](vos-competitive-analysis.md)，产品/架构/定价/迁移方案见 [docs/](docs/)。

## 当前状态：M0-M3 架构 spike ✅

已完成的端到端闭环（INVITE 锁定 → 通话 → BYE 结算 → CDR）：

- **计费核心（Go，自研，全部单测覆盖）**
  - `internal/rate`：费率前缀最长匹配（从被叫号码派生前缀探测哈希表，10 万条费率下微秒级）、时段窗口覆盖（周末/夜间费率）、7 位小数第 8 位进一（VOS 精度语义）
  - `internal/billing`：计费时长步长 60+6 / 6+6 / 1+1 / N+M、MinDur、首步价/增量价分解——不变量：**cost 永远精确对 billedSecs 定价**
  - `internal/account`：租户→多级代理→账户树、全链路原子预锁定（all-or-nothing 回滚）、后付账户跳过锁定、环检测
  - `internal/callstate`：预锁定→周期扣减→余额耗尽**强制拆线**（teardown 回调）→ 结算补扣/退还，CDR 输出
- **事件入口**：`cmd/gvos-core` HTTP API（`/v1/call/start|answer|end`、`/v1/stats`、`/healthz`），Kamailio 侧对接点
- **数据模型**：`migrations/001_init.sql`（账户/费率/网关/路由/CDR 月分区/账本/审计/黑白名单，NUMERIC(18,7)）
- **部署**：`deploy/docker-compose.yml`（gv-core + Kamailio + RTPengine + demo 网关 + PostgreSQL）一键起

```bash
# 本地跑测试与构建
go test ./... && go build -o bin/gvos-core ./cmd/gvos-core

# 冒烟：一通电话的完整计费闭环
./bin/gvos-core -listen :9997 &
curl -X POST -d '{"call_id":"c1","from_user":"1000","to_user":"8613800138000"}' :9997/v1/call/start
curl -X POST -d '{"call_id":"c1","duration":66}' :9997/v1/call/end
# -> {"CallID":"c1","BilledSecs":66,"Cost":0.055,"RatePrefix":"8613"}
```

## 里程碑（对齐 docs/01-prd.md）

| 阶段 | 内容 | 状态 |
|---|---|---|
| M0-M3 | 架构 spike：端到端计费闭环 | ✅ 完成 |
| M4-M8 | P0 全量 + 中文管理台 + 压测报告 v1 | 未开始 |
| M9-M10 | 3 家设计伙伴 alpha | 未开始 |
| M11-M12 | v1.0 GA：渗透测试 / HA 演练 / 迁移工具 beta | 未开始 |

## 工程约定

- Go 1.21（标准库 `ServeMux` 无方法路由，handlers 自行校验动词）
- 金额一律 `float64` 计算后 `roundUp7`（7 位精度、进一）；数据库层 `NUMERIC(18,7)`，生产对账走整数分（后续引入）
- 测试先行：计费语义变更必须先有失败测试（已覆盖 VOS 全部步长方案）
- 所有接口变更同步更新 `docs/` 对应文档

## 目录

```
cmd/gvos-core/     # 事件入口 + HTTP API（spike）
internal/rate/     # 费率引擎（前缀匹配 + 时段窗口）
internal/billing/  # 计费器（步长方案 + 精度）
internal/account/  # 账户树 + 余额锁定
internal/callstate/# 通话状态 + 强制拆线 + CDR
migrations/        # PostgreSQL schema
deploy/            # docker-compose + Kamailio cfg + Dockerfile
docs/              # PRD / 架构 / 定价 / 迁移方案
research/          # 竞品与市场原始调研
```
