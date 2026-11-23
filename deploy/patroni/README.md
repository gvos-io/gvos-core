# Patroni 三节点参考拓扑

替代 `compose-ha` 的轻量 watchdog，提供 **etcd 共识选举 + 自动切主 + HAProxy 读写入口**。

## 启动

```bash
docker compose -f deploy/patroni/docker-compose.yml -p gvos-pg up -d
# 初次 bootstrap 约 30–60s；探测：
curl -sf http://127.0.0.1:15433/  # 需经容器网络；宿主机查：
docker exec gvos-pg-pg-node1-1 curl -sf http://127.0.0.1:8008/primary
```

gv-core DSN：

```
postgres://gvos:gvos_dev@<host>:15433/gvos?sslmode=disable
```

或 compose 网络内：`pg-proxy:5000`。

## 切主演练

```bash
docker stop gvos-pg-pg-node1-1   # 假设当时是 primary
# 观察 node2/3 之一升主；haproxy 健康检查切到新 primary
# gv-core（pgx target_session_attrs 或单入口 haproxy）自动恢复写连接
```

## 与轻量版对照

| | compose-ha watchdog | 本拓扑 |
|---|---|---|
| 选举 | 单 standby 探活 promote | etcd 共识 |
| 脑裂防护 | 弱 | 强 |
| 交付复杂度 | 低 | 中 |
| 适用 | POC / 双机套餐 | 生产 / 客户 RTO&lt;60s |

Spilo 镜像与 etcd 版本可按环境钉死；生产必须换密码、开 TLS、接监控。
