import { useState, useRef, useEffect } from 'react'
import { useStore } from '@/stores/useStore'
import { api } from '@/lib/api'
import { Send, Trash2, Copy, Download, Terminal, MonitorPlay, MonitorX } from 'lucide-react'
import { PtyTerminal } from './PtyTerminal'

interface CommandJob {
  id: string
  command: string
  output: string[]
  status: 'pending' | 'running' | 'completed' | 'error'
  startedAt: number
}

// 超过该秒数仍未返回，提示用户命令可能因 agent 轮询/网络而延迟或丢失（但仍继续等待结果）
const STALE_THRESHOLD_SECONDS = 30

export function CommandConsole() {
  const { activeAgent } = useStore()
  const [command, setCommand] = useState('')
  const [history, setHistory] = useState<string[]>([])
  const [historyIndex, setHistoryIndex] = useState(-1)
  const [jobs, setJobs] = useState<CommandJob[]>([])
  const [currentJobId, setCurrentJobId] = useState<string | null>(null)
  const [ptySessionId, setPtySessionId] = useState<string | null>(null)
  const outputRef = useRef<HTMLDivElement>(null)
  const inputRef = useRef<HTMLInputElement>(null)

  // Auto-scroll to bottom
  useEffect(() => {
    if (outputRef.current) {
      outputRef.current.scrollTop = outputRef.current.scrollHeight
    }
  }, [jobs])

  // 每秒 tick，驱动 running job 的等待计时（用于显示已等待秒数和超时提示）
  const [, setTick] = useState(0)
  useEffect(() => {
    const interval = setInterval(() => setTick(t => t + 1), 1000)
    return () => clearInterval(interval)
  }, [])

  // 监听 WebSocket 消息
  useEffect(() => {
    const handleCommandOutput = (e: CustomEvent) => {
      const { jobID, response } = e.detail
      setJobs(prev => prev.map(job => {
        if (job.id === jobID) {
          return {
            ...job,
            output: [...job.output, response],
            status: 'completed'
          }
        }
        return job
      }))
    }

    window.addEventListener('command-output', handleCommandOutput as EventListener)
    return () => window.removeEventListener('command-output', handleCommandOutput as EventListener)
  }, [])

  // Quick commands
  const quickCommands = [
    { label: 'whoami', cmd: 'whoami' },
    { label: 'uname', cmd: 'uname -a' },
    { label: 'ls -la', cmd: 'ls -la' },
    { label: 'ip addr', cmd: 'ip addr' },
    { label: 'ifconfig', cmd: 'ifconfig' },
    { label: 'id', cmd: 'id' },
    { label: 'pwd', cmd: 'pwd' },
    { label: 'cat /etc/os-release', cmd: 'cat /etc/os-release' },
  ]

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!command.trim() || !activeAgent) return

    const jobId = `cmd-${Date.now()}`
    const rawCmd = command.trim()

    // Add to history (what user typed)
    setHistory((prev) => [...prev, rawCmd])
    setHistoryIndex(-1)

    // Display the raw command in UI
    const newJob: CommandJob = {
      id: jobId,
      command: rawCmd,
      output: [`$ ${rawCmd}`],
      status: 'running',
      startedAt: Date.now()
    }
    setJobs(prev => [...prev, newJob])
    setCurrentJobId(jobId)
    setCommand('')

    // Wrap non-builtin commands with exec --cmd "..." so they run on target shell
    // Builtins: ls, ps, pwd, net_helper, cd, kill, exec, and any starting with '!'
    let effectiveCmd = rawCmd
    if (!rawCmd.startsWith('!') && !/^(ls|ps|pwd|net_helper|cd|kill|exec)\b/.test(rawCmd)) {
      effectiveCmd = `exec --cmd ${JSON.stringify(rawCmd)}`
    }

    try {
      await api.sendCommand({
        AgentTag: activeAgent.Tag,
        Action: 'command',
        Command: effectiveCmd,
        JobID: jobId,
      })
    } catch (error) {
      setJobs(prev => prev.map(job => {
        if (job.id === jobId) {
          return {
            ...job,
            output: [...job.output, `Error: ${error}`],
            status: 'error'
          }
        }
        return job
      }))
    }
  }

  const handleKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'ArrowUp') {
      e.preventDefault()
      if (history.length > 0) {
        const newIndex = historyIndex === -1 ? history.length - 1 : Math.max(0, historyIndex - 1)
        setHistoryIndex(newIndex)
        setCommand(history[newIndex])
      }
    } else if (e.key === 'ArrowDown') {
      e.preventDefault()
      if (historyIndex !== -1) {
        const newIndex = historyIndex + 1
        if (newIndex >= history.length) {
          setHistoryIndex(-1)
          setCommand('')
        } else {
          setHistoryIndex(newIndex)
          setCommand(history[newIndex])
        }
      }
    }
  }

  const currentJob = jobs.find(j => j.id === currentJobId)
  const currentOutput = currentJob?.output || []

  const handleCopy = () => {
    navigator.clipboard.writeText(currentOutput.join('\n'))
  }

  const handleDownload = () => {
    const blob = new Blob([currentOutput.join('\n')], { type: 'text/plain' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `console-${activeAgent?.Tag || 'output'}.txt`
    a.click()
    URL.revokeObjectURL(url)
  }

  const handleClear = () => {
    if (currentJobId) {
      setJobs(prev => prev.filter(j => j.id !== currentJobId))
      setCurrentJobId(null)
    }
  }

  // Start an interactive PTY shell session on the target agent
  const [ptyConnecting, setPtyConnecting] = useState(false)
  const handlePtyStart = async () => {
    if (!activeAgent) return
    const sessionId = `pty-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`
    setPtyConnecting(true)
    setPtySessionId(sessionId)
    // Bootstrap the session with !shell; output of the start itself is a
    // pty_output frame which the terminal will render.
    try {
      await api.sendCommand({
        AgentTag: activeAgent.Tag,
        Action: 'command',
        Command: `!shell -s --job_id ${sessionId}`,
        JobID: sessionId,
      })
    } catch (error) {
      console.error('Failed to start PTY session:', error)
      setPtySessionId(null)
    } finally {
      setPtyConnecting(false)
    }
  }

  const handlePtyClose = () => {
    // PtyTerminal already sends pty_close over WS; just clear local state
    setPtySessionId(null)
  }

  return (
    <div className="h-full flex flex-col pb-16 md:pb-0">
      {/* Header */}
      <div className="p-3 md:p-4 border-b border-gray-800">
        <div className="flex items-center justify-between">
          <div>
            <h2 className="text-base md:text-lg font-semibold text-gray-100">Command Console</h2>
            {activeAgent && (
              <p className="text-xs md:text-sm text-gray-500">
                Target: {activeAgent.Hostname || activeAgent.Tag}
              </p>
            )}
          </div>
          <div className="flex items-center gap-2">
            <button
              onClick={ptySessionId ? handlePtyClose : handlePtyStart}
              disabled={!activeAgent || ptyConnecting}
              className={`px-2 py-1 md:px-3 md:py-1.5 rounded-lg text-xs font-medium transition-colors flex items-center gap-1 disabled:opacity-40 disabled:cursor-not-allowed ${
                ptySessionId
                  ? 'bg-red-900/30 text-red-400 hover:bg-red-900/50'
                  : 'bg-gray-800 text-emerald-400 hover:bg-gray-700'
              }`}
              title={!activeAgent
                ? '请先在 Agents 页选择目标 agent'
                : ptySessionId ? 'Close interactive shell' : 'Open interactive PTY shell'}
            >
              {ptySessionId ? <MonitorX className="w-3.5 h-3.5" /> : <MonitorPlay className="w-3.5 h-3.5" />}
              {!activeAgent ? 'Interactive TTY (先选 agent)'
                : ptyConnecting ? 'Connecting...'
                : ptySessionId ? 'Close TTY' : 'Interactive TTY'}
            </button>
            {currentOutput.length > 0 && (
              <>
                <button
                  onClick={handleCopy}
                  className="p-2 hover:bg-gray-800 rounded-lg transition-colors text-gray-400"
                  title="Copy output"
                >
                  <Copy className="w-4 h-4" />
                </button>
                <button
                  onClick={handleDownload}
                  className="p-2 hover:bg-gray-800 rounded-lg transition-colors text-gray-400"
                  title="Download output"
                >
                  <Download className="w-4 h-4" />
                </button>
                <button
                  onClick={handleClear}
                  className="p-2 hover:bg-gray-800 rounded-lg transition-colors text-gray-400"
                  title="Clear output"
                >
                  <Trash2 className="w-4 h-4" />
                </button>
              </>
            )}
          </div>
        </div>
      </div>

      {/* Job List - Shows all commands */}
      {jobs.length > 0 && (
        <div className="px-3 md:px-4 py-2 border-b border-gray-800 bg-gray-900/50">
          <div className="flex items-center gap-2 overflow-x-auto pb-2 scrollbar-hide">
            <span className="text-xs text-gray-500 whitespace-nowrap">Commands:</span>
            {jobs.map((job) => (
              <button
                key={job.id}
                onClick={() => setCurrentJobId(job.id)}
                className={`px-2 py-1 text-xs rounded-lg transition-colors whitespace-nowrap ${
                  currentJobId === job.id
                    ? 'bg-emp3r0r-500/20 text-emp3r0r-400'
                    : 'bg-gray-800 text-gray-400 hover:bg-gray-700'
                }`}
              >
                <span className="font-mono">{job.command.slice(0, 20)}{job.command.length > 20 ? '...' : ''}</span>
                {job.status === 'running' && (
                  <span className="ml-1 animate-pulse">●</span>
                )}
                {job.status === 'completed' && (
                  <span className="ml-1 text-green-500">✓</span>
                )}
                {job.status === 'error' && (
                  <span className="ml-1 text-red-500">✗</span>
                )}
              </button>
            ))}
          </div>
        </div>
      )}

      {/* Output Area */}
      {ptySessionId && activeAgent ? (
        <div className="flex-1 min-h-0">
          <PtyTerminal
            agentTag={activeAgent.Tag}
            jobId={ptySessionId}
            onClose={handlePtyClose}
          />
        </div>
      ) : (
      <div
        ref={outputRef}
        className="flex-1 overflow-y-auto p-3 md:p-4 font-mono text-xs md:text-sm bg-gray-950"
      >
        {!activeAgent ? (
          <div className="flex flex-col items-center justify-center h-full text-gray-500">
            <Terminal className="w-8 h-8 md:w-12 md:h-12 mb-4 opacity-30" />
            <p className="text-sm md:text-base">Select an agent from the Agents tab</p>
            <p className="text-xs mt-1">to start executing commands</p>
          </div>
        ) : currentOutput.length === 0 ? (
          <div className="text-gray-600">
            <p className="text-xs md:text-sm">Ready to execute commands on {activeAgent.Hostname || activeAgent.Tag}</p>
            <p className="text-xs mt-2">Type a command below or use quick commands</p>
          </div>
        ) : (
          <div className="space-y-1">
            {currentOutput.map((line, i) => (
              <div
                key={i}
                className={`whitespace-pre-wrap break-all ${
                  line.startsWith('$') ? 'text-emp3r0r-400 font-semibold' : 'text-gray-300'
                }`}
              >
                {line}
              </div>
            ))}
            {currentJob?.status === 'running' && (() => {
              const elapsed = Math.floor((Date.now() - currentJob.startedAt) / 1000)
              const stale = elapsed > STALE_THRESHOLD_SECONDS
              return (
                <div className={stale ? 'text-amber-400 animate-pulse' : 'text-gray-500 animate-pulse'}>
                  Executing... ({elapsed}s)
                  {stale && (
                    <span className="ml-2 text-xs text-amber-500/80">
                      ⚠ 长时间未返回：agent 可能处于长轮询/离线窗口，或命令已丢失。可点击上方命令重新发送。
                    </span>
                  )}
                </div>
              )
            })()}
          </div>
        )}
      </div>
      )}

      {/* Quick Commands - Scrollable on mobile */}
      {!ptySessionId && (
      <>
      <div className="px-3 md:px-4 py-2 border-t border-gray-800 bg-gray-900/50">
        <div className="flex items-center gap-2 overflow-x-auto pb-2 scrollbar-hide">
          <span className="text-xs text-gray-500 whitespace-nowrap hidden md:inline">Quick:</span>
          {quickCommands.map((qc) => (
            <button
              key={qc.cmd}
              onClick={() => setCommand(qc.cmd)}
              disabled={!activeAgent}
              className="px-2 md:px-3 py-1 md:py-1.5 bg-gray-800 hover:bg-gray-700 disabled:opacity-50 disabled:cursor-not-allowed text-xs text-gray-300 rounded-lg transition-colors whitespace-nowrap"
            >
              {qc.label}
            </button>
          ))}
        </div>
      </div>

      {/* Input Area */}
      <div className="p-3 md:p-4 border-t border-gray-800 bg-gray-900">
        <form onSubmit={handleSubmit} className="flex items-center gap-2">
          <div className="flex-1 relative">
            <span className="absolute left-3 top-1/2 -translate-y-1/2 text-emp3r0r-500 text-sm md:text-base">$</span>
            <input
              ref={inputRef}
              type="text"
              value={command}
              onChange={(e) => setCommand(e.target.value)}
              onKeyDown={handleKeyDown}
              placeholder={activeAgent ? 'Enter command...' : 'Select an agent first'}
              disabled={!activeAgent}
              className="w-full bg-gray-800 border border-gray-700 rounded-lg pl-8 pr-4 py-2 md:py-3 text-sm md:text-base text-gray-100 placeholder-gray-500 focus:outline-none focus:border-emp3r0r-500 disabled:opacity-50 disabled:cursor-not-allowed"
              autoFocus
            />
          </div>
          <button
            type="submit"
            disabled={!command.trim() || !activeAgent || currentJob?.status === 'running'}
            className="p-2 md:p-3 bg-emp3r0r-500 hover:bg-emp3r0r-600 disabled:opacity-50 disabled:cursor-not-allowed text-white rounded-lg transition-colors"
          >
            <Send className="w-4 h-4 md:w-5 md:h-5" />
          </button>
        </form>
      </div>
      </>
      )}
    </div>
  )
}
