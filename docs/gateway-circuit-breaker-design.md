# 网关熔断设计（在线目录 + 观测 + 解锁）

**状态**：草案（M12 裁决：解锁 API 无的放矢——先有在线熔断，才谈解锁）
**日期**：2026-11-28
**背景**：`gateways` legacy 表（001）有 `locked_until`/`consecutive_failures` 列，但那是**一次性引导路径**——settings 主路径的在线网关目录没有任何故障计数/冷却机制。呼叫打到坏网关只会一直失败（failover 到下一网关掩盖了症状，坏网关永远排在首位反复被试）。
**关联**：[api-gap-analysis.md](api-gap-analysis.md) M12 · [competitive-analysis-vos-sbc.md](competitive-analysis-vos-sbc.md)（VOS「智能路由」的对标项：故障冷却已是 LCR 排序的一环——但冷却数据源不存在）

---

## 1. 故障信号从哪来

网关失败的可观测证据在 core 已有（零新增采集）：

| 信号 | 来源 | 判定 |
|---|---|---|
| CDR 的 `sip_code` + `gateway_id` | `cdr` 表（每通电话落库） | 4xx/5xx + 无应答 = 一次失败候选 |
| 通话中拆线 | `release` ≠ normal | 失败候选（需阈值区分业务正常挂断） |
| 账单漂移 | 对账（CDR vs ledger） | 非熔断信号，另论 |

**裁决**：熔断器不挂在实时呼叫路径上（为打分加一次同步写是反模式）。信号由 **CDR 结算钩子** 异步记账——`callEnd` 之后对 `(gateway_id, sip_code)` 做一次内存计数（与 CDR 落库同一事务语义之外，best-effort，重启从 CDR 表回放最近窗口重算）。

## 2. 状态机（每网关）

```
healthy ──连续 N 次失败──> suspect ──M 窗口内失败率 > X──> tripped(冷却 T)
   ▲                          │                              │
   └──── 成功一通 ────────────┘              冷却到期 → half-open ──探测通→ healthy
                                              │
                                              └──探测失败→ tripped(冷却×2, 上限 TMax)
```

参数（settings 新键 `gateway_breaker`，走既有 settings 管道 + cfg.reload 广播）：
- `consecutive_failures`: 5（连续失败次数）
- `window_seconds`: 120 + `window_min_calls`: 10（触发统计窗口的最小样本，防低话量误熔断）
- `failure_rate`: 0.5（窗口内失败率阈值）
- `cooldown_s`: 300，`cooldown_max_s`: 3600（指数退避上限）

成功样本：`sip_code` 2xx 且 answered；失败样本：4xx（404/486 除外——被叫问题不是网关问题）/5xx/超时无响应。**404/486 明确不计**（打给空号不是网关的锅——熔断误伤好网关是最贵的错误）。

## 3. 生效路径

1. **选路排序**：`gateway/lcr.go` 的广播排序已经支持故障冷却位——熔断器把 `tripped` 网关的优先级压到全部 healthy 之后（不对，直接从广播集合剔除更干净）；half-open 保留在尾部作探测位。
2. **广播**：状态变化走既有 `gvos.cfg.reload`（entity=gateway 全量快照），边缘 htable 照常收敛，零边缘改动。
3. **观测**：`GET /v1/admin/gateways` 每行加 `breaker: {state, consecutive_failures, window_failures, trips_until}`；`GET /v1/admin/gateways/{id}/breaker` 详情（含最近窗口原始计数）。
4. **解锁**：`POST /v1/admin/gateways/{id}/unlock`（admin 面审计）——M12 的原始诉求，在熔断存在之后才有意义。手动解锁 = 清计数 + healthy。
5. **多副本**：计数器 per-replica 本地（结算钩子本就分散在各副本），状态变化经 cfg.reload 全量广播对齐；**窗口计数不跨副本合并**（每副本独立判定，偏差可接受——熔断是保护性动作，宁可略早触发）。

## 4. 边界与风险

- **无话量网关**：窗口最小样本数保护；从未有话务的网关永远 healthy（没有证据不动刀）。
- **瞬态上游抖动**：指数退避 + 探测位保证恢复路径存在。
- **单网关部署**：全部 tripped 时不能无路可走——选路在没有任何 healthy 网关时回退全量（带告警事件 `gvos.risk.alert`？不对——用 `gvos.cfg.reload` 之外新告警主题？复用 `gvos.risk.alert` 语义不符。**裁决：tripped 到「全部不可用」时发 `gvos.call.originate` 无关的新事件 `gvos.gw.breaker`（观测用），选路回退全量并记 audit**）。
- **CDR 回放重算**：重启后从 CDR 表回放 `window_seconds` 窗口重建计数（SQL 聚合一次）。

## 5. 实施切片

| 切片 | 内容 | 判定 |
|---|---|---|
| CB-1 | 结算钩子计数器 + 状态机 + boot 回放（纯 core，内存+PG） | 单测：连续失败→tripped→退避→恢复 |
| CB-2 | 选路剔除 + cfg.reload 广播 + 观测字段 + unlock API | 内存 e2e：坏网关被剔除、unlock 恢复 |
| CB-3 | `gvos.gw.breaker` 观测事件 + 文档 | 事件目录进 conventions |

## 6. 变更记录

| 日期 | 变更 |
|---|---|
| 2026-11-28 | 初稿（M12 裁决产物；解锁 API 排 CB-2） |
