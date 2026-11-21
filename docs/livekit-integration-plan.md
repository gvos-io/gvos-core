# Voice Agent 集成方案（二）：LiveKit SIP——WebRTC/多方场景

**状态**：规划稿（未实施；含 2026-11-20 已核实的上游事实）
**日期**：2026-11-20（v2：同步 Pipecat spike 验证结论）
**范围**：把自托管 LiveKit SIP 挂进 G-VOS 网关模型（Kamailio → rtpengine → livekit-sip → room → agent worker），覆盖：选型边界、呼出/呼入链路、agent 接入两条技术栈、转人工、录音取舍、部署容量。
**关联**：[pipecat-integration-plan.md](pipecat-integration-plan.md)（**主推荐路径：Pipecat SIPTransport 直连，Spike 已 e2e 验证**——安装方式/五个上游坑/CDR 证据见其 §1.2/§3/§2；平台侧四个硬前提见其 §0，本文不重复）· [02-architecture.md](02-architecture.md) §4 · [recording-oss-upgrade-plan.md](recording-oss-upgrade-plan.md)

---

## 0. 何时选 LiveKit（与 Pipecat 直连的选型边界）

Pipecat 自带 `SIPTransport` 后，**电话↔agent 单人通话不再需要 LiveKit**（少一跳组件、少一次编解码往返，且该路径已 e2e 验证——60s 计费通话落 CDR，见 [pipecat-integration-plan.md](pipecat-integration-plan.md) §1.1/§2）。LiveKit 在以下场景仍是正确选型：

| 场景 | 为什么需要 LiveKit | Pipecat 直连为什么不够 |
|---|---|---|
| 浏览器/WebRTC 参与者进同一通通话（网页客服、可視化面板） | room = 天然多方容器；WebRTC（Opus/DTLS/ICE）由 LiveKit 承担 | G-VOS 媒体面是 rtpengine **转发**不是混音器；SIP 侧与浏览器侧接不到一起 |
| 多路混合（caller + agent + 人工客服同房间） | room 参与者模型 + 房间级动态加入 | SIPTransport 一实例一通话，无房间语义 |
| agent 弹性扩缩走生态 | livekit-agents worker 按房间自动调度 | Pipecat 直连扩缩 = 手工网关条目管理（§3 对比） |
| 房间级录制/Egress | Egress 输出含 WebRTC 参与者的完整房间录像 | rtpengine fork 只见 SIP leg（取舍见 §8） |

**结论**：两条方案不是二选一，是**同一套 G-VOS 网关模型下的两个目的地类型**——普通 agent 通话走 Pipecat 直连（[pipecat-integration-plan.md](pipecat-integration-plan.md)），要进房间的通话把目的地指派到 livekit-sip。pipeline（VAD/ASR/LLM/TTS/打断）两侧可共用同一套 Pipecat 代码，只换 transport（SIPTransport ↔ LiveKit transport）。

## 1. 上游调研结论（2026-11-20 已核实，含出处）

来源：[livekit/sip](https://github.com/livekit/sip)（GitHub README）、[LiveKit 端口与防火墙](https://docs.livekit.io/transport/self-hosting/ports-firewall)、[SIP trunk 配置](https://docs.livekit.io/telephony/start/sip-trunk-setup)、[冷转接（SIP REFER）](https://docs.livekit.io/telephony/features/transfers/cold)。配置字段名以所选版本 README 为准。

| 机制 | 事实 | 对 G-VOS 的意义 |
|---|---|---|
| 端口面 | SIP 信令 **UDP 5060**（`sip_port` 可配）+ 媒体 **UDP 10000-20000**；生产建议 SIP-TLS | 容量预算口径 = 端口面；防火墙/安全组按此开 |
| 容器形态 | 默认大 UDP 端口段在 Docker 网络下难处理，**官方建议 `--network host`** | compose/K8s 形态需为 livekit-sip 单独处理网络模式 |
| 依赖 | livekit-sip 必须连 **LiveKit server + Redis** | G-VOS 栈内已有 Redis（redis-adoption Phase 0-5），**分实例或分 DB** 复用，避免 key 域冲突（见 §10） |
| trunk/dispatch | inbound trunk（号码/认证/host 匹配）→ dispatch rule（individual caller = 每caller一个 room / direct = 固定 room）；outbound trunk 用于 livekit-sip 向外呼出（本文主场景用不到，转接除外） | G-VOS 发起的 agent 通话，对 livekit-sip 而言是 inbound trunk 命中 + dispatch rule 建 room |
| agent 派遣 | room config `agents`（自动）或 explicit dispatch API | 按 DID/租户分 room 派不同 agent |
| 转接 | **冷转 = SIP REFER**（`transfer_sip_participant`），warm/bridge 模式另有模式 | 转人工原语可用；REFER 落 G-VOS 边缘，目标由 DID 路由接住（§7） |
| 版本节奏 | livekit/sip 迭代快，dispatch/trunk API 有过 breaking 调整（issue [#256](https://github.com/livekit/sip/issues/256) 类问题） | spike 锁版本；trunk/dispatch 配置脚本化以便升级迁移 |

## 2. 目标拓扑

```
                    ┌─ 信令: Kamailio (5060) ────────┐
租户/坐席/运营商 ─INVITE─►                            │ BILL_LOCK —— 计费/风控/CDR 不变
                    └─ 媒体: rtpengine ◄────────────┘  (录音 fork 挂点在此 leg)
                                      │
                                      │ SIP + RTP (PCMU)
                                      ▼
                     livekit-sip (host net, :5060, UDP 10000-20000)
                         │ inbound trunk(G-VOS) + dispatch rule
                         ▼
                     LiveKit room（每 caller 一个 room 或 direct room）
                         ▼
                     agent worker：livekit-agents（房间级扩缩）
                        或 pipecat（LiveKit transport，同一套 pipeline）
```

要点：钱面不动（同 [pipecat-integration-plan.md](pipecat-integration-plan.md) §2）——agent 通话就是一通普通话务；teardown 强拆对 LiveKit leg 同样成立（`gvos.call.teardown` → `dlg.terminate_dlg` 拆的是 G-VOS↔livekit-sip 的 dialog，room 随 SIP 拆线终止）。

## 3. G-VOS 侧配置：livekit-sip 就是网关

```json
POST /v1/admin/gateways
{"gateways":[
  {"id":"livekit-1","name":"livekit-sip","sip_addr":"livekit-sip:5060","priority":0,"enabled":true}
]}
```

- 经 `gvos.cfg.reload` 广播 → `gw` htable → `GW_DISPATCH` 零新边缘代码。
- **与 Pipecat worker 池的结构差异**：worker 池 = N 端口 N 条目（per-worker 熔断免费）；livekit-sip = **单端点**，熔断粒度是整个 livekit-sip 实例。要 per-实例熔断就跑多实例多条目（livekit-sip 自身水平扩，Redis/session 共享）。
- `Prefixes`/`CostRate` 留空：LiveKit 目的地同样不走 LCR 成本序，按 Phase 2 `gw_id` 指派（同 [pipecat-integration-plan.md](pipecat-integration-plan.md) §5.2）。

## 4. 呼出链路（电话 → agent）

1. 租户分机拨打 agent 号段 → 边缘流水线照常（BILL_LOCK 以 from_user=租户账户锁定，锁账成立）。
2. `GW_DISPATCH` → livekit-sip:5060。对 livekit-sip 而言这是 **inbound trunk 命中**（按 host/号码匹配 G-VOS trunk 配置）。
3. dispatch rule 建 room（individual caller：room 名含主叫号码与随机串；或 direct room）→ agent 派遣（room config agents 自动加入，或 explicit dispatch 指定 agent）。
4. worker（livekit-agents 或 pipecat LiveKit transport）加入 room 应答 → 200 OK 经原路返回边缘 → `gvos.svc.call.answer` → interim 扣减。
5. BYE → `gvos.cdr.raw` → CDRR → PG。

**计费语义零改动**；livekit-sip 拒接/超时走现有 `GW_FAILOVER`（5 分钟熔断 + 重试下一条目）。

## 5. 呼入链路（carrier DID → agent）

链路与缺口同 [pipecat-integration-plan.md](pipecat-integration-plan.md) §5（DID 表 + 呼入账户口径 + `gw_id` 指派，两方案共用同一批 Phase 2 建设，本文不重复）。LiveKit 侧增量只有一条：**dispatch rule 按 DID 建/选 room**（trunk 配置里的号码匹配），使不同 DID 落到不同 agent/租户 room。

LiveKit Cloud 选项：托管版免去自托管运维（host 网络/端口面/升级），但数据出域 + 按量计费，与 G-VOS 私有化交付姿态冲突——**默认自托管**；客户明确要求托管降级时才开。

## 6. agent 接入两条技术栈（pipeline 复用）

- **livekit-agents**：worker 常驻订阅 room（agent_name），房间级自动扩缩是它的主场；ASR/LLM/TTS 按其 service 生态选。
- **Pipecat + LiveKit transport**：与 [pipecat-integration-plan.md](pipecat-integration-plan.md) §7 的 pipeline **同一套代码**，构造器换 `LiveKitTransport`。当产品要「同一 agent 既接电话也接网页」时，这是唯一不用维护两套 pipeline 的路径。

选择规则：只用房间扩缩生态 → livekit-agents；要跨 SIP/WebRTC 统一 pipeline → pipecat transport。DashScope 适配约束（官方未内置，两条路径同 [pipecat-integration-plan.md](pipecat-integration-plan.md) §1.3/§7）在 livekit-agents 侧同样成立（自写 service 或社区插件）。

## 7. 转人工（Phase 3，依赖 DID 路由）

- agent 判定转人工 → `transfer_sip_participant` 冷转（[官方文档](https://docs.livekit.io/telephony/features/transfers/cold)）：livekit-sip 向 SIP leg（即 G-VOS 边缘）发 **REFER** → in-dialog loose_route 分支（`kamailio.cfg:246-274`）转发 → G-VOS 按 refer 目标（坐席 DID）路由接住。
- warm/bridge 转接模式（先咨询后拼接）涉及多方房间操作，Phase 3 单独设计。
- 新话单新计费、两段时长分列的语义与 [pipecat-integration-plan.md](pipecat-integration-plan.md) §6 相同（两方案共用 O3 开放问题）。
- 开放问题：REFER 经 rtpengine 的媒体重锚定（转接目标应答后 rtpengine 重协商）需 e2e 验证，spike 列入。

## 8. 录音取舍：fork 权威不动，Egress 是补充

- **权威不变**：rtpengine 在 G-VOS↔livekit-sip leg 上 fork（`record:true` → `start_recording()`，既有链路），拿到的是**全对话混合**（caller+agent 两方向都在这条 leg 上）——话务商验收口径的录音产物，与 [recording-oss-upgrade-plan.md](recording-oss-upgrade-plan.md) 管道直接衔接。
- **LiveKit Egress**（房间级录制）产出含 WebRTC 参与者的完整录像，但产物落在 LiveKit 侧、需拉回归档，且计费口径是 LiveKit 的——**仅作房间级补充，不作钱面录音权威**。
- 分轨（坐席/用户分开）仍走 recording-oss 方案的 rtpengine `--output-single`/`mix-method=channels`，与 LiveKit 无关。

## 9. 部署与容量

- **compose**：新增 `livekit`、`livekit-sip` 服务；livekit-sip 按[官方建议](https://github.com/livekit/sip) `--network host`（UDP 5060 + 10000-20000 端口段不适合常规 Docker 映射）；K8s 用 hostNetwork pod 或 NodePort+外部 LB 按 UDP 段扩。
- **Redis 复用纪律**：livekit 的 Redis 与 gv-core 的 Redis **分实例或分 DB index**（redis-adoption-plan 已划定 gv-core key 域所有权，跨组件混用会破坏 key 域纪律）；NATS 无 livekit 依赖。
- **容量预算口径**：并发通话数 × 媒体端口对（UDP 10000-20000 ≈ 单实例 5k 路 SDP 上限口径，实际以 livekit server 容量压测为准，spike 记录基线）。
- **安全**：livekit-sip ↔ G-VOS 全内网；公网仅暴露 Kamailio（同 [pipecat-integration-plan.md](pipecat-integration-plan.md) §8）；生产开 SIP-TLS；LiveKit API key 走 livekit 侧配置，不进 gv-core settings。

## 10. 里程碑与验证点

| 阶段 | 内容 | 出口 |
|---|---|---|
| Spike | 单机 compose（host net）livekit+livekit-sip+agent worker；SIPp 打通（对齐 [pipecat-integration-plan.md](pipecat-integration-plan.md) 已验证的 SIPp 场景与网关挂载法） | CDR 覆盖 answer/BYE/时长；teardown 强拆对 LiveKit leg 生效 |
| Phase 1 | dispatch rule 按 caller 建 room + livekit-agents 接入 + 录音 fork 验证 | 多 agent room 隔离实测；录音按 call-id 关联 |
| Phase 2 | 共用 DID 表/账户口径/`gw_id` 指派（与 Pipecat 方案同一批建设） | carrier 呼入→LiveKit room 全链路计费闭环 |
| Phase 3 | 冷转 REFER 经 rtpengine 重锚定 e2e + 房间级报表 | 转接两段话单分列；网页参与者通话跑通（本方案独有出口） |

## 11. 红线与边界

1. **电话↔agent 单人通话默认走 Pipecat 直连**（[pipecat-integration-plan.md](pipecat-integration-plan.md)）；LiveKit 只在需要房间语义时引入——不为单人通话多背两个常驻组件。
2. **录音权威在 rtpengine**，Egress 不替代 fork（§8）。
3. **钱面不动、Pipecat/livekit-agents 不碰 gv-core API**（同 Pipecat 方案 §11）。
4. **Redis 分实例/分 DB**，key 域纪律不破（§9）。
5. DID 账户口径不跳过（两方案共用）。

## 参考

- [livekit/sip GitHub](https://github.com/livekit/sip) · [端口与防火墙](https://docs.livekit.io/transport/self-hosting/ports-firewall) · [SIP trunk 配置](https://docs.livekit.io/telephony/start/sip-trunk-setup) · [冷转接（SIP REFER）](https://docs.livekit.io/telephony/features/transfers/cold) · dispatch/trunk 自托管配置讨论 [issue #256](https://github.com/livekit/sip/issues/256)
- 平台侧事实与 Phase 2 建设：[pipecat-integration-plan.md](pipecat-integration-plan.md) §0/§5（2026-11-19/20 核实）
