# MCP 接口设计（AI 代理自然语言话务管理）

**状态**：设计稿（roadmap Phase E 第 4 项立项，竞品无人区）
**日期**：2026-11-28
**定位**：把 gvos API 面暴露为 Model Context Protocol 工具集，让 AI 代理（客服台、运维 copilot、话批客户的自动化助手）用自然语言管理话务——查询余额、检索话单、试算费率、发起外呼。这是「API-first」的终态延伸：人 → console UI，程序 → SDK，AI 代理 → MCP。
**关联**：[api-roadmap.md](api-roadmap.md) Phase E · [click-to-call-design.md](click-to-call-design.md)（originate 已交付，MCP 的 call 工具直接复用）· [competitive-analysis-vos-sbc.md](competitive-analysis-vos-sbc.md)（VOS 系无此形态）

---

## 0. 一页版

MCP server 是一个独立轻量进程（`cmd/gvos-mcp`），SSE transport，鉴权透传 Bearer key（key 的 role 决定工具权限——与 HTTP 面同一套 ACL，零新鉴权模型）。工具面只读为主 + 两个受控写工具（originate/cancel），每个工具都是对既有 API 的薄封装——MCP 层不新增业务逻辑，契约单源于 openapi.json。

## 1. 工具集（首批 8 个）

| 工具 | 底层 API | role 要求 | 说明 |
|---|---|---|---|
| `get_balance` | `GET /v1/tenant/me` | tenant+ | 余额/限额/币种 |
| `list_accounts` | `GET /v1/tenant/accounts` | tenant+ | 子账户清单 |
| `query_cdrs` | `GET /v1/tenant/cdrs` | tenant+ | 时间/号码过滤 + 分页（翻页由 agent 驱动 cursor） |
| `get_usage` | `GET /v1/tenant/packages/usage` | tenant+ | 套餐用量 |
| `rate_quote` | `POST /v1/admin/rates/quote` | admin | 费率试算（写面只读语义） |
| `route_preview` | `POST /v1/admin/routes/preview` | admin | LCR 预演 |
| `originate_call` | `POST /v1/calls` | admin/operator/tenant | 发起外呼（**必须**强制 Idempotency-Key=工具调用 ID，防 AI 重试风暴） |
| `cancel_call` | `DELETE /v1/calls/{id}` | 同上 | 取消/拆线 |

写工具刻意只有 originate/cancel：充值（topup/allocate）**不进首批**——动钱操作经 AI 代理的风险（指令注入→资金转移）远高于收益，留待审计/确认流设计成熟后另议（见 §4）。

## 2. 形态

```
AI 代理 ←(SSE/stdio)→ gvos-mcp ←(HTTP+Bearer)→ gv-core :8080
```

- **transport**：SSE（`/mcp` 端点）为主（远程代理），stdio 为辅（本地 CLI 场景，`gvos-mcp -stdio`）。
- **进程边界**：独立 binary，不进 gv-core 主进程——MCP 协议演进（JSON-RPC 版本、工具 schema 格式）与计费核心解耦；挂掉只影响 AI 面。
- **鉴权**：MCP server 不持有凭据——代理侧每个工具调用带 key（header 透传），ACL/rate-limit/tenant 隔离全部复用 HTTP 面既有机制。`gvos-mcp` 自身只需 `GVOS_CORE_URL`。
- **schema**：工具的 input/output JSON Schema 从 openapi.json 的注册表生成（表驱动同源，spec 与 MCP 面不会漂移）。

## 3. 安全要点

1. **role 降权原则**：给 AI 的 key 永远最小 role（tenant 只读最常见）；写操作临时升权走人审批（console 域）。
2. **重试幂等**：originate_call 强制 `Idempotency-Key` = MCP 工具调用唯一 ID（`mcp-<session>-<seq>`）——AI 代理重试同一意图命中同一呼叫，不产生重复外呼。
3. **注入面**：工具参数全部服务端校验（复用 API 的 400 校验）；LLM 不可注入超出 key role 的操作（ACL 在 core，不在提示词）。
4. **审计**：MCP 调用落 core 的 audit_log（operator 前缀带 `mcp:` 标记），与人类操作同轨可查。

## 4. 分期

| 期 | 内容 | 判定 |
|---|---|---|
| MCP-1 | `cmd/gvos-mcp` + 8 工具 + SSE + schema 生成 | MCP inspector 连通，工具调用 e2e（对内存 server） |
| MCP-2 | stdio 模式 + 代理侧 key 透传协议文档 | 至少一个主流代理框架（Claude/自研）接入验证 |
| MCP-3 | 充值等写工具（含确认流：工具返回 pending confirmation，二次调用确认） | 安全评审过 |

## 5. 风险与边界

- **协议热度风险**：MCP 生态早期，协议 breaking 变更可能——独立进程隔离爆炸半径，工具 schema 从 spec 生成，协议层重写成本低。
- **成本控制**：AI 代理循环调用读工具可产生 API 洪水——复用既有 rate limiter（admin/tenant 面的 per-key 桶），无新增机制。
- **非目标**：不做「AI 直接操作 SIP/媒体面」；MCP 只到 API 层。

## 6. 变更记录

| 日期 | 变更 |
|---|---|
| 2026-11-28 | 初稿（Phase E 第 4 项立项；MCP-1 待排期） |
