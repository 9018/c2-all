import express from 'express'
import cors from 'cors'
import { WebSocketServer } from 'ws'
import { createServer } from 'https'
import { readFileSync, existsSync } from 'fs'
import { join } from 'path'

const app = express()
const PORT = 9443

app.use(cors())
app.use(express.json())

// 模拟数据
const mockAgents = [
  {
    Tag: 'agent-001',
    Name: 'target-server-1',
    ShortID: 'a1b2c3',
    Version: '1.0.0',
    Transport: 'h2conn',
    Hostname: 'web-server-prod',
    Hardware: 'VMware',
    Container: '',
    Uptime: '15d 4h 32m',
    Groups: 'root,admin',
    CPU: 'Intel Xeon E5-2680',
    GPU: '',
    Mem: '16GB',
    OS: 'Ubuntu 22.04 LTS',
    GOOS: 'linux',
    Kernel: '5.15.0-56-generic',
    Arch: 'amd64',
    From: '192.168.1.100',
    IPs: ['192.168.1.100', '10.0.0.5'],
    ARP: ['192.168.1.1', '192.168.1.101'],
    User: 'root',
    HasRoot: true,
    HasTor: false,
    HasInternet: true,
    NCSIEnabled: false,
    Process: {
      PID: 1234,
      PPID: 1,
      Cmdline: '/opt/emp3r0r/agent',
      Parent: '/sbin/init'
    },
    Exes: ['/usr/bin/python3', '/usr/bin/curl'],
    CWD: '/opt/emp3r0r',
    Product: null,
    UUID: '550e8400-e29b-41d4-a716-446655440000',
    PublicKey: '-----BEGIN PUBLIC KEY-----\n...\n-----END PUBLIC KEY-----',
    C2Host: 'c2.example.com',
    LastSeen: new Date().toISOString(),
    LastSeenRTT: '45ms',
    MeshRoute: 'gateway-1',
    Files: ['module1.so', 'payload.bin']
  },
  {
    Tag: 'agent-002',
    Name: 'workstation-1',
    ShortID: 'd4e5f6',
    Version: '1.0.0',
    Transport: 'h2conn',
    Hostname: 'WIN-DESKTOP-01',
    Hardware: 'Dell OptiPlex',
    Container: '',
    Uptime: '2d 8h 15m',
    Groups: 'Administrators',
    CPU: 'Intel Core i7-10700',
    GPU: 'NVIDIA GTX 1660',
    Mem: '32GB',
    OS: 'Windows 11 Pro 22H2',
    GOOS: 'windows',
    Kernel: '10.0.22621',
    Arch: 'amd64',
    From: '10.0.0.50',
    IPs: ['10.0.0.50', '172.16.0.10'],
    ARP: ['10.0.0.1'],
    User: 'Administrator',
    HasRoot: true,
    HasTor: false,
    HasInternet: true,
    NCSIEnabled: true,
    Process: {
      PID: 5678,
      PPID: 100,
      Cmdline: 'C:\\emp3r0r\\agent.exe',
      Parent: 'C:\\Windows\\system32\\svchost.exe'
    },
    Exes: ['C:\\Windows\\System32\\cmd.exe'],
    CWD: 'C:\\emp3r0r',
    Product: null,
    UUID: '660e8400-e29b-41d4-a716-446655440001',
    PublicKey: '-----BEGIN PUBLIC KEY-----\n...\n-----END PUBLIC KEY-----',
    C2Host: 'c2.example.com',
    LastSeen: new Date(Date.now() - 120000).toISOString(), // 2 minutes ago
    LastSeenRTT: '89ms',
    MeshRoute: '',
    Files: ['mimikatz.exe', 'cobaltstrike.dll']
  }
]

let activeAgent = null

// 模拟模块
const mockModules = {
  'clean_log': {
    Name: 'clean_log',
    Author: 'emp3r0r',
    Date: '2020-01-25',
    Comment: 'Delete lines containing keyword from xtmp logs',
    IsLocal: false,
    Platform: 'Linux',
    Fileless: true,
    Options: {
      keyword: {
        Name: 'keyword',
        Desc: 'Delete all log entries containing this keyword',
        Val: 'root',
        Type: 'string',
        Required: true
      }
    },
    AgentConfig: { Exec: 'built-in', Type: 'go', IsInteractive: false }
  },
  'listener': {
    Name: 'listener',
    Author: 'emp3r0r',
    Date: '2020-01-25',
    Comment: 'Start a listener to serve stagers or regular files',
    IsLocal: false,
    Platform: 'Generic',
    Fileless: true,
    Options: {
      action: {
        Name: 'action',
        Desc: 'Listener action: start, list, or stop',
        Val: 'start',
        Vals: ['start', 'list', 'stop'],
        Type: 'string',
        Required: true
      },
      port: {
        Name: 'port',
        Desc: 'Port to listen on',
        Val: '8080',
        Type: 'string',
        Required: false
      },
      stager: {
        Name: 'stager',
        Desc: 'Path to the stager file',
        Val: '',
        Type: 'string',
        Required: false
      }
    },
    AgentConfig: { Exec: 'built-in', Type: 'go', IsInteractive: false }
  },
  'steal_token': {
    Name: 'steal_token',
    Author: 'emp3r0r',
    Date: '2026-08-09',
    Comment: 'Steal a Windows access token from a running process',
    IsLocal: false,
    Platform: 'Windows',
    Fileless: true,
    Options: {
      pid: {
        Name: 'pid',
        Desc: 'PID of the process to steal the token from',
        Val: '',
        Type: 'uint',
        Required: true
      }
    },
    AgentConfig: { Exec: 'built-in', Type: 'go', IsInteractive: false }
  },
  'file_downloader': {
    Name: 'file_downloader',
    Author: 'emp3r0r',
    Date: '2020-01-25',
    Comment: 'Download a file from peers or CC over P2P mesh',
    IsLocal: false,
    Platform: 'Generic',
    Fileless: true,
    Options: {
      path: {
        Name: 'path',
        Desc: 'Path to the file to download',
        Val: '',
        Type: 'string',
        Required: true
      },
      peer: {
        Name: 'peer',
        Desc: 'Peer agent IP to download from',
        Val: '',
        Type: 'string',
        Required: false
      }
    },
    AgentConfig: { Exec: 'built-in', Type: 'go', IsInteractive: false }
  }
}

// Health check
app.get('/api/health', (req, res) => {
  res.json({ status: 'ok' })
})

// List agents
app.get('/api/agents', (req, res) => {
  // 更新 LastSeen
  const agents = mockAgents.map(a => ({
    ...a,
    LastSeen: new Date().toISOString()
  }))
  res.json(agents)
})

// Set active agent
app.post('/api/agents/active', (req, res) => {
  const { AgentTag } = req.body
  activeAgent = mockAgents.find(a => a.Tag === AgentTag)
  if (activeAgent) {
    res.json({ ...activeAgent, LastSeen: new Date().toISOString() })
  } else {
    res.status(404).json({ error: 'Agent not found' })
  }
})

// Send command
app.post('/api/command', (req, res) => {
  const { AgentTag, Command, JobID } = req.body
  console.log(`[Command] ${AgentTag}: ${Command} (JobID: ${JobID})`)
  
  // 模拟命令执行
  setTimeout(() => {
    broadcastMessage({
      type: 'command_output',
      data: {
        JobID,
        Response: `$ ${Command}\n`,
        Tag: AgentTag,
        Time: new Date().toISOString()
      }
    })
  }, 100)
  
  // 模拟输出
  const outputs = {
    'whoami': 'root',
    'id': 'uid=0(root) gid=0(root) groups=0(root)',
    'uname -a': 'Linux web-server-prod 5.15.0-56-generic #62-Ubuntu SMP Tue Nov 22 19:54:14 UTC 2022 x86_64 x86_64 x86_64 GNU/Linux',
    'ip addr': `1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue state UNKNOWN
    link/loopback 00:00:00:00:00:00 brd 00:00:00:00:00:00
    inet 127.0.0.1/8 scope host lo
2: eth0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc fq_codel state UP
    inet 192.168.1.100/24 brd 192.168.1.255 scope global eth0
    inet 10.0.0.5/24 brd 10.0.0.255 scope global eth0`,
    'ls -la': `total 52
drwxr-xr-x 5 root root 4096 Sep  2 10:30 .
drwxr-xr-x 3 root root 4096 Sep  2 09:15 ..
-rwxr-xr-x 1 root root 8388608 Sep  2 09:15 agent
-rw-r--r-- 1 root root  1024 Sep  2 10:30 config.json
drwxr-xr-x 2 root root 4096 Sep  2 10:00 modules`,
    'cat /etc/os-release': `PRETTY_NAME="Ubuntu 22.04.1 LTS"
NAME="Ubuntu"
VERSION_ID="22.04"
VERSION="22.04.1 LTS (Jammy Jellyfish)"
VERSION_CODENAME=jammy
ID=ubuntu
ID_LIKE=debian`
  }
  
  setTimeout(() => {
    const cmd = Command.toLowerCase().trim()
    let output = outputs[cmd] || `Command executed: ${Command}\nOutput would appear here...`
    broadcastMessage({
      type: 'command_output',
      data: {
        JobID,
        Response: output + '\n',
        Tag: AgentTag,
        Time: new Date().toISOString()
      }
    })
  }, 500)
  
  res.json({ success: true, JobID })
})

// Forget agent
app.post('/api/agents/forget', (req, res) => {
  const { AgentTag } = req.body
  console.log(`[Forget] ${AgentTag}`)
  res.json({ success: true })
})

// List modules
app.get('/api/modules', (req, res) => {
  res.json(mockModules)
})

// WebSocket
let wss

function broadcastMessage(message) {
  if (!wss) return
  const data = JSON.stringify(message)
  wss.clients.forEach(client => {
    if (client.readyState === 1) {
      client.send(data)
    }
  })
}

// Get local IP addresses for LAN access
import os from 'os';
const getLocalIPs = () => {
  const ips = [];
  const interfaces = os.networkInterfaces();
  for (const name of Object.keys(interfaces)) {
    for (const iface of interfaces[name]) {
      if (iface.family === 'IPv4' && !iface.internal) {
        ips.push(iface.address);
      }
    }
  }
  return ips;
};

// Start server - listen on all interfaces for LAN access
const server = app.listen(PORT, '0.0.0.0', () => {
  const lanIPs = getLocalIPs();
  console.log(`\n🎯 emp3r0r Mock API Server`)
  console.log(`   📡 HTTP: http://0.0.0.0:${PORT}`)
  console.log(`   🔑 Token: emp3r0r-test-token-12345`)
  console.log(`   🌐 LAN Access:`)
  lanIPs.forEach(ip => console.log(`      http://${ip}:${PORT}`))
  console.log(`   📊 Endpoints:`)
  console.log(`      GET  /api/health`)
  console.log(`      GET  /api/agents`)
  console.log(`      POST /api/agents/active`)
  console.log(`      POST /api/command`)
  console.log(`      GET  /api/modules`)
  console.log(`      WS   /api/ws`)
  console.log(`\n   Frontend: http://localhost:5173`)
  console.log(`   Press Ctrl+C to stop\n`)
})

wss = new WebSocketServer({ server, path: '/api/ws' })

wss.on('connection', (ws, req) => {
  console.log('[WebSocket] Client connected')
  
  // 发送欢迎消息
  ws.send(JSON.stringify({
    type: 'connected',
    data: { session: 'mock-session-' + Date.now() },
    timestamp: new Date().toISOString()
  }))
  
  // 发送 Agent 列表更新
  setTimeout(() => {
    broadcastMessage({
      type: 'agent_list',
      data: mockAgents.map(a => ({ ...a, LastSeen: new Date().toISOString() }))
    })
  }, 1000)
  
  ws.on('message', (data) => {
    try {
      const msg = JSON.parse(data)
      console.log('[WebSocket] Received:', msg)
      
      if (msg.type === 'ping') {
        ws.send(JSON.stringify({
          type: 'pong',
          timestamp: new Date().toISOString()
        }))
      }
    } catch (e) {
      console.error('[WebSocket] Parse error:', e)
    }
  })
  
  ws.on('close', () => {
    console.log('[WebSocket] Client disconnected')
  })
})

// 定期广播 Agent 心跳
setInterval(() => {
  broadcastMessage({
    type: 'agent_update',
    data: mockAgents.map(a => ({
      ...a,
      LastSeen: new Date().toISOString(),
      LastSeenRTT: `${Math.floor(Math.random() * 100) + 20}ms`
    }))
  })
}, 5000)
