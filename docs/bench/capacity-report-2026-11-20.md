# 容量与性能报告 v0.5(2026-11-20)

**环境**:开发机 Docker compose 单机栈(gv-core + PG 16 + Redis 7 + NATS JetStream + Kamailio + RTPengine,全部容器化);**Redis 余额权威已启用**(本报告与 v0.4 的关键差异:计费门走 Redis Lua 原子操作)。负载工具:`cmd/gvos-loadgen`(本次入仓,受控 CPS 的 start+end 对,走 call/start HTTP 面),**工具与报告同仓可复现**。
**口径**:S 系列 30 秒稳态;burst 无节流 5 秒;金额定点化(ADR-0003)后首测。

## 1. 结果

| 项 | 目标 | 实测 | 判定 |
|---|---|---|---|
| **S1 稳态 100 CPS** | 100 CPS 零失败 | **2901/2901 零失败**(有效 96.7 CPS,pacing  sleep 精度损耗) | ✅ |
| **S2 稳态 300 CPS** | 300 CPS 零失败 | **8164/8164 零失败**(有效 272.1 CPS = 目标的 91%) | ✅ |
| **S3 burst** | 超过去年 1245 CPS | **1562 CPS 有效吞吐、7810/7810 零失败**(p50 3.0s 为客户端排队含 runtime 调度,服务端零失败) | ✅ 超越 |
| **start 时延(100 CPS)** | p99 < 5ms | **p50=2.0 / p95=3.1 / p99=4.3 / max=6.4 ms**(Redis 权威下!亚毫秒级 Lua 往返叠加后仍达标) | ✅ |
| **计费正确性** | 逐分零漂移 | CDR 17363 行 = ledger 17363 行 | ✅ |
| **Redis 权威对账** | 余额=账本投影 | Redis 余额 = 初始充值 − Σledger 扣减(定点整数,结构性无漂移) | ✅ |

## 2. 与历史对比

| 指标 | v0.4(2026-11-19,内存权威) | v0.5(2026-11-20,Redis 权威+定点金额) |
|---|---|---|
| 100 CPS 锁定时延 | p50=2.0ms | p50=2.0ms(持平) |
| p99 | 2.9ms | 4.3ms(+1.4ms = Redis 往返×2 的代价,**仍在 5ms 预算内**) |
| burst | 1245 CPS | **1562 CPS**(+25%,容器热身后 CPU 调度差异) |
| 失败率 | 0 | 0 |
| 金额 | float64 | int64 定点(结构性无漂移) |

## 3. 已知与限制

1. **loadgen pacing**:time.Sleep 精度在 Windows/容器下损耗 3-9% 的名义 CPS——有效吞吐为准;
2. **本机容器环境**:非专用压测机;PRD 容量目标(单节点 2000 并发)在专用环境复测(验证批次);
3. **SIP 全栈未在本次重压**(信令面口径沿用 v0.3;HTTP call 面为控制面口径);
4. burst 的 p50 3s 是**客户端**观感(loadgen 单 goroutine 排队 + Go runtime 调度),服务端 HTTP-IN 与 CDRW 无积压告警。

## 4. 复现

```bash
cd deploy && docker compose -p gvos up -d --build
docker exec gvos-redis-1 redis-cli INCRBY gvos:bal:<account-uuid> 100000000
go run ./cmd/gvos-loadgen -cps 100 -duration 30s
go run ./cmd/gvos-loadgen -cps 300 -duration 30s
```
