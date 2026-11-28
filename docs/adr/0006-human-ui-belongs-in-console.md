# ADR-0006：人机面归属 gvos-console（跨仓边界指针）

**状态**：已接受（2026-11-26）  
**权威正文**：[gvos-console ADR-0002](../../../gvos-console/docs/adr/0002-console-core-domain-boundary.md)

## 背景

gvos-core 提供 Admin/Tenant HTTP 与运行时权威数据。人机 Web（运营台/租户门户、操作员 IAM、BFF 审计与导出任务）由独立仓 gvos-console 承担。若在 core 内继续扩张产品 UI 或把 console 域表塞进本库，会破坏「控制面可独立运行」与 console 的迭代节奏。

## 决定

1. **人机产品名与实现仓**：`gvos-console`；本仓管理 HTTP 继续称为 **Admin API**（`/v1/admin/*`），不要与 Console 产品名混用。
2. **运行时权威留在本仓**（账户/费率/网关/CDR/风控/套餐/DID/Webhook/API Key 等）；console 仅经 HTTP/NATS 消费，禁止以 console 库为账务/路由权威。
3. **人账号、RBAC、console 操作审计、导出任务元数据** 不进本仓业务库；细节与灰区裁决以 console ADR-0002 为准。
4. `internal/api/ui.go` 等内嵌页仅作 spike，**不当产品 UI 规范**；新能力先暴露 OpenAPI，再由 console 做代理 UI。（2026-11-28 更新：spike 页已删除，core 不再内嵌任何 HTML 页面，`/ui` 路由移除。）

## 后果

- 本仓评审「要不要加一张给人用的表」时：先问是否通话/计费运行时需要；否则指向 console。
- 跨仓边界变更：先改 console ADR-0002，再视需要修订本指针 ADR。
