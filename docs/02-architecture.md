# 技术架构设计:gvos

**版本**:v0.3(2026-11-22)
**v0.2→v0.3 修订**:多副本呼叫结算解禁(Call-ID 粘性 + Redis `callmeta`/`callsettled` remote-settle)、Mode A 叶子扣款、`GVOS_RISK_MODE`/`GVOS_PKG_MODE`、Kamailio gateway-snapshot 冷启动拉取、INVITE rtpengine 挪到 bill-lock resume。标记纪律不变:**✅ 已交付** / **⚠️ 交付但受限** / **🎯 目标**。
**上游依据**:PRD 容量基线(内部文档);竞品对标 [competitive-analysis-vos-sbc.md](competitive-analysis-vos-sbc.md)
**配套事实源**:NATS 转型 [nats-event-driven-upgrade.md](nats-event-driven-upgrade.md) · Redis 收口 [redis-adoption-plan.md](redis-adoption-plan.md) · 运维手册多副本 §6 [operations-manual.md](operations-manual.md) · API 体系 [api-roadmap.md](api-roadmap.md) · 录音管道 [recording-oss-upgrade-plan.md](recording-oss-upgrade-plan.md) · 部署 [deployment-plan.md](deployment-plan.md) · Voice Agent 接入 [pipecat-integration-plan.md](pipecat-integration-plan.md)（主路径）/ [livekit-integration-plan.md](livekit-integration-plan.md)（WebRTC/多方） · 压测 [bench10-report-2026-11-22.md](bench/bench10-report-2026-11-22.md)

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
│ NATS(事件总线,JetStream)✅ · Redis(运行时权威)✅     │
│ 对象存储(录音归档)🎯(recording-plan) · Prometheus    │
│ + Grafana ✅ · Patroni 🎯(PG 自动故障切换)            │
└────────────────────────────────────────────────────┘
```

技术选型(不变):Kamailio(25 年历史、原生 nats 模块——OpenSIPS 没有,这是选型决定性因素之一)、RTPengine(内核态转发+录音 fork)、Go 控制面(内存安全+goroutine 模型)、PG(NATS 桥接淘汰 Kafka:运维成本高,仅客户强制集成时加桥接层)。

**仓库形态**:Go 1.21+,零框架(标准库 ServeMux),vendor 含 pgx/go-redis/nats 等;docker-compose(单机)/compose-ha(双机)/Helm(K8s)三形态。

## 3. 状态权威矩阵(v0.2 新增——本系统的核心架构透镜)

"谁是某个状态的唯一权威"决定了一致性机制、故障语义与扩展上限:

| 状态类 | 当前权威 | 一致性机制 | 状态 |
|---|---|---|---|
| 配置(账户/费率/网关/IP 准入/黑名单/风控/套餐) | PG | 写 PG → `gvos.cfg.reload` 快照广播 → 60s 重播对账 | ✅ |
| CDR/账本 | PG 月分区 | `uq_cdr_call` 幂等 + 双路径持续对账 | ✅ |
| 事件 | NATS JetStream | durable 消费者 + NAK 重投 + MaxDeliver 防毒丸 | ✅ |
| **余额/预锁/并发/CPS/风控滑窗/套餐用量** | Redis(配置即开;未配置=内存单副本语义) | Lua 原子操作 + fail-closed 计费门;**Mode A 只扣叶子账户**;TopUp 绝对值写 + 小时回写 PG | ✅ |
| **呼叫结算元数据** | Redis `callmeta`/`callsettled` | Start 写 meta;peer End 可 remote-settle;SETNX 防双扣(TTL 2m) | ✅ 2026-11-22 |
| 注册镜像 | 进程内存 + Redis 持久镜像(ADR-0002) | reg 事件双写,hash TTL=expires;reg-lookup 兜底端点 | ✅ |

**结构性后果(已解除)**:"钱的权威 per-replica"曾是 gv-core 必须单副本的全部原因。✅ Redis Phase 0-5 + **跨副本结算**(callmeta)已交付:**多副本 gv-core 可部署**(须 Call-ID 粘性为主路径,见 §6)。**配置类明确不进 Redis**(PG+NATS 广播)。

**Mode A 叶子扣款**:`Reserve`/`Commit` 只动主叫叶子 prepaid 余额;上级代理保留 Concurrent/CPS 上限,不同额预锁。上级分润若需要走独立账本行。

**故障语义环境变量**:`GVOS_RISK_MODE=open|strict`(默认 open:风控 Redis 不可用则跳过);`GVOS_PKG_MODE=list_price|open`(默认 list_price:套餐 Redis 不可用则按列表价)。

金额表示:**✅ int64 定点化已落地**(ADR-0003)。多副本时钟纪律:🎯 单一时间源 + NTP ±50ms(ADR-0004)。注册状态:✅ ADR-0002。

## 4. 核心数据流

### 4.1 呼叫建立(计费锁定)——两种档位,部署时选择

**档位 A(NATS RPC)**:INVITE → `nats_publish(gvos.svc.call.start,载荷带 tindex/tlabel,reply=节点唯一主题)` → `t_suspend()`(worker 释放)→ gv-core 计费锁定 → 应答回 `gvos-edge-reply-<node>` → `t_continue` 恢复(网关重选在恢复路由内)→ 403/中继。✅ **2026-11-20 晚:compose 单机已启用并全链验收**——SIPp 10/10 通话成功,suspend→resume→dispatch 全链 **4ms**(3 节点集群上含 req/reply 往返);宕机兜底 `t_set_fr(5000,5000)`:总线全死时 INVITE 在 5s 收到 408 有界失败(实测精确 5.0s)。历史:compose 曾因"nats publish 静默丢"回退(commit 40065a4)——**根因当日定案并修复**(见下方修复清单),并非 Kamailio nats 模块缺陷。

**档位 B(HTTP 同步)**:Kamailio 直接 HTTP 调 `/v1/call/start`,阻塞 worker。✅ 可用,compose-ha/Helm 当前形态。children=16 / nats publish workers=4 为阻塞档位的容量缓解(300 CPS × 50ms 核心延迟 ≈ 15 worker-秒/秒);gv-core 侧热路径见 4.5。

| 部署形态 | WITH_NATS | WITH_NATS_CDR | WITH_NATS_RPC | NATS 拓扑 |
|---|---|---|---|---|
| docker-compose(单机) | 1 | 1 | **1(档位 A)** | **3 节点全互联,R3** |
| compose-ha(双机主力) | 1 | 1 | 未开(档位 B) | 单节点(集群化后升档) |
| Helm(K8s) | 1 | 1 | 未开(档位 B) | 外部 |

**RPC 档修复清单(2026-11-20,启用前置)**:
1. **静默丢根因(40065a4 的真身)**:gv-core 的 jsPoster 对无点边缘别名(`gvos-edge-*`,不在 stream subject 域)做 JetStream 发布 → 服务器不回 PubAck → 每条挂满 5s publishBudget,而单个 pump goroutine 串行排空队列 → 全部事件(含 CDR)堵在其后,"publisher 假死/静默吞"即此。修复:别名在 pump 内改走 corePoster(events.go `IsEdgeAlias`)——at-most-once 本就是别名的文档语义。
2. **Kamailio 缺 t_newtran()**:鉴权后的 re-INVITE 无事务上下文(`$T(id_index)=0:0`,挂起簿记不可用、请求载荷拼出非法 JSON)。修复:INVITE 路径在 BILL_LOCK 前显式建事务。
3. **t_continue 不能从 timer route 调用**(tmx 要求 SIP 消息上下文;实测 'cannot get transaction index'):janitor 从"503 释放者"降级为纯 GC(清 marker/ci/ans 家族),释放兜底移交 tm 自身 `t_set_fr(5000,5000)`。`t_set_fr_inv`(1 参)在此构建为 KEMI-only,`t_set_fr(fr, fr_inv)` 是脚本层形态。
4. 语法/逻辑小修:Kamailio 脚本无 `continue`、`else` 须独立行、if 块不能只含注释;`pv_www_authenticate` flags "0"→"2"(否则 HA1 被当明文密码再哈希,缓存鉴权 401 死循环——旧"stale-entry trap"的真相)+ 失败清缓存;tear 路由 `$ci`→`$var(cid)`(NAT 事件路由无 SIP 上下文,tag 查找永远落空)。

**NATS 3 节点集群(compose,2026-11-20 交付)**:**全互联路由**——星型(全部拨同一 seed)在 hub 死时脑裂(实测:nats-1 停后 nats-2/3 零路由、跨节点 RPC 无响应);全互联后 hub-down 演练通过、RPC 正常。JetStream 加 volume 持久化(此前容器重建即丢 durable 历史);stream R3(`GVOS_JS_REPLICAS`,对既有流 UpdateStream 收敛)。**Kamailio 边缘拨 DNS 轮询别名 `nats`**:模块 nats_url 只认单服务器字符串(逗号列表被当成一个主机名——实测;libnats natsOptions_SetServers 是唯一入口),集群 INFO 握手会把 mesh 作为 discovered servers 下发,单点重连即跨节点;别名重连时 DNS 再解析轮换成员(冷启动容错实测通过)。**运维注意:改 --routes 重启会孤儿化 R3 stream 状态 → 需全集群重启 + gv-core 重建流**(实测踩坑)。

**依赖声明(v0.2 收口口径,取代 v0.1 与 deployment-plan 的矛盾表述)**:gv-core → NATS 是**分档依赖**——RPC 档(档位 A)下 NATS 是呼叫建立关键依赖(宕机 = 新呼叫 5s 内 408 有界失败,不影响进行中通话);HTTP 档(档位 B)下 NATS 宕机只丢遥测/CDR 总线与配置镜像(60s 内收敛退化)。compose-ha 选 HTTP 档是历史保守选择;**升档前置已全部满足**(3 节点集群 + RPC 档 drill 通过,compose 单机已运行该形态),compose-ha 下一批次开闸。

**dev/生产形态(2026-11-20 晚修订)**:compose 单机 = 生产最小副本(3 节点集群 + 档位 A)——升档验证发生在 dev,交付物直接复用;此前"dev 停留在档位 B"的终态判断作废(升档验证需要真实形态承载)。

### 4.2 计费与拆线

- 通话中:interim driver 每 10s 扣减(callstate.go:60)→ 余额低于阈值 → `gvos.call.teardown` 广播 → 持有 dialog 节点 RPC 终结。✅ 代码就绪;⚠️ <1s 拆线延迟验收硬指标未测(nats-event-driven U3,P0)。
- 通话结束:BYE → `gvos.cdr.raw`(载荷含 rtpengine QoS 统计)→ CDRR durable 消费者(复用 callEnd 纯函数)→ PG。✅ e2e 实测。
- 🎯 QoS 统计(mos/jitter/packet_loss)解析填列(U6,列已建未填)。

### 4.3 配置下发(NATS 广播 + HTTP 冷启动拉取)✅

变更写 PG settings(重启兜底)→ `gvos.cfg.reload` 全量快照广播(网关/IP 准入/黑名单/费率/录音/风控/套餐)→ 各副本与 Kamailio 节点本地生效——**Kamailio 零 PG 依赖**。core NATS at-most-once → **启动立即重播** + 60s 周期重播;边缘另有 HTTP 对账:
- IP 准入:`GET /v1/internal/ipallow-snapshot`(30s 轮询)
- 网关:`GET /v1/internal/gateway-snapshot`(5s 轮询,`GW_POLL`)——关闭 Kamailio 重启后空表 503 窗口(bench N-3)

✅ LCR 成本路由:gateway.Prefixes/CostRate + 控制面按"费率最低→优先级"排序,LCR 序编码进广播 Priority。

### 4.4 风控与注册的边缘联动 ✅(代码就绪,e2e 待验证窗口)

风控告警(载荷带 SIP username)→ banned htable(1h 过期)→ 边缘 403 零控制面往返;注册事件 → gv-core 镜像 + reg_freq 规则;黑名单前缀 → bl htable 边缘 403(正则/白名单语义留控制面,双层防御)。Kamailio 订阅点号 subject 的词法限制:走 gv-core 双发无点别名(gvos-edge-cfg/-risk/-tear)。

### 4.5 呼叫建立热路径性能(2026-11-20 优化交付)

**问题**:`Manager.mu` 全局互斥锁内执行 4 次 Redis 往返(AdmitConcurrent/AdmitCPS 两 pipeline + Reserve Lua + StartChain pipeline),单副本 call-start 吞吐天花板 ≈ 1/锁持有时长;且准入检查与注册是两次独立往返,多副本下并发上限可超卖 1 路(TOCTOU)。

**交付**:
1. **AdmitCall 原子准入**(admission_redis.go):单 Lua 脚本一次往返完成"链上并发检查(带 cap 的 ZCARD+1)→ CPS INCR 检查 → 全链 ZADD 注册"。拒绝时不注册、并发拒绝不烧 CPS 槽(与旧两步语义一致);Redis 故障 = 准入整体失败(旧 StartChain 失败仅 log-and-continue,共享账本下语义收紧)。
2. **锁重构**(callstate.go Start/End):全部 Redis 往返移出 `m.mu`,临界区只剩 map 插入;Chain 只走一次,链错误(账户环)fail-closed;Reserve 失败回滚注册(EndChain,失败则 15min 僵尸 TTL 兜底);End 的账本清理同样出锁。
3. **CDR 环形缓冲**(api/callapi.go + server.go):`s.cdrs` 加 `cdrsMu`(修数据竞争——16 个 NATS responder 与 HTTP 读并发)且封顶 10000 条(PG 是历史权威,内存环是近期视图;U1 演练曾无界攒 19217 条)。
4. **Kamailio worker 扩容**(kamailio.cfg):children=4→16、nats publish workers=2→4(4.1 档位 B 注记)。

**基准**(loadgen 直打 gv-core /v1/call/start+end,compose 单机,i7-13700H,3 轮中位):

| 指标 | 基线 | 优化后 | 变化 |
|---|---|---|---|
| 微基准 Start+End(ledger 路径) | ~348µs/op(≈2,820 ops/s) | ~226µs/op(≈4,290 ops/s) | **-35% 延迟 / +52% 吞吐** |
| 端到端 300 CPS 档 p99 | 165.9ms | 136.1ms | -18% |
| 端到端 600 CPS 档 p99 | 350.6ms | 271.4ms | -23% |
| 端到端吞吐 | 268/476 CPS | 267/478 CPS | 持平(瓶颈已移出 gv-core 锁) |

注:端到端吞吐持平不是"无提升"——miniredis 微基准放大了锁收益的可见性,真实 Redis RTT 亚毫秒级;端到端 p99 的 18-23% 改善即锁队列消除的直接签名,剩余尾延迟来自 loadgen 自身 pacing 与 NATS JetStream 事件队列(1024 深,满载丢 CDR 事件,见 5 容量注记)。**单点锁天花板从 ~470 CPS 抬到 ~4,300 ops/s(微基准口径)**,多副本水平扩展(Redis 收口后)不再受此锁约束。

**验证**:`go test ./...` 全绿;容器内 `go test -race ./internal/...` 全绿(顺带修复 TestSweeperLoop 既有假时钟 race——基线同样报,非本次引入);新增测试:AdmitCall 语义 5 断言组、32 协程超卖回归(原子性)、reserve 失败回滚、CDR 环容量/并发。

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

## 6. 部署形态与可用性预算 / 多副本 gv-core

三形态详见 [deployment-plan.md](deployment-plan.md)。RPC 档关键路径可用性 ≈ P(NATS) × P(Redis)。

### 6.1 多副本解禁状态(v0.3)

| 步骤 | 状态 |
|---|---|
| Redis 收口(余额/准入/风控/套餐/注册) | ✅ |
| 跨副本结算(`callmeta` + `callsettled`) | ✅ |
| Call-ID LB 粘性(生产必配) | ⚠️ 部署层责任;缺省靠 remote-settle 兜底 |
| NATS 3 节点 R3(compose 单机) | ✅;compose-ha 升档待开 |
| Patroni | 🎯 |

**呼叫态分工**:
1. **主路径**:LB 按 Call-ID sticky,同一通话 Start/Interim/End 落同一副本(本机 map 持有 Interim/teardown)。
2. **兜底**:Start 写 `gvos:callmeta:{id}`;peer End 本地 miss → `ClaimSettle` + `Take` → `remote-settle`(日志 `REMOTE-SETTLE`)。`callsettled` TTL=2m 防双扣。
3. **验证**:架构压测 10 轮 ×400 通(50% 跨副本 End)双结算 0、叶子余额精确([bench10-report-2026-11-22](bench/bench10-report-2026-11-22.md))。

运维细则见 [operations-manual.md §6](operations-manual.md)。

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
| IP 准入 | 白名单 | CIDR 规则+默认拒绝;清空白名单需 `confirm_empty:true`(409 守卫,防手滑全网断话);冷启动 fail-open 有界:默认 90s(`GVOS_IPALLOW_BOOT_GRACE`)后翻 fail-closed,超时后每 60s 打 L_ERR 面包屑 | ✅ |
| **管理面鉴权** | API Key(Phase B) | `internal/shared/acl`,key 绑 (tenant,role),sha256 落库,一次性明文,bootstrap 从 GVOS_ADMIN_TOKEN;legacy 常量仅无 key 时回退(警告) | ✅ |
| **信令计费面鉴权** | 内部令牌 | `/v1/call/*` 走 `requireInternal`(fail-closed):`GVOS_INTERNAL_TOKEN` 未配置=503 拒新事件,令牌错=401;恒时比较;令牌永不落日志。边缘 http_connect 走 `?token=`,NATS 走 broker 认证 | ✅(文档曾滞后于代码,2026-11-26 修订) |
| 密码存储 argon2id | 有 | **无此实现**(管理面无密码存储;ha1 是协议内 MD5)| v0.1 失实,删除 |
| 审计表 | append-only | ✅ audit_log;operator = key prefix(sync+bus 双路) | ✅ |
| 防盗打默认 | 账户级上限 | ✅ 日/月消费上限、并发上限、异常封禁 | ✅ |
| License 闭源校验 | 有 | 未启动 | 🎯(等保/离线交付批次) |

**结论**:F4(信令面鉴权)已由 `requireInternal` 落地(上表已修订);剩余实缺口为管理面限流与审计身份细化,均在 [api-roadmap.md](api-roadmap.md) Phase C 内。SIP TLS/SRTP 的"全默认开启"叙事在验证完成前降级为"支持已配置,验证待做"。

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
| Redis 收口 | 多副本账务正确([redis-adoption-plan.md](redis-adoption-plan.md)) | ✅ Phase 0-5 |
| 跨副本结算 + Mode A | callmeta remote-settle + 叶子扣款 + risk/pkg 模式 | ✅ 2026-11-22 |
| LCR 成本路由 | 竞研唯一功能开发缺口([competitive-analysis-vos-sbc.md](competitive-analysis-vos-sbc.md) §3.1) | ✅ |
| 录音管道 | WAV/MP3 + OSS + 分轨 + presigned([recording-oss-upgrade-plan.md](recording-oss-upgrade-plan.md) R0-R4) | 🎯 方案定稿 |
| M18 | WebSocket 媒体流 beta(<500ms 端到端) | 🎯 |

CI 硬门槛的诚实口径:v0.1 列了覆盖率/trivy/SIPp 冒烟/模糊测试,**实际 CI 仅 vet+test+build(hermetic)**。🎯 分批补齐:U 系列 e2e 进 testcontainers(阶段 A/B)→ 对账回归门 → 覆盖率与镜像扫描(M12 前)。

---

**变更记录**:2026-10-06 v0.1 初稿 · 2026-11-19 v0.2 全量修订 · 2026-11-19 v0.2.1 Redis/ADR-0003/LCR/API A-B · **2026-11-22 v0.3 多副本结算解禁 + Mode A + gateway-snapshot + rtpengine resume**。
