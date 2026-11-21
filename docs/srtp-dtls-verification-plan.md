# SRTP/DTLS 端到端验证规程（2026-11-21）

**当前状态**：rtpengine 10.5.3（bookworm 官方包）**编译时即含 DTLS-SRTP**（ngcp 标准 build）；Kamailio 侧 `rtpengine_manage` 按 SDP offer 自动协商（offer 带 `UDP/TLS/RTP/SAVP` 或 `a=crypto` 即启用）。**配置面零改动即就绪**——缺的是带媒体栈的验证客户端。

## 验证路径 A：pjsua（SIP+SRTP 客户端，推荐）

1. 构建 pjsua 容器（Debian bookworm + pjsua 源码编译，约 30min）：
   - 依赖：build-essential + pjproject 2.14 源码（codeload.github.com/pjsip/pjproject）
   - `./configure --with-openssl && make dep && make && make install`
2. 场景：pjsua（SRTP=mandatory, DTLS-SRTP）→ G-VOS → SIPp gw（明文 RTP）
   - rtpengine 做 SRTP(A-leg) ↔ RTP(B-leg) 的转封装
3. 验证点：
   - pjsua 日志显示 `SRTP status: Active ... DTLS-SRTP`
   - rtpengine `query` 统计中该 call 的 `SDES`/`DTLS` flags 置位
   - 录音 pcap（recording-dir 已配）A-leg 段 payload 为 SRTP（tshark 显示 RTP over encrypted transport）
   - 通话时长计费正确（CDR duration 与 pjsua 通话时间一致）

## 验证路径 B：浏览器 WebRTC（产品级 DTLS-SRTP）

- G-VOS 已有 WSS 端点（1444，自签 TLS）；浏览器 WebRTC 的媒体**强制 DTLS-SRTP**（RFC 8122 已内建于规范）
- 实现：管理台增加 sip.js/WebRTC 软电话页（jssip 3.x 通过 WSS 注册 + 呼叫），RTPengine 收到浏览器 SDP（UDP/TLS/RTP/SAVPF）即 DTLS-SRTP
- 这是产品功能（Web 软电话），规划进 M15/WebRTC 计划，验证随实现交付

## 配置面核对清单（无需改动即就绪）

- [x] rtpengine 10.5.3 DTLS-SRTP 编译内置（bookworm ngcp 包）
- [x] kamailio rtpengine_manage 透传 SDP 协商（无明文-only 强制）
- [x] recording-dir 已配（pcap 验证材料）
- [ ] pjsua/浏览器客户端接入（本规程）
- [ ] tshark 断言脚本（pcap 里 A-leg encrypted / B-leg plaintext）
