# API 缺口分析与改造清单（五遍梳理）

**状态**：定稿（core/console 归属按 ADR-0002/0006 裁决，可直接进排期）
**日期**：2026-11-28
**方法**：三路并行代码审计——① migrations × API 对照（「模型有、API 无」）、② internal 包公开能力 × API 对照（「能力有、API 无」）、③ 64 条路由逐条实现质量审计 + gvos-console BFF 诉求核对；再与 `api-roadmap.md`、`api-conventions.md`、`competitive-analysis-vos-sbc.md` 三份既有文档交叉对照。
**复核声明**：TOP-3 事故级结论（recording GET 恒 405、限流器包整个 mux 且 per-key 桶死代码、PG 模式 rates 列表恒空）已人工对代码逐行复核属实（2026-11-28）。

**交付状态（2026-11-28，当日）**：§6 P0 全部五项（M1-M5）、§7 P0 事故级全部九项已交付；**P1 追加交付 M6（账户 PATCH/DELETE，disable 除外——schema/准入路径无 enabled 标志，需先设计）、M7（audit/ledger 读 API）、M8（webhook PATCH + tenant DELETE + 事件 4/4 补齐）、M9（套餐 unassign + 费率 DELETE）及 P1 改造小项（gateways PUT 串行化、registrations/DID 分页、deliveries 游标、tenant 爬树缓存、tenant stats 走 rollup + 锁外计算、usage 身份解析、幂等面扩容、包络统一、网关 401/405 双写）**。全部经 PG 门控 + 内存双模式验证，明细见 CHANGELOG [Unreleased]。
**M12 裁决修正**：调研发现「网关自动熔断」只存在于一次性引导的 legacy gateways 表加载路径（`locked_until` 过滤），settings 主路径的在线目录**没有任何熔断机制**——解锁 API 无的放矢。正确动作是先设计在线熔断（故障计数 + 冷却 + 观测面），再谈解锁。已从 P1 摘除，待立项。
剩余：M10 已按「tenant 可充值+建下级（划拨语义）、agent 只读」授权模型交付（2026-11-28）；M11 已立项并出设计稿 docs/click-to-call-design.md（待评审排期 F1/F2/F3）；P2 全部、账户 disable、在线网关熔断设计待排期。

---

## 0. 一页版结论

**路径覆盖率不是问题，问题是「半成品率」**：

- 表建了没 API（tenants/rate_groups/routes/audit_log/balance_ledger）
- API 建了没落库（keys/AXB/幂等记录全内存态；gateway DELETE 不落库）
- 落库了读不了（audit_log、balance_ledger 只写不读）
- 读到了形状不对（列表包络分裂、报表形状不符，console 真核下页面空白）
- 一个战略级空位：**信令面 `/v1/call/*` 是 internal-token 专用（Kamailio 消费），客户/AI 经 API 发起呼叫（click-to-call / AI 外呼）无任何入口**——VOS「Web 接口」选配模块的对标点，也是 pipecat/livekit 集成的前置。

最划算的两笔投入：
1. **统一列表包络 + CDR PG 化**——同时解锁 console 一半的空白页与导出能力；
2. **持久化三重奏（keys/AXB/幂等落库）**——解除多副本上线阻塞。

另：`api-roadmap.md` 头部状态行已过时（写着「C/D/E 未实施」，实际 C/D 路由 2026-11-21 已交付，仅 E 未做），建议顺手更新。

---

## 1. 第 1 遍 · 资源完备性：表建了，API 没有

| # | 缺口 | 证据 | 建议接口 |
|---|------|------|----------|
| A1 | 租户管理 | `migrations/001_init.sql:18` tenants 表只有 seed 一行，无任何 CRUD | `POST/GET/PATCH /v1/admin/tenants` |
| A2 | 费率组管理 | `001_init.sql:69` rate_groups 零创建入口，PUT rates 只能写已存在 group | `POST/GET /v1/admin/rate-groups` |
| A3 | **路由规则** | `001_init.sql:118,131` routes/route_legs 完整建模（lcr/ratio/asr/time 策略、failover、guard_route、ratio 分流），Go 侧**零引用零 API**；实际选路走 gateways dispatcher 列 + LCR 广播 | `/v1/admin/routes`（route+legs 原子提交） |
| A4 | dispatcher 独立表 | `004_dispatcher.sql:33` Go 零引用 | 查看 API 或明确弃用 |
| A5 | AXB 落库 | `010_axb_bindings.sql:6` 表完整，代码纯内存（`internal/api/server.go:216,245`），**重启丢绑定** | Bind/Revoke 落库 + 启动加载 |
| A6 | DID 双轨 | 真表 `013_did.sql:8` 零引用，实际走 settings KV（`admin_did.go:45,76`） | 迁真表（行级 upsert）或删迁移 |
| A7 | 审计回溯 | `001_init.sql:195` audit_log 每次变更都写（`pg.go:370`），**无读取 API** | `GET /v1/admin/audit?entity=&tenant_id=` |
| A8 | 账本流水 | `001_init.sql:56` balance_ledger 全落账（`pg.go:340,416`），唯一读路径是幂等检查 | `GET /v1/admin/accounts/{id}/ledger` |
| A9 | settings 通用 KV | `007_settings.sql` 只有 6 个硬编码键暴露，运维需直连 DB | `GET/PUT /v1/admin/settings/{key}` |
| A10 | 账户生命周期 | `admin_accounts.go` 只有 create/list/topup，**无改/删/停用/密码重置** | `GET/PATCH/DELETE /v1/admin/accounts/{id}` |

## 2. 第 2 遍 · 能力完备性：代码能力在，API 入口没有

| # | 能力 | 证据 | 建议接口 |
|---|------|------|----------|
| B1 | 代理树查询 | `account.Chain()`（`billingdomain/account/account.go:105`）只用于内部限额 | `GET /v1/admin/accounts/{id}/tree` |
| B2 | 网关熔断观测 | `001_init.sql:110` locked_until/consecutive_failures 只影响加载过滤，不可见不可解锁 | `POST /v1/admin/gateways/{id}/unlock`（或在 snapshot 暴露 locked_until） |
| B3 | 路由预览 | `gateway/lcr.go:27,75` LCR 排序仅在呼叫时算 | `POST /v1/admin/routes/preview {dest}` |
| B4 | 风控计数器 | `riskctrl.Engine.Counters()`（riskctrl.go:383）无非测试调用方 | 并入 `/v1/admin/risk-rules` 或 `/v1/stats` |
| B5 | 实时并发 | `callcore/admission_redis.go:102` LiveConcurrent() 无调用方 | `GET /v1/admin/accounts/{id}/live` |
| B6 | 媒体流统计 | `mediastream.go:238,144` Stream.Stats/Manager.Count 未暴露 | `GET /v1/stream/{call_id}/stats` |
| B7 | VOS CSV 导入 | `migrate.go:45,98,158` 解析+校验只在 CLI，无上传入库 API | `POST /v1/admin/migrate/import`（multipart） |
| B8 | 录音删除/保留 | `recpipe/storage.go:64` Delete + `009_recordings.sql` status 列无 API | `DELETE /v1/recording/...` + retention 配置 |
| B9 | webhook 重试策略/清理 | `webhook.go:58` 退避硬编码；deliveries 无清理 | 策略配置 + purge |
| B10 | 费率试算 | `billing/charger.go:76` EstimatedCost() 只内部用 | `POST /v1/admin/rates/quote {dest, seconds}` |
| B11 | 黑名单源管理 | env 驱动（`server.go:1043-1050`）只读 | `PUT /v1/admin/blocklist-sources` |

**非缺口说明**：`deprules` 只是架构守护测试；blocklist.SourceStats、regtable、ipallow.Check、pkgplan.Assign/Usage、recpipe 上传/播放、webhook CRUD/Deliveries/Replay 均已被 handler 覆盖。

## 3. 第 3 遍 · 现有接口自身残缺（接口存在，但是坏的）

### 3.1 事故级（人工复核属实 ★）

| 问题 | 证据 |
|------|------|
| ★ **限流器钳死信令面 + per-key 桶死代码** | `server.go:641` 把整个 mux（含 `/v1/call/*`、`/metrics`、`/healthz`）包进 `withRateLimit`，与 637-640 注释「信令面不限流」直接矛盾；`ratelimit.go:123-132` clientKey 读的 identity 在 handler 内（`admin.go:59,74` context.WithValue）才写入——中间件阶段恒为空，**实际全部按 IP 限 20 rps，遮蔽计费自身 429** |
| ★ **PG 模式 `GET /v1/admin/rates` 恒空** | handler 读 `s.rateList`（`admin_rates.go:82-96`），PG 启动只填 `s.rates`（`server.go:258`），rateList 永远 nil——费率列表接口在主模式下返回假数据 |
| ★ **`GET /v1/admin/recording` 恒 405** | `admin_recording.go:12` 对 GET/POST 一律 requirePOST，OpenAPI 明文承诺的状态查询坏死，无测试兜底 |

### 3.2 其余残缺

| 问题 | 证据 |
|------|------|
| 黑名单 GET 只回计数、white 条目经 REST 删不掉 | `admin_policies.go:75-77`（只回 `{"rules":N}`）、`:259,264`（DELETE 硬编码 kind=black） |
| DID `enabled` 缺省 false → 注册成功但**永不接通**（静默） | `admin_did.go:32-40`、`did.go:109` |
| gateway DELETE 不落库 → 重启**复活**；全集 PUT 读改写无锁；鉴权失败 401/405 双写 | `admin_gateways.go:108-131`（对比 `:42-47` PUT 有 PutSetting）、`:13-16,26-56` |
| webhook 无 update（改 URL 只能删了重建，secret 轮换 delivery 中断）；tenant 无 DELETE 自己的 webhook；内存模式创建即蒸发 | `admin_webhooks.go`、`server.go:708-709`、`admin_webhooks.go:79-81` |
| 套餐无法解除分配（Unassign 有，无 API）；assign 未包幂等；define+assign 半程 PutSetting → 内存/PG/副本三方漂移；usage 按 ID 而 assign 支持用户名（身份解析不一致） | `admin_packages.go:51-74,76-81,91-92`、`pkgplan.go:123` |
| `/v1/stats` 无鉴权；`caller_bal` 硬编码账户 "1000"（PG 模式恒缺）；纯全局瞬时值 | `server.go:516-518,546` |
| tenant 面规模：每请求 O(N) 爬全账户树 ×6 条路由；stats 持 `cdrsMu` 扫 10k 环**阻塞计费写路径**；today_spend 日话单超 1 万静默失真 | `tenant.go:66-94,224-240`、`callapi.go:292-307` |
| tenant CDR-WS：客户端 ping 被丢弃（无 pong）、无心跳、无 Last-Event-ID 续传、慢消费静默丢帧（64 缓冲） | `tenant_stream.go:53-123`（对比媒体 WS `mediaapi.go:96-166` 有 ping/pong/超时，成熟度差一档） |
| 媒体 WS 无 per-call 归属校验：任意 operator key 可听任意通话音频 | `mediaapi.go:32-46` |
| accounts：无分页全量返回；内存模式 ID=`acc-<user>-<HHMMSS>` 同秒撞 | `admin_accounts.go:197-234,29,56` |
| registrations/DIDs 全量无分页、regtable 返回顺序随机 | `admin_gateways.go:93-100`、`regtable.go:110` |
| deliveries 无游标翻页（limit 钳 50-200） | `admin_webhook_deliveries.go:32-33`、`pg.go:930-933` |
| reports：单日查询无范围、有无 account 参数隐式决定视图（语义耦合）、路由拆分后 GET 分支成死代码 | `admin_reports.go:174-199,89-116` |
| axb：ttl_hours 为 NaN 时静默通过 | `admin_axb.go:29` |
| recording 播放直接信任 DB 绝对路径（纵深不足） | `recordingapi.go:76` |

## 4. 第 4 遍 · 对标与承诺：roadmap 自己欠的账 + 竞品视角

| 缺口 | 出处 |
|------|------|
| **Webhook 事件只交付 2/4**：缺 `account.low_balance`、`risk.blocked`、`package.exhausted` | roadmap Phase D 明列 4 个；conventions §Webhook 事件枚举只见 cdr.completed + call.teardown |
| **对外发起呼叫 API 缺失（战略级）**：客户经 API 发起呼叫（click-to-call / AI 外呼）无任何入口 | 信令面 `/v1/call/*` 为 `X-GVOS-Internal` 专用（Kamailio 消费）；VOS「Web 接口」选配模块是对标点；pipecat/livekit 集成的天然前置 |
| 租户自助面写操作（充值下级、建下级账户）被显式推迟，至今未排期 | roadmap Phase C「留给 Phase D 或 admin 面授权模型」 |
| Phase E 全部未做：OpenAPI 请求体全是自由 object、**无 query 参数 schema、无分页 envelope schema**（SDK 生成器拿到的是空壳）、CI 契约校验、Go/Python SDK、MCP | roadmap Phase E；`openapi.go:26-85` 核实（spec→路由一致，但参数级 schema 全缺） |
| tenant 无法取自己通话的录音 | recordingapi 仅 admin（`recordingapi.go:19,55`） |
| console 侧证实 4 个缺口（BFF 逐条核实）：**CDR 无服务端过滤/无 PG 历史**（筛选参数被静默忽略 `server.go:765-798` 只解析 limit/cursor，导出只能拉 10k 内存环）、**无单账户 GET**（详情页拉全量列表找人）、**无聚合统计**（工作台卡片静默消失）、**报表响应形状不符**（console 读 `items`，core 回 `{rows,count}`，真核下报表页永远空） | console `web/src/api/ops.ts:107-113`、`api/internal/server/`、`Dashboard.vue:133-147`、`Reports.vue:58-62`；mock-core 与真核形状漂移是测试假象来源 |
| OIDC/SSO（roadmap 定为 P3+ 远期，知悉即可） | roadmap 非目标 |

竞品对标补充结论（`competitive-analysis-vos-sbc.md`）：VOS 七大能力域（计费/路由/多级代理/费率套餐/CDR 报表/强拆/余额锁定）**功能面已全部对齐**，差距不在功能而在 API 化纵深——本文件全部条目都是 API 化纵深的展开。

## 5. 第 5 遍 · 横切规范与多副本一致性

- **幂等**：只有 account create + topup 包了（`server.go:651,653`）；**packages assign（动钱，roadmap §2.2 点名）、rates PUT、keys/webhooks/AXB/DID create 全裸**；幂等 registry 纯内存（重启失效、多副本不共享，roadmap 承诺 PG+TTL 24h 未兑现）；**key 无路由命名空间**（`idempotency.go:27-44` 全局 seen map）——跨路由复用同 key 会重放出错误响应；pruneIdem 每请求 O(N) 全表扫
- **API Key 全内存**：`acl.go:41-48` 纯内存，重启全丢（只剩 bootstrap key `main.go:96`）、多副本鉴权分裂——roadmap Phase B 的 PG keys 表未兑现
- **持久化三重奏缺失**：keys / AXB 绑定 / 幂等记录全部内存态；叠加 gateway DELETE 不落库、DID settings 全列表覆写 last-writer-wins——**单副本内存模式没问题，一上多副本/重启全是雷**
- **列表包络分裂**：admin CDR 用 `next_cursor`、tenant CDR 用 `cursor+has_more`（`tenant.go:150-198`，违反自家 conventions §4）；keys `{keys}`/risk `{rules}`/registrations `{registrations}`/axb `{bindings}` 各回领域键而 console 全按 `items` 读——**统一包络可一次性消除 console 三类空白页**
- **OpenAPI**：spec 与路由双向一致（无假路由，`/openapi.json` 与 `/metrics` 未列属可接受），但无参数级 schema
- **死代码**：routeAccounts/routeRates/routeGateways/routeRatesLegacy/routeRiskLegacy（`server.go:801-835,937-955`，注释还写着 Go 1.21）、handleReports GET 分支、publishContract 空函数、事件枚举 `allowed` map 两处重复定义（`admin_webhooks.go:63` 与 `tenant.go:299`）
- 错误包络整体良好（统一 `{"error":{code,message}}` + 脱敏 + 403 三态区分），无实质问题

---

## 6. 缺失接口清单（按优先级，含归属）

### P0 —— 阻塞真核/console 上线

| # | 接口 | 归属 |
|---|------|------|
| M1 | CDR 查询 PG 历史 + 服务端过滤（account/dest/时间范围，admin+tenant 两面）+ 供 console 编排异步导出 | **core** |
| M2 | `GET /v1/admin/blocklist-entries` 返回真实条目（现在只有计数） | **core** |
| M3 | `GET /v1/admin/accounts/{id}`（单账户读取） | **core** |
| M4 | `/v1/stats` 补聚合维度或新 `/v1/admin/overview`（accounts/gateways/registrations 计数） | **core**（出数据），console（卡片展示适配） |
| M5 | 统一列表包络 `items` + `next_cursor`（一次改，消 console 三类空白页） | **core**（契约），console（基本不用动，它已按目标形状在读） |

### P1 —— 业务纵深

| # | 接口 | 归属 |
|---|------|------|
| M6 | 账户 `GET/PATCH/DELETE /{id}`（停用/限额/密码重置，响应一次性展示） | **core** |
| M7 | `GET /v1/admin/audit`、`GET /v1/admin/accounts/{id}/ledger`（审计与对账闭环） | **core** |
| M8 | webhook `PATCH` + tenant webhook `DELETE` + 补 3 个事件（low_balance/risk.blocked/package.exhausted） | **core** |
| M9 | 套餐 unassign、费率 DELETE、费率组管理 | **core** |
| M10 | 租户面写操作（充值下级/建下级，需授权模型） | **core**（必须 core 出 API，console 只代理+审批包裹，禁止计费语义复刻） |
| M11 | **对外呼叫发起 API（click-to-call / AI 外呼）**——战略缺口，建议立项 | **core** |
| M12 | 网关锁定观测 + 解锁 | **core** |

### P2 —— 增强/远期

| # | 接口 | 归属 |
|---|------|------|
| M13 | 代理树 / 实时并发 / 路由预览 / 费率试算 / 媒体流统计 | **core** |
| M14 | VOS CSV 导入 API、settings 通用 KV、录音删除 + retention、tenant 录音 | **core** |
| M15 | 租户管理 CRUD、路由规则表激活（或明确弃用）、dispatcher 表处置 | **core** |
| M16 | OpenAPI 参数级 schema + CI 契约校验 + Go/Python SDK + MCP 立项（roadmap Phase E） | **core** |
| M17 | 操作员 SSO/OIDC | **console**（人账号域；core 只保留机器鉴权 API Key，不引入会话概念） |

## 7. 需改造清单（按严重度，含归属）

### P0 事故级（全部 core）

限流器拆面 + 修 identity 时序 ★｜PG 模式 rates 列表回填 ★｜recording GET 实现 ★｜gateway DELETE 落库｜keys 落库｜AXB 落库 + 广播｜黑名单 white 删除｜DID enabled 缺省｜webhook 内存模式至少入 dispatch

### P1 契约/容量级（全部 core）

幂等面扩容 + PG + 路由命名空间｜tenant 面爬树缓存 + 锁外统计 + today_spend 走 rollup｜tenant CDR-WS 心跳/seq 续传/丢帧提示｜媒体 WS per-call 归属校验｜stats 去硬编码 + 鉴权｜gateways 竞态 + 401/405 双写｜packages 漂移次序 + usage 身份解析｜registrations/DID 分页｜deliveries 游标｜报表响应形状（core 改，console 适配）

### P2 整洁级（全部 core）

死代码清理（routeAccounts 等 8 处 + handleReports GET 分支 + publishContract）｜事件枚举去重｜recording locator 前缀校验｜AXB NaN 校验｜内存账户 ID 同秒撞｜api-roadmap.md 头部状态行更新

## 8. core / console 边界归属（判据与灰区裁决）

### 8.1 判据（ADR-0002/0006 已有）

1. **是不是运行时/账务权威数据，或它的变更入口？** → core
2. **机器/自动化/客户脚本要不要用？** → core（API-first 是产品代差；console 禁止复刻计费语义）
3. **只有操作员在 UI 里用，且与人身份/审批流/会话绑定？** → console

一句话：**凡是「数据/控制权」，core；凡是「人、流程、任务编排」，console**。

### 8.2 console 侧工作（很少，均为既有职责延伸）

| 事项 | 说明 |
|------|------|
| 展示层适配 | 工作台卡片/报表页/门户话单页消费 core 新接口 |
| 异步导出任务编排 | ADR 已裁：导出任务元数据是 console 域。core 只需把分页/过滤查询做够 |
| 充值/建户审批流 | 审批通过后调 core API（approvals.go 现模式），流程语义在 console |
| 告警中心 | core 发事件，console alerts 表收纳展示 |
| 操作员 SSO/OIDC | 人账号域在 console（ADR-0006 第 3 条） |

### 8.3 三个灰区裁决

1. **audit 拆两面**：core 的 `audit_log`（核心实体变更，`gvos.admin.changed`）→ core 出读 API；console 自己的操作审计（登录/点击/审批）→ console 自有库（已实现）。UI 分区展示。
2. **报表**：rollup 计算与新增视图（时间范围）是 core（数据在 usage_daily）；console 只补查询参数控件。
3. **低余额提醒**：阈值判断与事件发射在 core（`account.low_balance` 缺口正是此）；console 只消费展示，禁止轮询余额。

---

## 9. 剩余清单复核（2026-11-28，P0+P1 交付后）

### 接口缺失
| 域 | 项 |
|---|---|
| 战略 | M11 **F1 core 侧已交付**（/v1/calls + originate 指令 + 状态机 + call.state；UAC 拨号器二进制待实施）；F2/F3 未开工；账户 disable（需 schema+准入加 enabled 标志）；在线网关熔断+解锁（先立项熔断设计） |
| 资源缺口 | 费率组管理（全部锁死 demo 组）；租户 CRUD；路由规则 API（routes/route_legs 零引用，用或删）+ dispatcher 处置；settings 通用 KV |
| 能力缺口 | 路由预览 / 费率试算 / 账户树 / 实时并发 / 风控计数器 / 媒体流统计；VOS CSV 导入 API；录音删除+retention；webhook 重试策略可配+deliveries 清理；黑名单源增删；报表时间范围查询；OIDC/SSO（P3） |

### 功能/工程缺失
- ~~契约漂移~~ ✅ 包 1 已交付（2026-11-28，f78eb5d）：spec 51 路径 + 参数 schema + 事件枚举 + spec→路由一致性回归测试
- ~~协议成熟度~~ ✅ 包 2 已交付：tenant CDR-WS pong/心跳/since 续传/丢帧 ctrl 帧；媒体 WS per-call 归属校验（tenant/agent 按子树放行，此前任意 operator key 可听任意通话）
- ~~健壮性~~ ✅ 包 2 已交付：webhook_deliveries 7 天保留（GVOS_DELIVERY_TTL_DAYS 可调）；accounts 分页；内存账户 ID 改 UnixNano；recording locator 归档根校验
- **竞品验证线**：媒体面生产化（B 线）；20 万注册 / 2 万并发容量验证

## 10. 变更记录

| 日期 | 变更 |
|------|------|
| 2026-11-28 | 初稿：三路审计 + 五遍梳理定稿；TOP-3 结论人工复核；core/console 归属裁决 |
| 2026-11-28 | P0 全量交付（M1-M5 + §7 事故级九项 + 幂等/包络部分 P1），见交付状态注与 CHANGELOG [Unreleased] |
| 2026-11-28 | P1 追加交付：M6/M7/M8/M9 + 改造小项；M12 裁决修正（在线目录无熔断，解锁 API 无的放矢，需先立项熔断设计）；M10/M11 待决策 |
| 2026-11-28 | M10 按授权模型裁决交付（tenant 划拨式充值+建下级）；M11 立项，设计稿落 docs/click-to-call-design.md |
| 2026-11-28 | M11 F1 core 侧交付（calls 资源 + originate 协议 + 状态机 + call.state 事件；迁移 015）；包 1 契约对齐 + 包 2 健壮性收尾交付 |
