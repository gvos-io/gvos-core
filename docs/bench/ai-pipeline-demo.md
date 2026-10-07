# AI 媒体面端到端演示（2026-10-07）

**链路**：真实 SIP 呼叫（Digest 认证）→ Kamailio 计费锁定 → gv-core 通话建立 → AI 管道客户端（WebSocket）连 `/v1/stream/{call_id}` → 注入 TTS 音频 → RTT 遥测（预算 500ms）

**实测结果**：

- WS 升级 101（accept-key 校验通过）
- TTS 注入音频落入 uplink（其他订阅者可见）
- `echo` 控制帧回 RTT 遥测：0ms（预算内）
- 流随呼叫 start 打开、end 关闭；CDR 正常出账（PDD 6ms、成本 0.05）

**复现**：起栈后打一通 SIPp 认证呼叫（后台挂住），从 kamailio 日志的 `bill-lock ok` 行取 call-id，然后：

```python
import socket, base64, os, time
call_id = "<call-id>"
s = socket.create_connection(("127.0.0.1", 8080), timeout=5)
key = base64.b64encode(os.urandom(16)).decode()
req = ("GET /v1/stream/" + call_id + " HTTP/1.1\r\nHost: g\r\n"
       "Upgrade: websocket\r\nConnection: Upgrade\r\n"
       "Sec-WebSocket-Key: " + key + "\r\nSec-WebSocket-Version: 13\r\n\r\n")
s.sendall(req.encode())
print(s.recv(4096).decode(errors="replace").split("\r\n")[0])  # 101
# 帧协议：二进制帧=注入音频（进 uplink）；文本帧 "echo"=回 9 字节 RTT 遥测
```

**运维坑（已记录）**：重建 demo-gw 容器会换 IP，Kamailio 解析缓存指向旧地址导致 dispatch 的 INVITE 消失——重建 UAS 后需重启 Kamailio（同类的还有 compose restart 的级联停止问题）。
