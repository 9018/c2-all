# PTY 交互式终端 — 实现与修复记录

> 更新时间：2026-09-06
> 状态：✅ 已完成并通过真实浏览器（Chromium 145 headless）端到端验证
> 分支：`main`（github.com/9018/c2-all）

## 功能概述

在 Web 面板 Console 页为在线 agent 提供**持久化交互式 shell**（区别于一次性 RCE 命令）：

- Agent 侧：`creack/pty` 起真实 PTY + bash 会话，`PtySessions` 表常驻内存
- 传输侧：复用 MsgTunData 隧道，PTY 原始字节流（含 ANSI）以 **base64** 经独立 `pty_output` 帧回传
- Web 侧：xterm.js 终端，键盘输入/回显/resize 全双向实时
- 会话特性：**有状态**（cd/环境变量跨命令保留）、支持多会话（按 JobID 隔离）、resize 同步、优雅关闭/重开

## 协议设计

### 会话生命周期

```
浏览器                    CC (9443)                     Agent
  │ POST /api/command       │                            │
  │ !shell -s --job_id X ──►│ SendCmd ──► cobra ────────►│ 新建 PtySession(X)
  │◄─(WS) pty_output b64 ───│◄── NotifyC2Raw(JobID=X) ───┘ "shell: session X started (/bin/bash)"
  │ (WS) pty_input b64 ────►│ !shell --input=<b64> ─────►│ 写入 PTY stdin
  │◄─(WS) pty_output b64 ───│◄── 流式输出(同 JobID) ──────│ PTY stdout
  │ (WS) pty_resize ───────►│ !shell --resize RxC ──────►│ pty.Setsize
  │ (WS) pty_close ────────►│ !shell --kill ────────────►│ PtySession.Close
```

### 帧格式

- **下行** `{type:"pty_output", data:{JobID, Data:<base64 std>, AgentTag}}`
- **上行** `{type:"pty_input"|"pty_resize"|"pty_close", data:{AgentTag, JobID, Data}}`
  - input/resize 的 Data 为 base64；resize 格式 `"rows>xcols"`

### 关键决策

| 决策 | 理由 |
|---|---|
| 独立 `pty_output` + base64 | `command_output` 走 JSON string，无效 UTF-8 会被破坏；PTY 必须字节保真 |
| 帧识别 `TrimPrefix(CmdSlice[0],"!")=="shell"` | agent 流帧用 `["shell"]`、通知帧用 `["!shell"]`，trim 同时覆盖两种，无需服务端会话注册表 |
| 复用 JobID 作会话 ID | 服务端已把 JobID 预注册进 `live.CmdTime`，每帧携带即可路由 |
| `--input=<data>` flag 形式 | 任意字节（前导 `-`、空格、引号）安全通过 cobra 解析（SetArgs 收数组，不二次分词） |
| C2Commands `Use` 保留 `!` 前缀 | `HandleC2Command` 按 `HasPrefix(cmdSlice[0],"!")` 路由，子命令必须字面匹配 `"!shell"` |

## 代码地图

| 文件 | 职责 |
|---|---|
| `core/internal/agent/handler/pty_session.go` | `PtySession{ID,PTY,Process,Stdin,...}`、`NewPtySession`、`StreamOutput`、`WriteInput`、`Resize`、幂等 `Close` |
| `core/internal/agent/handler/pty_shell.go` | `shellCmdRun`：`-s/--start -k/--kill -i/--input -r/--resize --shell`；输出经 goroutine→`notifyPtyOutput`→`NotifyC2Raw`（同 JobID，`CmdSlice:["shell"]`） |
| `core/internal/agent/base/c2transport/sender.go` | 新增 `NotifyC2Raw(*def.MsgTunData)`：补齐 AgentUUID/Tag/签名后直接 send2CC |
| `core/internal/agent/handler/c2cmd.go` | `registerShellCmd(rootCmd)` 注册 `!shell`（在 platformCommands 前） |
| `core/internal/def/commands.go` | `C2CmdShell = "!shell"` |
| `core/internal/cc/server/handler_messagetun.go` | `!"shell"` 前缀帧 → 广播 `pty_output` 后 `continue`（位于 known-JobID 检查之前） |
| `core/internal/cc/server/web_server.go` | WS readPump 增加 `pty_input/pty_resize/pty_close` → `handleWebPTYMessage` → `agents.SendMessageToAgent` |
| `web/src/components/PtyTerminal.tsx` | xterm.js 封装：onData→b64→`pty_input`；`pty-output` CustomEvent→atob→`term.write`；ResizeObserver→fit→`pty_resize` |
| `web/src/components/CommandConsole.tsx` | `Interactive TTY` 按钮、`ptySessionId = pty-<ts>-<rand>`、PTY 模式下隐藏快速命令栏 |
| `web/src/hooks/useWebSocket.ts` | `pty_output` → `window.dispatchEvent(new CustomEvent('pty-output',...))` |
| `web/src/lib/websocket.ts` | WS 单例客户端（本次修复重点，见下） |

## 部署注意（重要）

1. **改 agent 代码必须重建 stub**：`genagent` 只往 `~/.emp3r0r/stub-amd64` 的 0xff 占位区 patch AES-GCM 配置（4096 字节对齐），**不编译代码**。改了 `core/` 下 agent 侧代码后要：
   ```bash
   cd core && GOFLAGS=-mod=mod GOPROXY=direct CGO_ENABLED=0 \
     go build -o ~/.emp3r0r/stub-amd64 ./cmd/agent
   ```
   否则新命令（如 `!shell`）永远不会到达 agent，报 `unknown command "!shell"`。
   旧 stub 备份：`/tmp/stub-amd64.bak`
2. **Web 静态目录是 `~/.emp3r0r/web`**（`filepath.Join(live.EmpWorkSpace,"web")`），不是 `web/dist`。构建后要 `cp -r web/dist/* ~/.emp3r0r/web/`
3. **Go 构建代理**：`GOFLAGS=-mod=mod GOPROXY=direct`（proxy.golang.org 在本环境超时）
4. **C2Commands 的 Use 必须带 `!`**

## 排障记录（真实浏览器验证过的两轮）

### 第一轮："PTY 不能用"

**根因是 UX 死锁而非协议问题**：未选 agent 时 `Interactive TTY` 按钮被条件渲染完全隐藏，页面只显示 "Select an agent from the Agents tab"，用户无感知。另需 Ctrl+Shift+R 排除旧 bundle 缓存。

修复：按钮常显，无 agent 时禁用态 + tooltip「请先在 Agents 页选择目标 agent」；点击后显示 `Connecting...`。

### 第二轮："回显重影 wwhhooaammii"（本节重点）

**现象**：每个字符显示两次、输出重复两份。

**定位过程**：Python 单连接 E2E（`/tmp/test_pty_dup.py`）发单字符 `'X'` → 服务器侧**恰好 1 帧**回显 ⇒ 服务端/agent 干净 ⇒ 重复发生在浏览器。CDP 连 Chromium 统计页面创建的 WS 连接数 ⇒ 命中根因。

**根因（两个前端 bug 叠加）**：

1. `websocket.ts` 僵尸重连：`onclose` 无条件 `attemptReconnect`，手动 `disconnect()` 也触发。生产构建同样开了 `React.StrictMode`（`main.tsx`）→ effect 双挂载：mount① 建连接 A → cleanup 断开 A → A 的 onclose 1 秒后悄悄建僵尸连接 B → mount② 建连接 C ⇒ **B、C 并存，服务端每帧广播两份**
2. `useWebSocket.ts` handler 泄漏：cleanup 只 disconnect 不 `off()`，effect 重跑后同一消息派发多次事件

**修复**：

- `websocket.ts`：`connect()` 先拆已有 socket；`intentionalClose` 标记禁止手动断开后重连；stale socket（非当前 `this.ws`）的 onmessage/onclose 直接忽略
- `useWebSocket.ts`：handler 保存引用，cleanup 里逐类型 `off()`，随后 disconnect

**验证**（`/tmp/pty_dup_browser_test.py`，CDP + Chromium）：
登录 → 选 agent → Interactive TTY → 敲 `whoami`：
- `whoami` 回显出现次数 = **1** ✅
- 页面 WS 连接数 = **1** ✅
- PTY 执行、resize、输出渲染全部正常

## 当前运行实例（测试环境）

| 项 | 值 |
|---|---|
| CC | nohup PID 122650，二进制 `/tmp/emp3r0r-cc-test`，日志 `/tmp/c2_webui_test.log` |
| 监听 | 9443（Web UI/API/WS）、7000（http_poll）；8888 被 nginx 占用（h2conn 未启用） |
| Agent | UUID `fdf9c995-8606-4df9-a8fe-8e708c9b625b`，tag `a9017\a9017-agent-fdf9c995-...`，http_poll 模式直连 `10.0.1.106:7000` |
| Web token | `~/.emp3r0r/web_token.txt` |
| 链路模式 | **纯直连**（`cdn_proxy=""`、`c2_transport_proxy=""`，无域名/CDN/代理，CC 裸 IP） |

## 测试脚本

| 脚本 | 用途 |
|---|---|
| `/tmp/test_pty_e2e.py` | PTY 协议全流程（启动/输入/状态保持/resize/退出/重开） |
| `/tmp/test_pty_state.py` | 会话状态持久性验证 |
| `/tmp/test_regress.py` | 回归：exec 走 `command_output`、PTY 走 `pty_output`，互不串扰 |
| `/tmp/test_pty_dup.py` | 服务端重复帧检测（单字符→帧数断言） |
| `/tmp/pty_cdp_test.py` / `/tmp/pty_dup_browser_test.py` | 真实浏览器 E2E（CDP 直连 Chromium，playwright 的 node driver 在本机损坏） |

## 后续方向（已讨论未实施）

- **会合式中继（rendezvous relay）**：CC 出站连接 CF Worker/Durable Object，agent 也出站连 Worker，Worker 只透传密文（内层 AES-GCM SecureConn 零信任）。CC 零公网暴露、双端只出站。与现有 `cdn_proxy`（入站回源反代）的本质区别：CC 从监听者变为纯客户端
- **多账号分布式**：CC 多宿主同时维持 K 条出站隧道；agent stub 预埋端点列表轮试；账号被封仅丢对应隧道的 agent，内层 UUID 保证会话可迁移，无跨账号状态同步
- 传输层落点：新注册 `worker_ws` channel（`RegisterC2Channel` 机制现成），CC 侧用 `ByteReadWriteCloser` 包出站 WS 喂现有 `handleMessageTunnelStream`
- 注意：Workers 跑 C2 中继违反 CF ToS；免费档 100k req/天/账号 ≈ 5-6 台 agent（5s beacon）；WS 消息 ~1MB 上限，大流量走旁路
