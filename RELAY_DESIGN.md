
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

## Agent 身份与重启语义（2026-09-07 实测撞出的边界）

**结论先行：普通二进制 agent 进程重启 ≠ 无感重连。**

身份模型由三块拼成，交互出以下语义：
- **UUID**：由主机名+用户+配置派生，跨进程稳定（同一个二进制在同一台主机上永远是同一个 UUID）
- **身份密钥**：`GetAgentKey()` 进程级临时（`sync.Once`，进程死即丢）；仅 stager 路径
  会从 FD3 注入的 seed 经 HKDF 派生出**确定性**密钥
- **CC 侧 TOFU pin**：首次 checkin 时把 agent 公钥钉进 DB，此后同 UUID 必须出示同一密钥

由此得出的运维矩阵：

| 场景 | 结果 |
|---|---|
| CC 重启，agent 进程不动 | ✅ 密钥没变，pin 仍匹配，零干预回连（已 3 次实测） |
| agent 进程重启，CC 不动 | ❌ 新进程 = 新临时密钥，CC 报 `key rotation is disabled` 拒绝；恢复路径 = `forget`（重置 DB pin）+ agent 重启重新 TOFU 注册（已实测闭环） |
| 真克隆 / 同 UUID 双进程并发 | ✅ 同样被拒（后到者 hellos 每 ~90s 刷一次 CRITICAL 日志，无害但吵） |
| stager（seed）路径重启 | ✅ seed 不变 → 密钥不变 → pin 匹配，无感重启 |

实战推论：
1. **测试环境**反复重启 agent 是常态，每次都要 forget + 重启，别只重启不 forget
   （会被钉死在 hello 重试循环里，日志刷 `pinned key verification failed`）
2. forget 走 `POST /api/agents/forget`，body 用裸 UUID
   （`2579f697-…`，别带 `a9017\\a9017-agent-` 前缀，带前缀会 404）
3. **持久化部署必须走 stager/seed 路径**，否则每次进程重启都等于丢身份——
   这不是 bug，是反克隆设计的代价；seed 机制正是为了两全
4. 若未来想让普通二进制也无感重启，候选方案是照 stager 思路从嵌入配置的
   per-agent secret 确定性派生密钥（密钥不落盘、UUID 本就稳定、不额外引入可关联性）

## 部署与测试环境（2026-09-07 快照）
- 生产 relay：`emp3r0r-cf-relay` Worker（双自定义域名，room-a/room-b，role=cc）
- 本地开发测试：`npx wrangler dev --port 8806 --var EMP_SHARED_SECRET:testsec`
  （`go test ./internal/transport/` 的 E2E 测试依赖它）
- 前端：React UI 由 CC 直接 serve `~/.emp3r0r/web/`（改前端后 `npm run build`
  并拷贝 dist 即可，无需重编 CC）
- 源码仓库：github.com/9018/c2-all（分支 `main`；Worker 源码在仓库内 `cf-relay/` 目录）

## 多端点 failover（2026-09-07 实现，方案 B 落地）

背景：CC 侧本就支持 N 条出站 relay 隧道（`relay_urls` 每条一个 listener goroutine），
genagent 构建时也把完整 agent 端点列表（role=cc→role=agent 交换）嵌进了 agent 二进制，
但 agent 只认单个 `CCAddress`——relay 域名被烧时在线舰队全体变砖，双隧道沦为摆设。

三层修复（agent 侧）：

1. **端点轮换**（`connector.go nextRelayEndpoint`）：任何 relay 拨号失败时，在嵌入列表
   （仅 `role=agent` URL）内循环轮换 `def.CCAddress`；直接连接模式 / 单端点 / CC-role
   列表一律不轮换。所有端点背后是同一个 CC，端点身份纯属传输层，会话/密钥/路由不受影响。
2. **DoH 随迁**（`connector.go rotateDoH`）：genagent 把 DoH 钉在第一个端点同域的
   `/dns` 路由上——域名被烧则 DoH 陪葬，连 failover 目标都解析不了。轮换时若 DoH
   与失败端点同域，同步重定向到新端点的 `/dns?secret=` 路由。
3. **DoH bootstrap 降级**（`cmd/agent/agent.go`）：`NewDoHResolver` 需要真解析 DoH
   服务器自身域名（bootstrap），失败会返回 nil；原代码把 nil 赋给 `net.DefaultResolver`
   导致后续拨号 panic（agent 变砖）。现在降级到系统 DNS 并告警。

真机验证：agent 嵌入 [dead.invalid, room-a(真), room-b(真)] 三端点，启动日志依次出现
`DoH bootstrap failed → falling back to system DNS` → `failing over: dead.invalid →
relay.at7ublkkc3` → `DoH re-homed` → `Checked in (verified by server)` → AgentToken，
全程零操作员介入。测试：`TestNextRelayEndpoint` / `TestRotateDoH`；全仓 44 包测试通过。

CC 侧零改动。剩余短板：双 worker 仍在同一 CF 账号（账号封禁=双杀），彻底冗余需双账号
部署同一 Worker（纯运维动作）。

## DO Hibernation 重写 — 计费根因修复（2026-09-08，新账号部署 E2E 全通）

### 根因：不是基础设施，是计费模式

旧版 DO 用 `server.accept()` + 内存 Map（`this.cc` / `this.agents` / `this.meta`）。
`accept()` 模式下 socket 生命周期锚定 DO 内存驻留：只要有连接（哪怕完全空闲），
DO 就 24/7 活跃。Duration 按 **128MB × 墙钟** 计费：免费额度 ~13,000 GB-s/月
只够一个常驻对象活 ~29 小时——**一天的空闲 socket 就烧穿整月配额**，之后
全账号所有 DO 调用 500 直到月初重置。GraphQL 分析证实：本月 DO 调用 147 次、
141 次报错；连一个 `WebSocketPair()` 最小测试 Worker 都 500，看起来极像
"CF DO 基础设施坏了"，实为账号级配额熔断。

### 修复：Hibernation API 重写（`cf-relay/src/relay_do.js`）

- `ctx.acceptWebSocket(ws, [tags])` 替代 `server.accept()`：空闲时 DO 从内存逐出，
  socket 由 CF edge 维持；**空闲 = 零 duration 计费**，仅在消息到达时唤醒。
- 状态全部"可重建"：每次唤醒从 `getWebSockets()` + 附件 + storage 重新派生——
  - 每 socket 的 `{role, tag}` 元数据走 `serializeAttachment()`/`deserializeAttachment()`
    （实测：`ws.tags` 属性**不存在**，这是唯一跨休眠存活的 per-socket 通道）；
  - CC 存活标记 `ccAlive` 存 storage——close 事件里 runtime 可能已把 socket
    从 tag 索引摘除，membership 不可靠；
  - agent 计数器 `nextTag` 存 storage，跨逐出/重启 tag 不冲突。
- `setWebSocketAutoResponse(ping/pong)`：固定请求/响应对由 edge 直接应答，
  不唤醒 DO。Go 侧保活本来就是 WS 协议层 ping（edge 自动应答，同样免唤醒）。
- 事件驱动：`webSocketMessage` / `webSocketClose` / `webSocketError` 类方法
  替代 `addEventListener`。
- 线协议**零改动**（Go 两侧不用动）：hello/agent-joined/agent-left 文本帧、
  agent→CC `[tagByte][payload]`、CC→agent 定向/`0xFF` 广播、cc-gone 4001 清扫、
  superseded 4000。

### 验证（新 CF 账号，全部通过）

协议回归 16/16：hello、tag 帧封装、定向、广播、双 agent、agent-left、cc-gone 清扫、
**75s 休眠窗口后**重新加入（fresh tag）+ 双向路由恢复。真机 E2E：agent 经
`relay.ubx1ujnzri.kdns.fr` checkin（PFS + AgentToken），立即命令与
**90s 休眠后命令**均正常执行（`HIBERNATION_LIVE` → `POST_HIB`）。

### 新账号部署要点（踩坑记录）

- **workers.dev 子域名是硬门槛**：账号没有它时，`wrangler deploy` 与 CF API
  （哪怕 `workers_dev=false` + routes）一律 10063 拒绝。**绕法**：
  `PUT /accounts/{id}/workers/subdomain` `{"subdomain":"名字"}` 直接注册——
  不需要 dashboard（本文档实测，Token 需 Workers Scripts Write）。
- 路由：wrangler.toml `routes = [{pattern="relay.<域>/*", zone_id=...}]` +
  同名 proxied A 记录（占位 IP 即可，`192.0.2.1`）。token 的 zone scope 必须
  覆盖目标 zone（本 token 只有 ubx1ujnzri，cr6kifimf5 的 route/DNS 均无权限）。
- 跨 zone 域名级冗余（relay 挂两个不同 zone）：`workers/domains` API 拒绝 API
  token（10405），需 dashboard 操作或扩 token scope——暂以**同域双 room**
  （room-a/room-b = 两个独立 DO 实例）作 DO 级冗余。
- 老账号（cc104f6c…）9 月配额已烧穿，WS 500 至 10/1 重置；Hibernation 版已
  部署过去，重置后即恢复。旧域名（at7ublkkc3/ea3vmj2vjp）DNS 仍指向老账号。

### 当前部署快照（2026-09-08）

- 生产 relay：`relay.ubx1ujnzri.kdns.fr`（新账号 f8b31eff…，workers.dev 子域
  `emp-relay-f8b3`，Worker `emp3r0r-cf-relay`，room: prod-room-a / prod-room-b）
- `~/.emp3r0r/emp3r0r.json` 的 `relay_urls` 已切到新域名（role=cc）
- 老 relay Worker 同源码已部署老账号（10/1 配额重置后自动可用）
