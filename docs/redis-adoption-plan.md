# 后期改进方案：引入 Redis 收口跨副本运行时状态

**状态**：规划稿（未实施）
**日期**：2026-11-19
**范围**：gvos-core 引入 Redis 作为**运行时状态权威**（余额/会话/注册快照），PG 保持配置与 CDR/台账权威，NATS 保持事件总线职责不变。

---

## 0. 背景与动机

代码与文档早已把 Redis 列为生产路线：

- `internal/account/account.go:28` —「production implementation swaps this for Redis (call locks) + PostgreSQL (authoritative)」
- `internal/pkgplan/pkgplan.go:14` —「production moves this to Redis with the same shape」
- `README.md:97` — 已知限制：「余额权威当前在各副本内存（预锁定语义正确），生产部署按路线图换 Redis 收口」
- `docs/02-architecture.md:57` — 设计目标：INVITE 时 `call_lock(account, estimated_cost, ttl)` 写 Redis

当前所有运行时状态都是每副本进程内 `sync.Mutex` 守护的 map。多副本（N>1）下的正确性缺口：

| # | 缺口 | 现状位置 | 后果 |
|---|---|---|---|
| 1 | 余额权威 per-replica | `internal/account/account.go`（Reserve/Commit 只改本地内存，仅 TopUp 走绝对值广播） | 两副本各 Reserve 同一余额→超卖；中间扣费不传播 |
| 2 | 并发上限 ×N | `internal/callstate/callstate.go:219-241`（admitConcurrent 只扫本副本 map） | 账户并发上限实际为 cap×N |
| 3 | CPS 上限 ×N | `internal/callstate/callstate.go:246-262`（admitCPS 同上） | 同上 |
| 4 | 崩溃副本的呼叫无人回收 | sweeper（callstate.go:80-101）只扫本进程 map | 副本挂→其呼叫与预锁资金永久蒸发 |
| 5 | 风控滑窗只统计本副本 | `internal/riskctrl/riskctrl.go:93-206`（calls/dayCost/regs 本地 map） | 欺诈频次/日费用上限失效为 ×N |
| 6 | 套餐周期用量 per-replica | `internal/pkgplan/pkgplan.go:43-56`（freeUsedSecs/spend 本地） | 免费额度实际 ×N，重启清零 |
| 7 | 注册镜像重启丢失 | `internal/regtable/regtable.go`（纯内存，无持久化） | 重启后表空到新事件到来 |
| 8 | 周期任务全副本重复执行 | 小时 rollup（admin.go:633）、60s 重播器（dispatch.go:348） | 幂等但冗余 |

Redis 是这些缺口的对症药：分布式原子计数/锁/TTL 结构天然跨副本。

## 1. 目标与非目标

**目标**

1. 余额与预锁权威收口到 Redis（Lua 原子链上多账户扣减）。
2. 并发/CPS/套餐用量/风控窗口成为全副本聚合值。
3. 任意副本可回收任意副本的僵尸呼叫（CDR 幂等靠 PG `uq_cdr_call` 兜底）。
4. Redis 不可用时的语义明确（计费门 fail-closed，其余 fail-open）。
5. Redis 关闭（未配置）时行为与现状完全一致（向后兼容）。

**非目标**

- **配置类快照不进 Redis**：gateway/ipallow/rate/blocklist 是配置，PG+NATS 广播已是正确通道（换 Redis 只是换传输层，无正确性收益，还把配置路径绑上 Redis 可用性）。留为未来可选项（版本化 key + pub/sub 失效广播）。
- **NATS/PG 职责不变**：JetStream durable worker、CDR 幂等（`uq_cdr_call` + ON CONFLICT DO NOTHING，pg.go:165-180）、配置持久化保持原样。
- **Redis Sentinel/集群**：本期单实例（compose-ha 同）；架构文档的 master+sentinel（02-architecture.md:86）留为部署层演进项。
- 混合模式不支持：一个部署里所有 gv-core 副本要么都配 Redis 要么都不配。

## 2. 全局设计决定

- **客户端**：`github.com/redis/go-redis/v9`（Go 1.21 兼容）；单测用 `github.com/alicebob/miniredis/v2`。依赖已 vendor，需 `go get` + `go mod vendor`。
- **配置**：flag `-redis` + env `GVOS_REDIS_URL` 回退（照抄 `-nats`/`GVOS_NATS_URL` 模式，`cmd/gvos-core/main.go:323-344`）。空 = Redis 关。
- **装配顺序**：Redis 在 `NewPGServer` 之前连接（余额权威必须在装载账户前就位）；**配置了但连不上 → 启动 fatal**（与 PG 同待遇，区别于 NATS 的 auxiliary 定位）。
- **故障语义（逐子系统）**：

| 子系统 | Redis 故障时 | 理由 |
|---|---|---|
| 余额 Reserve/Commit（call start 计费门） | **fail-closed**：错误→HTTP 503，Kamailio 拒呼 | 资金权威不可静默降级 |
| 呼叫注册/并发/CPS 准入 | fail-closed（同一条链路） | 限流失效=超卖 |
| pkgplan.Apply / risk.SettleCost（call end） | fail-open：按列表价落 CDR + ERROR 级日志 | 事后结算不得丢 CDR，PG 幂等兜底 |
| riskctrl.CheckCall / CheckReg | fail-open：跳过规则评估 + 告警日志 | 欺诈引擎宕机≠全平台停摆 |
| regtable 写入 | fail-open：仅内存镜像 | 纯可观测性 |
| 小时 rollup / 重播器单跑锁 | 拿不到锁→跳过本轮（另一副本会做） | 幂等任务 |
| 卡点/统计数据 | 直接取本地值 | 观测辅助 |

- **账户配置 vs 余额分离**：账户链/RateGroup/Concurrent/CPS 等仍走 PG 加载 + NATS 广播（本地 map）；**只有 `Credit` 余额与预锁进 Redis**。`Get/List` 在 Redis 开启时合并本地配置 + Redis 余额。
- **内存 demo 模式**（无 DSN）同样支持 Redis（测试与演示双副本）。

## 3. 分阶段实施计划

### Phase 0 — 基础设施与接线

1. `go.mod`/`go.sum`/`vendor/`：加 go-redis/v9 与 miniredis/v2。
2. 新建 `internal/rds` 包（照抄 `internal/events/events.go` 房式）：
   - `New(url) (*Client, error)`：URL 解析 + Ping + 重连参数；
   - 常量块：key 前缀 `gvos:`、超时（events.go:69-77 风格）；
   - 幂等 `Close()`；`Script` 封装（EvalSha 回退 Eval）；
   - `lock.go`：`Acquire(key, ttl)`（SET NX EX）供单跑者锁。
3. `cmd/gvos-core/main.go`：
   - flag/env（main.go:323-329、333-344 旁）；
   - `Server` 加字段 `rds *rds.Client // nil = Redis off`（照 main.go:50 events 注释风格）；
   - 构造顺序：redis → NewPGServer/NewServer（构造函数加 rdb 参数，nil 允许）→ NATS → HTTP；
   - shutdown 尾部（main.go:388-396 后）加 `s.rds.Close()`；
   - `statsBody()`（main.go:287-310）报 `redis: on/off`。
4. `deploy/docker-compose.yml` + `compose-ha.yml`：`redis:7-alpine` 服务，healthcheck `redis-cli ping`（用 postgres 的 5s/3s/10 次配方），gv-core `depends_on: service_healthy` + env `GVOS_REDIS_URL: "redis://redis:6379"`。
5. `deploy/helm/gvos/values.yaml` 加 `redis.url: ""`（默认外部，照 `nats:` 注释风格）+ `templates/gv-core.yaml` 条件注入 env。

### Phase 1 — 余额与预锁权威（account）

**与 ADR-0003（金额 int64 定点化）合并实施**：本 Phase 是账本重构的唯一免费窗口——动账本的同时把 `Credit/MaxDebt`/费率/中间量从 float64 改 int64 计费单位（初定 1e-4 元定标，实施前用全量费率样本测算是否需 1e-5/1e-6）。HTTP 契约金额仍输出十进制字符串（客户无感）。整数化后 Lua 用 INCRBY 原子运算，天然免双端浮点语义风险。

- 新建 `internal/account/balance_redis.go`：
  - key：`gvos:bal:{accountID}`（string int64 计费单位）；
  - **Reserve**：Go 侧从本地配置 map 算链（chain 不走 Redis），把 `(key, maxDebt, postpaid)` 列表交给 **Lua 脚本**原子执行「逐个比较 `bal - maxDebt >= est`，任一失败回滚已扣项」——与现内存版 `Reserve`（account.go:102-124）逐行语义对应，单次往返；
  - **Commit/Release**：Lua 对链上各 prepaid 账户 `bal += (est-actual)`；
  - 启动播种：`NewPGServer` 装载账户后对每个账户 `SET NX`（不覆盖存活值；副本重启不清别人改过的余额）。内存模式 demo 账户同样播种。
- TopUp（admin.go:117-160）：PG → Redis 写绝对值 → 保留现有绝对值广播（幂等，兼容无 Redis 副本读缓存）。
- 小时 rollup（admin.go:636）顺带把 Redis 余额快照回写 PG `accounts.credit`（灾后恢复点）。
- callcore.go：余额权威错误映射为 HTTP 503（新错误类型，与 403 不足额区分）。

### Phase 2 — 呼叫注册与准入（callstate）

- key 布局：
  - 每账户 ZSET `gvos:calls:{accountID}`（member=callID, score=start unix）＝并发计数来源（ZCARD）；
  - `gvos:cps:{accountID}:{unixsec}`（INCR + EXPIRE 1s）；
  - 呼叫明细哈希 `gvos:call:{callID}`（account/lock/accrued/start）TTL=僵尸超时+裕量。
- **准入改造**（callstate.go:219-262）：
  - `admitConcurrent`：本账户+祖先子树（Go 走配置树得后代清单）**pipeline ZCARD** 求和比对——callstate.go:275-277 注释「production moves this to a counter」落地；
  - `admitCPS`：INCR+EXPIRE。
- 本地 map 仍是呼叫运行时明细的 owner（interim driver/teardown 不动）；Redis 是共享准入账本 + 崩溃回收依据。
- **跨副本僵尸回收**：sweeper 扩展——本地扫描外，扫 `gvos:calls:{*}`（SCAN + pipeline）超时成员：非本副本的呼叫按注册哈希结算、退还预锁余款、`ZREM`+`DEL`，发 `gvos.cdr.completed`；PG `uq_cdr_call` 唯一约束使任意副本重复结算天然幂等。

### Phase 3 — 风控滑窗（riskctrl）

- key 布局：
  - `gvos:rfreq:{account}` ZSET（滑窗 60s：ZADD + ZREMRANGEBYSCORE + ZCARD 单 Lua）；
  - `gvos:costday:{yyyymmdd}:{account}` INCRBYFLOAT + TTL 48h；
  - `gvos:gfreq:{user}` ZSET（member=`{sec}.{nano}` 唯一，多副本重复 ZADD 天然去重）。
- `CheckCall`/`CheckReg`/`SettleCost` 在 `rdb != nil` 时走 Redis（热路径各 1 次往返；riskctrl.go:7-9 注释「microseconds」如实更新为「亚毫秒网络往返」）；Redis 故障 fail-open。

### Phase 4 — 套餐用量（pkgplan）

- 配置（plans/assignments）留本地 + 广播（是配置）；**用量计数**进 Redis：
  - `gvos:pkg:used:{account}:{cycleKey}` / `gvos:pkg:spend:{account}:{cycleKey}` INCRBYFLOAT，TTL=周期末+缓冲。
- `Apply`（pkgplan.go:153-184）改单 Lua 原子完成「免费额度消耗 → 折扣计算」返回 (freeConsumed, spend)，Go 侧拼 RateResult。周期切换由 cycleKey 变更自然重置（旧 key 靠 TTL 蒸发）。

### Phase 5 — 注册镜像（regtable）+ 辅助项

- `gvos:reg:{user}` 哈希（lastReg/expires/srcIP）TTL=expires；`Apply` 幂等 HSET（各副本写同值）；`Online` 读时按 expires 推导；`events` 计数由 `gfreq` ZSET ZCARD 派生（避免 ×N INCR）。重启不再丢表。
- **ADR-0002 扩展（用户拍板：本地一份 + Redis 一份，本地优先）**：gv-core 增只读内部端点（查 `gvos:reg:{user}`）；Kamailio cfg 加降级分支——usrloc 本地 miss 时经 gv-core 查 Redis 镜像，仍 miss 才 404；Redis 不可用时 fail-open（照旧 404，终端自然重注册）。usrloc `db_mode=0` 不变，边缘仍零 PG 依赖。
- **单跑者锁**（`rds.Acquire`）：小时 rollup（admin.go:633）与 60s 配置重播器（dispatch.go:348）加锁，全副本冗余→单副本执行。
- 顺手修三个进程内数据竞争（与 Redis 无关但同路径）：`s.cdrs`（callcore.go:161 无锁 append）、`s.recordEnabled`、`s.userIndex`。

## 4. Redis 数据布局总表

| Key | 类型 | 写者 | TTL | 说明 |
|---|---|---|---|---|
| `gvos:bal:{accountID}` | string(float) | account（Lua） | 无限 | 余额权威；PG 定期回写 |
| `gvos:calls:{accountID}` | ZSET | callstate Start/End | 成员自清 | 并发计数（ZCARD）+ 僵尸回收 |
| `gvos:cps:{accountID}:{unixsec}` | string(int) | callstate Start | 1s | CPS 计数 |
| `gvos:call:{callID}` | hash | callstate Start/End | 僵尸超时+裕量 | 跨副本结算明细 |
| `gvos:rfreq:{account}` | ZSET | riskctrl | 90s | 呼叫频次滑窗 |
| `gvos:costday:{day}:{account}` | string(float) | riskctrl | 48h | 日费用累计 |
| `gvos:gfreq:{user}` | ZSET | riskctrl | 90s | 注册频次滑窗 |
| `gvos:pkg:used:{account}:{cycleKey}` | string(float) | pkgplan | 周期末+1d | 免费分钟用量 |
| `gvos:pkg:spend:{account}:{cycleKey}` | string(float) | pkgplan | 周期末+1d | 周期消费 |
| `gvos:reg:{user}` | hash | regtable | =expires | 注册快照 |
| `gvos:lock:rollup` | string | admin rollup | <1h | 单跑者锁 |
| `gvos:lock:rebroadcast` | string | dispatch | <60s | 单跑者锁 |

## 5. 验证清单（实施时执行）

1. `go build ./...`、`go vet ./...`、`go test ./...`——回归（Redis off 路径全部现有测试不变）。
2. 各包 miniredis 单测：account Lua 回滚语义、callstate 准入/僵尸回收、riskctrl 滑窗、pkgplan 周期重置、regtable TTL。
3. `docker compose -f deploy/docker-compose.yml up` 冒烟：healthz、`/v1/stats` 报 redis on、起呼走通。
4. **fail-closed 演练**：`docker stop redis` → `/v1/call/start` 返回 503；重启后恢复。
5. **多副本**（compose-ha 或 scale=2）：并发上限=cap（而非 cap×N）；副本 A TopUp 后 B 立即可见；杀副本 A，副本 B 的 sweeper 结算其呼叫且 CDR 只落一条。

## 6. 风险与代价

- 需要网络拉取 go-redis/miniredis 模块（GOPROXY 可用）；vendor 目录体积增加。
- 热路径新增亚毫秒级 Redis 往返（call start ≈2 次往返：Reserve Lua + 准入 pipeline）；容量基准重测（bench 报告本就列了「Redis 余额权威改造后重测」）。
- Lua 脚本不可移植到未来其他存储（可接受：逻辑短小，单测覆盖）。
- 部署拓扑增加一个有状态组件（Redis 实例）的运维成本（监控、备份、容量）。

## 7. 里程碑建议

按 Phase 交付，每 Phase 独立可上线：

| 里程碑 | 内容 | 上线判定 |
|---|---|---|
| M0 | Phase 0 | compose 栈起来，`/v1/stats` 报 redis on |
| M1 | Phase 0+1 | 余额权威收口；单/多副本账务一致 |
| M2 | +Phase 2 | 并发/CPS=cap；杀副本僵尸被回收 |
| M3 | +Phase 3+4 | 风控与套餐聚合正确 |
| M4 | +Phase 5 | 全量完成；容量基准重测 |
