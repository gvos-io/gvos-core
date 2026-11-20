# Security Policy

G-VOS 处理计费与话务数据，安全问题的处理优先级最高。

## 报告漏洞

**不要通过公开 Issue 报告安全漏洞。** 请使用 GitHub 的
[Private vulnerability reporting](https://github.com/gvos-io/gvos-core/security/advisories/new)
私下报告，包含：受影响版本、复现步骤、影响评估。我们承诺在 72 小时内响应。

## 范围

- 计费正确性与绕过（锁定语义、步长精度、拆线时序）
- 认证/授权（管理 API、SIP Digest）
- 事件总线注入与跨副本一致性

## 部署安全基线

见 [operations-manual.md](docs/operations-manual.md)：生产环境必须更换
`dev-admin-token` 与 `gvos_dev` 演示凭据、启用 gv-core TLS、限制管理面暴露。
