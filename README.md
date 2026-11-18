<div align="center">

# G-VOS

**云原生话务运营平台 — 软交换编排 · 实时精确计费 · NATS 事件总线 · 内置风控**

[![CI](https://github.com/lixuanqun/gvos-core/actions/workflows/ci.yml/badge.svg)](https://github.com/lixuanqun/gvos-core/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.21%2B-00ADD8.svg)](go.mod)

</div>

> **English** — G-VOS is a cloud-native telecom operations platform (softswitch orchestration + real-time billing + fraud control) built as a Go control plane over Kamailio/RTPengine. All state changes flow through a NATS JetStream event bus: CDR persistence, audit trail, teardown execution and multi-replica config sync are durable consumers, so the billing answer never waits on a database. One command brings up a working demo; the billing core is fully unit-tested with a reconciliation-first design.

## 为什么是 G-VOS

传统话务运营系统把信令、媒体、计费塞在一个"一体化大盒子"里：扩展靠换大机器、计费语义是黑盒、出了问题无从对账。G-VOS 反其道而行：

- **控制面自研（Go）**：计费、路由决策、租户/代理链、风控全部代码透明，测试先行
- **事件驱动**：CDR 落库、审计、强制拆线执行、Webhook 分发全是 NATS durable 消费者——**计费应答永不等待数据库**，写失败进重投队列而非丢失
- **多副本即时同步**：费率/黑名单/账户/余额/录音开关/风控规则/套餐变更经总线广播，副本零重启生效，重启后从 `settings` 表恢复
- **对账优先**：金额 7 位小数第 8 位进一（兼容主流计费精度语义），CDR 与账本逐分对账是持续验证的不变量

## 核心能力

| 域 | 能力 |
|---|---|
| **计费引擎** | 前缀最长匹配（10 万条费率 133ns/次）、时段窗口（节假日/夜间）、步长 60+6 / 6+6 / 1+1 / N+M、首步价/增量价 |
| **账户体系** | 租户→多级代理→账户树、全链路原子预锁定（all-or-nothing 回滚）、后付账户、并发/CPS 限额 |
| **实时风控** | 频次/日成本/地区准入规则（block/alert）、黑白名单（前缀索引 220ns/次）、余额耗尽强制拆线 |
| **事件总线** | NATS JetStream：CDR 流、审计流、拆线指令队列、Webhook 分发（NAK 重投 + MaxDeliver 防毒丸）、多副本配置广播 |
| **套餐引擎** | 日/月免费额度、折扣、最低消费，按周期计数 |
| **AI 语音** | 通话媒体流 WebSocket 出站 + DashScope ASR 桥接（实时转写） |
| **迁移工具** | `gvos-migrate` 四步流水线（extract/validate/plan/load），从旧系统 CSV 导入账户/费率 |

## 架构

```
┌───────────────────── 控制平面（自研，Go） ─────────────────────┐
│  账户/多级代理 │ 费率引擎 │ 风控 │ 套餐 │ CDR/审计/账本 │ 管理 API  │
└──────────┬──────────────────────────┬─────────────────────────┘
           │ HTTP(同步锁定门)           │ NATS JetStream（事件/广播/队列）
┌──────────▼──────────┐    ┌───────────▼──────────────────────────┐
│ Kamailio ×N (信令)   │    │ durable workers: CDRW落库 AUDIT审计   │
│ RTPengine ×M (媒体)  │    │ TEARDOWN拆线 WEBHOOKS分发 cfg广播     │
└─────────────────────┘    └───────────┬──────────────────────────┘
                                ┌──────▼──────┐
                                │ PostgreSQL  │ (+ settings kv)
                                └─────────────┘
```

## 快速开始

```bash
# 一键演示：compose 拉起 gv-core + Kamailio + RTPengine + PostgreSQL + NATS，
# SIPp 打一通真实计费电话，打印管理台地址
bash scripts/demo-up.sh

# 或仅起控制面（无 Docker，纯内存演示模式）
go run ./cmd/gvos-core -listen :8080
curl -s -X POST -d '{"call_id":"c1","from_user":"1000","to_user":"8613800138000"}' :8080/v1/call/start
curl -s -X POST -d '{"call_id":"c1","duration":66}' :8080/v1/call/end
# -> {"CallID":"c1","BilledSecs":66,"Cost":0.055,"RatePrefix":"8613"}

# 全量测试（PG/NATS 集成测试按环境变量门控，见 internal/store/pg_test.go 头注释）
go test ./...
```

## 实测基准

Windows 开发机（i7-13700H，双副本 + 真 PG/NATS），方法与原始数据见 [capacity report](docs/bench/capacity-report-2026-11-19.md)：

| 指标 | 实测 |
|---|---|
| 呼叫建立（锁定路径） | **p50 = 0.8ms / p99 = 2.9ms** @ 100 CPS 稳态 |
| 瞬时冲击 | **1,245 CPS** burst 零失败（1,000 通 / 0.8s） |
| 并发通话 | 1,000 并发 × 30s 零失败，内存线性（+9MB），无泄漏 |
| CDR 事件落库 | burst 冲击 2.4s 追平积压（≈300 events/s）；稳态零滞后 |
| 对账 | 6,714 行 CDR vs 账本，**369.78 = 369.78 逐分零漂移** |

## 事件总线（NATS）

`-nats <url>` 开启后所有状态变化流经总线（JetStream stream `GVOS`；`gvos.svc.*` 服务面不进流）：

| Subject | 类型 | 用途 |
|---|---|---|
| `gvos.cdr.completed` | 事件 | CDR 流 → CDRW 落库 / WEBHOOKS 分发（可各自回放） |
| `gvos.call.teardown` | 队列 | 强制拆线指令（执行器宕机不丢，恢复续消费） |
| `gvos.cfg.reload` | 广播 | 费率/黑名单/账户/余额/开关/规则/套餐多副本同步 |
| `gvos.risk.alert` / `gvos.admin.changed` | 事件 | 风控告警 / 审计流（AUDIT worker 消费） |
| `gvos.svc.call.start/answer/end`、`gvos.svc.stats` | req/reply | 无需 HTTP 即可驱动完整通话 |

## 已知限制（诚实声明）

- SIP 数据面配置（Kamailio cfg / rtpengine）为 spike 级：单节点可用，生产级调优与大规模验证在进行中
- 余额权威当前在各副本内存（预锁定语义正确），生产部署按路线图换 Redis 收口
- 管理令牌默认 `dev-admin-token`、DB 密码 `gvos_dev`——均为演示默认值，**生产部署必须更换**（见 [operations manual](docs/operations-manual.md)）
- TLS 证书未随仓库分发，部署时自签：`openssl req -x509 -newkey rsa:2048 -nodes -keyout kamailio-key.pem -out kamailio-cert.pem -days 365 -subj "/CN=gvos.local"`
- PRD 中 20 万注册、媒体面大规模压测为进行中目标

## 文档

- [架构设计](docs/02-architecture.md) · [运维手册](docs/operations-manual.md)
- [自测报告（130 项）](docs/bench/selftest-report-2026-11-19.md) · [容量报告 v0.4](docs/bench/capacity-report-2026-11-19.md) · [验收报告 v0.3](docs/bench/acceptance-report-2026-10-07.md)

## 参与

Issue / PR 均欢迎——计费语义变更请先写失败测试（见 [CONTRIBUTING](CONTRIBUTING.md)）。

## License

[Apache-2.0](LICENSE)
