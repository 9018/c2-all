# 网络拓扑与数据流（面板 ↔ CC ↔ Agent）

> 2026-09-10 · 对应分支 `main` · 所有链路均已实测验证
>
> 注意：agent 的 preflight（条件 C2 直连探测）已于 2026-09-10 整体移除——
> relay 架构下 agent 与 CC 之间不存在任何直连流量，CC 无需暴露任何入站端口。

## 全景图

```
                        ┌─────────────────────────────────────────────┐
                        │                 Web 面板 (浏览器)             │
                        │   Dashboard / Agents / Console / Files /     │
                        │   Modules        (React + xterm.js)          │
                        └───────┬─────────────────────▲───────────────┘
                                │ REST (POST /api/*)  │ WebSocket (wss)
                                │                     │ agent-list / cmd-output /
                                ▼                     │ pty-output / log 推送
                        ┌─────────────────────────────┴───┐
                        │           CC  (10.0.1.106)      │
                        │  :9443 Web API/TLS  (0.0.0.0)   │
                        │  :7000 Agent HTTP     (0.0.0.0) │
                        │  :8888 Agent HTTP2    (0.0.0.0) │
                        │  RelayListener ×2 (room-a/b)    │
                        └──────┬───────────────────▲──────┘
                               │                   │
   直连模式（仅局域网测试） │                   │ relay 模式（生产，跨网）
                               ▼                   │
                    ┌──────────────────┐   wss://relay.ubx1ujnzri.kdns.fr/ws/prod-room-a
                    │  Agent (直连)     │   wss://relay.ubx1ujnzri.kdns.fr/ws/prod-room-b
                    │  http_poll →7000 │              │      （同域双 room = DO 级隔离）
                    │  http2     →8888 │              ▼
                    └──────────────────┘   ┌──────────────────────────┐
                                           │  Cloudflare Worker + DO   │
                                           │  (emp3r0r-cf-relay)       │
                                           │  RelayDO: room 分路、      │
                                           │  帧复用、cc-gone 级联、     │
                                           │  Hibernation 休眠          │
                                           └────────────┬─────────────┘
                                                        │ wss ?role=agent&secret=
                                                        ▼
                                           ┌──────────────────────────┐
                                           │  Agent (relay)            │
                                           │  worker_ws + hello 退避2-4min│
                                           │  + DoH: /dns?secret=      │
                                           └──────────────────────────┘
```

## 关键链路（箭头级）

### 1. Agent → CC 心跳/上线（直连，仅测试用）
```
Agent ── POST :7000 checkin (CBOR sysinfo, PFS) ──────▶ CC ──▶ agentdb 登记 + 会话准入
Agent ◀─ ACK "checkin-ok" ───────────────────────────── CC
```
（无 preflight：agent 连接即 checkin，不再做条件 C2 探测）

### 2. Agent → CC 心跳/上线（relay）
```
Agent ── WSS dial /ws/prod-room-a?role=agent&secret= ─▶ Worker ──▶ RelayDO
Agent ◀─ hello {cc: true|false} ────────────────────── DO   (等 CC 在场才发数据)
Agent ── checkin 帧 (tag 复用子流) ────────────────────▶ DO ──▶ CC RelayListener
                                                        CC ──▶ demux → vconn → handler
Agent ◀─ ACK (原路返回) ────────────────────────────── DO ◀── CC
```

### 3. 命令执行（面板 → agent）
```
面板 ── POST /api/command {AgentTag, cmd} ──▶ CC Web API
CC   ── MsgTunData{cmd} 经 message tunnel ──▶ Agent (直连 h2 或 relay DO)
Agent ── 执行 → Response 帧 ───────────────▶ CC
CC   ── WS 推送 command_output ────────────▶ 面板
```

### 4. 交互式 PTY
```
面板 ── WS "pty_start" ──▶ CC ── !shell -s ──▶ Agent: creack/pty → bash
面板 ◀─ WS pty_output (base64 原始字节, 双向) ─ CC ◀── tunnel ◀── Agent
面板 ── 键入 (stdin base64) ──▶ CC ──▶ Agent ──▶ bash
     · 有状态: cd/env 跨命令保留；resize/多会话/优雅关闭
```

### 5. DoH（relay agent 的 DNS 隐蔽解析）
```
Agent ── GET https://<relay>/dns?secret=…?dns=<RFC8484> ─▶ Worker ──▶ 上游 DoH
Agent ◀─ DNS 应答 ─────────────────────────────────────── Worker
```

### 6. 文件/代理等扩展路由
```
FTP 上传/下载:  Agent ── MsgAuth(cap=FTP/WWW) 建独立流 ──▶ CC handler_ftp_cbor
SOCKS5 pivot:  CC listener ── !proxy_start (专用流) ──▶ Agent dial target
```

## 韧性行为（出问题时的箭头）

| 故障 | 自动恢复路径 |
|---|---|
| CF 边缘空闲回收隧道 (1006) | 30s WS ping 保活预防；CC `supervise()` 指数退避重拨 (2s→60s) |
| CC 死亡 | DO 检测 CC socket 关闭 → 对所有 agent `close(4001,'cc-gone')` → agent 1s 内醒来重连 |
| Agent 先于 CC 到场 | DO hello 带 `cc:false` → agent `dialRelay` 等 CC 在场（30s 上限）才发 checkin |
| 会话行残留（抖动后 duplicate session） | 心跳静默 >3min 判死会话，已验证身份的 agent 原子接管；CC 出错回 `checkin-error` ACK；agent ACK 等待 60s 超时重试 |
| 空闲配额燃烧 | relay 模式 hello 退避 2-4min（原 5-60s 为 http_poll 拉命令节奏）；WS 协议层 ping 30s 由 edge 免费应答 |

实测：kill CC → agent 1s 醒 → 重启 CC → agent 36s 全自动重新上线（checkin+tunnel+PFS）。

## 端口与密钥速查

| 端点 | 用途 | 凭证 |
|---|---|---|
| `https://10.0.1.106:9443` | Web 面板 (REST + WS) | `web_token.txt` (Bearer) |
| `http://10.0.1.106:7000` | Agent checkin（仅直连模式测试） | 烧录在 agent 二进制 |
| `http://10.0.1.106:8888` | Agent HTTP2 流 | 同上 |
| `wss://relay.<domain>/ws/<room>` | relay 房间 | URL 内 `secret=` |
| `https://relay.<domain>/dns` | DoH | 同上 `?secret=` |
