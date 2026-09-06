// emp3r0r Agent 数据结构
export interface Emp3r0rAgent {
  Tag: string
  Name: string
  ShortID: string
  Version: string
  Transport: string
  Hostname: string
  Hardware: string
  Container: string
  Uptime: string
  Groups: string
  CPU: string
  GPU: string
  Mem: string
  OS: string
  GOOS: string
  Kernel: string
  Arch: string
  From: string
  IPs: string[]
  ARP: string[]
  User: string
  HasRoot: boolean
  HasTor: boolean
  HasInternet: boolean
  Process?: AgentProcess
  CWD: string
  UUID: string
  PublicKey: string
  C2Host: string
  LastSeen: string
  LastSeenRTT: string
  MeshRoute: string
  Files: string[]
}

export interface AgentProcess {
  PID: number
  PPID: number
  Cmdline: string
  Parent: string
}

// 模块配置
export interface ModuleOption {
  Name: string
  Desc: string
  Val: string
  Vals?: string[]
  Type: string
  Required: boolean
  Secret?: boolean
}

export interface ModuleConfig {
  Name: string
  Author: string
  Date: string
  Comment: string
  IsLocal: boolean
  Platform: string
  Fileless: boolean
  Options: Record<string, ModuleOption>
  AgentConfig: {
    Exec: string
    Type: string
    IsInteractive: boolean
  }
}

// 命令操作
export interface Operation {
  AgentTag: string
  Action: string
  Command?: string
  JobID?: string
  Options?: Record<string, string>
}

// 消息隧道数据
export interface MsgTunData {
  JobID: string
  CmdSlice: string[]
  Response: string
  Tag: string
  Time: string
}

// WebSocket 消息类型
export enum WSMessageType {
  AGENT_LIST = 'agent_list',
  AGENT_UPDATE = 'agent_update',
  COMMAND_OUTPUT = 'command_output',
  PTY_OUTPUT = 'pty_output',
  LOG = 'log',
  ERROR = 'error',
}

export interface WSMessage {
  type: WSMessageType
  data: any
  timestamp: string
}
