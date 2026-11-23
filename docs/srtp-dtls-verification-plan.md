# SRTP/DTLS 端到端验证规程（2026-11-21 / 2026-11-23 修订）

**当前状态**：rtpengine 26.3.1.4（上游稳定 tag 源码构建）**含 DTLS-SRTP**；Kamailio 6.1.5 `rtpengine_manage` 按 SDP 协商；entrypoint 已加 `--dtls-passive`。缺的是带媒体栈的客户端断言。

## 验证路径 A：脚本化冒烟（配置面 + rtpengine flags）

```bash
# 栈起来后（不要 GVOS_BENCH_MEDIA_OFF）
bash test/srtp/smoke-config.sh
```

断言：rtpengine 进程参数含 `dtls-passive`；`rtpengine-ctl`/`ng` list 可用；Kamailio 模块已加载。

## 验证路径 B：pjsua（SIP+SRTP 客户端，推荐 E2E）

1. 构建 pjsua 容器（Debian bookworm + pjproject，约 30min）——见 `test/srtp/pjsua.Dockerfile`。
2. 场景：pjsua（SRTP=mandatory）→ gvos → SIPp/demo-gw（明文 RTP）  
   rtpengine 做 **SRTP(A-leg) ↔ RTP(B-leg)**。
3. 验证点：
   - pjsua 日志 `SRTP status: Active`
   - rtpengine query 该 call 带 DTLS/SDES
   - CDR duration 合理

```bash
# 构建（一次）
docker build -f test/srtp/pjsua.Dockerfile -t gvos-pjsua test/srtp
# 跑（需 compose 网络 gvos_default，账号 1000 已种子）
bash test/srtp/run-pjsua-srtp.sh
```

## 验证路径 C：浏览器 WebRTC（产品级 DTLS-SRTP）

- WSS :1444 已开；浏览器媒体强制 DTLS-SRTP。
- 产品软电话（sip.js）随 WebRTC 计划交付；本规程不阻塞路径 A/B。

## 配置面核对清单

- [x] rtpengine DTLS-SRTP 编译内置
- [x] kamailio rtpengine_manage 透传协商
- [x] `--dtls-passive`（entrypoint 2026-11-23）
- [x] recording-dir 已配
- [ ] pjsua E2E 在目标环境跑绿（脚本就绪，依赖镜像构建）
- [ ] tshark 断言 A-leg encrypted / B-leg plaintext（可选增强）
