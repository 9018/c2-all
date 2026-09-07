
---

# 实现状态 (2026-09-06)

设计已落地，代码在两处：

## 1. Worker 侧 — `/home/a9017/c2/emp3r0r-cf-relay/`
- `src/worker.js`：路由 `/health`、`/dns`（RFC 8484 DoH 多上游容错）、`/ws/<room>?role=<cc|agent>&secret=`
- `src/relay_do.js`：Durable Object 每房间一实例；accept 模式（非 hibernation，`new_classes` 迁移）；tag a1..a254 动态分配；CC 顶替旧连接；`0xFF` 广播
- 鉴权：`EMP_SHARED_SECRET`（Bearer 或 `?secret=`，常数时间比较），未设置=dev 开放
- **测试**：Python 9 步协议测试全过（`/tmp/test_relay_ws.py`）；miniflare 偶发握手超时 (~20%) 是本地怪癖，生产不受影响

## 2. CC/agent 侧 — emp3r0r core
- `internal/transport/c2channel_workerws.go`：注册 `worker_ws` channel；agent 侧 `Dial` 走 gorilla WebSocket + hello 握手
- `internal/transport/relay_listener.go`：CC 侧 `RelayListener`（伪装 net.Listener）+ tag 多路分解 → 每agent一条虚拟 `net.Conn`
- `internal/cc/server/relay_listener_cc.go`：`StartRelayListeners` → 复用 `cborStreamAccept` 管线（与 TLS 路径完全一致）
- 配置：`emp3r0r.json` 的 `"relay_urls": [...]`（CC 多宿主），agent 的 `CCAddress` 填 agent 角色同房间 URL + `c2_channel_mode: "worker_ws"`
- def.Config 新增 `RelayURLs []string cbor:"36,keyasint"`

## 3. 验证层级
| 层 | 测试 | 结果 |
|---|---|---|
| Worker 协议 | `/tmp/test_relay_ws.py` 9步 | PASS |
| Go↔relay 通道 | `TestWorkerWSChannelE2E` 20次往返 | PASS |
| SecureConn over relay | `TestSecureConnOverRelay` 100KB/64KB | PASS |
| CC 编译集成 | `go build ./...` | PASS |

## 4. 修复的关键 bug（复用价值）
1. **miniflare `new_sqlite_classes` + WS upgrade = 500**：改 `new_classes` + DO 内 `server.accept()`
2. **`globalThis.EMP_SHARED_SECRET` 在 Workers 里不存在**：env 绑定必须走 `env` 参数
3. **`virtualConn.Read` 截断**：一次 `copy(p, data)` 丢弃超长帧剩余字节 → 加 `backlog`
4. **gorilla 需要 `ws://` scheme**：不能转成 `http://`
5. **测试房间污染**：快速重连同房间残留 agent 状态 → 房间名加时间戳

## 5. 待办
- [ ] 真实 CC 进程 + 真 agent 走 relay 的注册闭环（需 operator online 状态，适合下次会话）
- [ ] `wrangler deploy` 到真实 CF 账号（用户持有多账号+自定义域名）
- [ ] DoH 上游出站在 CF 边缘验证（本地无出站，部署后测 `/dns` 200）
- [ ] agent 侧 stub 嵌入 relay URL（`genagent` 配置补丁）

## 实现状态更新（2026-09-06 晚间，E2E 全通）

### 端到端验证（生产 relay）
- agent `828010b1`（worker_ws 模式）→ `wss://relay.at7ublkkc3.kdns.fr/ws/prod-room-a` → CC RelayListener → agentdb 登记 → PFS 握手成功
- Web UI 正确显示 relay agent（`relay` 标签 vs 直连 agent 的 IP 标签）
- **PTY over relay E2E**：浏览器 → CC Web panel → agent PTY bash 会话建立，`whoami`/`uname` 全双工正常（截图 `/tmp/relay_pty2.png`）

### 新增 6 个修复（含此前 3 个）
1. genagent relay 覆盖必须在 `MakeConfig` 之后（它重读 JSON 覆盖 RuntimeConfig）
2. agent `InitConfig` 对 `wss://` URL 跳过 `https://` 前缀/端口拼接（isRelay 分支）
3. agent relay 模式跳过 `CreateEmp3r0rHTTPClient`（wss:// 不是 HTTP endpoint）
4. `establishChannelStream` 对 ws/wss URL 豁免 HTTPClient-nil 检查
5. CC `RelayListener.supervise()`：CF 边缘空闲断连后指数退避重拨（2s→60s 上限）
6. **WS 保活 ping**：`relayConn.startPinger()` 每 30s 发 ping 控制帧，防 CF 边缘 1006 回收

### genagent 最终语义
- `--relay` 或 `--relay=same`：agent 加入 CC 的房间（role=cc → role=agent），worker_ws 模式
- `--relay=<room>`：改写房间号（仅当该房间也有 CC 监听时才有意义，否则 agent 挂在空房间等 ACK）
- relay agent 自动注入 `doh_server=https://<relay-host>/dns?secret=<SECRET>`（Worker /dns 路由，RFC 8484，ncruces/go-dns POST 兼容）

### 经验（新增 2 条）
7. **CC/agent 共享 relay_urls 但角色不同**：CC 存 role=cc，agent 由 genagent 运行时改写；SaveConfigJSON 会把改写后的 URL 写回 JSON——重启 CC 前必须确认 JSON 里是 role=cc
8. **空房间 = 无 ACK 挂死**：agent 连进没有 CC 的房间时，sysinfo 发出后永远等不到响应（不报错，只是静默卡住）；排查时先核对房间名

## 韧性加固（2026-09-06 深夜，全链路故障恢复验证通过）

### 两个"挂死"根因与修复
1. **发进虚空竞态**：agent 比 CC 先进房间时，DO 丢弃 agent 帧（`if (!this.cc) return`），
   agent 的 checkin 发出后永远等不到 ACK（读无超时）→ 修复：
   - DO hello(agent) 新增 `cc: true/false`
   - `dialRelay`（agent 角色）等到 `hello.cc == true` 才返回（30s 上限，2s 间隔重拨）
2. **CC 死亡无通知**：CC socket 死后 DO 里 agent socket 仍存活，agent 永远不知道 CC 走了 → 修复：
   - DO `onClose`：CC 离开时对全部 agent socket 发 `close(4001, 'cc-gone')` 级联关闭

### 故障恢复时序（实测）
CC kill → agent 1s 内收到 4001 cc-gone（不再挂死）→ agent 退避重连循环（dialRelay 等 CC 在场）
→ CC 重启 → agent 36s 后全自动 checkin + message tunnel + PFS 重建，零人工干预。

### WSS 出站 TLS 混淆（2026-09-07）
relay WSS 出站握手不再用 Go 默认指纹：`dialRelayTLS`（c2channel_workerws.go）通过
gorilla 的 `NetDialTLSContext` 完成 uTLS 握手，ClientHello 用 `HelloRandomizedNoALPN`——
每次连接生成全新随机 JA3（无稳定 Go 指纹可抓）。浏览器指纹（Chrome/Firefox）被刻意排除：
它们的 ALPN 含 h2，若边缘协商 h2 会破坏后续 HTTP/1.1 WS 升级；无 ALPN 即默认 HTTP/1.1。
注意：本地 `go test ./internal/transport/` 走 ws://（无 TLS），uTLS 路径仅对生产 wss 生效。

### 保活
`relayConn.startPinger()`：每 30s WS ping 控制帧（gorilla `WriteControl`，并发安全），
防止 CF 边缘把空闲 WebSocket 当 idle 连接回收（曾观测 ~19-30min 1006）。
DO 的 accept() socket 由 CF 运行时自动回 pong。

### 会话死锁修复（duplicate-session starvation，2026-09-07）

CC 重启/隧道抖动（1006）后 agent 可能被永久锁在门外：
- 旧会话行只靠隧道帧刷新心跳，隧道静默死亡后行仍"新鲜"（15 分钟 stale 窗口）
- 同进程（同 epoch）重连 checkin 被判 `forbidden: duplicate session` 拒绝
- CC 出错不回 ACK，agent 的 ACK 等待无超时 → 永久挂死（实测挂 10 分钟）

三层修复：
1. `StartSession`（agentdb.go）：同 epoch 旧会话心跳静默 >3 分钟（sessionLiveWindow）
   视为死会话，允许接管。安全性：checkin 在会话准入**之前**已通过 pinned 公钥
   签名验证，来者即同一身份；真正并发的克隆会话有心跳，仍被拒绝。
2. `dispatcher.go`：checkin 失败时回 `checkin-error` ACK（此前静默丢弃）。
3. `poll.go`（agent）：checkin ACK 等待加 60s 超时，超时后走退避重试。

## 部署与测试环境（2026-09-07 快照）
- 生产 relay：`emp3r0r-cf-relay` Worker（双自定义域名，room-a/room-b，role=cc）
- 本地开发测试：`npx wrangler dev --port 8806 --var EMP_SHARED_SECRET:testsec`
  （`go test ./internal/transport/` 的 E2E 测试依赖它）
- 前端：React UI 由 CC 直接 serve `~/.emp3r0r/web/`（改前端后 `npm run build`
  并拷贝 dist 即可，无需重编 CC）
- 源码仓库：github.com/9018/c2-all（分支 `main`；Worker 源码在仓库内 `cf-relay/` 目录）
