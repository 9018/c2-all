import { create } from 'zustand'
import { Emp3r0rAgent, ModuleConfig } from '@/types'
import { api } from '@/lib/api'

interface AppState {
  // 连接状态
  connected: boolean
  setConnected: (connected: boolean) => void

  // 认证
  token: string
  setToken: (token: string) => void

  // Agent 管理
  agents: Emp3r0rAgent[]
  activeAgent: Emp3r0rAgent | null
  loadingAgents: boolean
  fetchAgents: () => Promise<void>
  setActiveAgent: (tag: string) => Promise<void>
  forgetAgent: (uuid: string) => Promise<void>

  // 模块
  modules: Record<string, ModuleConfig>
  loadingModules: boolean
  fetchModules: () => Promise<void>

  // 命令输出
  commandOutputs: Record<string, string[]>
  addCommandOutput: (jobId: string, output: string) => void
  clearCommandOutput: (jobId: string) => void

  // 日志
  logs: Array<{ time: string; level: string; message: string }>
  addLog: (level: string, message: string) => void
  clearLogs: () => void

  // UI 状态
  sidebarOpen: boolean
  toggleSidebar: () => void
}

export const useStore = create<AppState>((set, get) => ({
  // 连接状态
  connected: false,
  setConnected: (connected) => set({ connected }),

  // 认证 - 从 localStorage 恢复 token
  // 刷新页面时 store 从 localStorage 恢复 token，但 api.setToken() 只在登录时
  // 调用过 —— 不同步的话，REST 客户端没有 token，所有请求 401 直到手动重新登录。
  token: (() => {
    const t = localStorage.getItem('emp3r0r_token') || ''
    if (t) api.setToken(t)
    return t
  })(),
  setToken: (token) => {
    api.setToken(token)
    set({ token })
    if (token) {
      localStorage.setItem('emp3r0r_token', token)
    } else {
      localStorage.removeItem('emp3r0r_token')
    }
  },

  // Agent 管理
  agents: [],
  activeAgent: null,
  loadingAgents: false,
  fetchAgents: async () => {
    set({ loadingAgents: true })
    try {
      const agents = await api.getAgents()
      set({ agents, loadingAgents: false })
    } catch (error) {
      console.error('Failed to fetch agents:', error)
      set({ loadingAgents: false })
    }
  },
  setActiveAgent: async (tag) => {
    try {
      const agent = await api.setActiveAgent(tag)
      set({ activeAgent: agent })
    } catch (error) {
      console.error('Failed to set active agent:', error)
    }
  },
  forgetAgent: async (uuid) => {
    try {
      await api.forgetAgent(uuid)
      const { agents, activeAgent } = get()
      set({
        agents: agents.filter((a) => a.UUID !== uuid),
        // 被删的 agent 若是当前选中目标，清空选择，否则 Console/PTY 会
        // 静默指向已删除的 agent（TTY 按钮无效且无提示）
        activeAgent: activeAgent?.UUID === uuid ? null : activeAgent,
      })
    } catch (error) {
      console.error('Failed to forget agent:', error)
    }
  },

  // 模块
  modules: {},
  loadingModules: false,
  fetchModules: async () => {
    set({ loadingModules: true })
    try {
      const modules = await api.getModules()
      set({ modules, loadingModules: false })
    } catch (error) {
      console.error('Failed to fetch modules:', error)
      set({ loadingModules: false })
    }
  },

  // 命令输出
  commandOutputs: {},
  addCommandOutput: (jobId, output) => {
    const { commandOutputs } = get()
    const existing = commandOutputs[jobId] || []
    set({
      commandOutputs: {
        ...commandOutputs,
        [jobId]: [...existing, output],
      },
    })
  },
  clearCommandOutput: (jobId) => {
    const { commandOutputs } = get()
    const { [jobId]: _, ...rest } = commandOutputs
    set({ commandOutputs: rest })
  },

  // 日志
  logs: [],
  addLog: (level, message) => {
    const { logs } = get()
    set({
      logs: [...logs.slice(-999), { time: new Date().toISOString(), level, message }],
    })
  },
  clearLogs: () => set({ logs: [] }),

  // UI 状态
  sidebarOpen: true,
  toggleSidebar: () => set((state) => ({ sidebarOpen: !state.sidebarOpen })),
}))
