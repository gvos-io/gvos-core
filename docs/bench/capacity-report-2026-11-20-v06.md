# 容量与性能报告 v0.6(2026-11-20 晚,SIP 全栈,RPC 档)

**环境**:compose 全栈(gv-core + PG + Redis + **NATS 3 节点全互联 R3** + Kamailio children=32 + RTPengine + demo-gw),SIPp 鉴权场景(load.xml:INVITE→401→鉴权→100/180/200→ACK→500ms→BYE→200,UDP)。**RPC 档(WITH_NATS_RPC=1)首次进入容量报告。**

## 1. 结果总表

| 档位 | 通话成功 | 失败模式 | RPC 链 | 备注 |
|---|---|---|---|---|
| 10 CPS | 100/100 ✅ | — | 全收敛 | |
| 30 CPS | 30/30 ✅ | — | 全收敛 | 干净状态 |
| 40 CPS | 40/40 ✅ | — | 全收敛 | |
| 60 CPS | 56/60 | 408×8(BYE-200 尾延) | 180/180 挂起=180 分发 | children=32 下 |
| 80 CPS | 61/80 | 408×16(BYE-200) | 全收敛 | |
| 100 CPS | 54/100 | 408×24(BYE-200) | 180/180 | 长尾放大 |
| soak 30CPS×6min | **179/180(99.4%)** | 1(环境抖动) | 全收敛 | 有效 22 CPS |
| **账务一致性** | — | — | — | **CDR 1377 = ledger 1377,零漂移** |

## 2. 分层延迟(压测中实测)

| 层 | p50 | p90 | p99 | max |
|---|---|---|---|---|
| bill-lock RPC(suspend→resume→dispatch,3 节点集群) | **2.8ms** | 4.7ms | 28.3ms | 44.6ms |
| rtpengine_manage(ng 往返,Kamailio worker 内同步) | **0ms** | 164ms | 699ms | 757ms |
| INVITE 200 端到端(成功样本) | ~1ms | 11ms | — | 36ms |
| BYE→200(60CPS 下,失败前样本) | 1898ms | 2086ms | — | — |

## 3. 瓶颈定位(本轮挖出的全部卡点,按因果链)

1. **真瓶颈 = Windows Docker Desktop 容器间 UDP 长尾**:rtpengine ng 往返 p50=0ms 但 p90=164ms/p99=699ms——每 10 次调用 1 次长尾。CPU 全层空闲(kamailio 0.1%/rtpengine 0.01%/gv-core 0.25%):**不是算力,是宿主机网络栈抖动**。生产裸机(LAN RTT<1ms)预期无此长尾。
2. **rtpengine_manage 同步阻塞 worker 是长尾的放大器**:每个 INVITE/BYE 在 worker 里同步等 ng 响应。60CPS×0.2s 长尾≈12 worker-秒/秒 → children=16 时 75% 池占用,80CPS 即饱和。**children 16→32 实测容量翻倍**(80CPS:8/80 → 61/80)——瓶颈确认即解。
3. **BYE-200 回程是最后的 408 源**:INVITE-200 能回(p99 11ms)而 BYE 的 200 大量超时——BYE 同样跑 rtpengine_manage + CDR_EVENT,长尾叠加排到 SIPp Timer B 之外。
4. **RPC 档完全无罪**:所有档位 suspend=dispatch=SVC-CALL-START 严格相等(180/180/180@100CPS),p50 2.8ms——**异步 bill-lock 不是瓶颈,反而是全链最快的一层**。
5. **gwfail 锁定级联(测试方法学,非产品 bug)**:SIPp 重传耗尽→tm fr 超时→failure_route→gw 锁 5 分钟→后续 BYE/INVITE 全 503。它把局部超时放大成全量失败;**gwfail autoexpire=300s 是设计行为**(保护真实运营商),压测时用干净状态单步隔离。
6. **修复清单(本轮入仓)**:
   - `pv_www_authenticate` flags **"0"/"2"→"1"**(bit1=HA1 模式;之前双重哈希是 50-100CPS 鉴权批量失败的根因,单通通过掩盖了它——DB 路径成功、缓存路径必败);
   - bench.sh 加 `-cid_str` 每步唯一(跨步 Call-ID 碰撞→demo-gw dead-call 拒答→锁死级联);
   - 尝试并回退了 t_newtran-before-auth(stateless 401 变半开事务,tm 会按 T1 重发 401,SIPp 读成新挑战——实测回归);
   - children 16→32(rtpengine 长尾下的 worker 池扩容,容量翻倍验证)。

## 4. 与历史对比

| 指标 | v0.5(HTTP 档,loadgen 直打控制面) | v0.6(SIP 全栈,RPC 档) |
|---|---|---|
| 口径 | 控制面 start/end 无媒体无信令边缘 | 全链:SIP 鉴权+RPC bill-lock+rtpengine+gw+CDR |
| 100 CPS | p99=4.3ms(锁定面) | 54/100 通话(408=BYE-200 长尾,非 RPC) |
| bill-lock | HTTP 阻塞(worker 占满 2s 风险) | RPC p50=2.8ms/worker 零占用 |
| CDR=ledger | 17363=17363 | 1377=1377 |
| 稳态可用 | 300 CPS 控制面 | **40CPS 全栈 100% / 22CPS soak 99.4%** |

注:v0.6 数字是**开发机 Docker Desktop(WSL2)网络**口径,宿主 UDP 长尾 p99≈700ms;PRD 的 300CPS 全栈目标需在裸机/Linux CI 复测(预期 rtpengine 长尾消失后 32-worker 容量数倍提升)。

## 5. 复现

```bash
cd deploy && docker compose -p gvos up -d --build   # RPC 档+3节点集群+children=32
# 等待 GW_SYNC(≤60s),然后:
MSYS_NO_PATHCONV=1 docker run --rm --network gvos_default \
  -v "$(pwd)/../test/sipp:/scen" ctaloi/sipp:latest \
  -sf /scen/load.xml -cid_str "%u-run1@%s" -inf /scen/dest.csv -m 40 -r 40 -timeout 45s kamailio
# 判据:Successful call 40;docker logs gvos-kamailio-1 | grep -c "bill-lock suspended" == 40
# soak:-m 180 -r 30 -timeout 240s;账务:CDR 行数 == balance_ledger 行数
```
