# gvos Helm chart（骨架）

K8s 交付轨：gv-core Deployment（健康探针/资源限制/Secret 注入 DSN）+
Kamailio 无状态信令组（水平扩展）+ SIP/API Service。数据库默认外部
（生产接 Patroni/托管 PG——compose-ha.yml 的主备+演练已验证复制语义）。

待补（v1.0 正式交付时）：kamailio.cfg ConfigMap 的 values 渲染、
rtpengine DaemonSet（媒体面要 hostNetwork）、cert-manager TLS 注入。
安装：`helm install gvos deploy/helm/gvos --set database.dsn="postgres://..."`
