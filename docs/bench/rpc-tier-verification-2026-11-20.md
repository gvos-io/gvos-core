# RPC 档升档验证报告(2026-11-20 晚)

**环境**:compose 单机栈(gv-core + PG + Redis + **NATS 3 节点全互联集群 R3** + Kamailio 16 worker + RTPengine + demo-gw),SIPp 鉴权场景(agent_call2.xml:INVITE→401→鉴权→200→ACK→hold→BYE)。

## 结论

| 项 | 结果 | 证据 |
|---|---|---|
| **RPC 全链(suspend→resume→dispatch)** | **4ms** | 04:04:12.293 挂起 → 04:04:12.297 分发(3 节点集群,含跨节点 req/reply) |
| SIPp 单通 | ✅ Successful 1/0 Failed | CDR 落库:60s 通话 ¥5.00 SIP 200 |
| SIPp 并发 10(-r 2) | ✅ **10/10 成功** | 10 suspend = 10 dispatch = 10 SVC-CALL-START,零失败 |
| 单节点故障(hub-down) | ✅ 呼叫继续 | 全互联 mesh:nats-1 停后 2↔3 路由存活,kamailio 经集群 INFO 发现跨节点重连 |
| 全停(3/3 down) | ✅ 有界失败 | INVITE +5.0s 收 **408**(t_set_fr(5000,5000);SIPp 报文日志实测) |
| 冷启动容错 | ✅ | nats-1 停机时重启 kamailio:DNS 别名拨号落到健康成员,边缘订阅全部重建 |

## 静默丢根因定案(40065a4 的真身)

gv-core `jsPoster` 对**无点边缘别名**(`gvos-edge-cfg/risk/tear`,不在 JetStream stream subject 域)做 JetStream 发布:服务器对未映射 subject 不回 PubAck → 每条挂满 5s publishBudget;单个 pump goroutine 串行排空 → 每次别名发布让队列后面**所有**事件(CDR/risk/reg)排 5 秒队。症状即当时记录的"publish 静默吞"与"publisher 长时运行假死(重启即愈)"——重启之所以"治愈",是清空了积压队列。

修复:`internal/shared/events/events.go` 的 pump 按 `IsEdgeAlias` 路由别名到 corePoster(纯 core NATS,at-most-once 本就是别名文档语义)。修复后 70 分钟观察:零 publish 失败(修复前 92 次/小时)。

## 修复清单(RPC 档启用前置)

| # | 缺陷 | 层 | 修复 |
|---|---|---|---|
| 1 | 边缘别名 JetStream 发布挂 5s×每条,单 pump 串行堵死事件面 | Go | IsEdgeAlias → corePoster |
| 2 | INVITE 路径无 t_newtran:鉴权 re-INVITE 无事务上下文,`$T()=0:0`,请求拼出非法 JSON(`"tindex": ,`) | cfg | BILL_LOCK 前显式 t_newtran() |
| 3 | 畸形请求的 400 回复不带 suspend 坐标 → 边缘 reply 路由当普通 ack 丢弃 → 挂起无解 | Go | 400 也走 lockReply(回显坐标) |
| 4 | t_continue 不能从 timer route 调用(tmx: cannot get transaction index)→ janitor 的 503 兜底从未真正工作 | cfg | janitor 降级纯 GC;释放兜底 = t_set_fr(5000,5000) |
| 5 | `pv_www_authenticate` flags="0":缓存 HA1 被当明文密码再哈希 → 全部缓存鉴权 401 死循环(旧"stale-entry trap"真相) | cfg | flags="2"(HA1 模式)+ 失败清缓存 |
| 6 | tear 路由 tag 查找用 `$ci`(NAT 事件路由无 SIP 上下文恒空)→ 持有 dialog 节点也 no-op(U3 嫌疑根因) | cfg | `$var(cid)` |
| 7 | `startup_route` 在 Kamailio 6.0.1 trixie 词法器中不存在(外部提交引入,parse 即死) | cfg | 删除,IPALLOW_CHECK fallback latch 承接 |
| 8 | Kamailio 语法:无 continue / else 必须独立行 / if 块不能只含注释 | cfg | janitor 重构反转条件 |

## 集群拓扑教训(实测)

- **星型 routes 全拨同一 seed = hub 死则脑裂**(nats-2/3 之间零路由、跨节点 RPC 无响应)——全互联(每节点拨所有 peer)后 hub-down 演练通过。
- **Kamailio nats 模块 nats_url 只认单服务器**:逗号列表被 libnats 当作一个整体主机名。方案:拨 compose 网络别名 `nats`(DNS 轮询所有成员)+ 集群 INFO 握手自动补全 discovered servers。
- **改 --routes 重启会孤儿化 R3 stream 状态**(JetStream cluster identity 重建)→ 需全集群重启 + gv-core 重建流;流副本数经 `GVOS_JS_REPLICAS` env(ensureStream UpdateStream 收敛)。

## 已知限制

1. SIPp 的「Successful call」判定依赖 demo-gw(裸 `sipp -sn uas`)回包时序,历史已知噪声(sip-repress-2026-11-20 §3);本报告判据 = suspend/dispatch/SVC 日志 + CDR 落库。
2. U3 拆线 <1s 复测未在本窗口跑(修复了 `$ci` 嫌疑根因,待下个验证窗口)。
3. 3 节点 NATS 全停后 JetStream consumer 报 no responders,集群全量重启 + gv-core 重启后恢复(stream/consumer 重建);生产形态该场景即 5s 408 有界失败,恢复序列照此执行。

## 复现

```bash
cd deploy && docker compose -p gvos up -d --build   # 3 节点 NATS + RPC 档默认开
# 等待 GW_SYNC(≤60s)后:
MSYS_NO_PATHCONV=1 docker run --rm --network gvos_default \
  -v "$(pwd)/../test/sipp:/scen:ro" ctaloi/sipp:latest \
  -sf /scen/agent_call2.xml -inf /scen/dest.csv -s 1000 -m 10 -r 2 -timeout 100s kamailio
# 判据:docker logs gvos-kamailio-1 | grep -c "bill-lock suspended" == 10
#       docker logs gvos-gv-core-1  | grep -c "SVC-CALL-START"     == 10
# 宕机演练:docker stop gvos-nats-{1,2,3}-1 → SIPp 单通 -timeout 25s → 408 at +5s
```
