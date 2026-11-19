# ADR-0002:注册状态归属——边缘本地为主,Redis 派生镜像为辅

**状态**:已接受(2026-11-19)

## 背景

Kamailio `usrloc db_mode=0`:注册表只在每个信令节点进程内存(`deploy/kamailio/kamailio.cfg:224`)。节点重启/漂移 → 本节点注册全丢,终端须重新注册(默认 max_expires 3600,长尾恢复最长 1 小时);节点间不共享。20 万注册目标下,注册风暴既是恢复放大器也是风控 reg_freq 误封禁源。

备选项:

| 选项 | 机制 | 代价 |
|---|---|---|
| ①usrloc db_mode=1/2 写 PG | Kamailio 原生 | **破坏"边缘零 PG"原则**——稳态 0 次 DB 查询的架构优势归零 |
| ②本地 + Redis 双份,本地优先 | 本地 miss 时查 Redis | Kamailio 需新增 Redis 查询路径(纯读,降级路径) |
| ③仅 gv-core regtable 进 Redis | redis-plan Phase 5 原案 | 只救 gv-core 镜像,**不解决 Kamailio 本地表恢复** |

## 决定(用户 2026-11-19 拍板)

**本地存一份,Redis 存一份;查找默认走本地,本地没有时查 Redis。**

即选项 ②,实施语义:

- **写路径**:注册事件(`gvos.reg.event`)→ gv-core regtable 写 Redis 镜像(`gvos:reg:{user}` hash,TTL=expires,redis-plan Phase 5 key 布局不变);
- **读路径**:Kamailio 稳态 lookup 走本地 usrloc;**本地 miss 时经 gv-core 内部 API 查 Redis 镜像**(contact/expires),用于:节点重启后的初始查找、跨节点场景排查——而不是把每通呼叫的 lookup 都打到 Redis;
- **边缘依赖面**:Kamailio 仍零 PG;Redis 查询是**降级路径**(fail-open:Redis 不可用时本地 miss 照旧,终端自然重注册),不是关键路径;
- usrloc `db_mode=0` 不变。

## 后果

- 节点重启后新 INVITE 查不到本地注册时,有 Redis 兜底窗口,呼叫不因"本地表空"而 404——终端重注册期间服务连续性提升;
- 代价:gv-core 增一个内部只读查询端点 + Kamailio cfg 一段降级分支(本地 miss → HTTP 查 gv-core → 仍 miss 才 404);
- 实施挂到 redis-adoption-plan Phase 5(regtable 改造时一并做);Kamailio 侧 cfg 改动复用 `GVOS_CORE_HOST`/`GVOS_INTERNAL_TOKEN` 既有通道;
- 20 万注册档位客户交付前重验:Redis 单点下的降级路径演练(列入验证批次)。
