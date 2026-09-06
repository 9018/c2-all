import { useEffect } from 'react'
import { wsClient } from '@/lib/websocket'
import { useStore } from '@/stores/useStore'
import { WSMessageType } from '@/types'

export function useWebSocket() {
  const { token, addLog, fetchAgents } = useStore()

  useEffect(() => {
    if (!token) return

    // Keep handler references so the cleanup can remove them; otherwise
    // handlers accumulate and every message is processed N times (double
    // PTY echo, duplicated logs, ...).
    const handleMessage = (msg: { type: WSMessageType; data: any }) => {
      switch (msg.type) {
        case WSMessageType.AGENT_LIST:
        case WSMessageType.AGENT_UPDATE:
          fetchAgents()
          break
        case WSMessageType.COMMAND_OUTPUT:
          if (msg.data?.JobID && msg.data?.Response) {
            // 触发自定义事件，让 CommandConsole 组件处理
            window.dispatchEvent(new CustomEvent('command-output', {
              detail: {
                jobID: msg.data.JobID,
                response: msg.data.Response
              }
            }))
          }
          break
        case WSMessageType.PTY_OUTPUT:
          if (msg.data?.JobID && msg.data?.Data) {
            // PTY 输出是 base64 编码的原始字节流（可能含 ANSI），交给 PtyTerminal 解码
            window.dispatchEvent(new CustomEvent('pty-output', {
              detail: {
                jobID: msg.data.JobID,
                data: msg.data.Data
              }
            }))
          }
          break
        case WSMessageType.LOG:
          addLog(msg.data.level || 'info', msg.data.message || '')
          break
        case WSMessageType.ERROR:
          addLog('error', msg.data.message || 'Unknown error')
          break
      }
    }

    const messageTypes = [
      WSMessageType.AGENT_LIST,
      WSMessageType.AGENT_UPDATE,
      WSMessageType.COMMAND_OUTPUT,
      WSMessageType.PTY_OUTPUT,
      WSMessageType.LOG,
      WSMessageType.ERROR,
    ] as const
    messageTypes.forEach((t) => wsClient.on(t, handleMessage))

    wsClient.connect(token)

    return () => {
      messageTypes.forEach((t) => wsClient.off(t, handleMessage))
      wsClient.disconnect()
    }
  }, [token, fetchAgents, addLog])

  return {
    send: wsClient.send.bind(wsClient),
  }
}
