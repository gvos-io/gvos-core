# 国际竞品调研：批发 VoIP 软交换 + 计费平台

（原始调研输出存档，调研时间 2026-10。方法：WebFetch 直抓厂商官网/GitHub/CSDN/新闻稿。未能核验处已标 [unverified]。）

## 逐厂商报告

### 1. Sippy Softswitch（加拿大 Sippy Software）——国际上与 VOS 最直接的对标

- 产品：Class 4+5 软交换（OpenSIPS + Asterisk，FreeBSD），RTP Media Gateway，FreightSwitch 集群，白标；模块含电话卡/回拨/DNC/STIR-SHAKEN/录音/云备份。
- 定位：MVNO、固网运营商、批发商、呼叫中心、零售/企业 SIP 提供商——明确瞄准新进入者。
- **公开定价**：AWS 托管 50 并发/15 CPS **$200/月**；100 并发/15 CPS $431/月；Flex 租用（自有服务器）2000 并发/75 CPS $1,280/月；**永久授权 2000 并发/75 CPS $18,500**（含 12 个月标准支持）；专业支持 $3,600/年；装机费 $500-1,000。
- 规模：全球部署合计约 900 亿分钟/年、100+ 国家、2005 年创立。https://www.sippysoft.com/pricing
- 状态：活跃且扩张——低价档位直接攻 VOS 的低端市场。

### 2. PortaOne PortaSwitch（加拿大）

- 产品：PortaSwitch（语音/消息/IoT/数据）+ PortaBilling（BSS/OCS），开放 API、Add-on Mart 模块市场。
- 定位：ITSP/运营商/MVNO/云 PBX/UCaaS——比 VOS 典型买家高一档。
- 定价：三种模式（永久/SaaS 自托管/PortaCloud）；**公开起步价：首年最低约 $150,000**（含专业服务）；按计费事件/活跃客户/SIM 计价。https://www.portaone.com/pricing
- 客户：2001 年以来 500+ 服务商、100+ 国家。状态：成熟稳定的老牌领导者。

### 3. 其他批发软交换厂商

- **Telcob：已死亡**（域名停泊在域名售卖页）。→ 该赛段小厂商会死的数据点。
- **CommPeak Switch**：[unverified]——公司转向云联络中心/拨号器/软电话/SIP 中继服务。
- **JeraSoft VCS**：计费+路由平台（不带软交换），面向批发 VoIP/SMS；v3.28（2026）；**集成名单里就有 VOS3000**（把 VOS 当作可对接的交换机之一）。定价不公开。https://www.jerasoft.net
- **Telcobridges**（加拿大，网关+ProSBC）：站点本次不可达 [unverified]。

### 4. 2600Hz Kazoo（现 Ooma 子公司）

- 开源分布式运营商平台（Erlang + FreeSWITCH + Kamailio + CouchDB），MPL 许可，社区版免费自建（GitHub ~990 星）。
- 商业版做白标 UCaaS/CPaaS（300+ API、市场应用），**不做批发 A-Z 计费**。
- **2023-10 被 Ooma 以约 $33M 现金收购**（约 $7M ARR——开源运营商平台的低退出倍数）。https://www.ooma.com/press-release/ooma-acquires-2600hz-inc-a-leader-in-communications-applications-for-service-providers/

### 5. 开源栈及其商业臂

- Kamailio（GPLv2+，"数千 CPS"，v6.1.4 活跃）：无官方商业发行版，靠第三方商业目录支持。
- OpenSIPS（GPL，"数万 CPS"）：**Sippy 本身就是 OpenSIPS 商业发行版**；还提供免费 Community Edition VoIP 平台包（含 SBC CE）。
- FreeSWITCH/SignalWire：SignalWire 已从"商业 FreeSWITCH 支持"转型为 Twilio 兼容 CPaaS + AI 语音（~27 亿交互/年、2,700+ 客户），**实质上退出了批发软交换市场**。https://signalwire.com/about
- 结构性结论：**在 VOS 价位上，全球只有 Sippy 一家卖成品批发软交换；开源栈上没有出现规模化交钥匙批发计费平台**——这是空位。

### 6. Brekeke / NetSapiens / Alepo

- Brekeke SIP Server：SIP 注册/代理组件，非计费软交换；Standard $500、Advanced $1,500（HA/TLS）、Premium（运营商，报价制）。https://www.brekeke.com/sip/
- NetSapiens（Crexendo CXDO 子公司）：白标 UCaaS 平台（8M 用户、超售许可模式），零售 UCaaS，非批发。https://www.netsapiens.com
- Alepo（得州）：数字 BSS（MVNO/运营商计费层，对标 PortaBilling 而非 VOS）。https://alepo.com

### 7. Ribbon / Metaswitch——传统巨头的退场

- Metaswitch：2020 被 Microsoft 收购（报 $270M）；Azure-for-Operators 战略失败；2024-06 裁员至多 1,500 人并宣布大批产品 EOL；**2025-03 转卖给 Alianza**。Alianza+Metaswitch 合计 1,000+ CSP、全球前 20 运营商中的 19 家。
- Ribbon（RBBN，2025 收入 $845M）：吸收 Metaswitch 流失需求（"唯一提供全面 Metaswitch 替代方案的厂商"，宣称迁移 55.2M 端口），但价格与形态面向 Tier1-3 运营商——**给中低价位 Class4 平台留下价格伞**。
- 意义：被 Metaswitch EOL 搁浅的 Tier2/3 运营商是活的获客机会。

### 8. 云平台/聚合商（改变"自建 vs 购买"的决策）

- Twilio SIP Interface/BYOC：$0.004/分钟（每方向）；自助 CPS 上限 30。https://www.twilio.com/en-us/voice/pricing
- Telnyx（持牌运营商）：出 $0.005/分钟、入 $0.0032/分钟；自营网络+PoP；机器自助销售（pricing.json、MCP server）。https://telnyx.com
- Bandwidth：FY2025 $753.8M，FY2026 指引 $900-910M（AI 需求拉动）。
- DIDWW（90+ 国家 DID+SIP 中继，REST API）、Voxbeam（200+ 国家 A-Z 自助批发）——小话务商"第一个批发账户"。
- Sangoma（VoIP Innovations 并入其 Carrier Services）：**2026-09-28 同意被 BRC Group 收购（~$204M EV）**——低端整合中。https://stockanalysis.com/quote/tsx/STC/
- 结论：**今天 2 人小批发商可以零基础设施开工**（Telnyx 终止+DIDWW 号码+JeraSoft 计费）；自建软开关的临界点约为"3+ 运营商关系且 >10-20M 分钟/月"[分析推断]。这从下方抽走 VOS 的历史入门客群。

### 9. VOS3000 破解版生态

- 被破解版本：2.1.1.5 / 2.1.2.0（最流传）/ 2.1.2.4 / 2.1.4.0 / 2.1.6.0 / 2.1.7.x / **2.1.8.05（GitHub freevos/vos3000-2.1.8.05，在线注册机 freevos.net）**，"2.0-6.0 全版本"打包；2024-2026 年 CSDN 仍有新帖教注册机用法。
- 渠道：CSDN 下载（积分下载）、GitHub/Gitee、吾爱破解（52pojie.cn）。
- **freevos.net 公然分发"运营级 VoIP 计费"VOS3000 2.1.8.05**（宣称 10,000 并发、预激活）；vos3000.com 提供 2.1.1.5-2.1.9.07 的 License Generator。
- 中文圈讨论确认大量盗版装机的存在（"盗版用户强烈建议转正"类文章；盗打/后门/定时炸弹告警文）。
- [推断] 破解版实质摧毁了昆石本土及东南亚/中东低端市场的收费能力；昆石产品阶梯（3000→3800→5000+SBC+Web 版）可解读为把价值移向难破解的版本/云端。
- 来源：https://github.com/freevos/vos3000-2.1.8.05 、https://freevos.net 、https://www.52pojie.cn/thread-1628058-1-1.html 、https://blog.csdn.net/vx_3307623172/article/details/146966561

## 对比表

| 厂商/产品 | 类别 | 目标 | 公开容量 | 计费 | HA | 定价模式 | 具体价格 | 状态 |
|---|---|---|---|---|---|---|---|---|
| 昆石 VOS3000/3800/5000 | Class4 软交换+计费 | 批发商（中国+新兴市场） | 6k-100k 并发 | 实时多租户 | 双机热备 | 按并发永久授权 | ¥488k-2.38M | 活跃，低端被盗版摧毁 |
| Sippy | Class4+5 软交换+计费 | 批发/ITSP/新玩家 | 50→2,000+ 并发档 | 集成白标 | FreightSwitch 集群 | 租用/永久 | $200/月→$18,500 | 活跃扩张 |
| PortaOne | BSS+软交换 | ITSP/运营商/MVNO | 未公开 | PortaBilling | 选配收费 | 永久/SaaS/云 | 首年 ~$150k | 成熟稳定 |
| JeraSoft VCS | 计费+路由（无交换） | 批发 VoIP/SMS | 集群版 | 核心产品 | 集群 | License+支持 | 未公开 | 活跃 21 年 |
| 2600Hz Kazoo | 开源运营商/UCaaS | 白标 UCaaS 转售 | 4.1M 设备 | 应用市场 | 内生分布式 | 社区免费/商业报价 | $0 / 报价 | Ooma $33M 收购 |
| Kamailio/OpenSIPS | 开源 SIP 服务器 | 自建者 | 千-万级 CPS | 无 | 自建 | GPL 免费 | $0 | 极活跃 |
| Brekeke | SIP 组件 | OEM/运营商 | 未公开 | 无 | Advanced+ | 按版本永久 | $500/$1,500/报价 | 活跃 |
| NetSapiens | 白标 UCaaS | SP/MSP | 8M 用户 | 含 | 云/混合 | 订阅/永久 | 未公开 | 增长 |
| Alepo | 数字 BSS | MVNO/运营商 | 百万级用户 | 核心产品 | 99.999% | 报价 | 未公开 | 活跃 |
| Ribbon | SBC/呼叫控制/IMS | Tier1-3 运营商 | 运营商级 | 无 | 运营商级 | 永久+服务 | 报价 | $845M 停滞 |
| Metaswitch→Alianza | 遗留 Class4/5+IMS | 固网运营商 | 运营商级 | 无 | 运营商级 | 遗留 | — | EOL 转卖 |
| Twilio/Telnyx | CPaaS 云 | 开发者/企业 | 30 CPS 自助 | 用量 | 云 | 按分钟 | $0.004-0.005/min | 高增长 |
| DIDWW/Voxbeam/Sangoma | 聚合商 | 小批发商 | — | 预付费门户 | 云 | 按分钟/通道 | 用量 | 活跃/整合中 |
| Telcob | 批发软交换 | — | — | — | — | — | — | **死亡** |

## 竞争对手的缺口（调研代理结论）

1. 中国以外"便宜+正版+有支持"的批发软交换档位只有 Sippy 一家；PortaOne $150k 起步与 Sippy $18.5k 之间存在 $30k-100k 透明定价空位。
2. 计费质量分化：开源栈不带计费（要再买 JeraSoft 再集成一次）；origin-based 计费、防欺诈、秒级计费+对账是开源不给的桌面筹码。
3. 盗版陷阱：低端必须用 SaaS/云 License 防盗（Sippy $200/月档实质防盗版）。
4. 在位者退场/整合（Metaswitch EOL、Sangoma 被购、Kazoo 贱卖）搁浅了 Tier2/3 用户，缺少中价迁移路径。
5. 云聚合商抽走底部客群但吃不掉中部（多运营商 LCR+自有价表+多租户转售仍是软交换领地）。
6. HA 交付参差：$100k 以下无人公布 RTO/RPO 或故障演练结果——可验证的 HA 是无人认领的差异化。
7. 合规是最新楔子（STIR/SHAKEN、防欺诈、RCD）：开源栈最弱处。
