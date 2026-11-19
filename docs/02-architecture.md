# 技术架构设计:G-VOS

**版本**:v0.2(2026-11-19)
**v0.1→v0.2 修订**:v0.1(2026-10-06,M8 时代)事实与承诺混写,读者无法分辨哪些已兑现。本版建立标记纪律:**✅ 已交付**(代码+测试为证,注明位置)、**⚠️ 交付但受限**(注明限制)、**🎯 目标**(未实施,注明载体文档)。每条声明可对到代码或规划文档,不再有第三种状态。
**上游依据**:PRD 容量基线(内部文档);竞品对标 [competitive-analysis-vos-sbc.md](competitive-analysis-vos-sbc.md)
**配套事实源**:NATS 转型 [nats-event-driven-upgrade.md](nats-event-driven-upgrade.md) · Redis 收口 [redis-adoption-plan.md](redis-adoption-plan.md) · API 体系 [api-roadmap.md](api-roadmap.md) · 录音管道 [recording-oss-upgrade-plan.md](recording-oss-upgrade-plan.md) · 部署 [deployment-plan.md](deployment-plan.md) · Voice Agent 接入 [pipecat-integration-plan.md](pipecat-integration-plan.md)（主路径）/ [livekit-integration-plan.md](livekit-integration-plan.md)（WebRTC/多方）

---

## 1. 架构总原则

1. **控制平面与数据平面分离**——代际优势与研发预算分配原则:信令/媒体用开源组件(Kamailio/RTPengine,不重造协议栈);计费/路由/租户/风控/CDR 全部自研(传统一体机最难被验证和替换的 80%)。✅ 已按此交付(自研面 internal/ 16 包;开源面 deploy/)。
2. **API-first**:管理台只是 REST API 的一个消费者。⚠️ API 面已可用但未成契约(见 §8 安全与 [api-roadmap.md](api-roadmap.md) 的十项缺口)。
3. **无状态信令、可水平扩展媒体**:✅ Kamailio 零 PG 依赖、配置/黑名单走 NATS 镜像(60s 重播对账);媒体直通模式信令面故障不影响进行中通话。
4. **计费零丢失**:CDR 与账本双路径独立持久化、`uq_cdr_call` 幂等落库、持续对账——✅ 6,714 行逐分零漂移(capacity-report-2026-11-19 §S6)。🎯 大规模长稳对账进 CI 回归门(nats-event-driven 阶段 B)。

## 2. 组件拓扑

```
┌────────────── 管理面(自研,⚠️ 未成契约) ──────────────┐
│  Web 管理台(ui.go,单页) ── REST API(gvos-core :8080)   │
│  🎯 OpenAPI 3.1 + Webhook 引擎(api-roadmap Phase E/D)   │
└──────────────────────┬──────────────────────────┘
┌────────────── 控制平面(自研 Go,✅) ───────────────────┐
│ 账户树/费率/风控/套餐/CDR 管道 gvos-core(计费/账户/风控/API) │
│ gvos-asr(DashScope ASR 桥接) · gvos-migrate(VOS 话单迁移)  │
└──────┬────────────────────────────┬─────────────────┘
       │ NATS 事件总线 gvos.>(JetStream)│ internal token(信令同步面)
┌──────▼────────────────────────────▼─────────────────┐
│ 数据平面(开源):Kamailio ×N(信令/认证/选路)           │
│ RTPengine ×M(媒体转发/录音 fork;直通模式)             │
│ FreeSWITCH B2BUA:🎯 未启动(v0.1 图有,无代码无规划认领,│
│ 客户需求出现时立项)                                    │
└──────────────────────────────────────────────┬─────┘
┌──────────────────────────────────────────────▼─────┐
│ PostgreSQL(配置/账务/CDR 权威,月分区)✅              │
│ NATS(事件总线,JetStream)✅ · Redis 🎯(redis-plan)   │
│ 对象存储(录音归档)🎯(recording-plan) · Prometheus    │
│ + Grafana 🎯(M15) · Patroni 🎯(PG 自动故障切换)       │
└────────────────────────────────────────────────────┘
```

技术选型(不变):Kamailio(25 年历史、原生 nats 模块——OpenSIPS 没有,这是选型决定性因素之一)、RTPengine(内核态转发+录音 fork)、Go 控制面(内存安全+goroutine 模型)、PG(NATS 桥接淘汰 Kafka:运维成本高,仅客户强制集成时加桥接层)。

**仓库形态**:Go 1.21,零框架(标准库 ServeMux),vendor 仅 pgx v5.6;~10.6k 行 Go;docker-compose(单机)/compose-ha(双机)/Helm(K8s)三形态;无 git remote(本地私有仓)。

## 3. 状态权威矩阵(v0.2 新增——本系统的核心架构透镜)

"谁是某个状态的唯一权威"决定了一致性机制、故障语义与扩展上限:

| 状态类 | 当前权威 | 一致性机制 | 状态 |
|---|---|---|---|
| 配置(账户/费率/网关/IP 准入/黑名单/风控/套餐) | PG | 写 PG → `gvos.cfg.reload` 快照广播 → 60s 重播对账 | ✅ |
| CDR/账本 | PG 月分区 | `uq_cdr_call` 幂等 + 双路径持续对账 | ✅ |
| 事件 | NATS JetStream | durable 消费者 + NAK 重投 + MaxDeliver 防毒丸 | ✅ |
| **余额/预锁/并发/CPS/风控滑窗/套餐用量** | Redis(配置即开;未配置=内存单副本语义) | Lua 原子链上操作 + fail-closed 计费门;TopUp 绝对值写 + 小时回写 PG 灾备点 | ✅ Phase 0-5 已交付(2026-11-19,compose 冒烟验证) |
| 注册镜像 | 进程内存 + Redis 持久镜像(ADR-0002) | reg 事件双写,hash TTL=expires;reg-lookup 兜底端点 | ✅ |

**结构性后果(已解除)**:"钱的权威 per-replica"曾是 gv-core 必须单副本的全部原因。✅ Redis 收口 Phase 0-5 已交付(2026-11-19):余额 Lua 原子权威、准入链镜像 ZSET 账本、风控滑窗、套餐用量、注册镜像全部收口;fail-closed 只给计费门(503 BILLING_UNAVAILABLE),其余 fail-open(本地回退仍是真引擎)。**多副本账务正确性前提就位**——多副本部署解锁(升档顺序见 §6)。**配置类明确不进 Redis**(换传输层无正确性收益,redis-plan §1 非目标,未变)。

金额表示:**✅ int64 定点化已落地**(ADR-0003:money.Amount 1e-4 元具名类型,计费链/PG NUMERIC/JSON decimal 字符串全链路;VOS 语义断言 66s=0.0550、6714 通零漂移)。多副本时钟纪律:🎯 单一时间源 + NTP ±50ms 预算(ADR-0004,验证批次收口)。注册状态:✅ 边缘本地 + Redis 持久镜像兜底(ADR-0002,usrloc db_mode=0 不变,已交付)。

## 4. 核心数据流

### 4.1 呼叫建立(计费锁定)——两种档位,部署时选择

**档位 A(NATS RPC,compose 主力已开)**:INVITE → `nats_publish(gvos.svc.call.start,载荷带 tindex/tlabel,reply=节点唯一主题)` → `t_suspend()`(worker 释放)→ gv-core 计费锁定 → 应答回 `gvos-edge-reply-<node>` → `t_continue` 恢复(网关重选在恢复路由内)→ 403/中继。兜底:1s janitor 扫挂起 >2s 的锁定 → 503 Billing Unavailable。✅ e2e 实测通过(挂起→RPC→恢复→选路→计费闭环)。

**档位 B(HTTP 同步,compose-ha/Helm 当前形态)**:Kamailio 直接 HTTP 调 `/v1/call/start`,阻塞 worker。✅ 可用,但 worker 占用与 HTTP 失败语义不同档。

| 部署形态 | WITH_NATS | WITH_NATS_CDR | WITH_NATS_RPC |
|---|---|---|---|
| docker-compose(单机) | 1 | 1 | **1** |
| compose-ha(双机主力) | 1 | 1 | **未开(档位 B)** |
| Helm(K8s) | 1 | 1 | **未开(档位 B)** |

**依赖声明(v0.2 收口口径,取代 v0.1 与 deployment-plan 的矛盾表述)**:gv-core → NATS 是**分档依赖**——RPC 档(档位 A)下 NATS 是呼叫建立关键依赖(宕机 = 新呼叫 2s 内 503 有界失败,不影响进行中通话);HTTP 档(档位 B)下 NATS 宕机只丢遥测/CDR 总线与配置镜像(60s 内收敛退化)。 compose-ha 选 HTTP 档是当前有意的保守选择(避免 HA 交付形态叠加 NATS 单点),NATS 3 节点集群化后升档(deployment-plan §3.2 缺口 1)。

### 4.2 计费与拆线

- 通话中:interim driver 每 10s 扣减(callstate.go:60)→ 余额低于阈值 → `gvos.call.teardown` 广播 → 持有 dialog 节点 RPC 终结。✅ 代码就绪;⚠️ <1s 拆线延迟验收硬指标未测(nats-event-driven U3,P0)。
- 通话结束:BYE → `gvos.cdr.raw`(载荷含 rtpengine QoS 统计)→ CDRR durable 消费者(复用 callEnd 纯函数)→ PG。✅ e2e 实测。
- 🎯 QoS 统计(mos/jitter/packet_loss)解析填列(U6,列已建未填)。

### 4.3 配置下发(NATS 广播驱动)✅

变更写 PG settings(重启兜底)→ `gvos.cfg.reload` 全量快照广播(网关/IP 准入/黑名单/费率/录音/风控/套餐)→ 各副本与 Kamailio 节点本地生效——**Kamailio 零 PG 依赖**(dispatcher db_url 已删)。core NATS at-most-once → 60s 周期重播即对账心跳(节点重启实测 60s 内 htable 自愈)。费率变更 60s 内全节点生效。✅ LCR 成本路由:gateway.Prefixes/CostRate + 控制面按"费率最低→优先级"排序,LCR 序编码进广播 Priority 字段(边缘 GW_DISPATCH 零改动);费率 upsert 即时重播路由序(competitive-analysis §3.1 唯一功能缺口,已关闭)。

### 4.4 风控与注册的边缘联动 ✅(代码就绪,e2e 待验证窗口)

风控告警(载荷带 SIP username)→ banned htable(1h 过期)→ 边缘 403 零控制面往返;注册事件 → gv-core 镜像 + reg_freq 规则;黑名单前缀 → bl htable 边缘 403(正则/白名单语义留控制面,双层防御)。Kamailio 订阅点号 subject 的词法限制:走 gv-core 双发无点别名(gvos-edge-cfg/-risk/-tear)。

## 5. 容量与性能预算

**实测基线(capacity-report-2026-11-19,控制面,双副本 loadgen)**:

| 指标 | 实测 | v1.0 目标 | 差距性质 |
|---|---|---|---|
| 稳态吞吐 | 100 CPS × 30s 零失败;锁定 p50=0.8ms/p99=2.9ms | 300 CPS(单节点) | 容量验证缺口 |
| 并发 | 1,000 并发零失败,内存线性(+9MB) | 2,000(单节点) | 同上 |
| 突发 | 1,245 CPS burst 零失败;CDRW 2.4s 追平 | 300 CPS 稳态 | 反超 |
| 对账 | 6,714 行逐分零漂移 | 100% 回放校验 | 达标口径 |
| SIP 全栈 | 未在本轮重压(v0.3 报告 2,000 并发/300 CPS 结论沿用,镜像未拉) | — | 复测列 v0.5 |

⚠️ 压测可复现性缺口:loadgen 不在仓库内(bench 报告引用但仓内无源码)——§8"可复现性叙事"落地前必须补齐(loadgen 入仓或换 SIPp 脚本,scripts 目录)。

**集群目标(6,000 并发)**:多副本账务前置(Redis 收口)已就位,剩 SIP 全栈复测与多副本实测(验证批次);20 万注册量级 regtable 具备但未验证。

## 6. 部署形态与可用性预算

三形态详见 [deployment-plan.md](deployment-plan.md)(定稿)。v0.2 补一层**关键路径复合可用性**视角:RPC 档下预付费呼叫建立可用性 = P(NATS 存活) × P(Redis 存活,Redis 收口后)。compose-ha 当前 NATS 单节点 + gv-core 单副本,故:

- 当前 compose-ha 呼叫可用性绑定 = PG 主备(演练 RTO/RPO 有证据)+ NATS 单点 + gv-core 单副本;
- **多副本解禁顺序**:Redis 收口(解锁 gv-core N>1)→ NATS 3 节点(解锁 RPC 档进 HA)→ Patroni(RTO<60s 自动切换,客户档位驱动);
- 每一步升档都应重算上表乘积并在交付报告里披露——这是"诚实容量叙事"的可用性版本。

## 7. HA 与容灾

- PG:✅ 双机流复制(compose-ha)+ 手动 promote(failover 是设计如此,对齐话务商"拔电源"验收心智;drill.sh 输出 RTO/RPO 报告,每次交付必跑)。🎯 Patroni 三节点同步复制。
- Redis:🎯 主从+Sentinel,余额/会话 30s 内恢复(RPO≤1 扣减窗口,CDR 回放补账)。
- Kamailio:✅ 无状态节点池(双节点在 compose-ha,前置任意 LB/DNS SRV;生产前缺口=接入层)。
- 媒体直通:✅ Kamailio 宕机不影响进行中通话(架构性容灾优势)。
- 双机热备套餐 = 2 节点精简打包 + 演练脚本 + 交付视频(销售证据)。✅ 载体就绪。
- ⚠️ HA/K8s 形态录音链路缺失(compose-ha 无 rtpengine;recording-plan R4 补齐)。

## 8. 安全设计(逐项核对——v0.1 此节与代码差距最大)

| 项 | v0.1 声明 | 现实 | 状态 |
|---|---|---|---|
| SIP TLS | 全默认开启 | kamailio.cfg:4444 WSS TLSv1.2 ✅;5060 明文 UDP 仍是主通道 | ⚠️ |
| SRTP/DTLS | 全默认开启 | rtpengine 支持但未见端到端验证与显式配置 | ⚠️ 待验证 |
| Digest 认证 | Password | ha1 MD5(SIP Digest 协议约束,auth_db)| ✅ |
| IP 准入 | 白名单 | CIDR 规则+默认拒绝;⚠️ 首轮快照到达前短暂 fail-open | ✅/⚠️ |
| **管理面鉴权** | API Key(Phase B) | `internal/shared/acl`,key 绑 (tenant,role),sha256 落库,一次性明文,bootstrap 从 GVOS_ADMIN_TOKEN;legacy 常量仅无 key 时回退(警告) | ✅ |
| **信令计费面鉴权** | (隐含可用) | `/v1/call/*` 仅 requirePOST,`main.go:251` | ❌ F4 |
| 密码存储 argon2id | 有 | **无此实现**(管理面无密码存储;ha1 是协议内 MD5)| v0.1 失实,删除 |
| 审计表 | append-only | ✅ audit_log;operator = key prefix(sync+bus 双路) | ✅ |
| 防盗打默认 | 账户级上限 | ✅ 日/月消费上限、并发上限、异常封禁 | ✅ |
| License 闭源校验 | 有 | 未启动 | 🎯(等保/离线交付批次) |

**结论**:安全面的实缺口集中于三项(F4:常量凭据/信令面鉴权/审计身份),修复全部在 [api-roadmap.md](api-roadmap.md) Phase A+B 内,无需新规划。SIP TLS/SRTP 的"全默认开启"叙事在验证完成前降级为"支持已配置,验证待做"。

## 9. 事件总线契约与治理(v0.2 新增——客户对接前的必设门槛)

Subject 体系(`internal/events/events.go`):`gvos.cdr.completed/.raw`、`gvos.call.teardown`、`gvos.risk.alert`、`gvos.reg.event`、`gvos.admin.changed`、`gvos.cfg.reload`、服务域 `gvos.svc.call.start/.answer/.end`、`gvos.svc.stats`,边缘无点别名 ×3;JetStream stream GVOS 持久化 `gvos.>` 正名域。

⚠️ 载荷是**无版本裸 JSON**。今天成本为零;[api-roadmap.md](api-roadmap.md) Phase D 将把载荷投影为客户 Webhook 契约,边缘别名机制又加字节级兼容约束,60s 重播+durable 意味旧载荷跨版本存活。**载荷演进规则必须在 Phase D 前成文**(并入 api-conventions.md):

1. 加字段:容忍(消费者必须忽略未知字段)。
2. 改字段名/语义:禁止原地改,走 v2 subject(如 `gvos.cdr.completed.v2`)→ 弃用期(双发+`Deprecation` 语义)→ 下线。
3. 破坏性变更前的兼容矩阵:边缘别名(字节一致)、durable 消费者(重放旧消息)、Kamailio jansson 解析(点路径,陷阱见 nats-event-driven §2.4)。

## 10. 关键风险与技术对策

| 风险 | 现状与对策 |
|---|---|
| Kamailio 集成复杂度 | ✅ 已被驯服(九个陷阱钉死,nats-event-driven §2.4);后续 Kamailio 侧改动的隐性成本主要在 cfg 词法/jansson |
| 计费精度纠纷 | ✅ 对账不变量在;🎯 金数据回放测试集进 CI(M12 迁移工具 beta 联动) |
| **"叙述领先于现实"速率差** | v0.2 本文档存在的理由:宪法/README/竞研描述的系统曾比代码领先半个版本;此后任何规划文档必须显式声明状态(已交付/目标),README 已知限制节维持诚实声明 |
| 关键路径证据最少 | RPC 面零 CI 覆盖、U1/U3 依赖人工"干净窗口";对策=验证批次结构化(进 testcontainers/CI,见 nats-event-driven 阶段 B/D) |
| 媒体面 spike 级 | 录音 fork 实测可用;生产级调优/大容量验证在路线图(README 已声明) |
| 4,000 CPS 突发(ICP-2) | CPS 瓶颈在信令分发不在媒体,Kamailio 节点池横向扩;v1.1 出集群包 |

## 11. 里程碑技术出口

| 里程碑 | 出口 | 状态 |
|---|---|---|
| M3 | 端到端 1 通计费通话 + 单测框架 | ✅ |
| M8 | 单节点压测报告 + compose 一键部署(验收 101 项 PASS) | ✅ |
| NATS 转型 | 三层全事件驱动(六项边缘改造 + 计费 RPC) | ✅ 代码;⚠️ e2e 验收 U1-U5 待窗口 |
| 首批交付 | compose-ha + RTO/RPO 演练 | ✅ 载体就绪 |
| M12(GA) | Helm 生产化 + 渗透测试 + 迁移工具 beta + NATS 集群化/SIP LB | 🎯 |
| M15 | API/Webhook 全覆盖 + 风控引擎 + Prometheus 面板([api-roadmap.md](api-roadmap.md) M-A~M-D = API 半壁) | ⚠️ M-A/M-B 已交付(Phase A/B);M-C/M-D 未开工 |
| Redis 收口 | 多副本账务正确(解锁扩容/集群容量目标,[redis-adoption-plan.md](redis-adoption-plan.md) M0-M4) | ✅ Phase 0-5 + 冒烟 |
| LCR 成本路由 | 竞研唯一功能开发缺口([competitive-analysis-vos-sbc.md](competitive-analysis-vos-sbc.md) §3.1) | ✅ |
| 录音管道 | WAV/MP3 + OSS + 分轨 + presigned([recording-oss-upgrade-plan.md](recording-oss-upgrade-plan.md) R0-R4) | 🎯 方案定稿 |
| M18 | WebSocket 媒体流 beta(<500ms 端到端) | 🎯 |

CI 硬门槛的诚实口径:v0.1 列了覆盖率/trivy/SIPp 冒烟/模糊测试,**实际 CI 仅 vet+test+build(hermetic)**。🎯 分批补齐:U 系列 e2e 进 testcontainers(阶段 A/B)→ 对账回归门 → 覆盖率与镜像扫描(M12 前)。

---

**变更记录**:2026-10-06 v0.1 初稿 · 2026-11-19 v0.2 全量修订(权威矩阵/档位矩阵/事件契约治理新增;失实项修正)· 2026-11-19 v0.2.1 Redis Phase 0-5 / ADR-0003 定点化 / LCR / API Phase A-B 交付翻牌。
