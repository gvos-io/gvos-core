# G-VOS 容量与性能报告 v0

**日期**：2026-10-07
**阶段**：M4-M8（单节点内存模式，spike 架构）
**对照**：PRD 容量验收线（单节点 2,000 并发/300 CPS 为 M8 出口——本报告是通往该目标的基线测量）

---

## 1. 测试环境（完全披露）

| 项 | 值 |
|---|---|
| 被测系统 | gv-core（Go 1.21, 内存模式）+ Kamailio 6.1.4（ghcr CI 镜像）+ SIPp 3.7（ctaloi/sipp 镜像）|
| 部署 | Docker Desktop, docker compose 单机（4 容器：gv-core/kamailio/demo-gw/postgres）|
| 宿主机 | 13th Gen Intel Core i7-13700H（20 逻辑核），Windows + WSL2 后端 |
| 网络 | docker bridge（gvos_default），SIP over UDP 5060 |
| SIP 场景 | `test/sipp/load.xml`：Digest 认证（401 挑战+重发）→ INVITE → 200 → ACK → **500ms 通话** → BYE → 200 |
| 认证 | SIP Digest（用户 1000，HA1 从 PG accounts 表校验）|
| 计费 | 每通实时锁定 0.05，60+6 步长，CDR 落内存 ring |

## 2. 结果总表

| 场景 | 目标速率 | 呼叫数 | 成功 | 失败 | 结论 |
|---|---|---|---|---|---|
| 单实例梯度 | 10 CPS | 100 | 100 | 0 | ✅ 全过 |
| 单实例梯度 | 30 CPS | 91 | 0 | 91 | ⚠️ SIPp 单线程认证积压超时（见 §4）|
| 单实例梯度 | 50 CPS | 500 | 424 | 76 | ⚠️ 同上，间歇 |
| **4 实例并行** | **~100 CPS 聚合** | **2,000** | **2,000** | **0** | ✅ **零失败** |
| 单实例长跑 | 20 CPS × 60s | 600 | 600 | 0 | ✅ 稳定（累计 1,200 通）|
| 递增累计 | — | 5,135 CDR | — | — | ✅ 计费全程精确，余额可对账 |

**计费一致性验证**：全程 5,135 张 CDR，账户余额与「初始 + 充值 − Σ通话成本」精确一致（7 位小数），零多扣/少扣/漏单。通话后 live_calls 归零（残留 1 例为 SIPp 超时强杀呼叫、无 BYE 事件的已知边界，见 §5）。

## 3. 组件级基准

| 组件 | 指标 | 值 | 预算 | 结论 |
|---|---|---|---|---|
| rate.Lookup（10 万条费率） | 每次查询 | **133 ns/op, 0 B, 0 allocs** | P99 < 5ms | ✅ 余量 4 个数量级 |
| gv-core /v1/call/start（含鉴权/限流/锁定） | 端到端 HTTP | **3.5 ms** | < 50ms | ✅ |
| Kamailio bill-lock（HTTP 同步调用） | 日志观测 | 无失败 | — | ✅ 691/691 |
| CDR 生成 | 每通 | 微秒级（内存）| < 2s | ✅ |

## 4. 已发现的瓶颈与根因（诚实披露）

1. **SIPp 单实例 >25 CPS 时认证阶段积压**：不是被测系统瓶颈——Kamailio 侧 bill-lock 691/691 全通过、失败批次期间 gv-core 3.5ms 响应。根因是 SIPp 单线程在 401 挑战+Digest 计算的排队。多实例并行即消除（4×25=100 CPS 零失败）。
2. **SIPp UAS（demo-gw）约 1,000 呼叫软上限**：累计 1,305 通后开始拒收（"Dead call aborted at index 6"），表现为"间歇性全失败且难自愈"。重启 UAS 恢复。压测脚本已改为分批+多实例规避。**生产无此问题（真实网关无此限制）**。
3. **僵尸通话泄漏**：SIPp 超时强杀呼叫时 BYE 不发出，gv-core 通话表残留（本次 5,135 CDR 后残留 1）。锁定余额 30 分钟不回收。修复项：通话超时清扫器（P1）。
4. Windows/WSL2 + Docker Desktop 的网络栈开销显著高于 Linux 裸机（UDP 代理层）；绝对数字偏保守，Linux 生产环境预期更高。

## 5. 对照 PRD 验收线

| PRD 指标 | 当前状态 |
|---|---|
| 单节点 2,000 并发 | 未测（需长通话场景+媒体面，当前 500ms 短呼）；无数据支撑前不宣称 |
| 300 CPS | **聚合 100 CPS 零失败**（信令+认证+计费全链路）；单实例受限于负载生成器 |
| 注册 20 万 | 未实现（REGISTER 返回 405，spike 阶段）|
| 计费正确性 100% | ✅ 5,135 CDR 对账一致 |
| 选路 P99 < 5ms | ✅ 133ns（微基准） |

## 6. 下一步（通往 v1 报告）

1. 长通话场景（60s+ hold）测真实并发容量；RTPengine 媒体面接入后测媒体转发
2. PG 模式下重跑（当前为内存模式——CDR 落库路径未在压测中）
3. 通话超时清扫器消灭僵尸泄漏
4. SIPp 换多实例常驻或 SIPP supervisor；UAS 换 FreeSWITCH/Kamailio 自回环
5. Linux 裸机环境复测，数据进公开报告 repo

## 7. 复现

```bash
docker compose -f deploy/docker-compose.yml -p gvos up -d
# 单实例梯度
bash test/sipp/bench.sh "10 20 30"
# 4 实例并行（~100 CPS）
for i in 1 2 3 4; do
  docker run -d --network gvos_default --name sipp-w$i \
    -v "$(pwd)/test/sipp:/scen:ro" ctaloi/sipp:latest \
    -sf /scen/load.xml -inf /scen/dest.csv -m 500 -r 25 -timeout 30s kamailio
done
# Go 微基准
go test -bench=LookupFast -benchtime=200000x -run=xxx ./internal/rate/
```
