# 后期改进方案：API 体系规划（五平面 + 横切规范 + 出站事件）

**状态**：⚠️ Phase A/B 已交付（2026-11-19，提交 189019a/0751bbb）；C/D/E 未实施
**日期**：2026-11-19
**范围**：gvos-core HTTP/NATS 对外契约的重构与扩展——分面模型固化、API Key 鉴权、横切规范（幂等/错误/分页/版本）、租户自助面、Webhook 引擎、OpenAPI 3.1。

---

## 0. 背景与动机

API-first 是本产品对传统软交换（VOS 系：桌面客户端 + 无公开 API）的代际差异点（`docs/02-architecture.md` §1.2、§10 M15「API/Webhook 全覆盖」）。但现状是按里程碑逐个长出来的 spike 路由，未收敛成契约：

| # | 缺口 | 现状位置 | 后果 |
|---|---|---|---|
| 1 | 鉴权是写死的常量 token | `cmd/gvos-core/admin.go:32` `const adminToken = "dev-admin-token"`（无 env 覆盖） | 部署即带默认凭据；无操作员身份 |
| 2 | 无租户维度 | `admin.go:27` `const demoTenant`；所有 admin 接口请求体无 tenant 字段 | 多租户 schema（`migrations/001_init.sql` 全表 tenant_id）与 API 面脱节 |
| 3 | 三种删除语义并存 | blocklist `{"remove":true}`（admin.go:319-）、ipallow `{"remove_id"}`（admin.go:421-）、risk 整组替换（admin.go:374-） | 客户自动化脚本三种写法；心智负担 |
| 4 | 动词式杂烩路由 | `topup`/`record`/`stream-push` 与资源名混在 `/v1/admin/*` | 资源模型不统一，OpenAPI 难生成 |
| 5 | 错误体是裸字符串 | `httpError`（各 handler 散用） | 客户无法按 code 编程化处理 |
| 6 | 文档与代码不一致 | operations-manual.md:92 写 `GET /v1/admin/risk`，代码 POST-only（admin.go:377） | 照文档调 API 得 405 |
| 7 | 无幂等 | topup/套餐/账户创建无 `Idempotency-Key` | 网络重试→重复充值/重复扣减 |
| 8 | 长列表无分页协议 | CDR 内存 ring + `limit`（main.go:455-470） | 数据量增长后不可用 |
| 9 | 无出站事件 | NATS 事件仅内部消费 | 客户 BI/对账/告警无法订阅 |
| 10 | 信令面无鉴权 | `/v1/call/*` 仅 `requirePOST` | 依赖网络隔离；恶意可达即可盗打 |

## 1. 目标与非目标

**目标**

1. 契约固化：五平面模型、资源化路由、横切规范（鉴权/幂等/错误/分页/版本）一份文档定死。
2. API Key 体系替代常量 token：key 绑定 (租户, 角色)，hash 入库，审计记真实操作者。
3. 新增两个平面：租户自助 API、Webhook 出站事件。
4. OpenAPI 3.1 从代码生成，作为唯一可信接口文档。

**非目标**

- 本期不做 OIDC/SSO（架构文档 §6 的可选项，留 P3+）。
- 不动 NATS 内部 subject 语义（cfg-reload/广播机制不变）；Webhook 是事件总线的 HTTP 投影，不是新事件源。
- 不做 API 网关限流组件（单二进制内置中间件够用）。
- `/v1/internal/ipallow-snapshot` 的 poller 协议保持不变（Kamailio 侧已按版本化快照消费，改动收益为负）。

## 2. 全局设计决定

### 2.1 五平面模型

| 平面 | 路径前缀 | 消费者 | 鉴权 | 现状 → 目标 |
|---|---|---|---|---|
| 信令事件面 | `/v1/call/*` | Kamailio → core | `X-GVOS-Internal`（复用现有 token） | 无鉴权 → 加 internal token，缺省 fail-closed |
| 内部控制面 | `/v1/internal/*` | SIP 边缘轮询 | `X-GVOS-Internal` | ✅ 保持 |
| 运营管理面 | `/v1/admin/*` | 部署方运营 | API Key（角色=admin，租户=部署方） | 常量 Bearer → API Key |
| 租户自助面 | `/v1/tenant/*` | 下级代理/企业客户 | API Key（角色=tenant/agent，限本租户） | ❌ 新建 |
| 出站事件面 | Webhook + WS | 客户系统 | HMAC 签名 | ❌ 新建 |

判据：信令/内部面是「给自己的机器用的」，管理面是「给部署方的」，自助面+出站面才是粘性来源——客户把余额/话单/告警接进自己的系统后迁移成本陡增。

### 2.2 横切规范（落成 docs/api-conventions.md，所有接口继承）

1. **鉴权**：`Authorization: Bearer gvos_<scope>_<random>`；库表存 hash，响应只回前缀+尾 4 位；一个 key 绑 `(tenant_id, role)`；吊销即删行。`GVOS_ADMIN_TOKEN` env 保留为兼容回退（警告日志），下版本移除常量默认值。
2. **幂等**：动钱接口（topup、套餐分配、账户创建）要求 `Idempotency-Key` 头：重复请求返回首次结果（含首次状态码）。key + 响应体存 PG，TTL 24h。
3. **错误包络**：`{"error":{"code":"INSUFFICIENT_CREDIT","message":"...","details":{...}}}`。code 全大写蛇形，枚举进 OpenAPI。现有 403/429/503 语义保留。
4. **分页**：cursor 式 `?cursor=&limit=`（默认 50，上限 500），响应带 `next_cursor`。CDR/审计/事件三类长列表共用。
5. **版本策略**：`/v1` 冻结语义；破坏性变更进 `/v2`；弃用期响应带 `Deprecation` + `Sunset` 头。Go 路由升级见 2.3。

### 2.3 路由资源化（Go 1.22 方法模式）

go.mod 升 `go 1.22`，`muxFor()`（main.go:419-451）改方法模式注册：

```go
mux.HandleFunc("POST /v1/admin/accounts", ...)
mux.HandleFunc("GET /v1/admin/accounts", ...)
mux.HandleFunc("POST /v1/admin/accounts/{id}/topup", ...)
```

`routeAccounts`/`routeRates`/`routeGateways` 三个手写 verb 分发器删除（405 由标准库出）。资源命名收敛：

| 现路径 | 目标路径 | 变化 |
|---|---|---|
| `/v1/admin/topup` | `POST /v1/admin/accounts/{id}/topup` | 资源+动作 |
| `/v1/admin/record` | `POST /v1/admin/recording` | 名词化（保留动作语义） |
| `/v1/admin/risk` | `GET/PUT /v1/admin/risk-rules` | GET 补上（修文档缺口#6）、整组替换语义显式化 |
| `/v1/admin/blocklist` + `{"remove":true}` | `POST/GET/DELETE /v1/admin/blocklist-entries` | 删除走 DELETE |
| `/v1/admin/ipallow` + `remove_id` | `POST/GET/DELETE /v1/admin/ipallow/rules/{id}` | 删除走 DELETE 路径参数 |
| `/v1/admin/stream-push` | 保留（测试钩子，标注 `x-internal: true` 不进公开 OpenAPI） | — |
| `/v1/admin/reports` | 拆 `POST /v1/admin/reports/rollup` + `GET /v1/admin/reports` | 写读分离 |

旧路径在 `/v1` 弃用期内 307 重定向到新路径（或双注册），`Deprecation` 头标弃用。

## 3. 分阶段实施计划

### Phase A — 收敛与规范（P0，纯内部，无新能力）

1. 写 `docs/api-conventions.md`（2.2 全文）。
2. `httpError` 改错误包络（全部现有 handler 一次过，code 映射表：`403→INSUFFICIENT_CREDIT` 等——注意 403 有两个成因「余额不足/黑名单/风控」，code 细分）。
3. Go 1.22 方法路由 + 路径资源化（2.3 表全量）+ 旧路径兼容注册。
4. 修文档不一致（operations-manual.md:92 等，全部 curl 示例对齐新路由）。
5. CDR cursor 分页（内存 ring 倒序 cursor = 索引；PG 模式 = keyset `start_time, call_id`）。
6. `/v1/call/*` 加 `X-GVOS-Internal` 校验：token 未配置时**默认拒绝**（fail-closed，与 ipallow-snapshot 同哲学），Kamailio cfg 同步带上头（kamailio.cfg:403 已有现成模式）。
7. `/v1/stats`、`/healthz` 保持无鉴权（探活语义）。

### Phase B — API Key 与租户上下文（P1，结构性）

1. PG 新表（migrations/00N_api_keys.sql）：

```sql
CREATE TABLE api_keys (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES tenants(id),
    role TEXT NOT NULL CHECK (role IN ('admin','operator','tenant','agent')),
    key_hash TEXT NOT NULL UNIQUE,     -- sha256
    prefix TEXT NOT NULL,               -- 展示用 gvos_admin_ab12
    created_by TEXT NOT NULL,
    revoked_at TIMESTAMPTZ,
    last_used TIMESTAMPTZ
);
```

2. `requireAdmin` 改为：Bearer → 查 hash → 载入 `(tenant, role)` 进 request context；审计 operator 从「"admin" 常量」（admin.go:263、dispatch.go:196）换成 key prefix；`demoTenant` 硬编码（admin.go:27/78）改从 context 取。
3. 启动时若无任何 key 且配置了 `GVOS_ADMIN_TOKEN` → 用该 token 生成 bootstrap key（一次性打印），消除常量默认值风险。
4. 幂等中间件：`Idempotency-Key` 头存在且动钱路由 → PG 查/存首次响应。
5. 内存 demo 模式：API Key 表退化为进程内 map（`internal/acl` 包，接口与 PG 实现同形）。

### Phase C — 租户自助面（P2）

路由前缀 `/v1/tenant/*`，鉴权 key role ∈ {tenant, agent}，**所有查询强制按 key 的 tenant_id 过滤**（库表本就有 tenant_id，是暴露问题不是建设问题）：

| 接口 | 复用的内部能力 |
|---|---|
| `GET /v1/tenant/me`（key 身份/余额/限额） | store.Get + Balance |
| `GET /v1/tenant/accounts`（下级账户） | store.List 过滤 tenant |
| `GET /v1/tenant/cdrs?cursor=` | CDR 按 tenant 过滤 |
| `GET /v1/tenant/packages/usage` | pkg.Usage/MinSpendShortfall（admin.go:793- 已有逻辑） |
| `GET /v1/tenant/stats`（本租户并发/今日消费） | statsBody 加租户维度投影 |
| `POST /v1/tenant/webhooks`（注册回调 URL + 事件选择） | 新表 webhook_subs |

写操作（充值下级、创建下级账户）留给 Phase D 或 admin 面授权模型，本期自助面只读——**先让客户「看得到」，再让他们「动得了」**。

### Phase D — Webhook 引擎 + CDR 流（P2）

事件源零新建：直接消费 `internal/events` 已有 subject（events.go 的 SubjectCDR/SubjectAlert/SubjectRisk 等）。

1. **投递器**（`internal/webhook`）：NATS 订阅 → 查订阅表 → POST + 重试（指数退避 1m/5m/30m/2h/6h，5 次后死信）→ 投递日志入 PG（webhook_deliveries 表，供自助排障页面）。
2. **签名**：`X-GVOS-Signature: t=<unix>,v1=HMAC-SHA256(secret, t + "." + body)`（Stripe 风格时间戳防重放），secret 创建时一次性下发。
3. **首批开放事件**（对账/充值提醒/防欺诈联动四场景）：
   - `cdr.completed`（对账：每通话单落地即推）
   - `account.low_balance`（阈值告警，阈值挂账户字段）
   - `risk.blocked`（风控命中）
   - `package.exhausted`（套餐耗尽）
   事件 schema 写进 api-conventions.md，事件目录表 = NATS subject 清单 + payload 字段表。
4. **WebSocket 只读 CDR 流** `GET /v1/tenant/cdr-stream`：NATS SubjectCDR 订阅按 tenant 过滤后透传，给实时看板。与媒体流 `/v1/stream/{call_id}`（media.go，双向音频）区分，纯数据流走标准库。
5. 契约文档化：事件 payload 进 OpenAPI（webhooks 段）。

### Phase E — OpenAPI 3.1 + SDK（P3）

1. 路由注册表驱动 spec 生成（方法模式路由的 pattern + handler 元数据 → openapi.yaml），或注解扫描；`/v1/internal/*`、`stream-push` 打 `x-internal` 不进公开 spec。
2. spec 进 CI 校验（oapi-codegen 或 schemathesis 冒烟，对运行实例发请求验证契约=实现）。
3. 客户 SDK：先 Go + Python（gengo 路线，Post 期评估）。
4. MCP 接口（AI 代理平台用自然语言管理话务——竞研定位的无人区）：`/v1/mcp` 暴露账户余额查询/话单检索/费率查询工具，SSE transport。本期仅立项不实施。

## 4. 与其他规划的关系

- **Redis 收口（redis-adoption-plan.md）**：正交。余额权威换 Redis 不改 HTTP 契约；Phase B 的 API Key 查询可缓存。两份计划并行实施不冲突，Webhook 投递器与 NATS durable worker 共用消费模型。
- **多租户化**：Phase B 是多租户 API 化的第一步（key→租户绑定）；riskctrl/gateway 的租户维度是独立的域模型工作，不在本计划内（避免范围爆炸），但 API 面从此不再阻挡它。

## 5. 验证清单（实施时执行）

1. `go build ./...`、`go vet ./...`、`go test ./...`——回归。
2. 新增 API Key 中间件单测：错误 key 401、错误角色 403、tenant 过滤逃逸（构造 A 租户 key 访问 B 租户 CDR 断言 404）。
3. 幂等单测：同 Idempotency-Key 二次 POST topup 断言同响应、余额只变一次。
4. 弃用路径：旧 `/v1/admin/topup` 请求断言 307 + `Deprecation` 头。
5. `test/security/baseline.sh`（baseline.sh:11 的 TOKEN 默认值）与 `scripts/demo-up.sh`、`test/sipp/soak.sh` 同步换 bootstrap key 流程。
6. Webhook 集成测：起 NATS + 本地 mock server，断言签名可验、重试序列、死信落库。
7. OpenAPI 校验：schemathesis 对运行实例冒烟，断言无 5xx 且响应结构符合 spec。

## 6. 风险与代价

- **路由迁移破坏现有调用方**：内部调用方只有 Kamailio（call 面，不受影响）、admin UI（同仓内同步改）、migrate 工具 load 步骤（cmd/gvos-migrate 调 admin API，同步改）。弃用期兜底。
- **Go 1.21 → 1.22**：vendor 全量刷新，工具链升级一次（CI 同步）。
- **API Key 引入新表 + 中间件热路径开销**：hash 查询可进程内缓存（key→身份 LRU，吊销走 NATS 广播失效——复用 cfg-reload 模式）。
- **Webhook 出站流量**：客户 endpoint 慢/挂会积压投递队列，需投递日志可观测 + 死信可见（Phase D 第 1 条已含）。

## 7. 里程碑建议

按 Phase 交付，每 Phase 独立可上线：

| 里程碑 | 内容 | 上线判定 |
|---|---|---|
| M-A | Phase A | 新路由全量 + 错误包络 + 文档一致；旧路径 307 | ✅ |
| M-B | +Phase B | dev-admin-token 常量删除；审计记 key 身份；topup 幂等 | ✅（常量降级为无 key 回退） |
| M-C | +Phase C | 租户 key 可查余额/话单/用量；跨租户访问 404 |
| M-D | +Phase D | 四事件 Webhook 签名投递 + 重试死信可观测 |
| M-E | +Phase E | openapi.yaml 由代码生成且 CI 验证契约 |

对齐 02-architecture.md §10 的 M15「API/Webhook 全覆盖」：M-A~M-D 合计即 M15 的 API 半壁；风控引擎与 Prometheus 面板为另一半，另行规划。
