# 边缘依赖：fork 策略与镜像钉死

**状态**：执行约定  
**日期**：2026-11-22  
**范围**：Kamailio / RTPengine（以及组织下其它上游 fork）  
**关联**：[deployment-plan](deployment-plan.md) · [02-architecture](02-architecture.md)

---

## 1. 结论一览

| 上游 | 组织仓库 | 是否保留 fork | 运行时如何拿到二进制 |
|---|---|---|---|
| kamailio/kamailio | [gvos-io/kamailio](https://github.com/gvos-io/kamailio) | **保留** | 现阶段：`deploy/kamailio.Dockerfile`（Debian 包 + 配置）；有源码补丁后改为 fork 源码构建 |
| sipwise/rtpengine | [gvos-io/rtpengine](https://github.com/gvos-io/rtpengine) | **保留** | 现阶段：`deploy/rtpengine.Dockerfile`（Debian 包）；有源码补丁后改为 fork 源码构建 |
| nats-io/nats-server | gvos-io/nats-server | **不维护**（建议 archive） | 官方镜像 `nats:<钉死版本>` |
| nats-io/nats.go | gvos-io/nats.go | **不维护**（建议 archive） | `go.mod` → `github.com/nats-io/nats.go` |

保留 fork 的理由：参与源码修改，并向上游提 PR。NATS 两侧当前无定制补丁，fork 只会制造同步噪音。

---

## 2. 分支约定（kamailio / rtpengine）

在各自 fork 上固定三组引用：

| Remote / 分支 | 用途 |
|---|---|
| `upstream` → 官方仓库 | 只读；定期 `fetch` |
| `origin/main`（或 upstream 默认分支镜像） | 与官方默认同步的干净基线，**不落 gvos 私有补丁** |
| `gvos/vendor` | 长期承载尚未上游化的补丁；镜像构建默认从此检出 |
| `gvos/<topic>` | 单议题分支；能上游化则开官方 PR，合入后从 `gvos/vendor` 摘除 |

工作流：

1. `git fetch upstream && git rebase upstream/<default> gvos/vendor`（冲突在 vendor 解）
2. 可贡献改动：从 `gvos/vendor` 抽出 `gvos/<topic>` → 对官方开 PR
3. 官方合入后：rebase `gvos/vendor`，删掉已上游化的 commit
4. 发版：在 `gvos/vendor`（或打 tag 的 commit）上构建，推送钉死标签的镜像

---

## 3. 镜像钉死

### 3.1 标签格式

```
gvos-kamailio:<upstream-ver>-gvos.<N>
gvos-rtpengine:<upstream-ver>-gvos.<N>
```

示例：`gvos-kamailio:6.0.1-gvos.1`、`gvos-rtpengine:11.5.1.18-gvos.1`。  
`<N>` 只在 gvos 侧补丁或打包方式变化时递增；纯配置挂载变化不换镜像 tag。

生产 / K8s / Helm **禁止** `:latest` 漂浮标签。

### 3.2 当前仓库落点

| 载体 | Kamailio | RTPengine |
|---|---|---|
| compose / compose-ha | 本地构建 tag（见 `deploy/docker-compose.yml`） | 同左 |
| `deploy/k8s/` | `gvos-kamailio:6.0.1-gvos.1` | `gvos-rtpengine:bookworm-gvos.1` |
| Helm `values.yaml` | 与自建镜像对齐（不再默认 `kamailio-ci` alpine——上游 alpine **无 nats.so**） | 与自建镜像对齐 |

Debian 包阶段尚未按上游精确 patch 版本钉 apt 时，tag 里的版本取「已知工作基线」；切到 fork 源码构建后改为精确 git describe / 上游版本号。

### 3.3 构建命令（本地）

```bash
docker build -f deploy/kamailio.Dockerfile -t gvos-kamailio:6.0.1-gvos.1 deploy/
docker build -f deploy/rtpengine.Dockerfile -t gvos-rtpengine:bookworm-gvos.1 deploy/
# 可选：同时打 compose 短名
docker tag gvos-kamailio:6.0.1-gvos.1 gvos-kamailio:latest
docker tag gvos-rtpengine:bookworm-gvos.1 gvos-rtpengine:latest
```

推送私有 registry 时把 repository 换成 `ghcr.io/gvos-io/...`（或客户侧 registry），**tag 字符串保持不变**。

---

## 4. 两阶段演进

**阶段 A（现在）**— Debian 包 + 挂载 `kamailio.cfg`  
- fork 用于开发/评审补丁与社区 PR，不强制进构建链路  
- 运行时仍钉死自建镜像 tag，避免 `apt` 无缓存重建时静默升包  

**阶段 B（有内核/模块级补丁时）**— 从 `gvos/vendor` 源码构建  
- Dockerfile 改为 `git clone` / multi-stage 编译，`ARG GVOS_EDGE_REF=gvos/vendor`  
- CI：上游有新 release → 开同步 PR 到 fork → 重建镜像 → gvos-core 里 bump tag  

---

## 5. NATS 清理

`gvos-io/nats-server`、`gvos-io/nats.go` 建议 **GitHub Archive**（保留历史，停止接收 push），不要删库。  
客户端继续走官方 module；服务端继续钉 `nats:2.10.x-alpine`（或当前验证过的小版本），写进 compose / k8s。

---

## 6. 检查清单（每次边缘发版）

- [ ] fork 已 `fetch upstream`，`gvos/vendor` 可 rebase  
- [ ] 镜像 tag 递增且写入 compose / k8s / Helm  
- [ ] 全栈 smoke：注册 + 一通电话 + CDR 平账  
- [ ] 若含可上游化补丁：官方 PR 链接记入本 tag 的 release note  
