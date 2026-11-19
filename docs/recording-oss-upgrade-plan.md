# 后期改进方案：录音 WAV/MP3 化 + OSS 归档 + 坐席/用户分轨

**状态**：规划稿（未实施；含已完成的调研验证结论）
**日期**：2026-11-19
**范围**：录音链路从「pcap 落本地卷」升级为「rtpengine-recording 转码 WAV/MP3 → Go uploader 上传 OSS → presigned URL 下载/调听」，支持坐席/用户分轨。

---

## 0. 现状与差距

当前链路（M0-M8 已交付）：

- 产生：Kamailio `route[BILL_LOCK]` 应答 `record:true` 时调 `start_recording()`（`deploy/kamailio/kamailio.cfg:436-452`）；开关是 gv-core 全局 `recordEnabled`（`POST /v1/admin/record`，PG settings 持久化 + NATS `gvos.cfg.reload` 广播）。
- 存储：rtpengine `--recording-dir /recording --recording-method pcap --recording-format eth`（`deploy/entrypoint.sh:8-10`），DUX pcap 落 Docker 命名卷 `recdata`；CDR 带 `recorded` 布尔列（`internal/store/pg.go:169-175`），文件按 call-id 关联。
- 调听：**无任何产品化出口**。UI 仅有 ● 徽标；调听只能运维进卷拿 pcap 用 Wireshark/tshark 播放。架构文档中「对象存储（录音/导出）」`docs/02-architecture.md:36` 一直是未来规划位。

目标形态：

1. 录音产物为 **WAV 或 MP3**（浏览器/iOS/Android 直接可播，ASR 可直接转写）。
2. 通话结束自动 **上传 OSS**，本地零积压。
3. **坐席、用户分轨**：每通电话产出两个独立音频文件（各说各话分开），可分别调听。
4. CDR/查询闭环：`GET /v1/recording/{call_id}?track=agent|user` → presigned URL → 前端 `<audio>` 播放。

## 1. 调研结论（2026-11-19 已实证）

### 1.1 核心机制：rtpengine 两段式录音管道

```
Kamailio start_recording()              ← 现有链路，不动
   ↓
rtpengine 守护进程（媒体面，已有）
   --recording-dir /spool               ← 现在的 recdata 卷
   产出: DUX pcap + metadata 文件
   ↓
rtpengine-recording 守护进程（解码面）★ 新增
   --spool-dir /spool（inotify 监听）≠ --output-dir /output
   产出: WAV / MP3 成品文件
   ↓
Go uploader（gv-core worker，新增）→ OSS + PG 索引 + NATS 事件
```

关键点：spool 与 output **必须是不同目录**且 spool 所在文件系统需支持 inotify。

### 1.2 rtpengine-recording 能力清单（bookworm 包 10.5.3.5-1+deb12u1，二进制级验证）

| 需求 | 机制 | 包内是否可用 |
|---|---|---|
| WAV/MP3 | `--output-format=wav\|mp3`（默认 PCM WAV）；`--mp3-bitrate=64000`；`--resample-to` | ✅ |
| 按参与者分文件 | `--output-single`（默认开）：每个 RTP SSRC 一个文件；另有 `--output-mixed` 混合轨（默认开，可关） | ✅ |
| 文件命名 | `--output-pattern`：`%c`=call-id（必选）、`%t`=SSRC/mix（必选）、`%r`=随机串、`%l`=参与者 label、`%{key}`=metadata 字段 | ✅ |
| 立体声分轨（单文件） | `--mix-method=channels`（每输入一个声道，最多 4 路） | ✅ |
| **S3/GCS 直传** | master 手册有 `--output-storage=s3`（`--s3-host/--s3-path/--s3-access-key/--s3-secret-key/--s3-region`） | ❌ **发行包没有** |

**S3 直传结论（重要）**：对 bookworm `rtpengine-recording-daemon 10.5.3.5-1+deb12u1` 做了二进制 grep 验证——`--output-storage` 的枚举只有 `file|db|both`（帮助原文：`output-storage Where to store audio streams file|db|both`），二进制中不存在任何 `s3-*`/`gcs` 字符串。S3/GCS 是上游 10.5.3.5 之后新增、Debian 发行包未回移。因此：

- 「OSS 走 S3 兼容端点直传」这条路**不存在**（不是签名兼容问题，是没有后端可配）；
- Go uploader 不是「推荐项」而是**唯一出口**；
- 除非未来切自编译 master 版 rtpengine（需评估 FFmpeg 依赖链与内核模块匹配），直传选项才会重新出现在桌面上。

### 1.3 分轨归属判定（spool 实测）

拿本机运行中 rtpengine 容器 `/recording`（同版本 10.5.3.5）的真实数据验证：

- spool 结构：`pcaps/`（DUX pcap，命名 `<call-id 归一化>-<随机串>.pcap`）+ `metadata/`（`rtpengine-meta-<call-id>-<随机串>.txt`），两目录靠尾部随机串配对。
- metadata 文件内容：pcap 引用 + 每条流的「Timestamp started/terminated + SDP 原文」；SDP 的 `o=` 行与 `c=` IP 可区分腿（坐席侧注册地址 vs 网关侧）。
- Kamailio 侧腿标识：A-leg from-tag（INVITE 发起方）= 平台/坐席侧；B-leg to-tag（经网关的腿）= 被叫用户。`%l` 参与者 label 因 Kamailio 6.0.1 rtpengine 模块 flags 不含 label 参数而不可用，**分轨归属 = uploader 解析 DUX 文件头 tag + metadata SDP 归属映射**。

## 2. 方案选型

### 2.1 分轨：按 SSRC 分文件（方案 A）

| 方案 | 取舍 |
|---|---|
| **A. `--output-single` 按 SSRC 分文件（选定）** | 零开发量拿到 agent/user 两个 MP3；uploader 解析归属后改名归档。业务上「分别调听」最直接 |
| B. `--mix-method=channels` 立体声 | 单文件好管理，但播放时两声道同放，「分离调听」体验差；留为质检场景可选项 |
| C. Kamailio 传 per-leg label（`%l`） | Kamailio 6.0.1 rtpengine 模块无 label flag，走不通（已排除） |

配置建议：`--output-single` 开、`--output-mixed` 关（混合轨按需再开）；`--output-format=mp3 --mp3-bitrate=64000`（≈0.5MB/分钟·单声道，全端兼容）；WAV（16kHz/16bit ≈2MB/分钟）仅在需无损转写时按需切换 `--output-format=wav`。

### 2.2 OSS 上传：Go uploader（唯一可行）

| | rtpengine 原生 S3 | Go uploader（选定） |
|---|---|---|
| 可行性 | ❌ 发行包无此后端 | ✅ 原生 OSS 协议（`aliyun-sdk-oss` 或 minio-go S3 兼容客户端） |
| 落库/事件 | 无 | 顺手写 PG 索引 + 发 NATS 事件，闭环完整 |
| 本地清理 | 无生命周期管理 | 上传成功即删本地文件，磁盘零积压 |
| 保留期 | 需桶生命周期策略 | OSS 桶 lifecycle 或 uploader 删前判断 |

DUX 侧不改 `--recording-method`；uploader 挂进现有 NATS worker 体系（asyncCDR 旁），或独立容器 watch output-dir。

### 2.3 下载/调听：presigned URL

- `GET /v1/recording/{call_id}?track=agent|user` → gv-core 鉴权（admin token / 坐席权限）→ 返回 **OSS presigned URL**（300s 有效）→ 前端 `<audio src>` 直接播。
- 音频流量不过 gv-core，core 只做鉴权和签名；MP3 是浏览器/iOS/Android 全兼容格式，调听零插件。

## 3. 端到端目标链路

```
rtpengine-recording → /output/<call_id>-<ssrc>.mp3（每 SSRC 一个）
  ↓ inotify/完成检测
recorder-uploader（gv-core worker 或独立容器）
  解析 DUX metadata → 判 agent/user 腿 → 归一化命名
  → OSS: recordings/{yyyy-mm}/{call_id}/agent.mp3 / user.mp3
  → PG recording 表: (call_id, agent_key, user_key, format, duration, size, status, uploaded_at)
  → NATS gvos.recording.uploaded
  → 上传成功删本地 → 失败进重试（at-least-once，幂等键=OSS object key）
CDR 侧：recorded 列已有；UI ● 徽标 → 可点链接（播放器/下载）
```

## 4. 分阶段实施计划

| Phase | 内容 | 上线判定 |
|---|---|---|
| R0 POC（半天） | compose 加 rtpengine-recording 容器（bookworm 包），喂现有 spool 的 3 个 DUX pcap，产出 MP3；确认分轨文件数与归属判定可行 | output 目录出现可播放 MP3；tshark/ffprobe 验证 |
| R1 | recording daemon 常驻化：Dockerfile + compose/Helm 适配（spool/output 双卷、目录权限）；`kamailio.cfg` 录音触发不变 | 冒烟通话后 output 自动出现成品文件 |
| R2 | Go uploader：OSS 客户端、DUX metadata 解析、agent/user 归属、PG `recording` 表迁移、NATS 事件、失败重试、本地清理 | 落一通电话，OSS 出现 agent/user 两文件，表里两行 |
| R3 | 下载/调听：`GET /v1/recording/{call_id}` presigned URL 端点 + 权限；UI 徽标变可点播放器 | UI 里点开即播，track 可切 |
| R4 | 保留期/成本：OSS lifecycle 规则（对齐 CDR 分区保留期）；磁盘预算重写（MP3 ≈0.5MB/分钟 vs pcap ≈100KB/分钟）；Helm/compose-ha 录音链路补齐（目前 HA/K8s 形态**缺失**） | ops 手册更新；HA 形态录音可用 |

注：compose-ha.yml 当前无 rtpengine 服务、Helm kamailio 模板无录音配置——**HA/K8s 形态录音链路本就缺失**，R4 一并补齐。

## 5. 风险与代价

- **容量口径变化**：MP3 64kbps ≈0.5MB/分钟·单声道，比 pcap（≈100KB/分钟）**大 5 倍**——转码换格式的代价，换回可调听+ASR 可用；磁盘与 OSS 存储预算按新口径重测。
- **rtpengine-recording 依赖**：bookworm 包自带 FFmpeg/libmp3lame 依赖链，镜像体积增加（估 +150-200MB）。
- **DUX 格式无官方规范文档**：tag 槽位语义靠上游源码+实测（POC R0 已覆盖现有 spool 样本；upstream 格式随版本演进需锁包版本）。
- **通话中挂 OOM/转码堆积**：recording daemon 转码失败会导致 output 目录积压（它不清理失败的输入）——uploader 需处理「孤儿 DUX」（超时未产出成品的 call-id 产生告警事件）。
- **多副本**：recording daemon 每 rtpengine 节点各一个（消费本节点 spool）；uploader 用 NATS queue group 保证每 call-id 只被一个副本处理。

## 6. 后续大版本安排建议

并入下一个大版本（与 Redis 收口方案 `docs/redis-adoption-plan.md` 并列的「vNext 演进项」）：

1. **版本载体**：建议作为独立 minor（如 v0.x 录音管道升级）而非与 Redis 收口混在同一发布——两者代码面不相交（Redis 动 account/callstate/riskctrl，录音动 deploy/recording/uploader），分开发布风险更小。
2. **顺序**：Redis 收口（账务正确性）优先于录音升级（体验/合规），录音 R0 POC 可先行（半天、不动生产代码）。
3. **依赖项**：OSS 桶与凭证、rtpengine 镜像重建、存储预算评审在 R1 前就位。
4. **回滚**：recording daemon 停掉即回退到现状（pcap 继续落卷），uploader 独立开关，无 schema 破坏（recording 表是新增）。
