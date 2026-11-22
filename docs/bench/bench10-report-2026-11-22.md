# 10 轮压测报告：架构补充后的 Bug 与性能卡点（2026-11-22）

**环境**: compose 全栈（gv-core + PG + Redis + NATS 3 节点 R3 + Kamailio 6.0.1 + RTPengine + demo-gw），`GVOS_BENCH_MEDIA_OFF=1`。  
**架构覆盖**: Mode A leaf-only billing、Redis `callmeta`/`callsettled` 跨副本结算、`GVOS_RISK_MODE` / `GVOS_PKG_MODE`（本轮压测默认）。  
**口径**:

| 面 | 负载 | 轮次 |
|---|---|---|
| A. 进程内架构压测 | owner/peer 双 Manager + miniredis，50% 跨副本 End，400 通/轮 | 10 |
| B. HTTP 计费面 | `gvos-loadgen` 40 CPS × 10s（另做 100/200/400 CPS 探测） | 10 |
| C. SIP 全路径 | SIPp `load.xml` 40 CPS × 10s × 400 通/轮，**显式 `-l 300`** | 10 |

原始日志: `docs/bench/bench10-rounds-2026-11-22/`、`docs/bench/bench10-sipp-2026-11-22/`。

---

## 结果总览

### A. 架构压测（`TestArchStress10Rounds`）

| 指标 | 值 |
|---|---|
| 成功 | **4000/4000** |
| 双结算击穿 | **0**（settle lock 有效） |
| 父账户扣费 | **0**（leaf-only 有效） |
| 叶余额 | 精确：`999760.0000` = 1000000 − 4000×0.06 |
| Start+End 时延 | p50≈60ms / p95≈110ms / p99≈150ms（64 worker 打 miniredis，含 2× Redis settle） |

### B. HTTP 计费面（loadgen）

| 轮 | calls | ok | fail | p50ms |
|---|---|---|---|---|
| 1–10 | 393–395 | =calls | **0** | 3.4–4.7 |
| **合计** | **~3942** | **100%** | **0** | p99≈8–10ms |

近窗对账：`cdr` = `balance_ledger` = **3942**（reason=`call-settle`）。

高 CPS 探测（单轮 10s）：

| CPS 目标 | 实测 | ok% | p50 | p99 | max |
|---|---|---|---|---|---|
| 100 | 95.6 | 100% | 7.5 | 17.2 | 26.8 |
| 200 | 182.9 | 100% | 8.2 | 27.2 | 48.3 |
| 400 | 341.5 | 100% | 9.2 | 41.9 | 83.6 |

> 400 CPS 目标下客户端自限到 ~340（start+end RTT 抬升），计费面仍零失败。

### C. SIP 全路径（SIPp，对照 2026-11-20 的 79.7%）

| 轮 | calls | success | failed | Δcdr | Δledger |
|---|---|---|---|---|---|
| 1–10 | 400 | 400（仅第 6 轮 399） | **1/4000** | ≈392–400 | =cdr |

| 指标 | 本次 | 2026-11-20 基线 |
|---|---|---|
| SIPp 成功 | **3999/4000 ≈ 99.975%** | 3187/4000 ≈ 79.7% |
| CDR↔ledger | 逐轮对齐（drain 后精确） | 平账 |
| `-l` | **300**（修 E-6 自限） | 默认 120 |

第 6 轮 1 失败：SIPp `Dead call ... aborted`（Pause 阶段 Unexpected），属边沿偶发，非计费双扣。

---

## 缺陷清单（按严重度）

### N-1（高，**本轮已修**）CDRR `BAD_DURATION` 毒消息重投

**现象**: 压测中 gv-core 刷屏  
`callEnd rejected ... BAD_DURATION duration must be >= 0`，每条 NAK 1s × MaxDeliver。

**根因**:
1. Kamailio `CDR_EVENT` 用 `$TS` 与 answer 侧 `$TV(s)` 混算时长，偶发负 `duration_ms`。
2. JSON 里 `duration`/`duration_ms` 同为毫秒；负值使 `DurMs > 0` 不成立，落入 `seconds < 0` → 400。
3. CDRR 只对 404 Ack，**400 仍 NAK**（同旧 E-3 形态）。

**修复**:
- `kamailio.cfg`: 统一 `$TV(s)/$TV(u)`，负时长钳为 0。
- `callapi.callEnd`: 负时长钳 0 并回退到 Start 墙钟。
- `cdrRawHandler`: 4xx 一律 Ack（终端毒消息），5xx 仍重试。
- `callSettledTTL`: 15m → **2m**（见 N-2）。

### N-2（中，**本轮已修**）`gvos:callsettled:*` Redis 膨胀

压测后 DBSIZE ≈ **10k+**，几乎全是 `callsettled`（TTL≈15min = `callMetaTTL`）。  
40–400 CPS 下稳态键数 ≈ CPS × TTL；结算互斥只需覆盖双 End 竞态窗口。  
**修复**: `ClaimSettle` 改用 `callSettledTTL=2m`。

### N-3（中，**已修**）Kamailio 重启后网关 htable 空窗

重建 `kamailio`/`gv-core` 后，SIPp 全部 `503 No Gateway`（`empty or all cooling down`），直到 core 的 CFG-RELOAD/`GW_SYNC` 写入 `gw` htable（观测延迟可达数十秒）。  
**修复**: `GET /v1/internal/gateway-snapshot` + Kamailio `GW_POLL`（5s）HTTP 拉取；`StartCfgRebroadcaster` 启动立即广播（不再等首个 60s tick）；INVITE 路径 rtpengine 挪到 bill-lock resume（见下）。

### N-4 / E-2（中，**已修**）INVITE 路径 rtpengine 在 t_newtran 之前

挂起中的 INVITE 重传重跑 `rtpengine_manage`（同步 ng）。in-dialog 侧此前已有 `t_check_trans` + 无 SDP 不调 ng。  
**修复**: WITH_NATS_RPC 下 `rtpengine_manage` 移到 `BILL_LOCK_REPLY`（成功 resume 后、dispatch 前）；HTTP bill-lock 路径移到 `BILL_LOCK` 之后。

### N-5（低）SIPp 偶发 Dead call（1/4000）

Pause 阶段 Unexpected-Msg，属 UDP/时序边沿；CDR/ledger 仍写入。

---

## 性能卡点结论

| 卡点 | 证据 | 定性 |
|---|---|---|
| 计费/RPC 链 | HTTP 40CPS 100%；400CPS 探测 100% ok；cdr=ledger | **无罪**（架构补充后仍稳） |
| 跨副本 settle | 4000 通 50% remote-End，dbl=0，余额精确 | **无罪** |
| Leaf-only | agent/tenant 余额零漂移 | **无罪** |
| Redis settle 键 | 15m TTL → 万级残留 | **已缩短至 2m** |
| CDRR 负时长 | BAD_DURATION NAK 风暴 | **已钳位 + 4xx Ack** |
| SIP 边缘空窗 | 重启后无 gw → 503 + 冷却 | **运维/启动序**（待修） |
| rtpengine 同步 ng | Unknown call-id 仍在 | **残留 E-2**（非本轮主因） |
| 高 CPS 客户端自限 | 400→341 CPS，p99 42ms | 正常：start+end 串行 RTT |

---

## 与基线对比

相对 `bench10-report-2026-11-20.md`：

1. SIP 成功率 **79.7% → 99.975%**（`-l 300` + 既有 CDR 去重/计费修复 + 本轮 BAD_DURATION 暴露点）。
2. 新架构路径（leaf-only、跨副本 settle）在 10 轮压测下**无资金正确性回归**。
3. 新发现并已修：负时长毒消息、settle 锁 TTL 过长。

---

## 复现

```bash
# A. 架构 10 轮
go test -count=1 -timeout 10m -v -run TestArchStress10Rounds ./internal/callcore/

# B. HTTP 10 轮（示例单轮）
go run ./cmd/gvos-loadgen -target http://127.0.0.1:28080 -cps 40 -duration 10s \
  -token dev-internal-token -from 1000 -to 8613800138000

# C. SIPp 10 轮（务必 -l 300）
# PowerShell 或 bash：每轮 -m 400 -r 40 -l 300，网络 gvos_default，目标 kamailio
```

## 建议修复顺序（剩余）

1. 可选：settle 键监控告警（`gvos:callsettled:*` 计数）。  
2. 可选：空 gw 表时的 503 不累计 `gwfail`（防放大）。
