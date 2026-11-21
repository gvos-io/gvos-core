# 阿里云真实容量天花板（2026-11-21，nmg_ztgwyc3）

**环境**：阿里云 ECS 4核/7.3GB/99GB（Alibaba Cloud Linux 4，内核 6.6），Docker 24 + compose 2.26；G-VOS 全栈（NATS 3 节点 R3 + Kamailio children=32 + RTPengine + PG16 + Redis7）。**loadgen 与被测栈同机**（SIPp 容器）。
**调优**：sysctl rmem_default=4MB/rmem_max=8MB（持久化 /etc/sysctl.d/99-gvos.conf）→ Kamailio SO_RCVBUF=4MB。

## 容量天花板（4核 ECS，同机 loadgen 口径）

| 维度 | 实测 | 判定 |
|---|---|---|
| **100 CPS × 6000** | **100% 满分**（PDD p99=4ms，CDR=ledger 平账） | 干净可重复 |
| 200 CPS 长轮 | 66%（修复前 50%） | 栈侧 p99 6ms 恒定；失败 = loadgen 同机自限 |
| 300/400 CPS | 22%/10%（短窗口 400CPS 可达 51%） | 同上：4 核被 loadgen 分食 |
| **并发通话 2000** | **2000/2000，60s 保持全部正确**（PDD p99=4ms，duration=61s×2000，去重+平账精确） | 干净可重复 |
| 栈资源余量 | p99 恒 6-15ms、CPU<30%、内存 5.9G 可用、NATS 队列零积压 | **平台未饱和，上限=loadgen 同机** |

**结论：4 核 ECS 上"可承诺口径"= 100CPS 稳态 / 2000+ 并发；栈侧从未成为瓶颈（所有失败形态的栈侧计数器全绿）。拿更高数字需要 loadgen 分离（跨机压测）或更多核。**

## 云上挖出并修复的真 bug（账务级）

### C-2（严重）：同 call_id 双 CDR —— 唯一索引形同虚设
- **现象**：200CPS 长轮每个 call_id 两行 CDR（真行 cost=500 + 幽灵行 cost=0/ledger 0，相隔 78s）。
- **根因双叠**：
  1. PG 分区表上 `CREATE UNIQUE INDEX ... IF NOT EXISTS` 在**分区已有数据后**执行时只建了 `ON ONLY` 父索引（不级联到分区）→ **不约束任何行**（手动验证：同键插入两次都成功）；
  2. BYE 重传（新 branch，t_check_trans 无法吸收）重新进 CDR_EVENT → cdr.raw 重发 → 与第一份在 CDRR 队列竞态 → 第二次 End 成功（幽灵 duration=0），真的那份反而 404-Ack。
- **修复**：① migration 008 改为 DROP+CREATE 强制级联（分区本地唯一索引，服务器已生效并验证拦截）；② Kamailio `meta:cd` 哨兵**不再清空**（挂到 600s autoexpire），从源头压制 BYE 重传的 cdr.raw 重发。

### C-1（高）：zombie 恶性循环 —— BYE 丢失通话拖垮 RPC
- **现象**：400CPS 轮 live_calls 冲到 21829，SVC-CALL-START 吞吐归零（RPC 回复饿死）→ 挂起堆积 → 恶性循环。
- **根因**：BYE 丢失通话在 gv-core 留活 10 分钟（默认 zombie sweep），每个 zombie 每 10s 骑 Interim/Redis 循环；数千 zombie 抢占 CPU 饿死 responder。
- **修复**：`GVOS_ZOMBIE_TIMEOUT` env（服务器设 90s bench 档；生产默认 10 分钟不变——真实 BYE 丢失率低且计费语义优先）。

### 部署坑（云上首次暴露，runbook 价值）
1. compose 只挂了 001_init.sql → settings 表缺失 → ipallow default-deny 全拒。**必须挂整个 migrations 目录**（已改）。
2. ip_allowlist 存储格式是 `[]ipallow.Rule` 数组（`{"id","cidr","action"}`），不是猜测的 allows/denies 包裹。
3. **PG credit 充值必须同步 Redis 链**（`gvos:bal:<uuid>`），否则 403 Insufficient Balance（记忆中的坑在云上复现）。
4. rtpengine 容器内 `[::1]` 绑不上（阿里云内核 IPv6 localhost 行为）→ entrypoint 加 `--listen-cli 127.0.0.1:2224`。
5. tls 证书目录必须随 deploy 一起传输（kamailio 无证书起不来）。
6. SSH 长连接在压测中被 reset → **压测一律 nohup 服务器本地跑 + 轮询日志**。

## 与本机（Windows Docker Desktop）对照

| 指标 | 本机（7.6G/多核） | 云 ECS（4核） |
|---|---|---|
| 100CPS | 10000/10000（多轮修复后） | 6000/6000 首轮即满 |
| 并发 | 9853（8 实例极限） | 2000 干净（可继续推，loadgen 限） |
| BYE-200 尾巴 | 有（WSL2 桥接） | **无**（Linux veth 干净） |
| 需要的修复 | E/F/H 系列 9 个 | C-2/C-1 两个（账务级） |

## 下一步（要 300CPS+ 数字时）
- loadgen 分离：公网 UDP 直打（需安全组开 5060）或专用压测机同 VPC；
- 8 核+ ECS 纵扩或 2 副本横向；
- K8s 形态验证（sysctl initContainer + hostNetwork，见 ceiling-report-2026-11-21.md K8s 清单）。
