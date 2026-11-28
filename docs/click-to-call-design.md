# 对外呼叫发起 API（click-to-call / AI 外呼）设计

**状态**：草案（立项已批准，待评审后排期）
**日期**：2026-11-28
**定位**：对标昆石 VOS「Web 接口」选配模块；pipecat/livekit AI 语音代理的前置；API-first 对桌面客户端软交换的代际差异点的最后一块。
**关联**：[api-gap-analysis.md](api-gap-analysis.md) M11（战略级缺口）· [did-inbound-design.md](did-inbound-design.md)（`media_agent` destination type 预留）· [livekit-integration-plan.md](livekit-integration-plan.md) / [pipecat-integration-plan.md](pipecat-integration-plan.md)

---

## 0. 一页版

现状：信令面 `/v1/call/*` 是 `X-GVOS-Internal` 专用（Kamailio 消费），**平台外部无法经 API 发起呼叫**。本设计新增客户面 origination API `/v1/calls`，复用现有计费/风控/黑名单准入管道，新增「平台主动外呼」的边缘执行路径。分三期：F1 双向回拨（click-to-call）→ F2 AI 外呼（media_agent）→ F3 批量外呼任务。

## 1. 模型

### 1.1 双向回拨（F1，click-to-call）

平台同时外呼 A 与 B、接通后桥接——**两侧都是被叫**，不存在客户侧 SIP 终端：

```
POST /v1/calls
{
  "caller": "13800001234",       // 平台先拨的一侧（计费账户挂这里）
  "callee": "8613987654321",     // 后拨的一侧
  "account_id": "…",             // 计费账户（tenant key 只能用自己的树内账户）
  "caller_display": "95013…",    // 显号（可选；外呼合规关键，见 §5）
  "max_duration_s": 1800,        // 保护性上限（默认取账户/全局配置）
  "custom": {"crm_id": "…"}      // 透传元数据，进 CDR custom 字段 + webhook
}
→ 202 {"call_id":"…","state":"pending"}   // 异步资源，非同步等接通
```

- 资源化：`POST /v1/calls`（发起）、`GET /v1/calls/{id}`（状态）、`DELETE /v1/calls/{id}`（取消/拆线，复用 teardown 语义）。**不**做「同步等接通」——长连接挂 HTTP 请求是反模式，状态经查询与 webhook 消费。
- 鉴权：API key（admin/operator 全量；tenant/agent 限自己树内 account_id，复用 M10 的 `tenantScopedAccount` 语义）。
- 幂等：`Idempotency-Key` 必需（动钱 + 可重试操作）。

### 1.2 AI 外呼（F2）

`to` 指向 `media_agent` destination（013 号迁移已预留该类型，当时 answered 501「never bills what it cannot route」）：外呼接通后不桥接人工，而是把媒体流桥进 `/v1/stream/{call_id}` WebSocket（livekit SIP / pipecat 已有桥接形态），ASR/TTS 代理（gvos-asr）照常工作。

```
POST /v1/calls {"caller":"…","to_agent":"support-bot-1","account_id":"…", …}
```

`to_agent` 解析为 agent 配置（新的 agents 设置键：destination + 流参数），复用 F1 的全部生命周期。

### 1.3 状态机与事件

```
pending → dialing_a → (a answered) dialing_b → answered → ended
                    ↘ a_failed → ended           ↘ b_failed/ declined → ended
任意态 → cancelled (DELETE) / balance-exhausted (计费拆线，复用现有链路)
```

- 事件：复用 webhook 契约**加法**新增 `call.state` 事件（v1 信封允许加键），或最小化仅 `cdr.completed`（客户轮询 GET）。倾向前者：外呼场景客户对「未接通原因」的实时性诉求强。
- CDR 照常落地（`direction='out'`，origination 即平台外呼），对账零新增。

## 2. 复用与新增

| 层 | 复用 | 新增 |
|---|---|---|
| 准入 | 黑名单、IP/CIDR、风控（CheckCall）、余额锁（Reserve）、费率（EstimatedCost 预锁）、CPS 门 | 外呼频控（按 account_id 的 origination CPS，独立于信令面 CPS） |
| 计费 | 现有 Reserve/Commit/Release + CDR/ledger 双路径 + 套餐 Apply | 无 |
| 控制 | `gvos.call.teardown` 拆线 | **`gvos.call.originate`**（控制面 → 边缘的发起指令） |
| 边缘 | Kamailio 现有路由/计费事件回报（call start/answer/end 事件流照常回报 core） | **UAC 发起**：选项 A `tm`+`uac_reg`/`dialplan` 静态 trunk；选项 B 独立轻量 UAC 微服务（Go siphello/sipgrep 风格）经 NATS 消费 originate 指令。**倾向 B**：边缘 cfg 零改动（复用 edge-deps-fork 策略的反面——把新增面放在可独立演进的进程），容量独立扩展，AI 外呼的 B-leg 桥接天然落在这里 |
| 媒体 | RTPengine 现有 offer/answer；`/v1/stream/` WS | agent 桥接（F2） |

## 3. 数据模型

`calls` 表（015 号迁移）——外呼是长生命周期资源，内存环不够：

```sql
CREATE TABLE calls (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL,
    account_id  UUID NOT NULL,          -- 计费账户
    caller      TEXT NOT NULL,
    callee      TEXT,
    to_agent    TEXT,                   -- F2
    state       TEXT NOT NULL DEFAULT 'pending'
                CHECK (state IN ('pending','dialing_a','dialing_b','answered','ended','cancelled')),
    fail_reason TEXT,
    max_duration_s INT NOT NULL DEFAULT 1800,
    custom      JSONB,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_calls_account ON calls(account_id, created_at DESC);
```

内存模式：进程内 map（demo 形态照旧）。

## 4. API 面（客户面新增）

| 方法/路径 | 说明 |
|---|---|
| `POST /v1/calls` | 发起（202 + call_id；幂等必需） |
| `GET /v1/calls/{id}` | 状态（tenant 树内隔离，同 M10 语义） |
| `GET /v1/calls?state=&from=&limit=&cursor=` | 列表（keyset，同 CDR 面协议） |
| `DELETE /v1/calls/{id}` | 取消/拆线（复用 teardown） |
| webhook `call.state` | 状态机事件（信封 v1 加键） |

## 5. 风险与合规

- **显号管理**：外呼显号（caller_display）是合规敏感面——白名单制（账户级 approved_display_numbers 设置键，校验后才允许下发），文档明示各国监管要求由部署方负责。
- **外呼频控**：origination CPS 独立配额（账户级 `max_originate_cps`，PATCH 账户面加字段），防止客户 API 打满信令面 CPS 挤掉正常话务。
- **边缘 UAC 容量**：选项 B 的 UAC 微服务需要独立容量标定（对标媒体面 B 线的做法，进 capacity ladder）。
- **风暴保护**：`POST /v1/calls` 在 pending/dialing 状态的同 (caller,callee) 对做 5 秒去重（幂等键之外的防抖）。

## 6. 分期与里程碑

| 期 | 内容 | 判定 |
|---|---|---|
| F1 | 双向回拨全链路（originate 指令 + UAC 微服务 + calls 资源 + call.state 事件 + 计费接入） | **core 侧 + 拨号器已交付（2026-11-28）**：/v1/calls 全套 API、gvos.call.originate 协议、gvos-originate 二进制（单腿 + RFC 3725 flow-I 双腿 3PCC、CANCEL/BYE、max_duration、durable 队列）；SIP 逻辑回环包级验证。**剩余门槛：真实 Kamailio 互通（SIPp 两端桥接 + CDR=ledger 对账）需联调环境** |
| F2 | AI 外呼（media_agent + 流桥接） | livekit/pipecat 桥接 e2e，ASR 代理工作 |
| F3 | 批量任务（campaign 资源 + 速率调度） | 压测 1000 级任务队列，CPS 配额生效 |

## 7. 非目标

- 不做客户侧 SIP 注册呼叫（那是 edge 的注册域，仍走 Kamailio）。
- 不做同步等待接通的请求形态。
- F1 不含批量/定时；campaign 留 F3。

## 8. 变更记录

| 日期 | 变更 |
|---|---|
| 2026-11-28 | 初稿（立项决议：M11 设计先行） |
