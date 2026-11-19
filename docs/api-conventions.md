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

## 7. 变更记录

| 日期 | 变更 |
|---|---|
| 2026-11-19 | v1:错误包络、资源化路由表、fail-closed 内部鉴权、cursor 分页、事件演进规则 |
