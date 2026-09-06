import { Emp3r0rAgent, Operation, ModuleConfig } from '@/types'

const API_BASE = '/api'

class ApiClient {
  private token: string = ''

  setToken(token: string) {
    this.token = token
  }

  private async request<T>(path: string, options?: RequestInit): Promise<T> {
    const headers: Record<string, string> = {
      'Content-Type': 'application/json',
      ...(this.token && { Authorization: `Bearer ${this.token}` }),
    }

    const response = await fetch(`${API_BASE}${path}`, {
      ...options,
      headers: { ...headers, ...options?.headers },
    })

    if (!response.ok) {
      const error = await response.text()
      throw new Error(error || response.statusText)
    }

    return response.json()
  }

  private async requestVoid(path: string, options?: RequestInit): Promise<void> {
    const headers: Record<string, string> = {
      'Content-Type': 'application/json',
      ...(this.token && { Authorization: `Bearer ${this.token}` }),
    }

    const response = await fetch(`${API_BASE}${path}`, {
      ...options,
      headers: { ...headers, ...options?.headers },
    })

    if (!response.ok) {
      const error = await response.text()
      throw new Error(error || response.statusText)
    }
    // no body expected
  }

  // Agent API
  async getAgents(): Promise<Emp3r0rAgent[]> {
    return this.request<Emp3r0rAgent[]>('/agents')
  }

  async setActiveAgent(tag: string): Promise<Emp3r0rAgent> {
    return this.request<Emp3r0rAgent>('/agents/active', {
      method: 'POST',
      body: JSON.stringify({ AgentTag: tag }),
    })
  }

  async sendCommand(operation: Operation): Promise<void> {
    const headers: Record<string, string> = {
      'Content-Type': 'application/json',
      ...(this.token && { Authorization: `Bearer ${this.token}` }),
    }

    const response = await fetch(`${API_BASE}/command`, {
      method: 'POST',
      headers,
      body: JSON.stringify(operation),
    })

    if (!response.ok) {
      const error = await response.text()
      throw new Error(error || response.statusText)
    }
    // 200 OK with no body - don't try to parse JSON
  }

  async forgetAgent(uuid: string): Promise<void> {
    await this.request('/agents/forget', {
      method: 'POST',
      body: JSON.stringify({ AgentTag: uuid }),
    })
  }

  // Module API
  async getModules(): Promise<Record<string, ModuleConfig>> {
    return this.request<Record<string, ModuleConfig>>('/modules')
  }

  // File Management API
  async listFiles(agentTag: string, path: string): Promise<Array<{name: string, ftype: string, size: string, date: string, perm: string}>> {
    return this.request<Array<{name: string, ftype: string, size: string, date: string, perm: string}>>('/ls', {
      method: 'POST',
      body: JSON.stringify({ AgentTag: agentTag, Path: path }),
    })
  }

  async removePath(agentTag: string, path: string): Promise<void> {
    await this.requestVoid('/rm', {
      method: 'POST',
      body: JSON.stringify({ AgentTag: agentTag, Path: path }),
    })
  }

  async makeDir(agentTag: string, path: string): Promise<void> {
    await this.requestVoid('/mkdir', {
      method: 'POST',
      body: JSON.stringify({ AgentTag: agentTag, Path: path }),
    })
  }

  async downloadFile(agentTag: string, filePath: string): Promise<Blob> {
    const headers: Record<string, string> = {
      ...(this.token && { Authorization: `Bearer ${this.token}` }),
    }

    const response = await fetch(`${API_BASE}/download`, {
      method: 'POST',
      headers,
      body: JSON.stringify({ AgentTag: agentTag, FilePath: filePath }),
    })

    if (!response.ok) {
      throw new Error('Download failed')
    }

    return response.blob()
  }

  async uploadFile(agentTag: string, filePath: string, content: string): Promise<void> {
    await this.request('/upload', {
      method: 'POST',
      body: JSON.stringify({ AgentTag: agentTag, FilePath: filePath, Content: content }),
    })
  }

  async stat(agentTag: string, path: string): Promise<{ name: string; permission: string; checksum: string; size: number }>{
    return this.request('/stat', {
      method: 'POST',
      body: JSON.stringify({ AgentTag: agentTag, Path: path }),
    })
  }

  async copy(agentTag: string, src: string, dst: string): Promise<void> {
    await this.requestVoid('/cp', {
      method: 'POST',
      body: JSON.stringify({ AgentTag: agentTag, Src: src, Dst: dst }),
    })
  }

  async move(agentTag: string, src: string, dst: string): Promise<void> {
    await this.requestVoid('/mv', {
      method: 'POST',
      body: JSON.stringify({ AgentTag: agentTag, Src: src, Dst: dst }),
    })
  }

  // 连接测试
  async healthCheck(): Promise<boolean> {
    try {
      await this.request('/health')
      return true
    } catch {
      return false
    }
  }
}

export const api = new ApiClient()
