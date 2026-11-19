# Voice Agent 集成方案（一）：Pipecat 原生 SIP（SIPTransport）——主推荐路径

**状态**：规划稿（未实施；含 2026-11-20 已核实的上游事实与 2026-11-19 已核实的平台侧呼叫流事实）
**日期**：2026-11-20
**范围**：用 Pipecat `SIPTransport`（trunk 模式）把 AI agent 通话直接挂在 G-VOS 现有网关模型上——Kamailio → rtpengine → pipecat worker **直连**，不经 LiveKit。覆盖：呼出/呼入两条链路、worker 池与故障切换、转人工、计费语义、DashScope 适配、部署与验证。
**关联**：[livekit-integration-plan.md](livekit-integration-plan.md)（何时选 LiveKit：WebRTC 参与者/多方/房间语义）· [02-architecture.md](02-architecture.md) §4（呼叫数据流）· [recording-oss-upgrade-plan.md](recording-oss-upgrade-plan.md)（录音管道，录音权威不动）

---

## 0. 平台侧现状与差距（接入方案的四个硬前提，2026-11-19 核实）

以下事实直接决定本方案的形状，引用均为代码位置：

1. **边缘无呼入/呼出之分**：所有 INVITE 走同一流水线（`deploy/kamailio/kamailio.cfg:294-334`：IP 白名单 → 封禁 → 黑名单前缀 → Digest → BILL_LOCK → GW_DISPATCH → rtpengine）。每通电话都**向网关出站**。
2. **无 DID 呼入终结**：INVITE 路径从不 `lookup("location")`（usrloc 只有 REGISTER 侧 `save`，`kamailio.cfg:276-289`）。「PSTN → DID → 注册用户/agent」不存在；carrier INVITE 还会被 Digest 认证拦（trunk 无凭据）。
3. **GW_DISPATCH 全局选路**（`kamailio.cfg:633-663`）：按最低 priority 选**一个**网关，不做被叫前缀匹配。`lcrGateways`（`internal/api/server.go:678-702`）受「htable 一 id 一 prio」契约限制，per-prefix LCR 是各前缀族最优索引近似。
4. **呼入锁不了账**：BILL_LOCK 以 `from_user=$fU` 锁定（`kamailio.cfg:490/505`），carrier 呼入时 `$fU`=主叫 MSISDN，映射不到账户。
5. **已有 AI 媒体面骨架**：`internal/mediamain/mediastream`（双向 chunk 扇出、500ms RTT 预算）+ `gvos-asr` DashScope 桥 + 4444 WSS 监听——spike 级，**不升级为 agent 媒体面**（见 §11 红线）。
6. **平台无 originate API**：控制面主动发起外呼的能力今天不存在（spike 用注册分机/客户端发起，见 §4）。

结论：**两个缺口（DID 路由、呼入账户口径）与 agent 传输选型无关，怎么接都绕不开**，归入 Phase 2（§5）。

## 1. 上游调研结论（2026-11-20 已核实，含出处）

### 1.1 Pipecat SIPTransport 机制

来源：[SIPTransport 官方参考](https://reference-server.pipecat.ai/en/latest/api/pipecat.transports.sip.transport.html)（API 字段名以安装版本为准）。

| 机制 | 事实 | 对 G-VOS 的意义 |
|---|---|---|
| 两种模式 | registered（REGISTER 到 SIP 服务器，`on_dialin_ready` 回 AOR）vs **trunk**（`reg_interval=0`，不 REGISTER，对端直拨监听端口） | **只用 trunk 模式**：registered 模式在 G-VOS 没用——边缘无 lookup 路由，注册了也路由不进来 |
| 入站 | 默认 `auto_answer=True`；事件链 `on_first_participant_joined → on_client_connected` | 对端 = Kamailio（经 rtpengine），INVITE 到 worker 端口即建立 |
| 出站 | `start_dialout` 支持 `sipUri`/`phoneNumber`（配 `SIPParams.trunk`），trunk 模式下对 Digest 挑战用 `auth_user` 应答 | agent 反呼/回拨可直接 INVITE 到 G-VOS 边缘，`auth_user` **正好对上 accounts 表 Digest 认证** |
| 并发模型 | **一 transport 实例一通话**；每实例独占一个 `SIPConnection`；runtime 级 `max_concurrent_calls` 默认 2 | 扩容 = N 个 worker 进程 × 独立端口 → 映射成 N 条网关记录（§3） |
| 媒体 | RTP 音频；codec 在连接级配置（`audio_codecs`）；`audio_out_10ms_chunks` 默认 2 贴合 RTP 节奏；视频 VP8（电话场景不用） | 电话侧 PCMU/8000 直达 pipeline，内部重采样；**少一次 PCMU↔Opus↔PCMU 往返（对比 LiveKit 路径）** |
| DTMF | `send_dtmf`（`telephone-event` 或 `sip-info`，连接级 `dtmf_mode`）；接收 `on_dtmf_event` | 收号/IVR 式交互可用 |
| 转接 | `sip_refer`（盲转）+ `sip_attended_transfer`（Replaces 询问转，收尾 reason=`transferred`）；`sip_call_transfer` 无 UA 等价物（Daily 云专属）→ **报错**；两者均要求 REFER-capable 对端（文档点名 FreeSWITCH/Asterisk/**Kamailio**） | 转人工原语可用；G-VOS 边缘 loose_route 可转发 REFER（§6） |
| 不可用项 | rooms/active-speaker/app 消息/转写与**录音事件**（Daily 云专属，被省略而非伪造） | 无碍：录音权威在 rtpengine fork，不依赖 transport 事件 |
| 代码形态 | 方法返回 `str \| None` 错误、不抛异常；事件词表镜像 DailyTransport | pipeline 换 transport 改构造器即可 |

### 1.2 版本与发布节奏

Pipecat 发布很快（v1.15.0 2026-10-29 → v1.16.0 2026-11-12，见 [releases](https://github.com/pipecat-ai/pipecat/releases)）。本文字段名以调研时 latest 为参考；**落地时以当时安装版本的 API 为准**，spike 里先锁版本。

### 1.3 DashScope 适配现状（关键约束）

- Pipecat **官方未内置** DashScope/阿里云服务（官方 supported services 目录无；相关 issue：[#2126](https://github.com/pipecat-ai/pipecat/issues/2126) Qwen TTS、[#3443](https://github.com/pipecat-ai/pipecat/issues/3443) 中国供应商支持）。
- 社区插件 [`pipecat-dashscope`](https://github.com/AbaoFromCUG/pipecat-dashscope) 提供 STT/LLM/TTS 封装与预设 profile。
- G-VOS 已有 DashScope WS 协议经验：`internal/mediamain/dashscope/dashscope.go`（gvos-asr 的实时转写桥）。

两条路径（§7 详述）：**自写 pipecat service**（推荐，~百行，协议经验已在仓内）或社区插件（快，但第三方维护强度未知）。

## 2. 目标拓扑

```
                        ┌─ 信令: Kamailio (5060) ──┐
租户/坐席/运营商 ──INVITE──►                       │ BILL_LOCK(NATS RPC/HTTP)
                        └─ 媒体: rtpengine ◄──────┘   计费/风控/CDR 不变
                                        │
                                        │ RTP (PCMU/8000), PCMU 直达
                                        ▼
                       pipecat worker ×N（trunk 模式, :5070-507x）
                       SIPTransport → pipeline:
                       VAD → ASR(DashScope) → LLM → TTS → 打断
                       （ASR/LLM/TTS 流量出公网，与 G-VOS 无 API 耦合）
```

要点：
- **worker 对 G-VOS 零 API 依赖**：信令走 Kamailio、媒体走 rtpengine，钱面（计费/风控/强制拆线/对账）全部留在 gv-core——agent 只是媒体对端的 B 端。
- worker 是普通 SIP 端点；teardown 广播（`gvos.call.teardown` → `dlg.terminate_dlg`）照常生效，余额耗尽强制拆线对 agent 通话同样成立。

## 3. worker 池 = 网关池（关键设计，复用现有模型）

N 个 worker 进程（各占一个端口）= admin API 里的 N 条网关记录：

```json
POST /v1/admin/gateways
{"gateways":[
  {"id":"agent-1","name":"pipecat worker 1","sip_addr":"agent-worker-1:5070","priority":0,"enabled":true},
  {"id":"agent-2","name":"pipecat worker 2","sip_addr":"agent-worker-2:5071","priority":1,"enabled":true}
]}
```

- 经 `gvos.cfg.reload` 广播 → `gw` htable 镜像 → 边缘 `GW_DISPATCH` 逐条可用，**零新边缘代码**。
- **per-worker 故障切换免费**：worker 无应答 → `failure_route[GW_FAILOVER]`（`kamailio.cfg:669-685`）把该 worker 锁进 `gwfail` 5 分钟并重试下一条——即 agent 实例熔断。
- 扩缩容 = 增删网关条目（现有广播机制即时生效）；每进程容量按 `max_concurrent_calls`（默认 2，需按进程显式调高）× 进程数预估，与 `priority` 无关。
- LCR/`Prefixes`/`CostRate` 字段对 agent 网关留空：agent 分诊**不走成本序**，按 Phase 2 的 `gw_id` 指派（§5），避免被全局 LCR 序误选。

## 4. 呼出链路（电话 → agent；Phase 1 spike 即此）

1. 租户分机（REGISTER+Digest）拨打 agent 号段（如 `6001`）。
2. 边缘流水线照常：IP 白名单 → 封禁 → 黑名单 → Digest → BILL_LOCK（**from_user=租户账户，锁账成立**）→ 应答 `record:true` 则 rtpengine 开录音。
3. `GW_DISPATCH` 选 worker（spike 阶段：单 agent 网关 compose，全局选优即指派成功；多目的地指派靠 Phase 2）。
4. INVITE → `agent-worker-n:507x`；worker `auto_answer` 应答 → 200 OK 回边缘 → `gvos.svc.call.answer` → gv-core interim 10s 扣减开始。
5. BYE → `gvos.cdr.raw`（duration/pdd/qos）→ CDRR durable → PG。

**计费语义零改动**：agent 通话就是一通普通话务；worker 拒接/崩溃 = 486/503 不产生话费（按接通语义）。全部链路复用现有机制，**Phase 1 无边缘改动、无控制面改动**。

**spike 验证点（只有一个）**：G-VOS CDR 完整覆盖 agent 通话的 answer/BYE/时长；外加 teardown 强拆对 agent 通话生效。

## 5. 呼入链路（carrier DID → agent）与两个前置缺口（Phase 2）

链路：运营商 trunk → G-VOS 边缘（IP 白名单准入）→ **DID 路由** → worker。两步建设：

### 5.1 DID 登记与账户口径（新增控制面能力）

- 新增 DID 表：`did`（号码、归属租户、目的地类型、启停）。admin API 管理 + `gvos.cfg.reload` 广播。
- BILL_LOCK 改造：INVITE 源命中「trunk 准入」（IP 白名单条目可扩展 trunk 语义）时，按 `$rU`（被叫 DID）反查归属租户账户并以其锁定——替代 `from_user=$fU` 口径。**这是呼入计费/风控/账单闭环的前提，不可跳过**。

### 5.2 按被叫选路（补 per-destination 缺口）

两条实现路径，推荐后者：

- 备选：边缘 `GW_DISPATCH` 加被叫前缀匹配（参照 `BL_CHECK` 的逐级探测，≤20 次哈希探测）——改动在 cfg 词法敏感区，九陷阱清单上加一条。
- **推荐：控制面在 BILL_LOCK 应答里带 `gw_id`**（DID→目的地已查好），边缘直接 `$du` 指派。更贴「控制面算、边缘执行」哲学；载荷演进规则允许加字段（旧边缘忽略新字段，见 02-architecture §9），向后兼容；**顺带把 per-destination LCR 做真**（替掉 server.go:685 的全局 best-index 近似）——通话时长/费率匹配本就在控制面，这是同一处逻辑的延伸。

### 5.3 呼入计费语义决策（需拍板）

呼入按 DID 归属租户计费（主叫免费型），或对 trunk 侧计费（运营商采购型）——影响 DID 表字段与 BILL_LOCK 锁定方向，Phase 2 开工前定稿。

## 6. 转人工（Phase 3，依赖 §5）

- worker 侧判定转人工（话术/情绪/置信度）→ `sip_refer`（盲转）或 `sip_attended_transfer`（Replaces）→ REFER/INVITE 落 G-VOS 边缘。
- 边缘 in-dialog 分支（`kamailio.cfg:246-274` loose_route/`GW_CONTACT`）已能转发；转接目标 = 某个坐席 DID → 由 §5 的 DID 路由接住 → 坐席话机或坐席侧 agent。
- **新话单新计费**：转接产生独立 leg，钱面天然闭环；refer 前后两段的时长归属在 CDR 上分列（转接 reason=`transferred` 已有语义锚点）。
- 开放问题：`sip_call_transfer` 在 SIPTransport 是报错项（见 §1.1），转接语义只可用 refer 族；询问转的两段合并计费口径 Phase 3 设计。

## 7. agent 技术栈（Pipecat pipeline + DashScope）

pipeline 组装（VAD → ASR → LLM → TTS → 打断），打断/barge-in/turn detection 是框架内建——这是选 Pipecat 而非自研编排的核心原因。DashScope 适配：

- **路径 A（推荐）自写 service**：参考 `internal/mediamain/dashscope/dashscope.go` 已验证的 WS 协议，按 pipecat service 接口（`stt.py`/`tts.py` 基类）封装 Realtime ASR（paraformer-realtime-v2 / Fun-ASR-Realtime）与 CosyVoice TTS，~百行 Python；协议经验仓内已有，无第三方维护风险。
- **路径 B 社区插件**：[`pipecat-dashscope`](https://github.com/AbaoFromCUG/pipecat-dashscope)（STT/LLM/TTS 封装 + 预设 profile）。快，但社区单点维护，供应商升级节奏不可控——**spike 可用路径 B 先跑通，生产切路径 A**。

trunk 模式 worker 骨架（字段名以安装版本为准）：

```python
# trunk 模式：不 REGISTER，等 Kamailio 直拨 :5070
conn = SIPConnection(
    user="agent-1", password="<trunk 凭据>",
    host="0.0.0.0", sip_port=5070,
    reg_interval=0,                      # trunk 模式
    audio_codecs=["PCMU/8000", "telephone-event/8000"],
    dtmf_mode="telephone-event",
    max_concurrent_calls=4,              # 默认 2，按进程显式调高
)
transport = SIPTransport(conn, SIPParams(audio_in_enabled=True, audio_out_enabled=True))
pipeline = Pipeline([transport.input(), vad, asr, llm, tts, transport.output()])
# 事件链: on_client_connected → 开跑; 转人工: sip_refer(目标=G-VOS 坐席 DID)
```

要点：ASR/LLM/TTS 的 API key 走 worker 自身配置（不进 G-VOS settings）；每实例独占 `SIPConnection`（handlers 绑定，连接不可跨实例复用——进程模型据此设计）。

## 8. 部署与安全

- **compose 形态**：新增 `agent-worker-1..N` 服务（python:3.12 + pipecat + 锁版本），仅内网暴露 `5070-507x`；`gw` 表指向内网名。K8s 形态按 Deployment×N + Service per-pod 端口（与 gateway 条目一一对应）。
- **监听面**：worker 端口只在内网可达（trunk 模式要求对端直达监听 socket；公网不可暴露 worker，公网入口只有 Kamailio）。
- **准入**：运营商 trunk → 现有 IP 白名单（默认拒绝语义）；worker ↔ 边缘全内网，不需要公网证书。
- **凭据**：trunk Digest 凭据 = accounts 表普通账户（`auth_user` 应答挑战）；DID/网关配置走现有 `settings` + 广播同步。
- **可观测**：worker 进程指标（通话数/RTT/打断次数，pipecat observers）出 Prometheus 文本端口；G-VOS 侧 CDR 的 pdd/qos 列已覆盖话务面。

## 9. 开放问题（Phase 2 开工前逐项拍板）

| # | 问题 | 影响 |
|---|---|---|
| O1 | 呼入计费方向：DID 归属租户 vs trunk 侧计费 | DID 表字段 + BILL_LOCK 锁定方向 |
| O2 | 平台主动外呼（拨号器/REST originate）：无 originate API，新增 or Phase 2 不做 | AI 外呼产品化范围 |
| O3 | REFER 转接的 CDR 语义：前后两段话单关联字段 | 转接报表/对账 |
| O4 | worker 进程模型：多线程单进程多连接 vs 多进程多端口（与 `max_concurrent_calls`、SIPConnection 不可复用约束相关） | 端口规划与网关条目数 |
| O5 | 通话中断线重连：worker 崩溃时进行中通话是否重派（现失败路由只覆盖建立期） | 体验与计费 |

## 10. 里程碑与验证点

| 阶段 | 内容 | 出口 |
|---|---|---|
| Spike | 单 worker compose + SIPp 打通 + CDR 覆盖验证 | CDR 覆盖 answer/BYE/时长；teardown 强拆生效 |
| Phase 1 | worker 池网关化 + 录音（record:true 路径）+ DashScope service 路径 A 定稿 | 多 worker 熔断重试实测；录音文件按 call-id 关联 |
| Phase 2 | DID 表 + 呼入账户口径 + `gw_id` 指派（per-destination LCR 做真） | carrier 呼入→agent 全链路计费闭环；对账零漂移维持 |
| Phase 3 | 转人工（refer 族）+ agent 报表 + O3/O5 语义落地 | 转接两段话单分列；agent 通话进运维手册 |

## 11. 红线与边界

1. **mediamain 不升级为 agent 媒体面**：保留它做监听、ASR 转写、录音后处理（其本行）；agent 媒体面由 SIPTransport 承担。
2. **Pipecat 不碰钱面**：计费/风控/对账全部留在 gv-core；worker 无 gv-core API 依赖。
3. **不跳过 DID 账户口径直接接 trunk**：呼入的风控规则与账单对不上是话务商验收第一个查的点。
4. **公网只暴露 Kamailio**：worker 端口内网 only。
5. 发行包不新增重型依赖：pipecat 以独立容器交付，不进 gv-core 依赖树。

## 参考

- [Pipecat SIPTransport API](https://reference-server.pipecat.ai/en/latest/api/pipecat.transports.sip.transport.html)（机制核实主源）
- [Pipecat Releases](https://github.com/pipecat-ai/pipecat/releases) · [pipecat-dashscope 社区插件](https://github.com/AbaoFromCUG/pipecat-dashscope) · issue [#2126](https://github.com/pipecat-ai/pipecat/issues/2126) / [#3443](https://github.com/pipecat-ai/pipecat/issues/3443)
- 平台侧事实：`deploy/kamailio/kamailio.cfg`、`internal/api/server.go:678`、`internal/customerdomain/gateway/lcr.go`（2026-11-19 核实，见 §0）
