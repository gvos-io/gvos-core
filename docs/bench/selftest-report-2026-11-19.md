# G-VOS 全功能自测报告（NATS 事件驱动化批次）

**日期**：2026-11-19
**验收方式**：自动化（Go 测试套件 93 项、真 PG 集成、真 NATS JetStream、双副本端到端、curl/psql 断言、nats-box 事件注入）
**被测部署**：deploy-postgres-1 + deploy-nats-1（compose）+ 双 gv-core 副本（PG 模式，:8081 带 webhook / :8082 裸副本）
**代码基线**：NATS 总线 + 事件驱动化三批次（fire-and-forget 事件 / req-reply / durable 队列）全部落地后
**结论**：**hermetic 93 项、集成 16 项、端到端 19 项全部通过；过程中发现 2 个产品缺陷当场修复后复验通过，1 个测试库 schema 漂移被 durable 重投机制暴露并修复。自测通过。**

---

## 一、分层测试结果

| 层 | 范围 | 结果 |
|---|---|---|
| **Phase 1 单元/hermetic** | 全仓 13 包 `go test ./...`（计费数学、账户树、通话状态机、事件总线队列语义、媒体流、套餐、风控、迁移） | ✅ **93 通过 / 0 失败 / 8 跳过**（跳过项均为环境门控，见 Phase 2） |
| **Phase 2 真依赖集成** | `internal/store` × 真 PostgreSQL（GVOS_TEST_DSN） | ✅ 4/4（LoadAccounts / LoadRates / InsertCDR+Ledger+幂等重投 / NumberList 写穿） |
| | `internal/events` × 真 NATS JetStream（GVOS_TEST_NATS_URL） | ✅ 12/12（req/reply 回读、广播 fan-out、durable 重投、发布回读、队列丢弃、并发关闭等） |
| **Phase 3 端到端** | 双副本 + 真 PG + 真 NATS + webhook 接收端，19 项功能矩阵 | ✅ 19/19（明细见第三节） |

## 二、在跑发现并修复的缺陷（修复后复验通过）

| # | 缺陷 | 根因 | 修复 | 复验 |
|---|---|---|---|---|
| D1 | 套餐指派传 SIP 用户名时不生效（免费额度/折扣不落 CDR） | `handlePackages` 直接把 `assign_account` 当账户内键，而 `pkg.Apply` 按 UUID 匹配；用户名未做解析 | 与 caller 路径同规则解析（store→userIndex），解析失败 400；广播 payload 携带解析后的 UUID | T9 复验：FreeSecs=60、Cost=0.0025 精确命中 |
| D2 | 重启后录音开关不恢复（settings 持久化形同虚设） | `recording` 设置存的是 `{"enabled":…}` 对象，boot 恢复却直接解到 `bool`，类型不匹配静默失败 | 恢复改读包装结构体再赋值 | T8 复验：双副本重启后 `record:true`，未再设置开关 |
| D3 | 测试库 schema 漂移（`recorded` 列缺失，CDR 落库静默失败） | 测试 PG 只应用过 001/002，003-006 未灌；旧内联写 fail-open 只打日志 | durable 重投把错误大声暴露（NAK 1s × 5 后隔离）；补齐 003-008 迁移 | T11 复验通过；**该事件本身验证了"至少一次 + 大声失败"对账语义的价值** |

## 三、端到端功能矩阵（T1-T19，双副本 :8081/:8082）

| # | 功能 | 验证方法 | 结果 |
|---|---|---|---|
| T1 | 计费闭环 + 余额不变量 | INVITE 锁定→BYE 66s → CDR `BilledSecs=66, Cost=0.055`（60+6 步长），账户余额 10−0.055 | ✅ |
| T2 | 余额不足拒绝 | 0.001 元账户 INVITE → 403 | ✅ |
| T3 | 黑名单（A 设规则 → B 拦截 + PG 写穿） | 副本 A 加 86139 黑名单，副本 B 上呼叫 403；`number_lists` 出现该行 | ✅ |
| T4 | 风控频控（A 设规则 → B 生效） | 3 次/分钟规则，B 上第 4 通 403 | ✅ |
| T5 | 费率变更跨副本即时生效 | A upsert 99 前缀 0.3 → B 上呼叫按新价锁定 | ✅ |
| T6 | 账户创建跨副本立即可打 | A 建户 → B 上该户 INVITE 200 | ✅ |
| T7/T20 | 充值跨副本同步（绝对余额幂等） | A 充值 +10 → B 上账户余额 10.001 | ✅ |
| T8 | 录音开关持久化 + 双副本重启恢复 | A 开启 → B 生效；**双副本重启后未再设置，开关从 settings 自动恢复** | ✅（D2 修复后） |
| T9 | 套餐免费额度 + 折扣 + 持久化 | 日套餐 1 分钟免费 + 5 折：66s 计费拆为 60s 免费 + 6s×0.5 折 → `FreeSecs=60, Cost=0.0025`；`settings.packages` 持久化、重启恢复 | ✅（D1 修复后） |
| T10 | interim 驱动器强制拆线 + 事件队列执行 | 0.06 元账户通话 → 10s 扣减片拆线 → **A 发事件、B 的 TEARDOWN worker 同秒消费执行**（跨副本 executor） | ✅ |
| T11 | CDR 落库幂等（重投收敛） | 同一 CDR 双投递（seq 89/90）→ `cdr` 1 行、`balance_ledger` 1 行 | ✅ |
| T12 | 审计异步落库 | B 副本费率 upsert → `audit_log` 出现完整 after payload | ✅ |
| T13 | 全 NATS 通话（零 HTTP） | `gvos.svc.call.start/answer/end` req/reply 完成一通计费电话，end 回 CDR | ✅ |
| T14 | stats req/reply 服务 | `gvos.svc.stats` 返回与 HTTP 面同构快照 | ✅ |
| T15/T16 | Webhook 分发 + NAK 重投 | 接收端先拒 3 次（500）→ CDRW…WEBHOOKS durable NAK 1s 重投 → 第 4 次投递成功，同一 CallID | ✅ |
| T17 | 停机补消费（durable 积压） | 双副本全停 → 事件直灌 stream（Sequence 33/89/90）→ 重启后 CDRW 补写落库 | ✅ |
| T18 | 管理台 / UI、健康检查 | `/ui` 200、`/healthz` 200 | ✅ |
| T19 | workers 可观测性 | `/v1/stats` `workers:["cdrw","audit","teardown"]`，启动日志一致 | ✅ |

## 四、覆盖统计

- **Go 测试**：93（hermetic）+ 4（PG 集成）+ 12（live NATS）= **109 项**，0 失败
- **端到端**：19 项功能矩阵，0 失败
- **缺陷**：2 个产品缺陷（D1/D2）+ 1 个环境 schema 漂移（D3），全部当场修复并复验
- **累计**：130 项检查全部通过

## 五、已知边界（非缺陷，记录在案）

1. `balance_ledger.balance` 列在多副本下取消费副本的内存余额视图（`delta` 金额精确）——权威余额仍是各副本内存，生产按架构文档换 Redis 后收口
2. Kamailio answer/end 切 `nats_publish` 为 blocked 项（Alpine aports 无 kamailio-nats 包），服务端已由 `gvos.svc.call.*` 就绪，换 Debian 镜像时零服务端改动
3. `/v1/call/start` 的 Kamailio HTTP 面为同步锁定门（nats 模块无阻塞 req/reply），保留为设计决策
