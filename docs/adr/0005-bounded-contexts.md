# ADR-0005:限界上下文目录结构——模块化单体,演进为服务的边界

**状态**:已接受(2026-11-19)

## 背景

M0-M8 长出的 `internal/` 16 包已是事实上的领域包,但存在四个结构问题:①`cmd/gvos-core` 是 3,900 行神对象(Server struct 23 字段,admin.go 879 行,admin API/NATS worker/teardown/webhook 全糊在 main 包);②无成文的包间依赖规则(下一个改 callstate 的人不知道边界);③空壳与垃圾目录(pkg/、internal/config/ 空,`;C` Windows 残留目录);④包名与领域语义不对齐(callstate 实为呼叫生命周期域)。

用户 2026-11-19 提出按微服务领域拆分视角重新规划目录。

## 备选项

| 选项 | 评价 |
|---|---|
| ①物理微服务拆分(多进程/多仓) | ❌ 时机错误:gv-core 单副本约束(Redis 收口前)、容量路径单机实测接得住、团队单人——拆进程只有成本没有收益 |
| ②分层目录(domain/service/repository) | ❌ 重复造层:领域包+接口层已经存在,机械分层把接口推得更远 |
| ③**限界上下文命名空间(选定)** | 目录即 BC,依赖规则即服务边界,进程拆分留给触发器 |

## 决定

**模块化单体:三个限界上下文 + 共享内核 + 接口层,依赖规则进 CI。**

```
internal/
├── billingdomain/    # BC-1 计费域:account · rate · billing · pkgplan
├── customerdomain/   # BC-2 客户/策略域:riskctrl · blocklist · ipallow · gateway · regtable
├── mediamain/        # BC-3 媒体域:mediastream · dashscope
├── callcore/         # 呼叫生命周期域(原 callstate):对话状态/interim/拆线
├── shared/           # 共享内核:events · store · migrate · config
└── api/              # 接口层:HTTP handler + NATS workers + Server 装配
```

**依赖规则(CI 强制,depguard)**:
1. `api` → 任意域 ✅(接口层在顶点);
2. BC 内自由;
3. `customerdomain` → `billingdomain` ✅ 单向(风控日费用读账户树);
4. `billingdomain` → `customerdomain` ❌(风控/网关配置经 shared/events 注入);
5. `mediamain` → `billingdomain` ✅;6. 所有域 → `shared` ✅;`shared` → 域 ❌(共享内核不得知域)——**修订(2026-11-20,deprules 测试首跑裁决)**:`shared/store` 是持久化适配器,投影域类型是功能性需求,登记为**认可例外**;`shared/events`、`shared/acl`、`shared/rds`、`shared/migrate` 仍不得知域。强制方式:`internal/deprules`(go list 直接 import 检查,进 `go test ./...` 主门禁);
7. `cmd/*` 只装配不含业务。

**演进为服务的触发器(满足其一方可拆进程)**:独立扩容需求 / 故障隔离需求 / 团队边界(多人各持一 BC)。拆分顺序:media(内存隔离收益最直观)→ 计费域(Redis 收口+容量到顶后)。拆分的实现形态 = BC 包移出 + 边界换 NATS 调用,域内代码不动——这正是 BC 隔离要买的东西。

**清理项**:pkg/(空)删除;internal/config/(空)删除;`;C` Windows 残留目录删除;bin/ 进 .gitignore;callstate → callcore 改名(避免与 api 层 callcore.go 撞名)。

## 后果

- 第一步(纯移动,零行为变更):git mv 三域 + api 下放 + 清理 + 回归,半天;
- 第二步(与 API Phase A 合并):Server 23 字段拆 4 个 Engine 装配结构(BillingEngine/PolicyEngine/MediaBridge/Infra),admin.go 按资源拆文件,dispatch.go 拆 workers/——**与 Phase A 路由资源化改同一批文件,合并 PR 省一半工作量**;
- import path 全量变更一次(vendored pgx 不受影响);
- 风险:纯移动用 git mv 保历史;行为级拆分全部在测试全绿基线上做。
