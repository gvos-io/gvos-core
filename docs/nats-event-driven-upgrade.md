# NATS 事件驱动改造：完成状态与升级路线

> 2026-11-19。本文档是"控制/信令/媒体三层全事件驱动"架构转型的单一事实源：
> 已交付内容（含实测证据）、未完成项（含原因与验证方法）、整体升级排期。
> 架构原则见 [02-architecture.md](02-architecture.md) §3；本文只管"做到哪、还差啥、何时做"。

## 1. 架构愿景（验收基准）

三层分离：**控制（gv-core）/ 信令（Kamailio）/ 媒体（RTPengine）**。
层间全部三类机制走 NATS，整体事件驱动：

| 机制 | 语义 | 承载 |
|---|---|---|
| 同步 | 状态快照镜像 | `gvos.cfg.reload` 广播 + 60s 重播对账 |
| 广播 | 事件扇出 | CDR / 注册 / 风控 / 拆线 / 配置变更 |
| RPC | 请求-应答 | `gvos.svc.*` req/reply + `t_suspend/t_continue` 异步锁定 |

HTTP 退出层间通信，只保留两个角色：**北向管理面**（admin API，人的接口）、
**南向出口**（webhook 给客户推事件）。媒体 ng/UDP 协议保留原生（rtpengine 无
NATS 客户端、SDP 控制亚毫秒预算、命令-应答语义与事件流不匹配）；媒体→控制
的事件流（QoS 遥测）走总线。

## 2. 已完成（两笔提交，16 包 build+test 全绿）

### 2.1 六项边缘改造（commit `aad932f`）

| # | 能力 | 交付物 | 实测 |
|---|---|---|---|
| 1 | 网关配置面 | `internal/gateway` 内存态 + `/v1/admin/gateways` + 快照广播 → Kamailio gw htable 自管选路（dispatcher db_url 已删，**边缘零 PG 依赖**） | ✅ API 推送 → `GW_SYNC applied 2 gateways` → SIPp 通话选路成功 |
| 2 | 风控边缘封禁 | risk.alert 载荷带 SIP username → banned htable（1h 自动过期）→ 下一通 INVITE 边缘 403 | 代码+单测就绪；e2e 被共享环境干扰（见 §4） |
| 3 | CDR 上总线 | BYE 时 `nats_publish gvos.cdr.raw` → CDRR durable 消费者（复用 callEnd 纯函数，uq_cdr_call 幂等） | ✅ `pdd_ms=24/43` 话单经总线落 PG |
| 4 | 注册事件 | usrloc save/expiry 钩子发布 `gvos.reg.event` → gv-core 注册镜像（`/v1/admin/registrations`）+ reg_freq 风控规则 | 代码+单测就绪 |
| 5 | 拆线执行闭环 | teardown 广播 → 持有 dialog 节点经 RPC `dlg.terminate_dlg` 终结（持有者执行、其余 no-op）；TEARDOWN durable 保留重试；离线缝 `GVOS_KAMAILIO_RPC` | 代码就绪（补齐了原 dispatch.go 占位符） |
| 6 | 黑名单边缘拦截 | blocklist 广播 → bl htable 前缀筛查，INVITE 早期 403（正则/白名单语义留控制面，双层防御） | 代码就绪 |

可靠性机制：**60s 全量快照重播**（core NATS at-most-once + nats 模块无 JS 回放
→ 重播即对账心跳；kamailio 重启后实测 60s 内 htable 自愈）。

### 2.2 计费锁定异步 RPC（commit `d226bf7`）

**Kamailio→gv-core 的最后一块 HTTP 退出关键路径**：

```
INVITE → nats_publish(gvos.svc.call.start, 载荷带 tindex/tlabel, reply=节点唯一主题)
       → t_suspend()（worker 释放，接下一通）
gv-core → callStart 计费锁定 → 应答回 gvos-edge-reply-<node>（envelope 回显坐标）
节点 event_route → 暂存答案到 billlock htable → t_continue(ti,tl,BILL_LOCK_REPLY)
恢复路由（failure-route 语义，网关重选）→ 403 / GW_DISPATCH → t_relay
兜底：1s janitor 扫描挂起>2s 的锁定 → t_continue → 503 Billing Unavailable
```

实测（compose 栈）：`bill-lock suspended tx=27545:...` → `dispatching to gw g1`
→ 200/ACK/BYE → CDR 经总线落 PG —— **挂起→RPC→恢复→选路→计费闭环一次跑通**。

附带交付：answer 事件上总线（`gvos.svc.call.answer` fire-and-forget）；
CDR 载荷带 rtpengine QoS 统计（`$rtpstat` 转义入 JSON）。

### 2.3 部署与镜像

- 自建 **Debian trixie** 镜像（`deploy/kamailio.Dockerfile`）：上游 CI alpine
  无 nats.so（237 模块实测缺失），bookworm 无 jansson 包，trixie 两者齐（6.0.1）
- **entrypoint 生成 cfg**（`@NODE@` sed 物化）：Kamailio 无环境变量插值进
  modparam/event_route 名的机制（`!!{}` 实测为字面量），节点唯一 queue group /
  回复主题 / event_route 名三处字节一致由启动脚本保证
- compose / compose-ha / helm 全部带上 `GVOS_NODE_HOST`（k8s 用 pod 名）、
  `WITH_NATS`、`WITH_NATS_CDR`、`WITH_NATS_RPC` 开关组

### 2.4 调试钉死的 Kamailio 陷阱清单（写下来，防再踩）

1. **event_route 名不允许点号**：词法器 `EVENT_RT_NAME` 正则
   `[a-zA-Z][0-9a-zA-Z-]*(":"[...])+` 排除 `.` → 点号 subject 走 gv-core 双发
   无点别名（gvos-edge-cfg/-risk/-tear，载荷与正名一致）
2. **jansson 路径数组下一级必须点**：`gateways[0].id` ✅ / `gateways[0]/id` ❌
   （静默返回空）
3. **jansson 源参数不能加引号**：`jansson_get("k", $natsData, ...)` ✅，
   `"$natsData"` 会被当字面量字符串解析
4. **`!!{}` defenv 插值在 modparam/event_route 名是字面量** → entrypoint sed
5. `sht_iterator_start(iname, hname)` 参数序（迭代器名在前）；PV 是 `$shtitkey`
6. `$expires`/`$ul(user)` 不存在：用 `$hdr(Expires)`、`$ulc(exp=>aor)`
7. `nats_publish_request` 只导出 KEMI，脚本层用三参 `nats_publish(s, p, reply)`
8. `t_continue` 恢复路由是 failure-route 语义：`$du` 不存活，网关重选必须放进恢复路由
9. CI alpine 镜像 237 模块无 nats；Alpine 3.24/edge 仓库也无 kamailio-nats 包

## 3. 关键架构决策记录

- **NATS 成为呼叫建立关键依赖**（WITH_NATS_RPC 开启时）：NATS 宕机 = 新呼叫
  2s 内 503 有界失败（janitor），不影响进行中通话；退路 = 开关回 HTTP 模式。
  已写入 02-architecture.md §3.1。部署形态按此选择开关。
- **队列组语义反转**：广播 = 每节点**唯一**队列组（NATS 按组各投一份）；
  共享组才是负载均衡。与直觉相反，文档已注明。
- **边缘别名不是语义分叉**：gv-core emit 缝对三个 subject 自动双发，别名载荷
  与正名字节一致；JetStream 只持久化正名域（`gvos.>`），别名是纯传输拷贝。
- **rtpengine ng 保留**：理由见 §1；如未来要传输统一，加 NATS→ng 桥接进程即可，
  那是运维统一性收益，不是架构必要。

## 4. 未完成（原因 + 验证方法 + 优先级）

| # | 项 | 现状 | 原因 | 验证方法 | 优先级 |
|---|---|---|---|---|---|
| U1 | **NATS 宕机演练**（janitor 503 兜底） | 代码路径就绪、独立可审 | 共享环境有并行会话持续改写 ipallow/bl 状态 + 拉下栈，演练信号被污染 | 干净窗口：`docker stop deploy-nats-1` + SIPp，预期 2s 内 503 Billing Unavailable、janitor 日志出现 TIMEOUT | **P0**（升级前必须） |
| U2 | 风控封禁 e2e | 单测覆盖（TestCheckReg 等）；API/广播/htable 代码全就绪 | 同 U1 环境干扰（deny 规则反复被外部改写） | 配 freq 规则→连续两通→查 banned htable→第三通边缘 403 且 gv-core 无 /v1/call/start 调用 | P1 |
| U3 | 拆线 e2e（<1s 验收硬指标） | dlg RPC 代码就绪；TEARDOWN durable 就绪 | 未在干净环境触发余额耗尽场景 | 余额拉到阈值下→SIPp 长呼→量 BYE 到达延迟（预期 <1s） | **P0**（验收指标） |
| U4 | 注册镜像 e2e + reg_freq 告警 | 单测覆盖 | 未跑 SIPp register 场景 | `sipp -sf register.xml` → `/v1/admin/registrations` 出现条目→配 reg_freq 规则连发注册→risk.alert 触发 | P1 |
| U5 | 黑名单边缘拦截 e2e | 代码就绪（外部会话曾用它测过 86139 前缀） | 未系统验证 | POST blocklist add → SIPp 拨该前缀 → 边缘 403（无计费调用） | P1 |
| U6 | **QoS 统计解析入库** | cdr.raw 载荷已带 `$rtpstat` 字符串 | cdr 表 mos/jitter/packet_loss 列存在但 Go 侧未解析填充 | CDRR 消费者解析 rtpstat 文本→填三列→报表可见 | P2 |
| U7 | **compose 端口回归** | 主机端口临时改为 28080/15060/15432/14222（本机端口冲突） | 开发机与其他项目冲突 | 干净环境恢复默认端口或改用 .env 覆盖 | P2 |
| U8 | 测试矩阵补 RPC 面 | go test 全绿但 bus req/reply 的 Kamailio 形状无集成测试 | 时间 | 用 nats-server testcontainer 模拟应答方测 lockReply envelope 形状 | P2 |

## 5. 整体升级排期（建议）

**阶段 A：验收收口（0.5-1 天，干净环境窗口）**
- U1 + U3 + U2 + U4 + U5 一次跑完（五项共享同一套 compose+SIPp 流程）
- 产出：验收记录追加到 docs/bench/（含时间戳证据日志）
- 退出条件：六项能力 + RPC + 宕机兜底全部有 e2e 证据

**阶段 B：生产化加固（2-3 天）**
- U6 QoS 解析入库（mos/jitter/packet_loss 列）
- U7 端口治理（compose .env 化）
- U8 RPC 集成测试矩阵
- 高压验证：SIPp 300 CPS 下挂起/恢复的内存与延迟表现（billlock htable
  增长、janitor 扫描开销）——异步路径首次进入容量报告
- NATS 集群化评估：订阅端单连接容灾（上游 issue #2916）对 RPC 回复主题
  的影响；必要时 NATS 前置 VIP

**阶段 C：事件驱动纵深（1-2 周，与路线图 M15/M18 合流）**
- 录音完成事件 `gvos.media.recording.done`（对象存储元数据）→ ASR 自动挂载
  （M18 的事件源）
- Prometheus 业务面板消费总线事件（M15）
- gvos-asr 事件驱动挂载（record=true 广播→自动桥接）
- WebSocket 媒体流的边缘遥测上总线
- tls.reload 证书热轮换（广播→边缘重载，公网部署前项）

**阶段 D：HA 形态验证（1 周，M12 出口）**
- compose-ha 双 Kamailio：广播语义（每节点唯一队列组）实测、拆线广播的
  持有者路由正确性、gv-core 双副本 RPC 应答竞争
- Helm chart 上 K8s（GVOS_NODE_HOST= pod 名）全链路
- 故障演练全套（节点宕机 RTO/RPO、总线宕机 503、PG 宕机只读）

## 6. 变更记录

| 日期 | 提交 | 内容 |
|---|---|---|
| 2026-11-19 | `aad932f` | 六项边缘改造：网关面/风控封禁/CDR 总线/注册/拆线/黑名单 + 60s 重播 + trixie 镜像 |
| 2026-11-19 | `d226bf7` | 计费锁定异步 RPC（t_suspend/t_continue/janitor）+ answer 上总线 + QoS 入载荷 + entrypoint 物化 |
