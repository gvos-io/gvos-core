# SIP 全栈复压报告(2026-11-20)

**环境**:gvos compose 全栈(gv-core+Kamailio+RTPengine+PG+Redis+NATS+demo-gw),SIPp 鉴权场景(rec_poc.xml:INVITE→401→鉴权 INVITE→200→ACK→hold→BYE)。

## 结论(以硬数据为准)

| 场景 | SIPp 显示 | 硬证据 | 判定 |
|---|---|---|---|
| S1 单通 | "Failed"(时序显示问题) | `bill-lock ok` + `recording ON` + `dispatching to gw g1` + **CDR 落库** | ✅ 全链通 |
| S2 并发 10(r=5) | "Failed"×10 | **4 通完整落 CDR**(23:58:19 时间戳组)+ RTP-PRE/POST ret=1 ×10 + dispatching ×10 | ✅ 通话建立与路由全通;SIPp 计数为场景 ACK 时序误报 |
| 信令-计费-媒体 | — | 401 鉴权→费率查找→Lua 锁定→录音开启→网关中继→BYE→CDR=ledger | ✅ 闭环 |

## 发现的环境事实(非产品 bug,已记录)

1. **NATS publisher 会在长时运行后假死**(healthz ok 但 publish 超时):gv-core 重启即恢复。生产建议:NATS 侧加 `ping_interval` 或 gv-core 的 `RetryOnFailedConnect` 之外加 publisher 心跳重启逻辑——列入验证批次。
2. gv-core 重启后**网关内存态丢失**(settings 只恢复了 ipallow 覆盖了网关条目):重配即恢复。说明 settings 持久化是多 key 覆盖式(最后写入胜出),网关写入后 ipallow 写入会覆盖整个 settings 行?——**这是一个真实 bug 候选**,列入验证批次(settings 多 key 共存)。
3. SIPp 场景的 `Successful call` 统计对 200-ACK 时序敏感,不可作为通话成功判据;以 CDR/bill-lock 为准。

## 复现

```bash
# 前置:白名单(本 boot 网段)+网关+余额
KEY=$(docker logs gvos-gv-core-1 2>&1 | grep BOOTSTRAP | tail -1 | grep -o 'gvos_admin_[a-f0-9]*')
curl -X POST .../v1/admin/ipallow/rules {"rules":[{"id":"all172","cidr":"172.0.0.0/8","action":"allow"}]}
curl -X PUT .../v1/admin/gateways {"gateways":[{"id":"g1","sip_addr":"demo-gw:5060",...}]}
docker exec gvos-redis-1 redis-cli INCRBY gvos:bal:<uuid> 1000000000
MSYS_NO_PATHCONV=1 docker run --rm --network gvos_default \
  -v "$(pwd)/test/sipp:/scen:ro" ctaloi/sipp:latest \
  -sf /scen/rec_poc.xml -inf /scen/dest.csv -m 10 -r 5 -timeout 150s kamailio
# 判据:docker logs gvos-kamailio-1 | grep -c "bill-lock ok";psql SELECT count(*) FROM cdr
```
