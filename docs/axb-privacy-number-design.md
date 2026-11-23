# AXB 隐私号完整技术方案

**状态**：设计定稿（预留实现；控制面骨架已有）  
**日期**：2026-11-23  
**范围**：中间号 X 绑定 A/B、呼叫改写、回呼、号码池、持久化、计费与边缘联动。  
**现状代码**：`internal/customerdomain/axb`（内存 Store）+ `POST/GET/DELETE /v1/admin/axb/*` + `callStart` 对 `Resolve(to)` 后下发 `rewrite_dest`。

---

## 1. 目标与非目标

### 1.1 目标

1. **A 拨 X → 平台改写到 B**：双方只看到 X，不暴露对方真实号。
2. **B 回拨 X → 路由到 A**（同一 binding 双向）。
3. **号码池**：可配置 X 号段；绑定限时；过期回收可复用。
4. **计费**：按真实被叫（B 或回呼时的 A）费率计费；CDR 记录 X + A + B。
5. **多副本**：绑定权威在 PG（+可选 Redis 热缓存）；与 Mode A 叶子扣款兼容。
6. **边缘**：Kamailio 收到 `rewrite_dest` 后改 R-URI/`$ru` 再选路。

### 1.2 非目标（本期不承诺）

- 运营商 AX 能力开放平台对接（号码采购/实名/短信 OTP）。
- 录音分轨与隐私号强绑定（可后续用 X 作合规关联键）。
- 完整 IVR「按键选线」式中间号。

---

## 2. 呼叫模型

```
正向：A ──INVITE X──► Kamailio ──bill-lock──► gv-core
                              Resolve(X)→{A,B}
                              bill leaf(A), rate(B)
                              return rewrite_dest=B
                         ◄── 200 + rewrite_dest
              Kamailio 改写 $ru/$tU → B，LCR 出网关

回呼：B ──INVITE X──► （DID 入局到本平台）
                              Resolve(X) 或 ResolveReverse(X,from=B)
                              → rewrite_dest=A
```

**DID 入局**：需在网关/中继上把 X 号段落地到本 Kamailio（`request_route` 对无注册用户走 AXB 解析，而不是 404）。本设计在控制面预留 `ingress: did|extension`；边缘路由规则属部署配置。

---

## 3. 数据模型

### 3.1 表 `axb_bindings`（迁移已有雏形则对齐）

| 列 | 类型 | 说明 |
|---|---|---|
| id | uuid/text PK | 绑定 ID |
| tenant_id | uuid | 租户隔离 |
| x_number | text UNIQUE | 中间号 |
| a_number | text | A |
| b_number | text | B |
| direction | text | `both`（默认）/`a2b`/`b2a` |
| expire_at | timestamptz | 过期 |
| active | bool | 软撤销 |
| pool_id | text | 所属号池 |
| meta | jsonb | 业务扩展 |
| created_at / updated_at | timestamptz | |

索引：`(tenant_id, a_number, b_number)` 活跃唯一（部分索引 `WHERE active`）；`x_number WHERE active`。

### 3.2 表 `axb_pools`

| 列 | 说明 |
|---|---|
| id / tenant_id / prefix | 号池 |
| start_n / end_n | 数字区间（或 CIDR 式号段描述） |
| strategy | `random` / `sequential` |

### 3.3 Redis（可选热路径）

- `gvos:axb:x:{X}` → JSON binding，TTL = expire 剩余  
- 写路径：PG commit → SET Redis → `gvos.cfg.reload` entity=`axb`（边缘若做本地 htable 缓存）  
- 读路径：`Resolve` 先 Redis，miss 查 PG 回填  

未配 Redis 时与现内存 Store 行为一致（单副本/演示）。

---

## 4. API 契约（预留）

已有管理面保留并扩展：

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/v1/admin/axb/bindings` | `{a_number,b_number,ttl_secs?,pool_id?,tenant_id?}` → binding |
| GET | `/v1/admin/axb/bindings` | 列表（cursor） |
| DELETE | `/v1/admin/axb/bindings/{x}` | 按 X 撤销 |
| GET | `/v1/admin/axb/stats` | 活跃数 / 池利用率 |
| PUT | `/v1/admin/axb/pools` | 号池定义（**新增**） |
| GET | `/v1/tenant/axb/bindings` | 租户自助绑定（**新增**，限本租户） |

OpenAPI：`privacy` tag（已在 `openapi.go` 注册 4 条；池与 tenant 面补登记）。

幂等：`Idempotency-Key` 作用于 Bind（同 A+B+窗口复用同一 X）。

---

## 5. 控制面流程

### 5.1 Bind

1. 校验 A/B、租户配额（每租户最大活跃绑定）。  
2. 若存在未过期同 pair → 延长 `expire_at` 或原样返回。  
3. 否则从 pool 分配 X（避让占用）。  
4. 写 PG → 热缓存 → 审计 `axb.bind`。

### 5.2 callStart 解析（已部分实现）

```
if binding, ok := axb.Resolve(to_user); ok {
  rewrite_dest = binding.BNumber   // 正向：A 拨 X
  // 计费账户仍为 from_user 叶子；费率按 rewrite_dest（B）最长匹配
  cdr 扩展字段建议：privacy_x, privacy_peer
}
```

回呼：若 `from` 匹配 `b_number` 且 `to` 为 X → `rewrite_dest = a_number`。

### 5.3 撤销 / 过期

- API Revoke：`active=false`，删 Redis。  
- Sweeper（1m）：扫 `expire_at < now()`，回收 X 入池。  
- 进行中通话：不强制拆线；新呼叫 Resolve 失败 → 普通无路由/403。

---

## 6. 边缘（Kamailio）

现状：bill-lock 应答体可含 `rewrite_dest`，`BILL_LOCK_REPLY` 写入 `meta=>rw:$ci` 后在 dispatch 前改写。

补强清单：

1. 文档化改写点：`$rU` / Request-URI / To（按网关要求）。  
2. 可选：边缘 htable `axb=>x:{X}` 镜像（cfg.reload），**仅加速**；权威仍在 core。  
3. DID 入局：对 `from` 为外线、`rU`∈X 池的 INVITE 同样走 bill-lock（不要当未注册用户拒）。

---

## 7. 计费与合规

| 点 | 决策 |
|---|---|
| 费率 | 按真实落地号（rewrite 后）匹配 |
| 账户 | Mode A：主叫叶子（A 或回呼时 B 对应账户） |
| CDR | `dest`=落地号；另存 `privacy_x`；报表可按 X 聚合 |
| 风控 | 对 A/B 真实号做频次；X 不参与「异常被叫」误伤 |
| 录音 | 文件键可用 `call_id`；合规导出按 X 关联双方 |

---

## 8. 分阶段实施

| Phase | 内容 | 判定 |
|---|---|---|
| A0 | **本设计 + OpenAPI 预留**；保持内存 Store | 文档合入 |
| A1 | PG 持久化 + 启动加载 + 过期 sweeper | 重启绑定不丢 |
| A2 | Redis 热缓存 + 多副本一致 | 双副本 Resolve 一致 |
| A3 | 号池 API + 租户自助 Bind | 池耗尽 409 |
| A4 | 回呼 DID 路径 e2e（需入局中继） | B→X→A 通 |
| A5 | 运营商号池对接适配器（可选） | 接口稳定 |

---

## 9. 风险

- **X 号资源**：未接运营商时只能用自有号段；生产必须 DID 落地。  
- **改写与鉴权顺序**：须在 Digest 通过后、出网前改写，避免鉴权 realm 混乱。  
- **隐私合规**：日志默认脱敏（只打 X 后 4 位）；审计保留完整绑定供取证。

---

## 10. 与现有代码映射

| 模块 | 现状 | 下一刀 |
|---|---|---|
| `axb.Store` | 内存 | 加 `PGStore` 实现同一接口 |
| `callapi.callStart` | Resolve→rewrite | 补回呼方向 |
| `migrations` | 查 `axb_bindings` | 对齐 §3.1 + pools |
| Kamailio | rewrite_dest 已接 | DID 入局路由文档化 |
| OpenAPI | 4 endpoints | +pools +tenant |
