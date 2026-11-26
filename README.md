<div align="center">

# gvos

**云原生话务运营平台 — 软交换编排 · 实时精确计费 · NATS 事件总线 · 内置风控**

[![CI](https://github.com/gvos-io/gvos-core/actions/workflows/ci.yml/badge.svg)](https://github.com/gvos-io/gvos-core/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.21%2B-00ADD8.svg)](go.mod)

</div>

> **English** — gvos is a cloud-native telecom operations platform (softswitch orchestration + real-time billing + fraud control) built as a Go control plane over Kamailio/RTPengine. Runtime money/admission state lives in Redis; multi-replica gv-core uses Call-ID stickiness plus Redis callmeta remote-settle. All state changes flow through a NATS JetStream event bus (CDR, audit, teardown, config sync) so the billing answer never waits on a database. One command brings up a working demo; the billing core is fully unit-tested with a reconciliation-first design.

## 为什么是 gvos

传统话务运营系统把信令、媒体、计费塞在一个"一体化大盒子"里：扩展靠换大机器、计费语义是黑盒、出了问题无从对账。gvos 反其道而行：

- **控制面自研（Go）**：计费、路由决策、租户/代理链、风控全部代码透明，测试先行
- **事件驱动**：CDR 落库、审计、强制拆线执行、Webhook 分发全是 NATS durable 消费者——**计费应答永不等待数据库**，写失败进重投队列而非丢失
- **多副本即时同步**：费率/黑名单/账户/余额/录音开关/风控规则/套餐变更经总线广播，副本零重启生效，重启后从 `settings` 表恢复
- **对账优先**：金额定点化（ADR-0003，1 单位 = 0.0001 元，4 位小数第 5 位进一），CDR 与账本逐分对账是持续验证的不变量

## 核心能力

| 域 | 能力 |
|---|---|
| **计费引擎** | 前缀最长匹配（10 万条费率 133ns/次）、时段窗口（夜间/周内；跨时段不拆分、无节假日）、步长 60+6 / 6+6 / 1+1 / N+M、首步价/增量价 |
| **账户体系** | 租户→多级代理→账户树；**Mode A 叶子扣款**（只预锁/结算主叫叶子，上级仅并发/CPS）；后付账户 |
| **实时风控** | 频次/日成本/地区准入规则（block/alert）、黑白名单（前缀索引 220ns/次）、余额耗尽强制拆线；`GVOS_RISK_MODE` |
| **信令边缘** | Kamailio 6.1.5 + nats 模块：鉴权 htable 缓存、IP 准入、异步 bill-lock RPC（suspend/resume 4 分片回复消费）、重传吸收、网关门限故障切换 |
| **媒体面** | RTPengine 26.3.1.4 用户态转发（端口池 30k）、录音管道（pcap→MP3→OSS）、20 万注册实测承载 |
| **事件总线** | NATS JetStream 3 节点 R3：CDR 流、审计流、拆线指令队列、Webhook 分发（NAK 重投 + MaxDeliver 防毒丸）、多副本配置广播 |
| **租户自助面** | `/v1/tenant/*`：me/accounts/cdrs/stats/packages/webhooks——API key 按 tenant_id 强制过滤（逃逸测试覆盖） |
| **Webhook 契约** | v1 版本化信封（{v,event,occurred_at,tenant,seq,data}）、Stripe 风格 HMAC 签名、at-least-once + 幂等键、SSRF 门（https+公网强制） |
| **可观测** | Prometheus `/metrics`（呼叫/RPC 直方图/账务写入/总线健康）+ Grafana 预置面板 + 告警规则（计费停滞/zombie/总线故障） |
| **套餐引擎** | 日/月免费额度、折扣、最低消费，按周期计数；`GVOS_PKG_MODE`（list_price / open） |
| **AI 语音** | 通话媒体流 WebSocket 出站 + DashScope ASR 桥接（实时转写） |
| **迁移工具** | `gvos-migrate` 四步流水线（extract/validate/plan/load），从旧系统 CSV 导入账户/费率 |

## 架构

```
┌───────────────────── 控制平面（自研，Go）×N ───────────────────┐
│  账户/叶子计费 │ 费率 │ 风控 │ 套餐 │ CDR/审计/账本 │ 管理 API   │
│  Redis: 余额/准入/callmeta·callsettled（跨副本结算兜底）        │
└──────────┬──────────────────────────┬─────────────────────────┘
           │ HTTP / NATS RPC(bill-lock) │ NATS JetStream（事件/广播）
┌──────────▼──────────┐    ┌───────────▼──────────────────────────┐
│ Kamailio ×N (信令)   │    │ durable: CDRW / AUDIT / TEARDOWN /   │
│ RTPengine ×M (媒体)  │    │ WEBHOOKS；cfg.reload + gateway-snapshot│
└─────────────────────┘    └───────────┬──────────────────────────┘
                                ┌──────▼──────┐
                                │ PostgreSQL  │ (+ settings kv)
                                └─────────────┘
```

多副本部署：**Call-ID 粘性为主路径**；粘性失效或滚动发布时 peer 用 Redis
`callmeta`/`callsettled` 做一次 `remote-settle`（详见 [运维手册 §6](docs/operations-manual.md)）。

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

**开发机**（Windows Docker Desktop，全栈含信令+媒体+计费）与**阿里云 ECS 4 核**（同机 loadgen 口径），方法与原始数据见 [docs/bench/](docs/bench/)：

| 指标 | 开发机 | 云 ECS 4 核 |
|---|---|---|
| 全栈 100CPS × 1 万通 | **10000/10000**（PDD p99 5ms） | **100%**（PDD p99 4ms） |
| bill-lock RPC | p50 2.8ms（3 节点 R3） | p99 4-6ms |
| 并发通话 | **9,853 同时在线**（CPU 余量大） | **2,000 精确保持**（duration=61s 全对） |
| 20 万注册 | — | **199,552/200,000 = 99.78%** @36/s × 95min |
| 对账 | CDR = ledger 逐分零漂移（多轮 1 万通级验证） | 同左 |

> 200CPS+ 的测量上限是压测机（SIPp/桥接）自限——栈侧计数器全绿、CPU<30%。生产 Linux 节点 + 横向扩展的完整容量矩阵见 [云上天花板报告](docs/bench/cloud-ceiling-2026-11-21.md)。

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

- **单租户（demo tenant）形态**：所有账户共用写死的 `demo` 费率表，CDR 的 `tenant_id` 为常量；账户上的 `rate_group` 字段只存不用。多租户数据模型（每租户独立费率组/账户树/CDR 归属）是尚未实现的功能，不是配置项
- 计费语义为 **Mode A 叶子扣款**（只动主叫叶子余额；上级不分摊同额预扣）。时长费率按起呼时刻取一次（跨时段不拆分、无节假日）
- **单机 2 万并发线已验证**（2026-11-26 阶梯压测 1k→20k 七档，CDR↔ledger 逐分零漂移、PDD p99 65ms @20k，见 [capacity ladder](docs/bench/capacity-ladder-2026-11-26.md)）；另有 100CPS 全栈/20 万注册既有实测（[bench10 2026-11-22](docs/bench/bench10-report-2026-11-22.md)）；300CPS+ 需 loadgen 与被测栈分离复测；**DID 呼入终结（被叫方计费）已交付并 e2e 验证**（[设计文档](docs/did-inbound-design.md)）
- 多副本运行时状态（余额/并发/CPS/风控滑窗/套餐用量/注册镜像）已收口 Redis（`GVOS_REDIS_URL`；未配置 = 单副本内存语义）；计费门 fail-closed，风控/套餐可用 `GVOS_RISK_MODE` / `GVOS_PKG_MODE` 切换。无 Redis 的 PG 模式：通话结算会同步扣减 `accounts.credit`（2026-11 修复），但充值/结算并发量大时仍建议配 Redis
- **gv-core 多副本已解禁（粘性 + 远程结算）**：热路径呼叫态仍在本机 map（Interim/拆线），生产须 **Call-ID LB 粘性**；粘性失效时 Redis `callmeta`/`callsettled` 兜底单次结算（架构压测 4000 通双结算 0）。结算锁按「Call-ID + 起呼时刻」区分通话实例（Call-ID 复用设备不再丢结算）
- 历史数据注意：**2026-11 修复前的 CDR/账本金额在 PG 里放大了一万倍**（`money.Amount` 以定点整数直写 NUMERIC）。修复后新写入正常；旧库需按 ÷10^4 修正或重建（对账口径见运维手册 §1.1）
- 管理令牌默认 `dev-admin-token`、DB 密码 `gvos_dev`、NATS 演示口令 `gvos-demo-nats`——均为演示默认值，**生产部署必须更换**（见 [operations manual](docs/operations-manual.md)）；NATS 已启用用户名/口令认证（`GVOS_NATS_USER/PASSWORD`，k8s 走 `nats-auth` Secret）
- 生产节点需设 `net.core.rmem_default=4MB`（UDP 缓冲，实测 416KB 在 200CPS 丢 401）
- K8s 形态清单：RTPengine/Kamailio hostNetwork、节点 sysctl initContainer、安全组收敛 28080/Grafana
- TLS 证书未随仓库分发，部署时自签：`openssl req -x509 -newkey rsa:2048 -nodes -keyout kamailio-key.pem -out kamailio-cert.pem -days 365 -subj "/CN=gvos.local"`
- SRTP/DTLS：rtpengine 编译内置、配置面就绪，E2E 验证待媒体客户端（pjsua 或 WebRTC 软电话，规程见 [docs/srtp-dtls-verification-plan.md](docs/srtp-dtls-verification-plan.md)）
- admin/tenant API 已内置限流（默认 100 burst/20 rps，`GVOS_RATE_BURST`/`GVOS_RATE_RPS` 可调，0=关）；`ul.dump/ul.lookup` RPC 在 20 万条目下有缺陷（重注册数据完好）；dev 凭证一旦被使用会打 `gvos_dev_credentials_total` 指标并触发 critical 告警
- 套餐「最低消费」只有差额计算（`MinSpendShortfall`），周期收尾的自动补扣未实现

## 文档

- [架构设计](docs/02-architecture.md) · [部署规划](docs/deployment-plan.md) · [运维手册](docs/operations-manual.md) · [API 约定](docs/api-conventions.md)（含 Webhook 契约与版本规则）· [OpenAPI](http://127.0.0.1:28080/openapi.json)（运行时 `GET /openapi.json`）
- [竞品对标（昆石 VOS/SBC）](docs/competitive-analysis-vos-sbc.md)（功能差距 + 排期输入）
- [AXB 隐私号技术方案](docs/axb-privacy-number-design.md)（预留实现）· [录音 OSS 方案](docs/recording-oss-upgrade-plan.md)（R0–R3 部分交付）· [Patroni 参考拓扑](deploy/patroni/README.md)
- [Redis 收口方案](docs/redis-adoption-plan.md)（已实施 Phase 0-5）
- [Voice Agent 接入：Pipecat 原生 SIP（主路径）](docs/pipecat-integration-plan.md) · [LiveKit SIP（WebRTC/多方）](docs/livekit-integration-plan.md)（规划稿）
- 压测与容量：
  - [自测报告（130 项）](docs/bench/selftest-report-2026-11-19.md) · [验收报告 v0.3](docs/bench/acceptance-report-2026-10-07.md) · [RPC 档验收](docs/bench/rpc-tier-verification-2026-11-20.md)
  - [10 轮缺陷压测（11-20）](docs/bench/bench10-report-2026-11-20.md) · [架构补充后 10 轮（11-22）](docs/bench/bench10-report-2026-11-22.md) · [100CPS 修复收官](docs/bench/bench100-report-2026-11-20.md)
  - [容量基线 vs 裸 Kamailio](docs/bench/capacity-baseline-2026-11-20.md) · [天花板报告](docs/bench/ceiling-report-2026-11-21.md) · [云 ECS 天花板 + 20 万注册](docs/bench/cloud-ceiling-2026-11-21.md)
  - [渗透自测](docs/bench/pentest-selfaudit-2026-11-21.md) · [SRTP/DTLS 验证规程](docs/srtp-dtls-verification-plan.md)

## 参与

Issue / PR 均欢迎——计费语义变更请先写失败测试（见 [CONTRIBUTING](CONTRIBUTING.md)）。

## License

[Apache-2.0](LICENSE)
