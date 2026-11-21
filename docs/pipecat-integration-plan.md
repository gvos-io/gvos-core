# Voice Agent 集成方案（一）：Pipecat 原生 SIP（SIPTransport）——主推荐路径

**状态**：规划稿 + **Spike 已验证**（2026-11-20 e2e 打通：SIPp → Kamailio → rtpengine → pipecat worker，60s 计费通话落 CDR；安装与五个上游坑全部实测，见 §2/§3）
**日期**：2026-11-20
**范围**：用 Pipecat `SIPTransport`（trunk 模式）把 AI agent 通话直接挂在 G-VOS 现有网关模型上——Kamailio → rtpengine → pipecat worker **直连**，不经 LiveKit。覆盖：安装、Spike 交付物与实测结论、呼出/呼入链路、worker 池、转人工、计费语义、DashScope 适配、部署与 Phase 1-3。
**关联**：[livekit-integration-plan.md](livekit-integration-plan.md)（何时选 LiveKit：WebRTC 参与者/多方/房间语义）· [02-architecture.md](02-architecture.md) §4（呼叫数据流）· [recording-oss-upgrade-plan.md](recording-oss-upgrade-plan.md)（录音管道，录音权威不动）
**Spike 交付物**：[deploy/agent-worker/](../deploy/agent-worker/)（bot.py + Dockerfile）· [test/sipp/agent_call.xml](../test/sipp/agent_call.xml)（agent 通话场景）

---

## 0. 平台侧现状与差距（接入方案的四个硬前提，2026-11-19 核实）

以下事实直接决定本方案的形状，引用均为代码位置：

1. **边缘无呼入/呼出之分**：所有 INVITE 走同一流水线（`deploy/kamailio/kamailio.cfg:294-334`：IP 白名单 → 封禁 → 黑名单前缀 → Digest → BILL_LOCK → GW_DISPATCH → rtpengine）。每通电话都**向网关出站**。
2. **无 DID 呼入终结**：INVITE 路径从不 `lookup("location")`。「PSTN → DID → 注册用户/agent」不存在；carrier INVITE 还会被 Digest 认证拦（trunk 无凭据），且 `$fU`=主叫 MSISDN 锁不到账。
3. **GW_DISPATCH 全局选路**（`kamailio.cfg` GW_DISPATCH route）：按最低 priority 选**一个**网关，不做被叫前缀匹配。per-destination LCR 受「htable 一 id 一 prio」契约限制，是各前缀族最优索引近似（`internal/api/server.go` lcrGateways）。
4. **已有 AI 媒体面骨架**：`internal/mediamain/mediastream`（双向 chunk 扇出、500ms RTT 预算）+ `gvos-asr` DashScope 桥——spike 级，**不升级为 agent 媒体面**（见 §11 红线）。

结论：**DID 路由与呼入账户口径与 agent 传输选型无关，怎么接都绕不开**，归入 Phase 2（§7）。

## 1. 上游调研结论（2026-11-20 已核实，含出处）

### 1.1 Pipecat SIPTransport 机制

来源：[SIPTransport 官方参考](https://reference-server.pipecat.ai/en/latest/api/pipecat.transports.sip.transport.html) + GitHub main 分支源码级核实（commit 834903d）。

| 机制 | 事实 | 对 G-VOS 的意义 |
|---|---|---|
| 两种模式 | registered（REGISTER 到 SIP 服务器）vs **trunk**（`reg_interval=0`，不 REGISTER，对端直拨监听端口） | **只用 trunk 模式**：G-VOS 边缘无 lookup 路由，注册了也路由不进来 |
| 入站 | `auto_answer=True` 默认应答；事件链 `on_first_participant_joined → on_client_connected` | 对端 = Kamailio（经 rtpengine），INVITE 到 worker 端口即建立 |
| 出站 | `start_dialout`（`sipUri`/`phoneNumber`），对 Digest 挑战用 `auth_user` 应答 | agent 反呼可直接 INVITE 到 G-VOS 边缘，正好对上 accounts 表认证 |
| 并发模型 | **一 transport 实例一通话**；每实例独占 `SIPConnection`；runtime 级 `max_concurrent_calls` 默认 2 | 扩容 = N 进程 × 独立端口 → 映射成 N 条网关记录（§6）；spike 实测一个 call task 结束会拆整个 runtime（进程须按一通话一生命周期设计，或改造为 per-call 重建） |
| 媒体 | RTP；codec 连接级配置（`audio_codecs`，baresip 名 `pcmu`/`pcma`）；`audio_out_10ms_chunks` 默认 2 贴合 RTP 节奏 | PCMU/8000 直达 pipeline；**少一次 PCMU↔Opus↔PCMU 往返（对比 LiveKit 路径）** |
| DTMF | `send_dtmf`（`rtpevent`/`info`/`auto`，连接级 `dtmf_mode`）；接收 `on_dtmf_event` | 收号/IVR 式交互可用 |
| 转接 | `sip_refer`（盲转）+ `sip_attended_transfer`（Replaces 询问转）；`sip_call_transfer` 报错（Daily 云专属）；要求 REFER-capable 对端（文档点名 Kamailio） | 转人工原语可用（Phase 3） |
| 不可用项 | rooms/active-speaker/app 消息/**录音事件**（被省略而非伪造） | 无碍：录音权威在 rtpengine fork |

### 1.2 安装现状（spike 实测，2026-11-20）

- **PyPI `pipecat-ai` 1.16.0（2026-11-12 发布）不含 `transports/sip`**——wheel 内容与 extras 清单逐一核过（102 个 extras 无 sip，base 包无 sip 模块）。`pip index versions pipecat` 顶上的 `pipecat 0.3.0` 是**无关同名包**，别装。
- **SIPTransport 在 GitHub main 分支**（`src/pipecat/transports/sip/{__init__,connection,transport}.py`，装出来版本号为 `1.16.1.dev269`）。安装：`pip install "pipecat-ai[sip] @ git+https://github.com/pipecat-ai/pipecat.git@<pin>"`，**钉 commit**（spike 用 834903d9a0ccc95038fb7890c7e35a47d680bcbd）。
- `sip` extra = `baresip-python>=0.6.0,<0.7` + `opencv-python-headless`。baresip-python 0.6.0 **有 manylinux wheel**（无需编译原生库）；Python 3.12-slim 基镜像可装。注意 slim 无 git：Dockerfile 里临时装 git 装完后 purge（见交付 Dockerfile）。
- PyPI 发布跟进后换版本化 wheel 即可（`pipecat-ai[sip]>=1.17` 或当时版本）。

### 1.3 DashScope 适配现状

- Pipecat 官方未内置 DashScope/阿里云服务（issue [#2126](https://github.com/pipecat-ai/pipecat/issues/2126)、[#3443](https://github.com/pipecat-ai/pipecat/issues/3443)）。
- 社区插件 [`pipecat-dashscope`](https://github.com/AbaoFromCUG/pipecat-dashscope) 提供 STT/LLM/TTS 封装与预设 profile。
- G-VOS 已有 DashScope WS 协议经验：`internal/mediamain/dashscope/dashscope.go`。两条路径见 §10。

## 2. Spike 实测结论（2026-11-20，e2e 已通）

**验证环境**：compose demo 栈（Kamailio 6.0.1 + rtpengine + gv-core + PG/Redis/NATS）+ `gvos-agent-spike` 容器（trunk 模式 worker）+ SIPp UAC（Digest 认证场景 `agent_call.xml`）。

**✅ e2e 证据**（1 通成功通话）：

- SIPp 侧：`Successful call 1, Call Length 60s`（401 挑战 → 带认证 INVITE → worker 180/200 → ACK → 60s → BYE）。
- worker 侧：`INCOMING`（sipCallId/From/To 事件载荷齐全）→ `SIP handshake established` → `CLOSED`。
- **G-VOS CDR 完整覆盖**（spike 唯一验证点，通过）：`CallID/AccountID/Dest/StartTime` 正常，`Duration 60 / BilledSecs 60 / Cost 0.0500 / RatePrefix 8613 / PDDMs 159 / SIPCode 200 / Recorded true`——计费、录音开关、PDD 全部落到 agent 通话上，**零控制面改动**。
- 挂载方式即 §6 的网关模型：`PUT /v1/admin/gateways` 加 `agent-1 → sip:<worker-ip>:5070, priority 0`，NATS 广播 → GW_DISPATCH 选它，`gwfail` 熔断/`GW_FAILOVER` 重试语义原样继承。

**⚠️ 已知限制（spike 观测，未做修复性开发）**：

- worker **一进程一通话**：call task 结束时 pipecat runner 把 transport/runtime 一起收走（日志：`runtime closed → PipelineTask finished → WorkerRunner finished`），进程存活但不再接电话。与上游文档「一实例一通话」一致；生产形态 = per-call 重建 transport 或 K8s 单通话 pod 生命周期（§6 O4）。
- teardown 强拆对 agent 通话的验证**未完成**：测试窗口内 demo 栈被并行会话反复重启（gv-core 换了 4 把 admin key、allowlist/gateway 表被清），余额扣减/强拆链路没能完整跑完——已验证到 TEARDOWN consumer 在岗（`gvos.call.teardown` workers=1），dlg 侧按 U 系列既有结论待干净窗口复测（列 §12 验证批次）。

**Kamailio 侧三个真实发现**（spike 期间定位并修复/确认，属平台 bug 不属 agent 集成，修复已留在工作树 cfg 里）：

1. **401-challenge ACK 被误转发到网关**：caller 对 401 的 ACK（无 Route、带 To-tag）落进 `has_totag()` in-dialog 分支 → `GW_CONTACT` fallback 到 `GW_DISPATCH` → 把 ACK 发给 agent worker（worker 481）。修复（cfg）：request_route 顶部 ACK 处理 = `t_check_trans()` 吸收事务 ACK + `$dlg(state)` 为空丢弃 challenge-ACK（真 200-ACK 走 loose_route/有 dialog，不受影响）；`GW_CONTACT` 对无 `meta:gw` 的 ACK 直接丢弃（BYE 仍回退重选路）。
2. **GW_SYNC 60s 重播竞态**：GW_SYNC 先 wipe `dest:` 键再重插，dispatch 落在间隙即 `no routable gateway → 503`（实测复现一次）。根因待修（方案：wipe 前置版本比对 / 双表切换），spike 期间靠避开窗口重试。**该竞态对普通网关路由同样存在，非 agent 专属**——建议单独立项修复。
3. **authcache stale 陷阱复现**：`pv_www_authenticate` 对缓存 HA1 的 nonce 校验在长时间运行后拒绝正确凭据（401 循环）；`kamcmd htable.delete authcache ha1:<user>` 恢复。commit 40065a4 已记录过同一陷阱——属已知问题清单，非新增。

## 3. worker 侧五个坑（SIPTransport 生产化必读，全部实测）

1. **无法指定监听端口（上游缺口）**：`SIPConnection` 构造器无监听参数（无 sip_port/host）；`domain` 只是 AOR（地址记录）；`extra_config_text` 会被 `_RuntimeSettings.to_config()` **硬编码丢弃**（只渲染 jitter buffer 行）——即当前版本没有任何文档化方式让 worker 监听指定端口。**绕法（spike 已验证）**：monkey-patch `_RuntimeSettings.to_config`，把 baresip 原生配置行 `sip_listen 0.0.0.0:<port>`（空格分隔，非 `=`）追加进 `extra_config_text`。上游修了就删补丁（值得提 PR）。
2. **Via/Contact/SDP 广播 0.0.0.0**：默认 socket 绑定不带真实地址（200 OK 的 Via 出现 `0.0.0.0`）。**修法**：`SIPConnection(net_interface=<容器主 IP>)`（baresip 会把它写进 Via/Contact/SDP；容器内用 UDP-connect 技巧取主 IP，零流量）。
3. **入站 INVITE 的 AOR 匹配**：baresip 按 Request-URI 匹配 account，不匹配直接 **404 Not Found**（实测）。Kamailio `t_relay($du)` 转发时**保留原始被叫 URI**（`sip:<号码>@<边缘>`），所以 worker 的 `SIPConnection(user, domain)` 必须**认领边缘放进 R-URI 的那个 (user, domain) 对**（demo：user=8613800138000, domain=<kamailio-ip>:5060）。生产用 agent 号段 + 边缘域名。
4. **auto_answer 只在 pipeline RUNING 时生效**：baresip account 是 `answermode=manual`；`SIPParams(auto_answer=True)` 由 transport 的事件翻译层执行，**前提是有 PipelineTask 在跑**。只 `connect()` 然后挂着的 worker 收到 INVITE 不应答（实测卡死 31s 超时）。spike 形态：`incoming` 事件 → 起 `PipelineTask([transport.input(), transport.output()])` → `PipelineRunner().run(task)`。
5. **一实例一通话**（上游文档明示，实测确认）：每通话需独占 `SIPConnection` + `SIPTransport`；runtime 级 `max_concurrent_calls` 默认 2，多通话须显式调大且**全进程一致**（第一个 connection 固定 runtime 级设置，后续不一致直接 ValueError）。进程模型见 §6 O4。

以上全部体现在 [deploy/agent-worker/bot.py](../deploy/agent-worker/bot.py)（含逐条注释）；那是 spike 级骨架（无 LLM/ASR/TTS，INVITE 到达即应答、挂到 BYE），Phase 1 在 pipeline 槽位插 VAD/ASR/LLM/TTS。

## 4. 目标拓扑

```
                        ┌─ 信令: Kamailio (5060) ──┐
租户/坐席/运营商 ──INVITE──►                        │ BILL_LOCK(NATS RPC/HTTP)
                        └─ 媒体: rtpengine ◄───────┘   计费/风控/CDR 不变
                                        │
                                        │ RTP (PCMU/8000) 直达 worker
                                        ▼
                       pipecat worker ×N（trunk 模式, :5070-507x）
                       SIPTransport → pipeline:
                       VAD → ASR(DashScope) → LLM → TTS → 打断
                       （ASR/LLM/TTS 出公网，与 G-VOS 无 API 耦合）
```

- **worker 对 G-VOS 零 API 依赖**：信令走 Kamailio、媒体走 rtpengine，钱面全部留在 gv-core——agent 只是媒体对端的 B 端。
- teardown 广播（`gvos.call.teardown` → `dlg.terminate_dlg`）照常生效（U3 既有验证链路；agent 通话复测列 §12）。

## 5. 呼出链路（电话 → agent；spike 即此，已验证）

1. 租户分机（REGISTER+Digest）拨打 agent 号段。
2. 边缘流水线照常：IP 白名单 → 封禁 → 黑名单 → Digest → BILL_LOCK（from_user=租户账户，锁账成立）→ `record:true` 则 rtpengine 开录音。
3. `GW_DISPATCH` 选 worker；INVITE → worker `auto_answer` → 200 → `gvos.svc.call.answer` → interim 10s 扣减。
4. BYE → `gvos.cdr.raw`（duration/pdd/qos）→ CDRR durable → PG。

**计费语义零改动**（spike CDR 已证）；worker 拒接/崩溃 = 486/503 不产生话费（按接通语义）。

## 6. worker 池 = 网关池（spike 已验证的挂载方式）

N 个 worker 进程（各占端口）= N 条网关记录：

```json
PUT /v1/admin/gateways
{"gateways":[
  {"id":"agent-1","name":"pipecat worker 1","sip_addr":"<ip>:5070","priority":0,"enabled":true},
  {"id":"agent-2","name":"pipecat worker 2","sip_addr":"<ip>:5071","priority":1,"enabled":true}
]}
```

- 经 `gvos.cfg.reload` 广播 → `gw` htable → `GW_DISPATCH` 逐条可用，**零新边缘代码**（spike 验证路径）。
- **per-worker 熔断免费**：worker 无应答 → `GW_FAILOVER` 锁 `gwfail` 5 分钟 + 重试下一条。
- **地址稳定性**：容器 IP 会漂移（spike 实测重建容器 IP 不变是运气）。生产用 K8s Service 名/Stable IP；compose 靠服务名 DNS（注意 Kamailio 容器对 `docker run --name` 的非 compose 容器**不解析**——spike 实测 `agent-spike` 名不可解析，须用 compose 管理的 service 名或 IP）。
- LCR/`Prefixes`/`CostRate` 对 agent 网关留空：agent 分诊不走成本序，Phase 2 按 `gw_id` 指派。

## 7. 呼入链路（carrier DID → agent）与两个前置缺口（Phase 2）

链路：运营商 trunk → G-VOS 边缘（IP 白名单准入）→ **DID 路由** → worker。

1. **DID 登记与账户口径**：新增 DID 表（号码、归属租户、目的地、启停），admin API + 广播；BILL_LOCK 在 trunk 准入语义下按 `$rU`（被叫 DID）反查归属账户锁定——替代 `from_user=$fU`。**不可跳过**。
2. **按被叫选路**：推荐**控制面在 BILL_LOCK 应答里带 `gw_id`**（DID→目的地已查好），边缘直接 `$du`；载荷加字段向后兼容（02-architecture §9），并顺带把 per-destination LCR 做真（替掉全局 best-index 近似）。备选：边缘 GW_DISPATCH 加被叫前缀匹配（cfg 词法敏感区，慎动）。
3. **呼入计费方向**（DID 归属租户 vs trunk 侧计费）开工前拍板（O1）。

## 8. 转人工（Phase 3，依赖 §7）

- worker 判定转人工 → `sip_refer`/`sip_attended_transfer` → REFER 落 G-VOS 边缘 → in-dialog loose_route 转发 → 目标（坐席 DID）由 §7 的 DID 路由接住。
- 新话单新计费；两段时长分列（close reason=`transferred` 有语义锚点）。
- 开放问题：`sip_call_transfer` 在 SIPTransport 报错（Daily 云专属），转接语义只可用 refer 族。

## 9. 部署与安全

- **compose**：agent worker 进 compose 服务（spike 用了独立 `docker run`，为的是快速迭代；正式形态进 `docker-compose.yml`，可仿 recording 服务用 `profiles: ["agent"]` 可选档）。
- **监听面**：worker 端口只在内网可达；公网入口只有 Kamailio。
- **准入**：trunk → IP 白名单；worker ↔ 边缘全内网。
- **凭据**：spike 期实测 admin key 会因 gv-core 重启轮换（BOOTSTRAP 打日志一次性输出）——运维脚本要从日志取当前 key，或预先在 PG 里落固定 key。
- **可观测**：worker 出 Prometheus 文本端口（通话数/RTT/打断次数）；G-VOS 侧 CDR pdd/qos 已覆盖话务面。

## 10. agent 技术栈（pipeline + DashScope）

- **路径 A（推荐）自写 service**：参考 `internal/mediamain/dashscope/dashscope.go` 的 WS 协议经验，按 pipecat service 基类封装 Realtime ASR + CosyVoice TTS；无第三方维护风险。
- **路径 B 社区插件** [`pipecat-dashscope`](https://github.com/AbaoFromCUG/pipecat-dashscope)：spike 起跑可用，生产切 A。

## 11. 红线与边界

1. mediamain 不升级为 agent 媒体面（只做监听/转写/录音后处理）。
2. Pipecat 不碰钱面：worker 无 gv-core API 依赖。
3. 不跳过 DID 账户口径直接接 trunk。
4. 公网只暴露 Kamailio。
5. 发行包不新增重型依赖：pipecat 独立容器交付，不进 gv-core 依赖树。

## 12. 开放问题与验证批次

| # | 问题 | 状态 |
|---|---|---|
| O1 | 呼入计费方向（DID 租户 vs trunk 侧） | 待拍板 |
| O2 | 平台 originate API（拨号器/REST 主动外呼）不存在 | Phase 2 范围决策 |
| O3 | REFER 转接的两段话单关联字段 | Phase 3 设计 |
| O4 | worker 进程模型：per-call transport 重建 vs 单通话 pod 生命周期 | Phase 1（spike 观测 §2 已给输入） |
| O5 | worker 中途崩溃的进行中通话重派（失败路由只覆盖建立期） | Phase 3 |
| V1 | teardown 强拆对 agent 通话 e2e | **待干净窗口**（spike 期被环境冲突打断，见 §2） |
| V2 | GW_SYNC 60s 重播竞态（平台 bug，建议单独立项） | 已复现待修 |
| V3 | 多 worker 池熔断重试实测（2+ 网关条目） | Phase 1 |
| V4 | 上游跟进：SIPTransport 进 PyPI 正式版 / 监听端口 API / `pipecat-ai[sip]` wheel | 换版本时删 monkey-patch |

## 13. 里程碑

| 阶段 | 内容 | 出口 |
|---|---|---|
| **Spike** | 单 worker + SIPp + CDR 验证 | **✅ 2026-11-20 完成**（§2） |
| Phase 1 | pipeline 接 DashScope（路径 A）、多 worker 池、per-call transport 生命周期（O4）、V1/V3 补验 | 全链路含 ASR 转写；V1 teardown 通过 |
| Phase 2 | DID 表 + 呼入账户口径 + `gw_id` 指派（per-destination LCR 做真） | carrier 呼入→agent 计费闭环 |
| Phase 3 | 转人工（refer 族）+ agent 报表 + O3/O5 | 转接两段话单分列 |

## 参考

- [Pipecat SIPTransport API](https://reference-server.pipecat.ai/en/latest/api/pipecat.transports.sip.transport.html) · [pipecat-ai GitHub](https://github.com/pipecat-ai/pipecat)（main 分支 `src/pipecat/transports/sip/`）
- [pipecat-dashscope 社区插件](https://github.com/AbaoFromCUG/pipecat-dashscope) · issue [#2126](https://github.com/pipecat-ai/pipecat/issues/2126) / [#3443](https://github.com/pipecat-ai/pipecat/issues/3443)
- spike 记录：`deploy/agent-worker/`（bot.py 注释含五坑与绕法）· `test/sipp/agent_call.xml` · 本文档 §2/§3
