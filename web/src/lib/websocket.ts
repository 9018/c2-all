import { WSMessage, WSMessageType } from '@/types'

type MessageHandler = (message: WSMessage) => void

class WebSocketClient {
  private ws: WebSocket | null = null
  private handlers: Map<WSMessageType, MessageHandler[]> = new Map()
  private reconnectAttempts = 0
  private maxReconnectAttempts = 10
  private reconnectDelay = 1000
  private intentionalClose = false

  connect(sessionId: string) {
    // Tear down any existing socket first: two live sockets would each
    // deliver the same broadcast, doubling every message (e.g. PTY echo).
    if (this.ws) {
      this.intentionalClose = true
      try {
        this.ws.close()
      } catch {
        /* already closed */
      }
      this.ws = null
    }
    this.intentionalClose = false

    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
    const wsUrl = `${protocol}//${window.location.host}/api/ws?session=${sessionId}`

    const ws = new WebSocket(wsUrl)
    this.ws = ws
    ws.binaryType = 'arraybuffer'

    ws.onopen = () => {
      console.log('WebSocket connected')
      this.reconnectAttempts = 0
    }

    ws.onmessage = (event) => {
      // Stale sockets must not dispatch: only the current one may deliver.
      if (ws !== this.ws) return
      try {
        // 服务器发送 JSON 格式的消息
        const message: WSMessage = JSON.parse(event.data)
        this.handleMessage(message)
      } catch (e) {
        console.error('Failed to parse WebSocket message:', e)
      }
    }

    ws.onclose = () => {
      // Only the current socket may trigger reconnects; never reconnect
      // after an intentional disconnect().
      if (ws !== this.ws || this.intentionalClose) return
      console.log('WebSocket disconnected')
      this.attemptReconnect(sessionId)
    }

    ws.onerror = (error) => {
      console.error('WebSocket error:', error)
    }
  }

  private handleMessage(message: WSMessage) {
    const handlers = this.handlers.get(message.type) || []
    handlers.forEach((handler) => handler(message))
  }

  private attemptReconnect(sessionId: string) {
    if (this.reconnectAttempts >= this.maxReconnectAttempts) {
      console.error('Max reconnection attempts reached')
      return
    }

    this.reconnectAttempts++
    const delay = this.reconnectDelay * Math.pow(2, this.reconnectAttempts - 1)

    setTimeout(() => {
      console.log(`Attempting to reconnect (${this.reconnectAttempts}/${this.maxReconnectAttempts})...`)
      this.connect(sessionId)
    }, delay)
  }

  on(type: WSMessageType, handler: MessageHandler) {
    if (!this.handlers.has(type)) {
      this.handlers.set(type, [])
    }
    this.handlers.get(type)!.push(handler)
  }

  off(type: WSMessageType, handler: MessageHandler) {
    const handlers = this.handlers.get(type)
    if (handlers) {
      const index = handlers.indexOf(handler)
      if (index > -1) {
        handlers.splice(index, 1)
      }
    }
  }

  send(data: any) {
    if (this.ws?.readyState === WebSocket.OPEN) {
      this.ws.send(JSON.stringify(data))
    }
  }

  disconnect() {
    // Mark as intentional so onclose skips attemptReconnect.
    this.intentionalClose = true
    if (this.ws) {
      this.ws.close()
      this.ws = null
    }
  }
}

export const wsClient = new WebSocketClient()
