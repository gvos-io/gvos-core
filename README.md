# G-VOS

面向持证话务商、呼叫中心服务商与 AI 语音平台的**云原生、API-first、内置合规的下一代话务运营系统**（软交换 + 计费 + 路由 + 风控）。定位是 VOS（南京昆石）的替代品，不是复刻。

> 战略与市场依据见 [vos-competitive-analysis.md](vos-competitive-analysis.md)，产品/架构/定价/迁移方案见 [docs/](docs/)。

## 当前状态：M4-M8 完成（含媒体面/录音/黑名单/NATS 事件总线，soak 运行中）

已完成的端到端闭环（INVITE 锁定 → 通话 → BYE 结算 → CDR → 落库）：

- **计费核心（Go，自研，全部单测覆盖）**
  - `internal/rate`：费率前缀最长匹配（从被叫号码派生前缀探测哈希表，10 万条费率下微秒级）、时段窗口覆盖（周末/夜间费率）、7 位小数第 8 位进一（VOS 精度语义）
  - `internal/billing`：计费时长步长 60+6 / 6+6 / 1+1 / N+M、MinDur、首步价/增量价分解——不变量：**cost 永远精确对 billedSecs 定价**
  - `internal/account`：租户→多级代理→账户树、全链路原子预锁定（all-or-nothing 回滚）、后付账户跳过锁定、环检测、List
  - `internal/callstate`：预锁定→周期扣减→余额耗尽**强制拆线**（teardown 回调，拆线原因写入 CDR）→ 结算补扣/退还，CDR 输出
- **持久化（`internal/store`，pgx v5.6）**：账户/费率加载（窗口费率→Effective）、CDR 落月分区表、账本/审计（jsonb before/after）、账户创建/充值（事务）/费率 upsert
- **事件总线（`internal/events`，NATS）**：JetStream stream `GVOS`（subjects `gvos.>`，无 JS 时自动降级 core pub/sub）；非阻塞发布——队列满或 broker 失联只记日志，**计费应答永不等待总线**
- **双运行模式**：`-dsn` 接 PG（加载+落库）；无 `-dsn` 纯内存（演示/测试）。存储失败只记日志不阻塞计费应答
- **管理 API**（`/v1/admin/*`，Bearer 守卫）：账户创建/列表、充值（PG 模式下事务+账本+审计）、费率 upsert **下一通电话即时生效**；全部变更写审计
- **数据模型**：`migrations/001_init.sql` + `002_seed.sql`（演示数据）
- **部署**：`deploy/docker-compose.yml`（gv-core + Kamailio + RTPengine + demo 网关 + PostgreSQL + NATS）

```bash
# 本地跑测试与构建
go test ./... && go build -o bin/gvos-core ./cmd/gvos-core

# 真 PG 集成测试（跳过条件：未设置 DSN）
docker compose -f deploy/docker-compose.yml up -d postgres
docker exec -i deploy-postgres-1 psql -U gvos < migrations/002_seed.sql
GVOS_TEST_DSN="postgres://gvos:gvos_dev@127.0.0.1:5432/gvos?sslmode=disable" \
  go test ./internal/store/ -v

# 冒烟：内存模式一通电话的完整计费闭环
./bin/gvos-core -listen :9997 &
curl -X POST -d '{"call_id":"c1","from_user":"1000","to_user":"8613800138000"}' :9997/v1/call/start
curl -X POST -d '{"call_id":"c1","duration":66}' :9997/v1/call/end
# -> {"CallID":"c1","BilledSecs":66,"Cost":0.055,"RatePrefix":"8613"}

# PG 模式 + 管理 API
./bin/gvos-core -listen :8080 -dsn "postgres://..." &
curl -X POST -H "Authorization: Bearer dev-admin-token" \
  -d '{"username":"user1","credit":10}' :8080/v1/admin/accounts

# 事件总线（-nats 或环境变量 GVOS_NATS_URL；不传即关闭）
./bin/gvos-core -listen :8080 -dsn "postgres://..." -nats nats://127.0.0.1:4222 &
# 订阅侧（compose 环境直接用 nats-box）：
docker run --rm --network deploy_default natsio/nats-box \
  nats sub -s nats://nats:4222 "gvos.>"
```

## 里程碑（对齐 docs/01-prd.md）

| 阶段 | 内容 | 状态 |
|---|---|---|
| M0-M3 | 架构 spike：端到端计费闭环 | ✅ 完成 |
| M4-M8 | P0 全量 + 中文管理台 + 压测报告 v1 | 未开始 |
| M9-M10 | 3 家设计伙伴 alpha | 未开始 |
| M11-M12 | v1.0 GA：渗透测试 / HA 演练 / 迁移工具 beta | 未开始 |

## 工程约定

- Go 1.21（标准库 `ServeMux` 无方法路由，handlers 自行校验动词）
- 金额一律 `float64` 计算后 `roundUp7`（7 位精度、进一）；数据库层 `NUMERIC(18,7)`，生产对账走整数分（后续引入）
- 测试先行：计费语义变更必须先有失败测试（已覆盖 VOS 全部步长方案）
- 所有接口变更同步更新 `docs/` 对应文档

## 目录

```
cmd/gvos-core/     # 事件入口 + HTTP API（spike）
internal/rate/     # 费率引擎（前缀匹配 + 时段窗口）
internal/billing/  # 计费器（步长方案 + 精度）
internal/account/  # 账户树 + 余额锁定
internal/callstate/# 通话状态 + 强制拆线 + CDR
internal/events/   # NATS 事件总线（JetStream 优先，core 降级）
migrations/        # PostgreSQL schema
deploy/            # docker-compose + Kamailio cfg + Dockerfile + Helm
docs/              # PRD / 架构 / 定价 / 迁移方案
research/          # 竞品与市场原始调研
```


## 事件总线（NATS）

`-nats <url>`（或 `GVOS_NATS_URL`）开启后，gv-core 把事件发布到 NATS：JetStream 可用时写入 stream `GVOS`（`gvos.>`），否则降级 core pub/sub（`/v1/stats` 的 `nats` 字段显示 off/core/jetstream）。发布严格非阻塞——broker 失联只记日志，计费路径零影响。

**三种 NATS 机制，三种真实场景**（`internal/events`）：

| 机制 | NATS 能力 | 场景 |
|---|---|---|
| 异步调用 | request/reply（`_INBOX` 异步等应答） | 拆线指令分发 `gvos.svc.teardown`：计费回调发出请求，执行器应答；生产期执行器移到 Kamailio 侧，gv-core 不变 |
| MQ | JetStream durable pull consumer（竞争消费、at-least-once） | Webhook 分发器：`-webhook <url>`（或 `GVOS_WEBHOOK_URL`）开启，durable `WEBHOOKS` 消费 CDR 事件 POST 到目标；失败 NAK 1s 后重投，MaxDeliver=5 防毒丸；多副本共享 durable 自动负载均衡 |
| 广播通知 | core pub/sub fan-out | 配置变更广播 `gvos.cfg.reload`：费率 upsert / 黑名单变更 → 所有 gv-core 副本即时从 PG 重建快照（"费率 60s 窗口"变即时生效） |

| Subject | 触发 | Payload |
|---|---|---|
| `gvos.cdr.completed` | BYE 结算完成（内存/PG 双模式都发） | 完整 CDR（Go 字段名 JSON，与 `/v1/call/end` 响应同构） |
| `gvos.call.teardown` | 余额耗尽强制拆线 / zombie 结算 | `{call_id, reason}` |
| `gvos.risk.alert` | 风控规则命中告警（非阻断） | `{account, rule_id, reason, to}` |
| `gvos.admin.changed` | 全部管理 API 变更 + 定时 rollup 完成 | `{action, entity, entity_id, before/after}` |
| `gvos.cfg.reload` | 配置/状态变更广播（多副本即时同步） | `{entity: rate\|blocklist(op+entry)\|account\|credit\|recording\|risk\|package, ...}` |
| `gvos.svc.teardown` | 拆线指令 req/reply | req `{call_id, reason}` → rep `{dispatched}` |
| `gvos.svc.call.start/answer/end` | 通话生命周期 req/reply（与 HTTP 面同语义） | req 同 HTTP 请求体 → rep `{code, body}` |
| `gvos.svc.stats` | 运维快照 req/reply | rep = `/v1/stats` 内容 |

**多副本语义**：账户创建/充值（绝对余额，幂等）、费率、黑名单（op-payload，且 PG 模式写穿 `number_lists`）、录音开关、风控规则、套餐定义/指派全部经 `gvos.cfg.reload` 即时同步并持久化到 `settings` 表（`migrations/007_settings.sql`），重启/新副本启动时恢复。interim 扣减驱动器（10s）让"余额耗尽强制拆线"在长通话上真正生效（拆线指令走 `gvos.svc.teardown`）。

```bash
# Webhook 分发（durable 消费，重启不丢；接收端 2xx 即 Ack）
docker run --rm -p 9090:9090 mcwebb/http-echo &        # 任意 2xx 接收端
./bin/gvos-core -listen :8080 -dsn "postgres://..." \
  -nats nats://127.0.0.1:4222 -webhook http://127.0.0.1:9090

# 全 NATS 客户端：无需 HTTP 即可完成一通计费电话
nats req -s nats://127.0.0.1:4222 gvos.svc.call.start '{"call_id":"c1","from_user":"1000","to_user":"8613800138000"}'
nats req -s nats://127.0.0.1:4222 gvos.svc.call.end   '{"call_id":"c1","duration":66}'
```

**不换 NATS 的面**（各有硬理由）：Kamailio→gv-core 的 `/v1/call/start` 锁定（同步 403 门，Kamailio nats 模块无阻塞 req/reply）；媒体流 WS 与 gvos-asr（20ms 二进制流）；管理 API/UI、gvos-migrate、`/healthz`（API-first 产品面与探针）；Kamailio auth_db 直读 PG（凭据不进总线）。Kamailio 的 answer/end 事件切 `nats_publish` 需换 Debian 基础镜像（Alpine aports 无 `kamailio-nats` 包），服务端已由 `gvos.svc.call.*` 就绪，切换时零服务端改动。




## 录音 / 黑名单（P1 模块）

- **录音**：管理台“录音: 开/关”或 `POST /v1/admin/record {"enabled":true}`；开启后每通电话
  rtpengine 落 pcap+元数据（volume `recdata`，按 call-id 命名），CDR `recorded` 列标记
- **黑名单**：`POST /v1/admin/blocklist {"kind":"black|white","scope":"src|dest","pattern":"86139"}`
  （加 `"remove":true` 删除）；前缀索引 220ns/次（10 万规则）；白名单进入 default-deny 模式；
  拦截发生在计费之前（403 带命中规则）
- **长稳**：`bash test/sipp/soak.sh 1440 10 2`（每波对账检查点；UAS 波间重启）

## 管理台

compose 起来后打开 `http://127.0.0.1:8080/ui`，输入开发令牌 `dev-admin-token`：
- **账户**：三层链视图 + 创建账户 + 充值/调整（写审计）
- **费率**：前缀表 + 保存（下一通电话即生效）
- **话单 CDR**：真实 SIP 呼叫的话单（含计费秒数/费用/释放原因）
- 顶部统计卡每 5 秒刷新（在线呼叫/累计 CDR/余额）

## VOS 迁移工具（gvos-migrate）

```bash
# 四步流水线（对 VOS 导出的账户/费率/网关 CSV）
go run ./cmd/gvos-migrate extract  -accounts vos-accounts.csv -rates vos-rates.csv -gateways vos-gw.csv -out snapshot.json
go run ./cmd/gvos-migrate validate -snapshot snapshot.json     # 孤儿账户/负费率拦截
go run ./cmd/gvos-migrate plan     -snapshot snapshot.json -out plan.md  # 语义差异表
go run ./cmd/gvos-migrate load     -snapshot snapshot.json -api http://127.0.0.1:8080 -token dev-admin-token

# 已验证：6 账户+6 费率导入运行中系统，导入账户立即可计费
# 负余额清零导入（欠费留在旧账）、非数字前缀自动清洗、中英文表头自适应
```
