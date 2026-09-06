# emp3r0r Web Panel - 项目完成总结

## 📁 已创建的文件结构

```
emp3r0r/
├── web/                                    # 前端目录 (新建)
│   ├── src/
│   │   ├── components/
│   │   │   ├── AgentList.tsx              # Agent 列表组件
│   │   │   ├── CommandConsole.tsx         # 命令控制台组件
│   │   │   ├── Dashboard.tsx             # 仪表盘组件
│   │   │   ├── Layout.tsx               # 主布局组件
│   │   │   ├── Login.tsx                # 登录组件
│   │   │   ├── ModulePanel.tsx          # 模块面板组件
│   │   │   └── Sidebar.tsx             # 侧边栏组件
│   │   ├── hooks/
│   │   │   └── useWebSocket.ts          # WebSocket hook
│   │   ├── lib/
│   │   │   ├── api.ts                   # API 客户端
│   │   │   └── websocket.ts            # WebSocket 客户端
│   │   ├── stores/
│   │   │   └── useStore.ts             # Zustand 状态管理
│   │   ├── types/
│   │   │   └── index.ts                 # TypeScript 类型定义
│   │   ├── main.tsx                      # 入口文件
│   │   └── index.css                     # 样式文件
│   ├── public/
│   │   └── favicon.svg                   # 图标
│   ├── dist/                              # 构建输出
│   ├── package.json
│   ├── vite.config.ts
│   ├── tailwind.config.js
│   ├── tsconfig.json
│   ├── postcss.config.js
│   ├── start-dev.sh                       # 开发启动脚本
│   ├── build.sh                           # 生产构建脚本
│   └── README.md
│
└── core/internal/cc/server/
    └── web_server.go                      # Web API 服务器 (新增)
```

## 🎯 功能模块

### 1. 前端组件

| 组件 | 功能 | 文件 |
|------|------|------|
| **Dashboard** | 概览面板、统计信息、OS分布图 | `Dashboard.tsx` |
| **AgentList** | Agent列表、详情、选择、删除 | `AgentList.tsx` |
| **CommandConsole** | 交互式命令行、输出查看 | `CommandConsole.tsx` |
| **ModulePanel** | 模块浏览、参数配置、执行 | `ModulePanel.tsx` |
| **Sidebar** | 导航菜单、连接状态 | `Sidebar.tsx` |
| **Login** | Token认证 | `Login.tsx` |

### 2. 核心库

| 文件 | 功能 |
|------|------|
| `api.ts` | REST API 客户端 (agents, commands, modules) |
| `websocket.ts` | WebSocket 客户端 (实时更新) |
| `useStore.ts` | Zustand 全局状态管理 |

### 3. 后端 API (`web_server.go`)

| 端点 | 方法 | 功能 |
|------|------|------|
| `/api/health` | GET | 健康检查 |
| `/api/agents` | GET | 获取 Agent 列表 |
| `/api/agents/active` | POST | 设置活动 Agent |
| `/api/agents/forget` | POST | 删除 Agent |
| `/api/command` | POST | 发送命令 |
| `/api/modules` | GET | 获取模块列表 |
| `/api/ws` | WS | WebSocket 连接 |

## 🚀 使用方法

### 开发模式

```bash
cd emp3r0r/web
npm install
npm run dev
# 访问 http://localhost:5173
```

### 生产构建

```bash
cd emp3r0r/web
npm run build
# 产物在 dist/ 目录
```

### 启动后端

Web 服务器会在 emp3r0r C2 服务器启动时自动运行：
- 端口: 9443 (HTTPS)
- Token: 自动生成并显示在日志中
- 位置: `~/.emp3r0r/web_token.txt`

## 🔧 技术栈

### 前端
- **框架**: React 18 + TypeScript
- **构建**: Vite 5
- **样式**: TailwindCSS 3.4
- **状态**: Zustand
- **UI**: Lucide Icons
- **通信**: WebSocket + Fetch API

### 后端
- **语言**: Go
- **路由**: Gorilla Mux
- **WebSocket**: Gorilla WebSocket
- **认证**: Bearer Token
- **TLS**: 自签名证书 (ECDSA P-256)

## 📊 架构图

```
┌─────────────────────────────────────────────────────────────┐
│                      Web Browser                            │
│  ┌───────────────────────────────────────────────────────┐  │
│  │                   React App                           │  │
│  │  ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌─────────┐  │  │
│  │  │Dashboard │ │AgentList │ │ Console  │ │Modules  │  │  │
│  │  └──────────┘ └──────────┘ └──────────┘ └─────────┘  │  │
│  └───────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────┘
                          │
                          │ HTTPS (JSON + WebSocket)
                          ▼
┌─────────────────────────────────────────────────────────────┐
│              emp3r0r C2 Server - Web API                   │
│                    (port 9443)                              │
│  ┌───────────────────────────────────────────────────────┐  │
│  │                   web_server.go                        │  │
│  │  - Token Authentication                                │  │
│  │  - REST API (JSON)                                     │  │
│  │  - WebSocket Server                                    │  │
│  │  - Static File Server                                  │  │
│  └───────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────┘
                          │
                          │ 内部调用
                          ▼
┌─────────────────────────────────────────────────────────────┐
│              emp3r0r C2 Server Core                         │
│  - Agent 管理                                               │
│  - 命令执行                                                 │
│  - 模块系统                                                 │
│  - 消息隧道                                                 │
└─────────────────────────────────────────────────────────────┘
```

## 🔐 安全特性

1. **Token 认证**: 所有 API 需要 Bearer Token
2. **HTTPS**: TLS 1.2+ 加密传输
3. **WebSocket**: 安全的实时通信
4. **CORS**: 生产环境应限制来源

## 📝 下一步

### 短期
- [ ] 完善 Agent 详情页
- [ ] 添加命令历史记录
- [ ] 实现模块执行功能
- [ ] 添加日志查看器

### 中期
- [ ] P2P 拓扑可视化
- [ ] 文件管理界面
- [ ] Token/Session 管理
- [ ] 多语言支持

### 长期
- [ ] 接入 Cloudflare Tunnel
- [ ] 移动端适配
- [ ] 实时通知系统
- [ ] 审计日志

## 🐛 已知问题

1. WebSocket 在生产环境需要配置 CORS
2. 自签名证书在浏览器中会显示警告
3. 模块执行功能尚未完全实现

## ✅ 2026-09-06 更新：PTY 交互式终端 + WS 修复

### 新功能：Interactive TTY（PTY）

Console 页新增持久化交互式 shell（xterm.js ↔ agent 侧 creack/pty），有状态、多会话、
resize 同步。协议与实现细节见 **[PTY_FEATURE.md](./PTY_FEATURE.md)**。

- 前端：`PtyTerminal.tsx`（新增）、`CommandConsole.tsx`（PTY 按钮/模式切换）、
  `useWebSocket.ts`（pty_output 分发）、`types/index.ts`（PTY_OUTPUT）
- 后端：agent 侧 `pty_session.go`/`pty_shell.go`（新增）、`sender.go`（NotifyC2Raw）、
  `c2cmd.go`/`def/commands.go`（!shell 命令）；CC 侧 `handler_messagetun.go`（pty_output 广播）、
  `web_server.go`（pty_input/resize/close 上行处理）

### 修复：WebSocket 双连接导致的回显重影

生产构建同样启用 `React.StrictMode`，effect 双挂载时旧代码会留下一条僵尸 WS 连接，
服务端每条广播被处理两次（症状：终端回显 `wwhhooaammii`、输出重复两份）。

- `lib/websocket.ts`：`connect()` 先拆旧 socket；新增 `intentionalClose` 防手动断开后重连；
  stale socket 不再派发消息/触发重连
- `hooks/useWebSocket.ts`：cleanup 逐类型 `off()` 解绑 handler，防止累积重复派发

### UX：Interactive TTY 按钮常显

未选 agent 时按钮不再隐藏，改为禁用态 + tooltip「请先在 Agents 页选择目标 agent」；
连接中显示 `Connecting...`。

## 📚 参考文档

- [emp3r0r 主文档](../README.md)
- [Web Panel README](./web/README.md)
- [API 文档](./web/README.md#api-endpoints)
