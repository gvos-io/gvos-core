# 部署规划：架构、形态与生产化路线

**状态**：定稿（随代码演进维护）
**日期**：2026-11-19
**范围**：G-VOS 全组件的部署架构、已落地的部署形态、各形态操作要点与生产前缺口，以及分阶段的部署方式建议。
**关联**：[02-architecture](02-architecture.md)（架构设计）· [operations-manual](operations-manual.md)（日常运维）· [redis-adoption-plan](redis-adoption-plan.md)（gv-core 多副本前置项）

---

## 1. 部署架构总览

```
管理面    Web 管理台 ── REST API（gvos-core :8080，OpenAPI 3.1）
              │
控制平面  gv-core（Go，单二进制）：计费/账户/路由/风控/CDR/API
              │ NATS 事件总线（gvos.>，JetStream stream GVOS）
数据平面  Kamailio ×N（信令：注册/认证/选路/负载均衡，无状态池）
         RTPengine ×M（媒体：转发/DTLS-SRTP/录音 fork，直通模式）
         FreeSWITCH（可选：B2BUA 通话保持/IVR 客户，P1 起挂载）
              │
存储      PostgreSQL（账务/配置/CDR 权威，月分区）│ NATS（事件总线）
         对象存储（录音/导出）│ Redis（规划中，见 §4.4）│ Prometheus + Grafana
```

组件与载体清单：

| 组件 | 镜像/构建 | 角色 | 状态性 |
|---|---|---|---|
| gv-core | `deploy/gv-core.Dockerfile`（自建） | 计费控制平面 + REST API | 无状态（运行时状态在内存，见 §4.4） |
| Kamailio | `deploy/kamailio.Dockerfile`（Debian trixie 自建，带 nats.so） | SIP 信令边缘 | 无状态（配置/黑名单走 NATS 镜像到本地 htable） |
| RTPengine | `deploy/rtpengine.Dockerfile`（Debian 包，用户态转发） | 媒体中继/录音 | 有状态（通话媒体流）；直通模式下信令面故障不影响进行中通话 |
| PostgreSQL | `postgres:16-alpine` | 账务/CDR 权威 | **有状态，唯一硬权威** |
| NATS | `nats:2-alpine`（`-js` JetStream） | 事件总线 | 有状态（JetStream stream；core NATS 部分可丢） |
| gvos-asr | 自建 | 阿里 DashScope ASR 桥接（P2） | 按需部署 |

## 2. 组件耦合与容错语义（部署顺序由这些决定）

- **gv-core → PG 硬依赖**：启动门禁 `service_healthy`；PG 不可用则计费/API 全停。
- **gv-core → NATS 分档依赖**（口径见 02-architecture.md §4.1 v0.2）：HTTP 档（compose-ha/Helm 当前，bill-lock 走 HTTP）下 `RetryOnFailedConnect`，broker 宕机降级为 log-and-continue，不阻塞启动（compose 中 NATS 仅 `service_started`）；RPC 档（单机 compose 已开 WITH_NATS_RPC）下 NATS 是呼叫建立关键依赖，宕机 = 新呼叫 2s 内 503 有界失败。
- **Kamailio → 零 PG 依赖**：网关表/IP 准入/黑名单/风控封禁全部经 NATS 全量快照广播（`gvos.cfg.reload`）镜像到本地 htable，60s 周期重播兜底——节点重连/重启最多 60s 收敛。**NATS 长时间不可用时新节点起不来配置**（首轮快照未到前 IP 准入 fail-open，其余拒绝）。
- **Kamailio → gv-core 运行时依赖**：鉴权/选路同步调用（`GVOS_CORE_HOST` + `GVOS_INTERNAL_TOKEN`），所以 compose 里信令节点排在 gv-core 之后启动。
- **每信令节点唯一 NATS queue group**（`GVOS_NODE_HOST` 派生）：广播语义，每个节点收到每条消息一份——扩信令副本不需要改配置。compose 里手工指定节点名，Helm 里用 pod 名（fieldRef `metadata.name`），语义一致。
- **强制拆线（`gvos.call.teardown`）**：广播到全部信令节点，持有 dialog 的节点执行 `dlg.terminate_dlg`，其余 no-op；TEARDOWN durable 消费者在节点全下线时保留指令、重启后补发。

## 3. 部署形态与适用场景

| 形态 | 载体 | 客群 | 生产可用度 |
|---|---|---|---|
| 单机 all-in-one | `deploy/docker-compose.yml` | 试用/POC/500 并发档小客户 | 演示级（开发默认值密码/令牌，见 §6） |
| 双机热备 | `deploy/compose-ha.yml` | **ICP-1 主力交付形态** | 可交付，三个缺口见 §3.2 |
| K8s 集群 | `deploy/helm/gvos` | ICP-3 大客户/自营 SaaS（M12 GA 起） | chart 骨架就绪，见 §3.3 |
| 离线交付 | 私有化 tar 包 + 云端/本地 License | 政企/等保客户 | 规划项 |

### 3.1 单机 all-in-one

```bash
docker compose -f deploy/docker-compose.yml up -d --build
```

PG + NATS + gv-core + Kamailio + RTPengine + demo 网关一键起。用途边界：POC 与小客户低并发档。**不做任何 HA 承诺**。压测/验收均以此形态跑（见 docs/bench/）。

### 3.2 双机热备（compose-ha）——当前主力形态

```bash
docker compose -f deploy/compose-ha.yml -p gvosha up -d
```

组成：PG primary + hot-standby（WAL 流复制）、双 Kamailio 信令节点、gv-core 单副本、NATS 单节点。

**首次部署顺序（有一个坑）**：standby 的首次 `pg_basebackup -U replicator` 要求 primary 上已存在 `replication` 角色，但该角色**不在 compose/migrations 里**——`test/ha/drill.sh` §0 会幂等创建它。正确的首次顺序：

```bash
docker compose -f compose-ha.yml -p gvosha up -d pg-primary nats
# 幂等创建复制角色（drill.sh 的 §0，或手动执行同款 SQL）
bash ../test/ha/drill.sh   # 跑一次即完成角色创建 + 全套演练
docker compose -f compose-ha.yml -p gvosha up -d   # standby basebackup 成功，其余组件就位
```

直接整栈一次 `up` 时 standby 可能因角色不存在而 basebackup 失败退出——`docker compose start pg-standby` 重试即可恢复。

**每次交付必跑演练**：`bash test/ha/drill.sh` 输出 `drill-<ts>.md`（standby 复制延迟 = RPO 证据；停 primary 到手动 promote 的窗口 = RTO 证据）。**failover 是手动 promote，设计如此**——演练文档化切换窗口，正是对齐话务商"拔电源"验收心智的销售证据，交付留视频。

**上生产前的三个缺口（按优先级）**：

1. **NATS 单点**（compose 注释已自认）：生产先改 3 节点 NATS 集群跑同一 JetStream stream——配置量小（routes 互联 + 共享 supercluster），是当前性价比最高的补齐项。
2. **SIP 入口**：两节点 5060/5061 直暴仅适合演练；生产前置任意 UDP/TCP LB 或 DNS SRV（节点无状态，分发无需亲和）。
3. **PG 自动故障切换（已落地轻量版，2026-11-20）**：双机热备的自动切换分两层——①应用层：gv-core 的 DSN 改多主机（`pg-primary,pg-standby`）+ `target_session_attrs=read-write`，pgx 按序探测、只连可写节点，standby 被 promote 后**无需改配置即自动收敛**（pgx 原生能力）；②提升层：pg-standby 容器内置 watchdog（探 primary 连续 3 次失败 → `pg_ctl promote` 自动提升）。**Patroni 三节点同步复制仍是生产推荐**（共识选举、RPO 控制、脑裂防护），轻量版适合 compose-ha 交付档位。
3. **PG 自动故障切换**：当前手动 promote；容量/客户档位上来后上 Patroni 三节点同步复制（架构文档已规划，代码未进）。

### 3.3 K8s 集群（Helm chart）

`deploy/helm/gvos`：gv-core Deployment（可水平扩）+ Kamailio Deployment（`GVOS_NODE_HOST` 取 pod 名）+ SIP LoadBalancer Service（UDP/TCP 5060 + WS 4444）。

chart 的边界（按 values.yaml 设计）：

- **不带数据库与 NATS**：`database.dsn` / `nats.url` 指向外部 HA Postgres（Patroni 或托管）与 NATS 集群。这是刻意的——K8s 形态的存储层 SLA 由专职组件承担。
- **Kamailio 镜像必须 override**：values 默认 `ghcr.io/kamailio/kamailio-ci:6.1-alpine`，该镜像**没有 nats.so**（2026-11-19 实测：237 模块含 kafka/mqtt/rabbitmq、无 nats；Alpine 仓库亦无包）。K8s 部署用自建 `gvos-kamailio`（Debian trixie，`deploy/kamailio.Dockerfile`），推到私有 registry 后改 `image.kamailio.repository`。
- **RTPengine 外接**：`media.rtpengine.host` 指向媒体节点池（K8s 内 Pod 网络对 RTP 性能不友好，媒体面建议独立主机池，信令/控制面在集群内）。
- 媒体直通模式下，Kamailio Pod 漂移/重建不影响进行中通话，只影响新呼叫——信令面 HPA 放心扩缩。

### 3.4 离线交付（政企/等保）

私有化 tar 包（二进制 + 容器镜像导出 + 离线迁移工具）+ 云端 License（30 天离线宽限期）或本地 License 服务。部署形态上仍是 §3.1/§3.2 的组合，差别在License 与合规审计（append-only 审计表 + 等保测评材料）。

## 4. 部署方式建议（分阶段）

**结论：现阶段以 compose-ha 为主力交付形态，docker-compose 服务 POC；Helm/K8s 留给 M12 GA 后的大客户与自营 SaaS。**

1. **首批客户（ICP-1）**：compose-ha 双机交付。上生产前完成 §3.2 的缺口 1（NATS 集群化，配置改动一天内）与缺口 2（LB/DNS SRV）；缺口 3（Patroni）在单客户容量触及上限或客户要求 RTO<60s 自动切换时再上。
2. **gv-core 多副本的前置条件是 Redis 收口**：当前余额/并发/CPS/风控滑窗权威在各副本进程内存，N 副本即上限 ×N 超卖（详见 [redis-adoption-plan](redis-adoption-plan.md) 的 8 项缺口表）。**compose-ha 里 gv-core 保持单副本是正确现状**——多副本前先实施该方案。
3. **大客户/SaaS（M12 GA 起）**：Helm chart + 外部 Patroni PG + NATS 集群 + 媒体主机池。信令 HPA、控制面按容量扩，存储层 SLA 外置。
4. **硬件基线**（继承容量报告口径）：媒体节点 16 vCPU / 32GB / 万兆；信令节点 8 vCPU / 16GB。压测环境与生产环境一致性写进交付报告。

## 5. 环境矩阵

| 项 | 开发/自测 | POC/演示 | 生产（compose-ha） |
|---|---|---|---|
| 形态 | docker-compose | docker-compose | compose-ha |
| PG | 单机 16 | 单机 16 | 16 主备流复制（→Patroni） |
| NATS | 单节点 | 单节点 | 单节点（→3 节点集群） |
| gv-core 副本 | 1 | 1 | 1（多副本待 Redis 收口） |
| Kamailio 节点 | 1 | 1 | 2（前置 LB/DNS SRV） |
| 令牌/密码 | `dev-*` 默认值 | `dev-*` 默认值 | **必须全部更换**（operations-manual §5） |
| TLS | 自签演示证书 | 自签 | 正式证书（SIP TLS + SRTP 全默认开启） |

## 6. 生产部署必做清单

承载于 [operations-manual](operations-manual.md)，此处只列索引：

- 更换 `GVOS_INTERNAL_TOKEN` / 管理令牌 / `gvos_dev` DB 密码（README 已知限制，演示默认值全部禁用）；
- Kamailio PG 模块口令、NATS 端口不对公网暴露（8222 监控端口仅内网）；
- IP 白名单首轮快照加载前的 fail-open 窗口：部署完成后**立即下发全量 IP 准入快照**再放流量；
- 每日计账一致性对平 + 备份（operations-manual §1.1/§2.1）；
- 每次交付跑一次 HA 演练并归档 RTO/RPO 报告与视频。

## 7. 里程碑对齐

| 里程碑 | 部署相关交付项 | 状态 |
|---|---|---|
| M8（已完成） | docker-compose 一键部署 + 单节点压测报告 | ✅ |
| 首批交付（当前） | compose-ha + HA 演练脚本/RTO-RPO 报告 | ✅ 载体就绪 |
| 生产化补齐 | NATS 3 节点集群 + SIP LB 接入 | 待做（§3.2 缺口 1/2） |
| M12（GA） | Helm chart 生产化 + 双机 HA 演练视频 + 渗透测试报告 | 规划 |
| 后续 | gv-core 多副本（Redis 收口前置）+ Patroni | 规划（[redis-adoption-plan](redis-adoption-plan.md)） |
