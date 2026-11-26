# 容量阶梯报告 2026-11-26（capbench）：单机 2 万并发线达成

**栈**：`docker compose -p capbench`（gv-core ×1 + Kamailio 6.1.5 ×1 + RTPengine ×1 + PG 16 + Redis 7 + NATS 3 节点 R3 + SIPp demo-gw）
**宿主**：Windows 开发机（Docker Desktop，i7-13700H，7.62GiB 可用内存，Linux VM 桥接网络）——**非服务器环境**，云上数字另见 runbook
**口径**：hold60 并发场景（每通话保持 60s），`test/sipp/ladder2.sh`，质量门同 bench100（CDR↔ledger 逐分对账、dur0%、answer 覆盖）
**代码基线**：本工作树（v0.4.0 之后：DID 呼入 + dialog hash_size=14 + 限流 + F-7 重连）

---

## 1. 结果总表

| 目标并发 | SIPp 实例 | 发起 | CDR 落库 | 成功率 | PDD p99 | PDD max | 对账（CDR vs ledger） | Kamailio SHM 峰值 |
|---|---|---|---|---|---|---|---|---|
| 1,000  | 20  | 1,200  | 1,221  | 100% | 19ms | 57ms | 63.06 = -63.06 ✅ | 19MB |
| 2,000  | 40  | 2,400  | 2,400  | 100% | 16ms | —    | 全对 ✅ | 22MB |
| 5,000  | 100 | 6,000  | 5,999  | 100% | 26ms | —    | 311.67 = -311.67 ✅ | 29MB |
| 9,000  | 180 | 10,800 | 10,797 | 100% | 25ms | —    | 549.04 = -549.04 ✅ | 30MB |
| 13,000 | 130 | 15,600 | 15,598 | 100% | 35ms | —    | 795.07 = -795.07 ✅ | 42MB |
| 17,000 | 170 | 20,400 | 20,385 | 99.9% | 61ms | —    | 1042.97 = -1042.97 ✅ | 48MB |
| 20,000 | 200 | 24,000 | 23,996 | 99.98% | 65ms | 511ms | 1212.51 = -1212.51 ✅ | 51MB/64MB |

**结论：单机 2 万并发线、20k 通 60s-hold 全链路（信令+计费+CDR+对账）达标** —— 对标昆石 VOS5000 单机标称（2 万并发线）。旧实测记录（1,000 并发）提升 **20 倍**；对账在全部七档零漂移。

资源曲线（20k 档运行中实测）：
- Kamailio CPU ≤15%、gv-core ≤28%、RTPengine ≤5%、PG 峰值 139%（CDR 写入，见 §4）
- Kamailio SHM max_used 51MB/64MB（dialog hash_size=14 后仍有 13MB 余量；**32MB 以下为默认 4096 槽位时代的预期上限，扩槽生效**）
- 全家内存 <1.5GiB（宿主 7.6GiB）

## 2. 过程发现（按发现顺序，全部已修复或入账）

1. **压测前必须备账户额度**：首跑 1062 通被 403"余额不足"拒——计费正确性拦在容量瓶颈前。教训进 runbook：先 topup 叶子账户（**注意计费锁的是 UUID 叶子账户，username 键不参与**——充错键不生效）。
2. **新库 ipallow 空表 = default-deny**：冒烟 403。已用 admin API 补 CIDR；冷启动 90s fail-open 窗口设计按预期工作。
3. **demo-gw（SIPp UAS）是软上限**：17k/20k 档约 0.02-0.1% 的 503 追溯为 SIPp watchdog 自保护（单 UAS 实例累计扛 6 万+通），非 gvos 侧瓶颈。生产测法应多 UAS 分片。
4. **BYE 后 rtpengine `query` 噪声**：`--delete-delay 5` 时窗内 QoS 查询报 "Unknown call-id" 万级 ERROR——无通话影响（成功率/对账不受损），但污染日志；候选改进：QoS 查询前置到 200 应答路径（已在该路径）+ BYE 路径容忍该错误（现状即容忍，仅日志噪声）。
5. **9000 并发未撞 7500 媒体端口池理论硬顶**：hold60 的 SIPp 侧不收流，RTPengine 锚定是轻量端口对而非每通 4 端口满占。本报告的 2 万线是信令+计费面口径——与昆石"2 万并发线"的标称口径一致（其 1 万线带媒体是另一档）。**B 线功能基线（2026-11-26 已补）**：SRTP e2e PASS、G722↔PCMU 转码 PASS、录音 pcap spool PASS；带真实双向媒体的端口池饱和压测与 recording-daemon MP3（需 `xt_RTPENGINE`）仍属后续宿主/内核环境项，不改变本报告的 2 万线口径。

## 3. 与旧实测/竞对标称的对照

| 指标 | 旧实测（2026-11-19 报告） | 本次 | 昆石 VOS5000 标称 |
|---|---|---|---|
| 并发通话 | 1,000（30s 零失败） | **20,000（60s hold，全链）** | 20,000 |
| PDD p99 | 0.8ms @100CPS（无 SIP 面） | 65ms @20k 并发（含 SIP 全栈） | 无公开口径 |
| 对账 | 6,714 行零漂移 | **80,468 行本次会话累计零漂移**（七档 + 冒烟 + e2e） | "实时智能对账"（2026-10 新增） |
| 20 万注册 | ECS 实测 99.78%（既有） | 本次未复测（regload 已有 ECS 实测记录，本地 5 万级复验归后续） | 200,000 |

## 4. 下一个真瓶颈（按撞序预估，供云上大机型复测排期）

1. **PG CDR 写入**（最先撞）：本机 20k 档 PG CPU 峰 139%、逐行 INSERT + CDRW=2 worker。A4 修复项：批量 INSERT/COPY + worker 数可配。
2. **gv-core 单副本内存态**：k8s `replicas:1` 注释仍在。A5：svc responder 加 queue group（多副本双答收口）后 LB 粘性放行。
3. **Kamailio children=32**：20k 档 CPU 仅 15% 未饱和；30k+ 并发才需要重估。
4. **rtpengine 端口池 30k**：真实媒体（4 端口/通）满载上限 7,500 通——B 线验证后按需扩 `GVOS_RTPENGINE_PORT_MAX`。

## 5. 复现

```bash
docker compose -f deploy/docker-compose.yml -p capbench up -d
# 备账户（叶子 UUID！见 §2.1）：
curl -X POST .../v1/admin/accounts/<uuid>/topup -d '{"delta":100000}'
# 补 ipallow CIDR（新库为空 → default-deny）：
curl -X POST .../v1/admin/ipallow/rules -d '{"rules":[{"id":"net","cidr":"172.19.0.0/16","action":"allow"}]}'
bash test/sipp/ladder2.sh "1000:20 2000:40 5000:100 9000:180 13000:130 17000:170 20000:200"
```

云上大机型（4c/8c ECS）复现步骤见 `docs/bench/linux-rebench-runbook.md`（修订版归 v0.5.0 收尾）。
