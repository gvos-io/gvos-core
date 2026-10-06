# 全球市场调研：VoIP 软交换 / 批发话务 / CPaaS / AI 语音代理

（原始调研输出存档，调研时间 2026-10。方法说明：调研代理在无专用搜索工具的环境下直接抓取一手来源（TeleGeography、厂商官网、财经数据聚合器）。所有数字均对过活页面；未能核验的已明确标注。）

## 1. 批发/运营商话务市场（VOS 的祖传市场）

- 批发商 2024 年落地约 **2,170 亿分钟，同比 -8%**；批发话务量过去十年平均年降 1.5%；国际话务整体年降约 6%。批发商仍承担全部国际话务的 71%（2024）。来源：TeleGeography（2026-02-06）https://resources.telegeography.com/how-do-wholesale-carriers-fit-in-the-global-traffic-picture
- 前值基线：2023 年约 2,360 亿分钟（-8%）；年度国际话务降幅：2019 -6.2%、2020 -7.2%、2021 -6.1%、2022 -5.8%、2023 -7.8%。国际话务峰值在 2014 年。来源：TeleGeography（2025-02-03）https://resources.telegeography.com/traffic-talk-whats-the-difference-between-wholesale-retail-and-direct
- OTT 替代：OTT 应用（WhatsApp/Messenger/WeChat/QQ/Viber/Line/KakaoTalk 合计约 6 亿月活用户，2024-09）2016 年已超过运营商国际话务量；2024 年估算 **OTT ~1.3 万亿分钟 vs 运营商 ~3,050 亿分钟**。来源：TeleGeography（2025-01-09）https://resources.telegeography.com/what-is-ott-traffic
- 收入：批发话务收入**十年累计 -25%**；呼叫移动落地收入约 $101 亿（十年基本持平）；前 8 家批发商承载 41% 全球流量（约 1,250 亿分钟），2024 年仅 BTS 一家增长；零售国际通话收入从 2014 年 $960 亿下滑；国际长途占运营商总收入从 2014 年 7.1% 降至 2023 年 3.1%。
- 每分钟价格：方向性事实——收入降幅（-25%/十年）快于分钟数降幅（约 -15%/十年），即每分钟价格持续侵蚀。付费报告（International Voice Report）https://www2.telegeography.com/telegeography-report-and-database
- ⚠️ 未核验：CIT/Analysys Mason/GlobalData 的"批发话务市场 $X B"总盘子数字（站点不可达）。

**结论：经典 A-Z 批发是量价齐缩、集中度提升的残杀市场。**

## 2. SBC/软交换设备市场

- 企业级 SBC 市场：**2025 年约 $19 亿 → 2032 年约 $58 亿，CAGR ~17.5%**。来源：MarketsandMarkets https://www.marketsandmarkets.com/Market-Reports/session-border-controller-market-163929246.html ⚠️ 偏乐观，需对照厂商实收：
  - Ribbon（RBBN）：收入 $844.96M（2021）→ $844.56M（2025）零增长；TTM（2026-06）$797.64M（-7.6%）；Cloud & Edge 板块从 $556.66M（2021）降至 $511.43M（2025）。https://stockanalysis.com/stocks/rbbn/financials/
  - AudioCodes（AUDC）：$275.09M（2022）→ $242.18M（2024）→ $245.6M（2025）。https://stockanalysis.com/stocks/audc/financials/
- Ribbon 产品线印证云原生+合规化趋势：Cloud-Native Core SBC、SBC SWe/CNe、Call Trust/STI-CA（STIR/SHAKEN 证书授权已成商品化产品层）。
- ⚠️ 未核验：任何当期"VoIP 软交换市场"总规模数字（Grand View 页面处于"开发中"，Mordor/Precedence 404）。

## 3. CPaaS 市场

- Twilio：FY2023 $4,154M → FY2024 $4,458M（+7.3%）→ FY2025 $5,067M（+13.7%）；TTM（2026-06）$5,572M（+17.8%）；2026 指引 18-18.5%；市值 ~$45B；2026-10-06 进 S&P 500。Gartner CPaaS MQ 连续 4 年领导者。https://stockanalysis.com/stocks/twlo/financials/
- Bandwidth：FY2023 $601.1M → FY2024 $748.5M（+24.5%）→ FY2025 $753.8M；TTM $828.2M（+9.3%）。https://stockanalysis.com/stocks/band/financials/
- Infobip：3,500+ 员工、190+ 国家、Gartner/Juniper/Omdia 多榜 Leader。https://www.infobip.com/company
- ⚠️ 未核验：CPaaS 市场总规模（各研报站不可达）；Sinch/Vonage 2025 收入。

## 4. AI 语音代理爆发（最强增长信号）

- Vapi：$50M B 轮（2026-05，总 $72M，Peak XV 领投）；企业收入年增 10 倍；累计 10 亿通电话；100 万开发者；TAM 叙事"每年 3 万亿分钟 B2C 电话"。客户：Amazon Ring 100% 入呼跑在 Vapi 上。https://vapi.ai/blog/series-b
- Bland AI：$50M C 轮（2026-06，总融资 >$100M）；**每周 350 万通电话**；累计约 7.06 亿通；FedRAMP。https://www.bland.ai/blog/series-c
- Retell AI：种子 $4.6M；上线约 6 个月年化收入 $3M+；定价 $0.07-0.31/分钟；原生 SIP 中继对接。https://www.retellai.com/
- ElevenLabs Conversational AI：500 万+代理、90+ 语言；官方 FAQ 明说对接"Twilio、Genesys、Vonage、Telnyx、Plivo 或任意 SIP 兼容 PBX"。https://elevenlabs.io/conversational-ai
- Twilio SIGNAL 2026：发布 agentic 平台（Conversation Memory/Orchestrator/Agent Connect 开源桥/Conversation Relay）。https://www.stocktitan.net/news/TWLO/
- **基础设施需求 = 软交换功能清单**（来自 Vapi 文档 https://docs.vapi.ai/llms.txt）：SIP 中继+allowlist、BYO 运营商导入（Twilio/Telnyx/DIDWW/Plivo/Chime 等）、WebSocket 双向媒体流、实时呼叫控制、监听、录音。
- 欺诈新面：2026-08 Twilio 官方博客记录的 AI 语音 demo 遭 traffic-pumping 攻击：4 小时 921 通/$982（折合 $0.46/分钟以色列高费率号段）。https://www.twilio.com/en-us/blog/developers/best-practices/secure-voice-agent-demos
- ⚠️ 相邻市场数字（非话务分钟）：AI 语音生成市场 2025 $4.16B → 2031 $20.71B（MarketsandMarkets）。OpenAI Realtime API 定价页未能核验。

## 5. 监管驱动

- STIR/SHAKEN（美国）：TRACED Act 框架；Ribbon 把 STI-CA（证书授权）做成产品线是"合规可商品化"的直接证据。⚠️ fcc.gov 全程 403，FCC 对小运营商/网关商的具体截止日、AI 语音披露规则生效日未能重新核验。
- EU：⚠️ 未能核验欧盟级 STIR/SHAKEN 等效强制要求。

## 6. 技术趋势

- Kamailio：v6.1.4（2026-08-20）；"数千 CPS"；用例含 SIP 负载均衡/LCR/SBC/WebRTC/IMS；用户含 sipgate、Airbnb、DIDX、OV500（开源批发计费+交换栈，Kamailio+RTPProxy+FreeSWITCH——VOS 级开源对标物）。https://www.kamailio.org/w/
- FreeSWITCH：20 年、"每年数十亿次呼叫"，由 SignalWire（作者创办）维护，收费层 FreeSWITCH Advantage + 云平台——开源电信 SaaS 化的样板。https://signalwire.com/freeswitch
- OpenSIPS：4.0.2（大版本 4.0，2026-06-23，TCP/TLS 重设计）；"数万 CPS、百万级并发"；120+ 模块；定位含 Class 4/批发中继/SBC。https://www.opensips.org/
- 云原生：Ribbon 的 Cloud-Native Core SBC（无硬件）+ SBC CNe；Twilio/Telnyx API-first 是同一趋势的消费端。⚠️ CNF 采纳率统计未能核验。

## 对新进入者的含义（调研代理结论，供主报告引用）

1. 不要把传统 A-Z 批发当增长故事（量价齐缩）。
2. 增长引擎是 AI 语音代理，而其基础设施需求清单恰是软交换功能清单。
3. SBC 市场的增长在云原生软件新玩家，不在老设备商。
4. 计费+防欺诈+合规是差异化，交换本身不是。
5. 开源既是威胁也是技术栈；差异化在控制平面。
6. CPaaS 层健康增长（Twilio 再加速至 18%）。
