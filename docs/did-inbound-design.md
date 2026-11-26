# DID 呼入终结（被叫方计费）设计文档

**状态**：已实现并 e2e 验证（2026-11-26）
**决策来源**：pipecat-integration-plan §7 / O1 拍板（被叫方计费）；livekit-integration-plan Phase 2 共用件
**验证环境**：capbench compose 栈（Kamailio 6.1.5 + gv-core + PG + Redis + NATS×3）

---

## 0. 一页结论

运营商 trunk 的 INVITE 进入 gvos 边缘后，按 **DID 号码**路由到注册用户或外部二段腿，**计费锁在 DID 归属租户账户（被叫方）**。呼入费率 = 该账户自己的 rate_group（独立"呼入费率组"就是给该账户配一个专用组，schema 零变更）。CDR 加 `direction`（'in'/'out'）与 `gateway_id` 落库。media_agent 类型是 AI voice-agent 的**预留出口**，命中即 501（计费面拒绝为不可路由的呼叫锁账——这正是"只预留口子不实现"的形态）。

## 1. 数据面

- `migrations/013_did.sql`：`dids` 表（did_number UNIQUE、account_id 归属、destination_type ∈ {user, external, media_agent}、destination、enabled）；`cdr.direction` 列（'out' 默认，CHECK 约束）。
- `internal/customerdomain/did`：内存注册表（Upsert/Resolve/Delete/Reload），快照经 settings KV（"dids"）持久化，重启恢复。
- `internal/api/admin_did.go`：`POST/GET/DELETE /v1/admin/dids` + `GET /v1/internal/did-snapshot`（边缘 poller）。
- 广播三落点：`StartCfgRebroadcaster`（60s 全量重播）、`StartCfgReloadSubscriber`（副本间 entity="did" apply）、边缘 `event_route[nats:gvos-edge-cfg]` → `DID_SYNC`。

## 2. 控制面口径（callapi.go callStart）

呼入请求携带 `direction:"inbound"`（边缘注入）。控制面行为：

1. `dids.Resolve(to_user)` 未命中 → **404**（号码未登记，不是 403——被叫无过错）。
2. **计费账户 = did.account_id**，跳过 `$fU` 用户名解析（carrier MSISDN 永远不落账）。
3. owner 账户不存在 → 404（先销号后呼入的窗口）。
4. `destination_type=media_agent` → **501**（AI 预留口，不实现）。
5. 黑白名单：主叫号码按 **src** scope、DID 按 dest scope；external 二段腿再过一遍 dest。
6. 费率：`rates.Lookup(did_number)` —— 按 DID 前缀（被叫方自己的费率组）。
7. 应答（BILL_LOCK 200 body）新增：`direction`、`destination_type`、`rewrite_dest`（= destination）、`gw_id`（external 时控制面预选网关，per-destination LCR 取代全局 best-index 近似）。
8. CDR：`Call.Direction`/`Call.GatewayID` → CallMeta（Redis，跨副本 remote-settle 保留）→ CDR → `pg.insertCDRSQL`（direction 空→'out'，gateway_id 空→NULL，向后兼容旧调用方）。

## 3. 边缘分枝（kamailio.cfg）

```
INVITE → IPALLOW_CHECK（CIDR 准入，已有）
       → BL_CHECK（DID 前缀 dest 拦截，已有）
       → DID_INBOUND（新）：$rU 命中 did:bind:<number> htable
            → $avp(did_inbound)=1 → 跳过 AUTH_CACHE（trunk 无凭据）
       → record_route / dlg_manage / t_newtran / BILL_LOCK
            body 追加 "direction":"inbound"
       → BILL_LOCK_REPLY（应答处理，改造）：
            body 信封展平（{code,body:{...}} → blr_body）
            rewrite_dest 真正应用到 $rU（修复 AXB meta=>rw 只存不用的假接 bug）
            destination_type=user → lookup("location") 终结
                meta=>didusr=1：in-dialog ACK/BYE 无 Route 头时
                $ru = sip:<注册用户>@" + $Ri 再 lookup（SIPp 式 UAC 不认
                2xx Contact 的场景），481 当离线
            destination_type=external → $du = gw:dest:<gw_id>（预选网关直派）
            其余 → GW_DISPATCH（全局兜底）
```

- `did` htable：`did:bind:<number> = "<type>|<destination>"`；NATS `DID_SYNC` + 5s HTTP `DID_POLL`（版本戳）双通道，与 gw htable 同构。
- 对话同步：DID_APPLY/DID_SYNC/DID_POLL 三路由，全量幂等重放。

## 4. e2e 验证记录（2026-11-26）

SIPp `test/sipp/inbound.xml`（carrier 角：无凭据 INVITE → 100/180/200 → ACK → 5s → BYE → 200）：

- 3/3 呼叫 Successful；
- CDR：`direction='in'`，`dest_prefix='8621'`，`release='normal'`，`answer_time` 非空；
- 计费：3×60s 起步计费 0.15 元 = ledger delta -0.15 **逐分零漂移**；
- 被叫账户余额实时扣减（Redis 权威）。

## 5. 顺带修复（本次实证的 bug，非设计项）

1. **AXB rewrite_dest 假接**：`meta=>rw` 从未被应用到 `$rU`（axb 设计文档写"已接"，实际只存不用）——BILL_LOCK_REPLY 现在真正替换。
2. **边缘 NATS 认证断裂**（两因叠加）：compose 未注入 `GVOS_NATS_USER/PASSWORD`；entrypoint 渲染的 `nats_user` modparam 在本构建 nats 模块中不存在（启动即致命）。修复：凭证嵌入 `nats://user:pass@host` URL userinfo（libnats 原生解析）。
3. **BYE R-URI 自环**：用户终结的对话，carrier 把 BYE 发到我们的 R-URI（DID）而非 Contact——GW_CONTACT 按 `meta=>rw` 换 `$ru` 用户部分再 lookup。
4. **应答信封展平**：lockReply 的 `{code, body:{...}}` 嵌套结构从未被边缘 jansson 路径正确寻址（`direction`/`rewrite_dest`/`gw_id` 一直在 body 下）。

## 6. e2e 验证记录（external 二段腿，2026-11-26）

`did_ext.csv`（8621088005678，destination_type=external，destination=13800138000，demo-gw 服务 138 前缀）：

- SIPp 1/1 Successful；
- 边缘日志：`rewrite_dest applied rU=13800138000` + `DID second-leg dispatch to pre-picked gw 00000000-...-0b1`（**控制面预选网关生效，per-destination LCR 取代全局近似**）；
- CDR：`direction='in'`、计费 0.05、ledger 1 行对账。

## 7. 边界与后续

- `external` 二段腿的 AXB 反向绑定（B 回呼 X → 路由到 A）走同一 `dids` 表（destination= A 号码）——已 e2e 验证（§6）。
- 多 trunk 区分信任网段：ipallow 规则粒度是 CIDR 准入，DID 路由本身不区分来源网段——运营商多 trunk 场景给不同 CIDR 配 DID 前缀不重叠即可。
- `media_agent`（AI voice-agent）：呼到即 501。pipecat Phase 2 落地时把 501 分枝改为媒体面挂接，计费面零改动（这正是"预留出口"的含义）。
