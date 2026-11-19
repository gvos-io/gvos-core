# G-VOS 运维手册

**适用**：G-VOS v0.3+（compose 单机 / compose-ha 双机）
**读者**：系统运维工程师、交付实施工程师

---

## 1. 日常运维

### 1.1 健康检查（每日）

```bash
# 组件健康
docker compose -f deploy/docker-compose.yml -p gvos ps
curl -s http://127.0.0.1:8080/healthz        # 应返回 ok

# 计账一致性（核心 invariant，任何时刻 CDR 与账本必须对平）
docker exec gvos-postgres-1 psql -U gvos -t -c "
SELECT 'cdr', count(*), round(sum(cost)::numeric,7) FROM cdr
UNION ALL
SELECT 'ledger', count(*), round(-sum(delta)::numeric,7) FROM balance_ledger
WHERE reason='call-settle';"
# 两行金额必须相等；不等 → 立即停写并联系研发

# 僵尸通话（live_calls 持续 >0 超过 11 分钟为异常）
curl -s http://127.0.0.1:8080/v1/stats
```

### 1.2 关键日志

```bash
docker logs gvos-gv-core-1   # 计费控制面：TEARDOWN/MEDIA/REPORT/RISK-ALERT
docker logs gvos-kamailio-1  # 信令面：bill-lock/dispatching/in-dialog
docker logs gvos-rtpengine-1 # 媒体面
```

告警关键词：`RISK-ALERT`（风控命中）、`TEARDOWN`（强制拆线）、
`cdr event failed`（Kamailio→gv-core 链路异常）、`bill-lock REJECTED`
（余额/风控拒绝——量大时检查客户余额或规则）。

## 2. 备份与恢复

### 2.1 备份（每日）

```bash
# 逻辑备份（全部业务数据：账务/费率/审计/CDR）
docker exec gvos-postgres-1 pg_dump -U gvos -Fc gvos \
  > backup/gvos-$(date +%F).dump

# 录音文件（独立卷）
docker run --rm -v gvos_recdata:/data -v $PWD/backup:/b alpine \
  tar czf /b/recording-$(date +%F).tgz -C /data .
```

### 2.2 恢复

```bash
docker exec -i gvos-postgres-1 pg_restore -U gvos -d gvos --clean backup/gvos-YYYY-MM-DD.dump
# 录音卷：解包回 recdata 卷
```

**验证恢复**：恢复后必跑 §1.1 的计账一致性查询；CDR 总数与金额应与
备份时刻一致。**恢复演练每季度至少一次**（在隔离环境做，不动生产）。

### 2.3 compose-ha 双机

复制链由 PostgreSQL 流复制承担，standby 实时同步。主库故障：
`docker exec gvosha-pg-standby-1 pg_ctl promote -D /var/lib/postgresql/data`
（参考 `test/ha/drill.sh`——已验证 RTO 1s、RPO 334ms）。
**提升后原主库不可直接回切**（时间线分叉），需重新 basebackup。

## 3. 升级

```bash
# 0. 升级前：完整备份（§2.1）+ 记录当前镜像版本
# 1. 拉新代码/镜像
# 2. 滚动升级（计费面秒级中断窗口）
docker compose -p gvos up -d --build gv-core
# 3. 数据库迁移：按序执行 migrations/00N_*.sql
docker exec -i gvos-postgres-1 psql -U gvos < migrations/00N_xxx.sql
# 4. 冒烟：跑 test/security/baseline.sh + 一通 SIPp 认证呼叫
```

规则：**先起新版本容器再切流量**（无状态设计支持并行验证）；
数据库迁移脚本只增不改（新增列/表，永不改旧列语义）——回滚 =
旧镜像 + 数据不变。

## 4. 常见故障排查

| 现象 | 排查顺序 |
|---|---|
| 呼叫全部 403 | gv-core stats 余额 → 账户 credit；风控规则（`GET /v1/admin/risk-rules`）；黑名单 |
| 呼叫 503 No Gateway | `kamcmd dispatcher.list` 看网关 FLAGS（IP=探测失败）→ 检查网关容器/网络；恢复后探测自动恢复 |
| bill-lock REJECTED rc=3 | Kamailio→gv-core 网络断（`docker ps` 看 gv-core 状态） |
| live_calls 持续增长 | UAS/网关不发 BYE；10 分钟后清扫器自动结算；持续增长=泄漏，报研发 |
| REGISTER 失败 | accounts 表 ha1 是否为 CHAR(32)；version 表 accounts=7 |
| dispatcher 启动失败 | version 表 gateways/dispatcher=4；gateways 表列形状（flags/priority） |
| CDR 与账本不平 | 停写 → 导出两张表 → 联系研发（正常情况下不可能，WAL/事务保证） |

## 5. 安全加固清单（生产部署必做）

- [ ] 管理令牌换随机值（**不要用 dev-admin-token**）；启用 gv-core TLS（`-tls-cert/-tls-key`）
- [ ] PostgreSQL 强密码 + `sslmode=require`（内网可 `verify-ca`）
- [ ] 管理端口（8080）不对公网暴露——仅 VPN/内网/堡垒机
- [ ] SIP 端口暴露面最小化；启用 fail2ban 级别的来源限制
- [ ] 每季度跑 `test/security/baseline.sh` + 第三方渗透测试
- [ ] 审计日志异地归档（audit_log 表 append-only，禁止 UPDATE/DELETE 权限）
- [ ] Docker 密钥管理：生产不用 bind-mount 明文 key（用 k8s secret/docker secret）

## 6. 容量规划参考

- **实测基线**（见 `docs/bench/capacity-report-v0.md` v0.3）：单节点
  2,000 并发 / 100 CPS 聚合零失败，账目零漂移
- 扩容路径：信令节点无状态（加 Kamailio 容器 + LB）；媒体加
  rtpengine 节点；计费控制面垂直扩容（Go 单进程，当前阈值远未触顶）
- 数据增长：CDR 月分区自动路由；**每月建下月分区**（预留
  `migrations/006_partition_maintenance.sql` 手工执行或挂 cron）；
  超过保留期的分区 `DETACH + 归档到对象存储`
- 磁盘预算：CDR ≈ 1KB/通（含索引）；录音 ≈ 100KB/分钟（pcap）——
  按话务量预估，录音卷建议独立磁盘

## 7. 支持与升级通道

- 工单/邮件：support@gvos.example
- 安全漏洞：security@gvos.example（PGP 可用，72h 响应）
- 升级窗口建议：话务低谷期（本地时间 03:00-05:00），提前 24h 备份

## IP 白名单（源 IP 准入）

管理入口（Bearer admin token）：

```bash
# 全量替换规则（空表 = 默认拒绝所有 SIP 源）
curl -X POST -H "Authorization: Bearer <token>"   -d '{"rules":[{"id":"office","cidr":"203.0.113.0/24","action":"allow"},
                {"id":"quarantine","cidr":"203.0.113.66/32","action":"deny"}]}'   http://<gv-core>:8080/v1/admin/ipallow

# 查看当前规则
curl -H "Authorization: Bearer <token>" http://<gv-core>:8080/v1/admin/ipallow

# 删除单条
curl -X POST -H "Authorization: Bearer <token>"   -d '{"remove_id":"quarantine"}' http://<gv-core>:8080/v1/admin/ipallow
```

语义与传播路径：

- **默认拒绝**：未命中任何 allow 规则的源一律 403；deny 规则优先于 allow（例外语义）
- 变更即时广播到所有 gv-core 副本（`gvos.cfg.reload`）；Kamailio 信令节点每 30s 经
  `/v1/internal/ipallow-snapshot` 拉取版本化快照到内存 htable，快照未加载前 fail-open（日志可见）
- 规则持久化在 `settings` 表（`ip_allowlist` key），重启恢复
- Kamailio 侧需设置环境变量：`GVOS_CORE_HOST`（gv-core 地址）与 `GVOS_INTERNAL_TOKEN`
  （内部快照令牌，必须与 gv-core 一致；生产环境更换默认值）
- Digest HA1 认证走 authcache htable（24h TTL）——稳态下每通呼叫 0 次 PG 查询

## 时钟纪律（ADR-0004）

计费时间戳的权威是 **gv-core 进程时钟**（CDR 落库以 gv-core 收到事件时刻为准，Kamailio 侧载荷时间为辅）；PG 的 now() 仅作审计行时间。多副本部署的硬性前置：

- **所有 gv-core / Kamailio / PG 节点必须 NTP 同步，偏差预算 ±50ms**；
- 每次交付跑 `bash test/security/baseline.sh`（含 NTP 同步与 chrony 偏移检查项）；
- 跨副本对账报告含"时钟偏差抽检"一节；
- 套餐周期 key（cycleKey）与风控日费用窗口由 gv-core 统一生成，Kamailio 不生成周期边界 key。
