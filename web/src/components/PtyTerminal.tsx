import { useEffect, useRef } from 'react'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'
import { wsClient } from '@/lib/websocket'

interface PtyTerminalProps {
  agentTag: string
  jobId: string
  onClose: () => void
}

/**
 * PtyTerminal renders an interactive xterm.js terminal bound to an agent-side
 * PTY shell session (!shell). Keystrokes are forwarded to the agent as base64
 * over the WebSocket (pty_input), and raw PTY output arrives via pty_output
 * WebSocket messages which are decoded and written into the terminal.
 */
export function PtyTerminal({ agentTag, jobId, onClose }: PtyTerminalProps) {
  const containerRef = useRef<HTMLDivElement>(null)
  const termRef = useRef<Terminal | null>(null)
  const fitRef = useRef<FitAddon | null>(null)

  useEffect(() => {
    if (!containerRef.current) return

    const term = new Terminal({
      cursorBlink: true,
      fontSize: 13,
      fontFamily: 'Menlo, Monaco, "Courier New", monospace',
      theme: {
        background: '#0a0a0a',
        foreground: '#d4d4d4',
        cursor: '#ffffff',
      },
      scrollback: 5000,
      allowProposedApi: false,
    })
    const fit = new FitAddon()
    term.loadAddon(fit)
    term.open(containerRef.current)
    fit.fit()
    termRef.current = term
    fitRef.current = fit

    // Forward keystrokes to the agent PTY session
    const dataDisposable = term.onData((data) => {
      wsClient.send({
        type: 'pty_input',
        data: {
          AgentTag: agentTag,
          JobID: jobId,
          Data: btoa(unescape(encodeURIComponent(data))),
        },
      })
    })

    // Write incoming PTY output into the terminal
    const handlePtyOutput = (e: CustomEvent) => {
      const { jobID, data } = e.detail
      if (jobID !== jobId) return
      try {
        const bytes = atob(data)
        const binary = new Uint8Array(bytes.length)
        for (let i = 0; i < bytes.length; i++) {
          binary[i] = bytes.charCodeAt(i)
        }
        term.write(binary)
      } catch (err) {
        console.error('Failed to decode PTY output:', err)
      }
    }
    window.addEventListener('pty-output', handlePtyOutput as EventListener)

    // Resize the agent PTY to match the on-screen terminal
    const doResize = () => {
      if (!termRef.current || !fitRef.current) return
      try {
        fitRef.current.fit()
        const dims = termRef.current.cols + 'x' + termRef.current.rows
        wsClient.send({
          type: 'pty_resize',
          data: {
            AgentTag: agentTag,
            JobID: jobId,
            Data: dims,
          },
        })
      } catch (err) {
        // ignore resize when element is hidden
      }
    }
    const resizeObserver = new ResizeObserver(doResize)
    if (containerRef.current) {
      resizeObserver.observe(containerRef.current)
    }
    const timer = setTimeout(doResize, 300)

    return () => {
      dataDisposable.dispose()
      window.removeEventListener('pty-output', handlePtyOutput as EventListener)
      resizeObserver.disconnect()
      clearTimeout(timer)
      term.dispose()
      termRef.current = null
      fitRef.current = null
    }
  }, [agentTag, jobId])

  // Close session: tell the agent to kill the PTY, then unmount
  const handleClose = () => {
    wsClient.send({
      type: 'pty_close',
      data: {
        AgentTag: agentTag,
        JobID: jobId,
        Data: '',
      },
    })
    onClose()
  }

  return (
    <div className="flex flex-col h-full min-h-0">
      <div className="flex items-center justify-between px-3 py-1.5 bg-gray-900 border-b border-gray-800 text-xs text-gray-400">
        <span className="font-mono flex items-center gap-2">
          <span className="w-2 h-2 rounded-full bg-green-500 animate-pulse" />
          Interactive PTY &mdash; {agentTag}
        </span>
        <div className="flex items-center gap-2">
          <span className="hidden md:inline text-gray-600">点 x 关闭可发送 exit</span>
          <button
            onClick={handleClose}
            className="px-2 py-0.5 bg-gray-800 hover:bg-red-900/50 text-gray-300 hover:text-red-400 rounded transition-colors"
            title="Close shell session"
          >
            ✕
          </button>
        </div>
      </div>
      <div ref={containerRef} className="flex-1 min-h-0 bg-[#0a0a0a] p-1" />
    </div>
  )
}