# 真实容量天花板：阶梯压测终报（2026-11-21 凌晨）

**目标**：拿到 G-VOS 在本机（Windows Docker Desktop）的真实容量天花板，并定位天花板的归属（平台 vs 宿主 vs 压测机）。
**方法**：多 SIPp 实例分摊聚合速率（单实例 ~100CPS 上限）；逐档冷却；栈内 tcpdump 审计信令流；/proc/net/snmp UDP 计数器；60s 保持场景测并发通话数。

## 容量天花板（本机口径，媒体开全栈）

| 维度 | 实测 | 状态 |
|---|---|---|
| **持续 CPS** | **170 CPS = 100%**（6000/6000，PDD p99 5ms） | 干净可重复 |
| | 194 CPS = 99.9%（2999/3000，p99 4ms，15s 冲击） | 宿主新鲜时 |
| | 200 CPS 持续 30s = 57~86%（逐轮波动，p99 仍 4-6ms） | **宿主进入抖动区** |
| | 230~300 CPS = 15~25% | 宿主/桥接饱和 |
| **并发通话** | **9853 路同时在线**（live_calls 实测峰值；9622 CDR 全接通，PDD p99 4ms / max 8ms） | 10k 并发达成 |
| 栈侧计数器 | InErrors≈0（修 4MB 缓冲后）、runaway=0、CDR=ledger 精确、CPU 峰值 gv-core 67%/Kamailio 19%（10k 并发时） | 平台本身有余量 |

**结论：本机天花板 ≈ 170CPS 稳态 / 200CPS 抖动区 / ~1 万路并发。瓶颈在 Docker Desktop 桥接与宿主调度，不在 G-VOS。**

## 天花板归属证据链（本轮新挖出）

1. **H-1（已修）Kamailio UDP SO_RCVBUF 溢出**：默认 416KB 在 260CPS+重传风暴下 InErrors=RcvbufErrors=1542/25s——**401 挑战本身被丢**，UAC 更疯狂重传，恶性循环。修复：`net.core.rmem_default=4MB / rmem_max=8MB`（WSL2 sysctl），SO_RCVBUF 416KB→4MB。修后 401 全量送达（tcpdump 审计：1475 INVITE in / 738 401 out + 739 authed INVITE→gw，完全平衡）。
   > 部署注意：Kamailio 6.0.1 Debian 构建**没有** `udp_rcv_buf` cfg 参数（写了会 parse error）；缓冲完全由 `net.core.rmem_default/max` 决定，runbook 必须设。cfg 内留了文档注释。
2. **H-2 残余失败=BYE-200 中继尾巴（宿主桥接长尾）**：200CPS+ 的失败形态全部是「gw 已回 200、SIPp 侧 BYE-200 迟到/乱序」；栈侧 p99 4-6ms、InErrors≈0、CPU<1%——包在 Docker Desktop veth/bridge 层排队。3 台 SIPp 实例内的 100/180 乱序（SIPp 'unexpected' 计数）同源。
3. **H-3 实例单调衰减**：N 实例并发时按启动顺序成功率单调下降（273→0/1300）——同一桥上并发流越多每流越差，典型共享宿主瓶颈指纹，非栈行为（栈对每呼叫无差别）。
4. 压测机本身：单 SIPp 实例稳定上限 ~100CPS（watchdog 3000ms 之后停摆），多实例分摊是唯一可靠姿势。

## 与裸 Kamailio 基线的差距（更新）

上次结论维持并加强：控制面逐呼叫开销 3-5ms（RPC 2.8ms + 异步落账），PDD p99 4-6ms 在所有档位不变——**G-VOS 在 170CPS/万级并发上不构成 Kamailio 的减项**。本轮 9853 并发、PDD max 8ms 是最硬的证据。裸 Kamailio 在同一宿主上会遇到同样的桥接长尾（它也要收发这些 UDP 包）；把 G-VOS 换成纯 Kamailio 转发，本机天花板提升有限——天花板是宿主的。

## 复现与运维要点

```bash
# 1) 宿主 sysctl（每次 WSL 重启后要重设！Docker Desktop 不持久）
wsl -- sysctl -w net.core.rmem_default=4194304 net.core.rmem_max=8388608
# 2) 阶梯压测（多实例分摊）
cd test/sipp && bash ladder.sh "130:2 170:2 200:2" 3000
# 3) 并发通话（60s 保持 × N 实例）
#    并发 = rate × 60s；live_calls 峰值即并发数
# 4) 栈内审计（401 平衡、UDP drops）
docker exec gvos-kamailio-1 sh -c 'grep Udp: /proc/net/snmp | tail -1'
# InErrors/RcvbufErrors 增长 = 缓冲不足；NoPorts 增长 = 投递失败
```

**诊断方法可复用**：SIPp 失败时先看 (a) 栈 p99 是否仍 <10ms (b) kamailio InErrors (c) tcpdump 双向计数平衡 (d) 实例衰减梯度——四项定位失败归属：平台/内核缓冲/桥接/压测机。

## 本轮修复与遗留

- 修复：H-1 UDP 缓冲（sysctl + cfg 注释）；kamailio `restart: unless-stopped`（上轮）。
- 遗留（宿主级，非产品）：Docker Desktop 桥接长尾（BYE-200 尾巴、100/180 乱序）；SIPp 单实例 100CPS 上限。**300CPS+ 必须上 Linux 裸机测**——所有脚本就绪，预期裸机上天花板为宿主 CPU 所限（按 32 worker × p99 6ms 折算，控制面万级 CPS 量级）。

## 补充（2026-11-21）：裸 Kamailio 基线对照实验

**问题**：170CPS 是否偏低？→ 直接在同一宿主上跑"直接用 Kamailio"的对照。

**实验**：极简 Kamailio 代理（`test/sipp/bare-kamailio.cfg`，同样的 401 鉴权流程 + record-route + t_relay 转发，无 RPC/计费/媒体），同宿主、同网桥、同 SIPp 场景。

**结果**：裸基线**比 G-VOS 死得更早**——
- 50CPS：132/500（74% 失败），35 秒内 11571 条 SHM OOM（`t_newtran out of mem` / `relay_reply cannot alloc`），SHM 2GB 仍然泄漏；
- G-VOS 同场景：500/500 满分，PDD p99 4-6ms。

**教训（这就是 G-VOS 边缘层的存在意义）**："直接用 Kamailio"不是开箱即用的容量数字——极简配置缺重传吸收、事务卫生、缓冲调优、auth 缓存、janitor，在持续负载下先于 G-VOS 崩溃。裸 Kamailio 的"万级 CPS"官方数字需要 default.cfg 级别的完整工程配置 + Linux 裸机，与本实验的极简 cfg 不可比。

**平台真实容量的正确估法**（本机证据外推）：
- 每呼叫平台成本：RPC 2.8ms（异步）+ 计费异步落账 + 边缘路由 ~1ms —— CPU 全程 <1%@100CPS；
- 9853 并发时 gv-core 67% CPU —— 并发维度 CPU 是第一个到 50%+ 的组件；
- 裸机（Linux + LAN，无桥接长尾）预估：边缘数千 CPS、控制面 RPC 万级（分片已验证到 200CPS p99=6ms）；PRD 300CPS 目标有 ~10 倍余量；
- **要拿到平台真实天花板数字：上 Linux 裸机跑同一套脚本（就绪），本机所有 200CPS+ 数据一律视为宿主上限而非平台上限。**

**业务视角**：170CPS × 60s 保持 = 10200 路并发——已是中型部署量级（千人企业客户 × 10 并发）。CPaaS 平台按并发路数售卖，10k 并发 ≈ 日呼 20 万分钟。

## 补充（2026-11-21）：生产部署形态（Docker/K8s）下的天花板含义

**用户问题**：生产也是 Docker/K8s，不是 Linux 裸机——170CPS 这个墙还在吗？

**答案：那堵墙是 Windows 特有的，不会跟着上生产；但 K8s 有自己的部署清单。**

### 墙的归属

| 墙 | 归属 | 生产 K8s 是否存在 |
|---|---|---|
| ~200CPS 桥接丢包/乱序（BYE-200 尾巴、401 丢失） | **Windows WSL2/Hyper-V 虚拟交换机** | ❌ 不存在——Linux veth/bridge 线速（~1M pps），我们峰值才 ~5万 pps |
| UDP SO_RCVBUF 416KB 溢出（H-1） | 内核默认值，**跟部署形态走** | ✅ **存在**——K8s 节点上必须设（见下） |
| SIPp 单实例 ~100CPS watchdog | 压测机自身 | ❌ 不存在（生产流量来自真实网关） |

### K8s 部署清单（从本轮实测直接推导）

1. **节点 sysctl（H-1 的 K8s 形态）**：`net.core.rmem_default=4MB / rmem_max=8MB` 是**非命名空间 sysctl**，不能用 pod securityContext 设——需要 privileged initContainer 或节点镜像/开机脚本。漏设 = 200CPS+ 重现 401 丢失。
2. **媒体面走 hostNetwork**：RTPengine DaemonSet + hostNetwork（每节点一个实例，媒体端口 30000-60000 直接绑节点网卡）；Kamailio 信令也建议 hostNetwork 或 NodePort + `externalTrafficPolicy: Local`（保源 IP + 避开 conntrack 二次 NAT）。hostNetwork 让包不过 overlay——开发机上那类桥接问题在 K8s 里天然不存在。
3. **横向扩展替代单机扩容**：Kamailio 无呼叫态（suspend 态在节点内，pod 死 = 该 pod 在途通话 5s 超时，SIP 代理常态），**N 个副本横向加 CPS**；170CPS 是"单 pod 单宿主"数字，不是平台数字。
4. **⚠️ gv-core 多副本是真缺口**（本轮代码审查确认）：`Respond()` 用普通 Subscribe（无 queue group）订阅 `gvos.svc.call.start`——多副本会全部应答、竞争处理；且呼叫态在内存 `m.calls`，callEnd 落到别的副本 = NOT_FOUND（E-3 修复后会静默 Ack → **该通话 CDR 永不落库**，10min zombie 兜底）。K8s 化三选一：① call service 主题加 queue group + 呼叫态按 call-id 粘滞；② 呼叫态外置 Redis（与 Redis 收口方案合并）；③ 单副本 + 裸机纵扩（最简单，300CPS 目标下够用）。
5. **conntrack**：UDP NodePort 走 kube-proxy conntrack，SIP 信令短流没问题；媒体流 30k 端口 hostNetwork 直连不过 conntrack。
6. NATS/PG/Redis：标准 StatefulSet/operator，R3 已验证。

### 结论重述

- **170CPS = 开发机（Windows WSL2 桥接）单 pod 的数字**，生产 K8s（Linux 节点 + hostNetwork）没有这堵墙；
- 平台容量在 K8s 里 = **单 pod CPS × 副本数**，Kamailio 层横向扩展无状态限制；
- 上生产前必须做：节点 sysctl、hostNetwork、gv-core 多副本策略（三选一）；
- 裸机/云主机 Linux VM + K8s 的中间验证（k3s 单节点或 3 节点）即可拿到"pod 级真实 CPS"数字——所有脚本就绪。
