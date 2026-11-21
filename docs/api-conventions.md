# API 横切规范(API Conventions)

**状态**:v1(Phase A 落地版,2026-11-19)
**适用范围**:gvos-core 全部 HTTP 端点与 NATS 事件载荷(事件部分见 §6)
**依据**:[api-roadmap.md](api-roadmap.md) §2.2 · [ADR-0005](adr/0005-bounded-contexts.md)(接口层归 internal/api)

---

## 1. 平面与鉴权

| 平面 | 前缀 | 鉴权 | 状态 |
|---|---|---|---|
| 信令事件面 | `/v1/call/*` | `X-GVOS-Internal: <GVOS_INTERNAL_TOKEN>` | ✅ fail-closed:token 未配置=503 拒绝新事件 |
| 内部控制面 | `/v1/internal/*` | 同上 | ✅ |
| 运营管理面 | `/v1/admin/*` | `Authorization: Bearer <token>` | ⚠️ Phase A 仍为常量 `dev-admin-token`;Phase B 换 API Key |
| 媒体流 | `/v1/stream/{call_id}` | WebSocket(Phase B 收口) | — |
| 探活 | `/healthz`、`/v1/stats` | 无(只读) | ✅ |

**弃用告警**:旧 spike 路径(topup/record/blocklist/risk/ipallow/gateways/reports)在弃用期内 307 重定向或原语义透传,响应带 `Deprecation: true`;下版本移除。

## 2. 资源化路由(Go 1.22 方法模式)

| 资源 | 新路由 | 旧路由(弃用) |
|---|---|---|
| 账户 | `POST/GET /v1/admin/accounts` | — |
| 充值 | `POST /v1/admin/accounts/{id}/topup` | `/v1/admin/topup` |
| 费率 | `GET/PUT /v1/admin/rates` | `POST=upsert/GET`(verb 分发) |
| 风控规则 | `GET/PUT /v1/admin/risk-rules` | `/v1/admin/risk`(POST-only) |
| 黑名单 | `GET/POST /v1/admin/blocklist-entries` + `DELETE .../{scope}:{pattern}` | `{"remove":true}` 请求体标志 |
| IP 准入 | `GET/POST /v1/admin/ipallow/rules` + `DELETE .../{id}` | `{"remove_id"}` 请求体字段 |
| 网关 | `GET/PUT /v1/admin/gateways` + `DELETE .../{id}` | verb 分发 |
| 录音开关 | `GET/POST /v1/admin/recording` | `/v1/admin/record` |
| 报告 | `GET /v1/admin/reports` + `POST /v1/admin/reports/rollup`(写读分离) | `/v1/admin/reports`(POST=rollup) |
| 话单 | `GET /v1/admin/cdrs?cursor=&limit=` | — |
| 套餐 | `GET/PUT /v1/admin/packages` + `GET .../usage` | — |

405/404 由标准库按方法模式自动出,业务 handler 不再手写 405 分支。

## 3. 错误包络

全部错误响应统一:

```json
{"error":{"code":"INSUFFICIENT_CREDIT","message":"account \"1000\" insufficient balance ..."}}
```

- `code`:稳定枚举,全大写蛇形,调用方按 code 编程;新增 code 只增不改。
- `message`:人类可读细节,内容不承诺稳定。
- 状态码→默认 code 映射:400→`BAD_REQUEST`、401→`UNAUTHORIZED`、403→`FORBIDDEN`、404→`NOT_FOUND`、405→`METHOD_NOT_ALLOWED`、429→`RATE_LIMITED`(伴随 `Retry-After`)、500→`INTERNAL`、503→`UNAVAILABLE`。
- **403 细分(计费门)**:`INSUFFICIENT_CREDIT`(余额不足,HTTP 403)、`RATE_LIMITED`(并发/CPS/套餐限流,HTTP 429);黑名单/风控拦截仍是 403 + `FORBIDDEN`。
- 呼叫计费门 Redis 故障(Phase 2 起):503 + `BILLING_UNAVAILABLE`。

## 4. 分页

cursor 式:`?cursor=&limit=`(默认 50,上限 500),响应带 `next_cursor`(空=末页)。
内存模式 cursor = ring 索引;PG 模式将切 keyset(`start_time, call_id`)。
适用:CDR、审计、事件类长列表(后两者 Phase B/C 接入同一协议)。

## 5. 版本策略

- `/v1` 语义冻结:字段只增不删不改名。
- 破坏性变更 → `/v2` 并行发布 + `/v1` 响应带 `Deprecation`/`Sunset` 头 + 弃用期(≥1 个 minor 版本)。
- NATS 服务主题(`gvos.svc.*`)与 HTTP 面保持同一载荷语义(两面镜像)。

## 6. 事件载荷演进(Phase D 前生效)

1. **加字段**:消费者必须忽略未知字段(容忍)。
2. **改字段名/语义**:禁止原地改。走 v2 subject(如 `gvos.cdr.completed.v2`)+ 弃用期双发 → 下线。
3. 约束来源:边缘别名载荷字节一致、JetStream durable 消费者重放旧消息、Kamailio jansson 点路径解析(陷阱清单 nats-event-driven-upgrade §2.4)。
4. 契约测试(Phase D 时引入):golden payload fixture 防静默漂移。

## 7. 事件目录(subject 清单 + payload 字段表)

发布侧契约由 `internal/shared/events/testdata/contract/` 的 golden fixture 机器守护(改 payload 必须显式更新 fixture 并过消费者评审)。

| Subject | 载荷字段 | 消费者 |
|---|---|---|
| `gvos.cdr.completed` | CallID, AccountID, Dest, Duration, BilledSecs, Cost(dec), RatePrefix, Price(dec), Release, PDDMs, SIPCode, Recorded, FreeSecs, Mos/JitterMs/PacketLoss | CDRW worker(PG)、Webhook(Phase D) |
| `gvos.cdr.raw` | call_id, event=end, duration, pdd_ms, sip_code, release, qos(rtpstat 文本) | CDRR worker(callEnd 终结+落库) |
| `gvos.call.teardown` | call_id, reason | 边缘 dlg 终结、TEARDOWN durable |
| `gvos.risk.alert` | account, username, rule_id, reason, to | 边缘 banned htable、告警面 |
| `gvos.reg.event` | user, op(reg/expire), expires, src_ip | regtable 镜像 + reg_freq 规则 |
| `gvos.admin.changed` | action, entity, entity_id, operator(key prefix), tenant_id, before, after | AUDIT worker(append-only 表) |
| `gvos.cfg.reload` | entity(gateway/ip_allowlist/blocklist/rate/account/credit/recording/risk/package) + 全量快照 | gv-core 副本 + 边缘 htable(GW_SYNC/IPALLOW_APPLY/BL_APPLY) |
| `gvos.recording.completed` | call_id, track(agent/user), object_key, backend, size_bytes, format | 上传下游、Webhook(Phase D) |
| `gvos.svc.call.start/.answer/.end`、`gvos.svc.stats` | req/reply 服务面(与 HTTP 面镜像) | Kamailio(RPC 档)、运维工具 |

边缘无点别名(gvos-edge-cfg/-risk/-tear)载荷与正名字节一致,不入 JetStream。

## 8. 变更记录

| 日期 | 变更 |
|---|---|
| 2026-11-19 | v1:错误包络、资源化路由表、fail-closed 内部鉴权、cursor 分页、事件演进规则 |
| 2026-11-20 | v1.1:事件目录表(golden fixture 守护);query-token 内部鉴权形式;403 细分 code 落地实录 |

## Webhook 契约与载荷版本规则（Phase D，2026-11-21 交付）

### 信封
每个 webhook POST 的 body 都是版本化信封：

```json
{
  "v": 1,
  "event": "cdr.completed",
  "occurred_at": "2026-11-21T03:00:30Z",
  "tenant": "<tenant-uuid>",
  "seq": 42,
  "data": { ...事件投影... }
}
```

### 版本规则
- `v` 是**信封版本**。v1 内：字段只增不改不删，枚举值不新增成员；破坏性变更发 v2，双版本并行 ≥6 个月。
- `data` 是**稳定投影**（`internal/webhook/contract.go` 的 typed struct），内部结构体重构不外泄。
- **seq 每订阅单调递增**：跳号 = 有事件丢失（客户用 CDR API 对账）；重投**同 seq**（at-least-once）——消费方必须按 `(tenant, event, seq)` 幂等。

### 事件枚举（封闭）
| event | data 投影 |
|---|---|
| `cdr.completed` | CDRCompletedData（call_id/account/dest/时间三件套/duration/billed/cost 小数字符串/release/sip_code/pdd/recorded） |
| `call.teardown` | CallTeardownData（call_id/reason: balance-exhausted\|zombie-timeout\|admin） |

新事件类型 = 新键（加法，允许但进 changelog）；已知事件的新成员字段同上。

### 签名与重放
`X-GVOS-Signature: t=<unix>,v1=<hex(hmac-sha256(secret, t + "." + body))>`，重放窗口 5 分钟（`Verify` maxSkew）；`X-GVOS-Event` / `X-GVOS-Version` 头伴随。签名覆盖**信封完整字节**。

### 安全
订阅 URL **admin 创建/更新时强制 https + 公网地址**（webhook.ValidateSubscription：拒 http/私网/loopback/元数据地址/解析到私网的域名）；投递时二次 DNS 校验。残余风险：DNS rebinding——生产叠加出口策略（operations manual）。

### 投递语义
at-least-once，重试 1m/5m/30m/2h/6h 共 5 次后 dead-letter（PG delivery log）。**幂等键 = (tenant, event, seq)**。
