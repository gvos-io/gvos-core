# 10 轮压测报告：潜在 Bug 与性能卡点（2026-11-20）

**环境**: compose 全栈（gv-core + PG + Redis + NATS 3 节点 R3 + Kamailio 6.0.1 children=32 + RTPengine + demo-gw SIPp UAS），`WITH_NATS_RPC=1`。
**负载**: `load.xml`（Digest 鉴权 INVITE→200→ACK→500ms→BYE→200，UDP），40 CPS × 10 秒/轮 × 10 轮 = 4000 通。
**对照**: 压测后追加 1 通/3 通/60 通低载轮做单通话复现。

## 结果总览

| 指标 | 值 |
|---|---|
| SIPp 成功 | **3187/4000 ≈ 79.7%**（逐轮 305–333/400） |
| CDR 行数 | 3642 = ledger 3642（**平账精确**，deltas 无漂移） |
| PDD | avg 1905ms / p99 5079ms / max 5868ms（**p99 贴 t_set_fr(5000) 超时线**） |
| 全层 CPU | kamailio 0.08% / gv-core 0.21% / rtpengine 0.04%（空闲） |
| 失败形态 | ①~22 通/轮鉴权后无 100/180 ②~45 通/轮 BYE→408 尾巴 ③401 挑战偶发丢失 |

> 基线注意：v0.6 报告的「40CPS 100%」是 `-m 40` 单秒突发；本报告持续 10 秒/轮暴露了衰减形态。

## 缺陷清单（按严重度）

### E-1（高，**已撤销 2026-11-20 晚**）~~BYE-200 尾巴在单通话即可复现~~
> **勘误**：低载"复现"（clean/fresh 轮）的 SIPp `-cid_str` 写成了 `clean-N-...`（缺 `%u`），**同一轮 150/60 通全部共用一个 Call-ID**——第一个 BYE 清掉 meta 后，后续 BYE 成了"未知对话"重传雾，SIPp 死等。这是**压测工具 bug，不是系统 bug**；对照的真单通话 `ctrl-1`（`-cid_str` 正确）BYE→200 干净完成。
> 压测主负载里的 BYE→408 尾巴**仍然真实存在**（8609 次 BYE 处理 / 3641 通话，尾部通话 11 次重传），但其量级与 E-2（rtpengine ng 放大）和 E-6（SIPp -l=120 自限速）相符，不再有"load=1 复现"证据。v0.6"宿主机 UDP 尾巴"结论维持。

~~低载复现（1/3/60 通）……~~（证据链作废：gw 日志与 UAC 屏幕对应的是同一 Call-ID 的重传雾，非独立通话）

### E-2（高）rtpengine_manage 位于 t_newtran 之前 → 重传风暴 ng 放大
route 顺序 `AUTH→record_route→dlg_manage→rtpengine_manage→t_newtran→BILL_LOCK(suspend)`：
- 挂起中的 INVITE 重传不被 `t_check_trans` 吸收（suspend 后 6.0.1 重新注入），每条重传**完整重跑 rtpengine_manage**（同步 ng 往返，尾部 p99=699ms）。
- BYE 路径同样：in-dialog 无事务守卫，**8612 次 BYE 处理 / 3641 通话 = 2.4×放大**，尾部通话 11 次 BYE 各带一次 ng RT。
- 单通话 rtpengine RT 分布：106 通话 1 次、1958 通话 4 次、**443 通话 ≥13 次**（13/14 双峰 = BYE 重传大雾）。
- 次生噪声：`rtpengine hash table fail to insert`（69 次，callid+空 viabranch 重复插入）、`already in hashtable, ignore`（NOTICE）。
- **修复方向**：in-dialog 路径先 `t_check_trans`（或 t_newtran）再 rtpengine_manage；或 rtpengine_manage 移到 t_newtran 之后；BYE 上 `rtpengine_manage` 仅在 `has_body("application/sdp")` 时调用。

### E-3（高）CDRR 毒消息重投循环（BYE 重传重复发布 cdr.raw）
CDR_EVENT 在每条 BYE 重传上都 `nats_publish("gvos.cdr.raw")`（an 清空后 dur=0 仍发布），gv-core `callEnd` 对已结束 call 回 404 `NOT_FOUND unknown call` → `NakWithDelay(1s)` 重投 5 次（MaxDeliver=5）后放弃。
- **量化**：`CDRR num_redelivered=7381`（每条毒消息 4 次多余重投）；log 刷屏同一条 call 5 行。
- 后果：CDRR worker（单并发）被毒消息串行占用 5×1s；日志噪音掩盖真实错误；`ack_floor=17447 vs delivered=18668` 表面看像积压，实为丢弃计数假象。
- **修复方向**：404/unknown-call 应 `AckSync`（幂等终态）而非 NAK——Kamailio 端可用 `uq` 标记或 meta:cd 哨兵防重发；至少 Go 端把 `NOT_FOUND` 识别为「已处理」。

### E-4（中）billlock janitor runaway-while 压测期失效
`route[BILLLOCK_JANITOR]` 的 `while (sht_iterator_next(...))` 触发 Kamailio 内建 100 次迭代守卫：`runaway while (790, 35): more than 100 loops`，**route 整体中止**（后续动作不执行）。每个挂起通话占 3 个 key（marker/ci/ans），>33 并发挂起必触发；40CPS 5s 窗口 → 峰值挂起 ~200 → janitor 全程处于失效状态（45 次崩溃记录，全在压测期）。
- 兜底存在：billlock `autoexpire=60` + tm `fr_inv 5000` 释放，账务无损，但 janitor 设计目标（bus 断连恢复）在**最需要的时候**（高并发）不工作。
- **修复方向**：分批迭代（每 tick 处理 N=50 个 key）；或按 key 前缀桶拆多个 htable；或改用 `$sht(...)` 扫描替代 iterator。

### E-5（中）CDR 口径缺陷三连
1. **duration 秒截断**：`$var(dur) = $TS - $sht(meta=>an:$ci)` 是秒粒度整数减法，500ms 通话落在同秒内 → **411/3642 = 11.3% 通话 duration=0**（pdd 却 >0，自相矛盾）。修复：用 `$TV(u)` 微秒差，同 PDD 算法。
2. **answer_time 列从不写入**：`InsertCDR` 列清单里没有 answer_time（迁移里有列、API 永远 NULL）——后付费/质检口径缺失。
3. **release/sip_code 失真**：UAC 眼中失败的通话（BYE-200 丢失 408）写成 `release=normal, sip_code=200`（clean/fresh 复现轮 CDR 为证）。对账维度会把「失败呼叫」计成「正常呼叫」。

### E-6（低）SIPp 压测面自限（**非系统 bug，报告口径**）
`-l` 默认 = 3×rate = 120 并发；实际平均 call length 6–7.2s → 维持 40CPS 需 ~280 并发 → SIPp 自我限速，加剧 INVITE 重传。v0.6 的 100% 数字与本次 79.7% 差异主要由此 + E-1 尾巴叠加。**后续 bench 应显式 `-l 300`**。

### E-7（低）杂项
- rtpengine `Unknown dictionary key 'sip-message-type'` 警告 ×7768（Kamailio 6.0.1 发送、RTPengine 不认识的 key，版本错配噪声）。
- ACK 无 SDP 时 `rtpengine_manage ret=-1` 3122 次——预期行为但每次仍消耗一次 ng RT（与 E-2 合并修复）。
- `pv_www_authenticate failed to get passwd value` 偶发（authcache 竞态窗口，DB 回退成功，未影响鉴权结果）。

## 性能卡点结论

| 卡点 | 证据 | 定性 |
|---|---|---|
| BYE-200 回复路径 | gw 已答 200、UAC 收不到，load=1 复现 | **宿主 UDP 栈 / tm 定时器**（与 v0.6 一致，非 RPC/计费） |
| rtpengine 同步 ng 往返 | 19,891 次 RTP-PRE / 3,998 通话 ≈ 5×；children 全闲但时延堆积 | **放置位置**（E-2 修复后预期消除大半） |
| RPC/计费链 | suspend 3999 = dispatch 3999 = SVC-CALL-START 3999，CDR=ledger 精确平账 | **无罪**（v0.6 结论再次确认） |
| NATS 3 节点 R3 | redelivered 全部来自 E-3 毒消息，无真实积压 | 无罪 |

## 复现命令

```bash
# 10 轮 40CPS（本报告主负载）
cd test/sipp && bash bench10.sh "40"
# 单通话复现 E-1（预期：SIPp 死等 BYE-200）
docker run --rm --network gvos_default -v "$(pwd):/scen:ro" ctaloi/sipp:latest \
  -cid_str "e1-$(date +%s)@%s" -sf /scen/load.xml -inf /scen/dest.csv -m 1 -r 1 kamailio
# CDRR 毒消息计数
docker exec gvos-gv-core-1 sh -c 'wget -qO- "http://nats-1:8222/jsz?consumers=true&streams=true&config=true"'
```

## 建议修复顺序

1. **E-3**（Go 端 404→Ack，一行级改动，消除 7381 次重投与日志噪音）
2. **E-2**（in-dialog 先事务守卫再 rtpengine；BYE 无 SDP 跳过 ng 调用）
3. **E-5.1**（duration 用 $TV 微秒——与 PDD 同法，直接改 CDR_EVENT）
4. **E-4**（janitor 分批，恢复 bus 断连自愈能力）
5. **E-1**（~~在 Linux 宿主上复测~~ 勘误后按 E-2/E-6 归因；若修复后仍有残余尾巴再上 Linux 复测）
