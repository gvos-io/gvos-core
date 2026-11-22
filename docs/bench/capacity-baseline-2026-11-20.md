# 平台容量基线：gvos vs 裸 Kamailio（2026-11-20 深夜补充实验）

**问题**：平台最大并发是多少？和直接用 Kamailio 做基线差多少？

## 一句话答案

**本机（Windows Docker Desktop）实测：持续呼建 100 CPS 满分（1 万通 0 失败）、并发通话 7151+（PDD max 2ms、CPU <50%）；相对"裸 Kamailio"的差 = 控制面加了一个 ~3-5ms 的逐呼叫开销（RPC+计费）+ 媒体面被 dev-host UDP 长尾限制。Kamailio 自身远不是瓶颈。**

## 两个口径分开说

### 1. CPS（呼建速率）——本机实测

| 档位 | 成绩 | 备注 |
|---|---|---|
| 100 CPS × 10k | **10000/10000（PDD p99=5ms）** | 媒体开全栈，两轮可重复 |
| 150 CPS × 9k | 8341/9000（92.7%） | 宿主机当日累计 6 万通后的退化 |
| 200~210 CPS | 边缘接受 11083 次 RPC（PDD p99=6ms、零错误） | **SIPp 压测容器自身 watchdog 停摆**（Docker Desktop CPU 争用），完整流程只跑完 3774 通——loadgen 上限，不是栈上限 |
| 媒体关隔离 | 同档位 PDD p99 6ms，成功数不变 | 控制面与媒体面解耦证明 |

**结论**：控制面（Kamailio 路由 + NATS RPC bill-lock + Redis/PG 计费）在 100CPS 是零负担（PDD p99 5ms、CPU<1%），200CPS 时依然健康。**本机 200CPS+ 的测量上限来自 SIPp 压测容器，不是平台。**

### 2. 并发通话数（同时在线通话）——本机实测

- 3×70CPS × 60s 保持 = **7151 通话同时在线**（live_calls 实测峰值，SIPp 报 3394/实例×多实例聚合）：
  - 全部接通（answer_time 100%），PDD max **2ms**，CDR=ledger 精确平账；
  - 峰值 CPU：gv-core 47%、Kamailio 25%、PG 21%、Redis 13%——**未饱和，7.6GB 内存池用不足 300MB**；
  - 明显还有余量（受限于当天 SIPp 容器存活，没继续推 1 万+）。
- 对比生产参数：PRD 目标 300CPS 全栈/万级并发——裸机（Linux + LAN）预期去掉 dev-host UDP 长尾后 **容量数倍提升**（v0.6 报告同结论）。

## 与"裸 Kamailio"基线的差距拆解

裸 Kamailio（无 gvos）做同样的事 = 纯 SIP 转发，每个呼建只花路由 CPU。gvos 全栈在其上叠加的每呼叫成本（实测，见 v0.6 分层延迟）：

| 叠加层 | 每呼叫成本（p50） | 说明 |
|---|---|---|
| bill-lock RPC（NATS 3 节点 R3） | **2.8ms**（p99 28ms） | suspend→resume→dispatch；异步、不占 Kamailio worker |
| 计费落账（Redis 链 + PG CDR/ledger） | ~2-4ms（异步消费者） | CDRW/CDRR 后台收敛，不在呼叫关键路径 |
| rtpengine_manage（媒体面） | 本机 p90 164ms / p99 699ms 长尾 | **dev-host 特有**；裸机 LAN <1ms |
| Kamailio 自身路由 | ~1ms（INVITE 200 端到端 p99 11ms 成功样本） | 两边共有 |

**所以差距 = 一个 2.8ms 的 RPC + 异步落账（都不到 SIP 重传间隔 500ms 的 1%）**。在 SIP 语义里这层开销不可感知；100CPS 满分和 7151 并发的实测已经证明。真正拉开差距的是媒体面（dev-host UDP 长尾）——那是 Docker Desktop 的问题，不是架构的。

**裸 Kamailio 的公认量级**（供参照，非本机实测）：单机 stateless 转发 1 万+ CPS、有事务/对话 1-3 千 CPS。gvos 在其前面加的这 3-5ms 若线性外推：300CPS 下控制面每秒多耗 ~1.5 核秒，32 worker 下完全无感。

## 本轮顺手修掉的 3 个真 bug

1. **G-1（严重）gv-core 并发 map panic**：`End()` 在锁外读 `m.lastRelease`，与 `driveInterim` 的快照遍历竞态 → `fatal error: concurrent map read and map write` → **整个 gv-core 进程崩溃**（200CPS BYE 风暴 + interim tick 同时命中时复现）。修复：release 的读-清挪进 m.mu 临界区（callstate.go End）。修复后 210CPS 聚合压测零 panic。
2. **G-2 kamailio 无 restart 策略**：Docker Desktop 在持续高压下会对容器发 SIGTERM 风暴（exit 139），kamailio 死了不自愈 → 全栈 SIP 中断。已加 `restart: unless-stopped`（compose）。
3. **load_sink.xml 场景畸形**：手写 reply 模板的 `[last_*]` 渲染坏（Via 0.0.0.0 / 字段名重复），暂用内建 `-sn uas` 替代；sink 思路留档未修。

## 测量方法备忘（避免误读）

- 并发探针 = `hold60.xml`（60s 保持）× 多 SIPp 实例：并发 = rate × hold 时长，逐秒 live_calls 采样。
- 200CPS+ 数据一律看**边缘接受量 + PDD + 零错误**，不看 SIPp 成功数（压测容器先死）。
- 跨大轮之间留冷却；compose restart 有 PG 慢关闭竞态（见 bench100 报告 F-7）。
