import { useEffect, useState } from 'react'
import { useStore } from '@/stores/useStore'
import { api } from '@/lib/api'
import { 
  Monitor, 
  Shield,
  Wifi,
  RefreshCw,
  Trash2,
  ChevronRight,
  Globe,
  X,
  Terminal
} from 'lucide-react'
import { Emp3r0rAgent } from '@/types'

function AgentCard({ agent, isActive, onSelect, onConsole }: { 
  agent: Emp3r0rAgent
  isActive: boolean
  onSelect: () => void
  onConsole: () => void
}) {
  const lastSeen = new Date(agent.LastSeen)
  // Online window matches the agent hello cadence: relay transports back
  // off to a 2-4min liveness probe (DO request billing), so a healthy
  // idle agent's LastSeen can be minutes old.
  const isOnline = Date.now() - lastSeen.getTime() < 5 * 60 * 1000

  // External IP retest: fire-and-forget command; the agent_update WS
  // broadcast refreshes the store, which flips the button back via effect.
  const [retesting, setRetesting] = useState(false)
  useEffect(() => {
    if (retesting && agent.ExternalIP) setRetesting(false)
  }, [agent.ExternalIP, retesting])
  const retestExternalIP = async (e: React.MouseEvent) => {
    e.stopPropagation()
    if (retesting) return
    setRetesting(true)
    try {
      await api.sendCommand({
        AgentTag: agent.Tag,
        Action: 'command',
        Command: '!extip --quiet',
        JobID: crypto.randomUUID(),
      })
    } catch {
      setRetesting(false)
    }
    // safety net: never stay stuck if the agent never answers
    setTimeout(() => setRetesting(false), 15000)
  }

  return (
    <div
      onClick={onSelect}
      className={`p-3 md:p-4 rounded-xl border cursor-pointer transition-all active:scale-[0.98] ${
        isActive
          ? 'border-emp3r0r-500 bg-emp3r0r-500/10'
          : 'border-gray-800 bg-gray-900 hover:border-gray-700'
      }`}
    >
      <div className="flex items-center justify-between">
        <div className="flex items-center gap-3">
          <div
            className={`w-10 h-10 md:w-12 md:h-12 rounded-xl flex items-center justify-center ${
              isOnline 
                ? agent.GOOS === 'windows' ? 'bg-blue-500/20' : 'bg-emp3r0r-500/20'
                : 'bg-gray-800'
            }`}
          >
            <Monitor
              className={`w-5 h-5 md:w-6 md:h-6 ${
                isOnline 
                  ? agent.GOOS === 'windows' ? 'text-blue-500' : 'text-emp3r0r-500'
                  : 'text-gray-600'
              }`}
            />
          </div>
          <div>
            <div className="flex items-center gap-2">
              <h3 className="font-medium text-gray-100 text-sm md:text-base">
                {agent.Hostname || agent.Tag}
              </h3>
              {agent.HasRoot && (
                <Shield className="w-3 h-3 md:w-4 md:h-4 text-yellow-500" />
              )}
            </div>
            <p className="text-xs text-gray-500 font-mono">{agent.Tag}</p>
          </div>
        </div>
        <div className="flex items-center gap-2">
          <span className={`w-2 h-2 rounded-full ${isOnline ? 'bg-green-500' : 'bg-gray-600'}`} />
          <ChevronRight className="w-4 h-4 md:w-5 md:h-5 text-gray-600" />
        </div>
      </div>

      {/* Agent Info - Compact on mobile */}
      <div className="mt-3 grid grid-cols-2 gap-2 text-xs md:text-sm">
        <div className="flex items-center gap-1.5 text-gray-400">
          <Globe className="w-3 h-3 md:w-4 md:h-4 text-gray-500" />
          <span className="truncate">{agent.GOOS}/{agent.Arch}</span>
        </div>
        <div className="flex items-center gap-1.5 text-gray-400">
          <Wifi className="w-3 h-3 md:w-4 md:h-4 text-gray-500" />
          <span className="truncate" title={agent.ExternalIP || agent.From}>
            {agent.ExternalIP || agent.From}
          </span>
        </div>
      </div>

      {/* Action Buttons */}
      <div className="mt-3 flex gap-2">
        <button
          onClick={(e) => {
            e.stopPropagation()
            onConsole()
          }}
          className="flex-1 flex items-center justify-center gap-2 px-3 py-2 bg-emp3r0r-500/20 hover:bg-emp3r0r-500/30 text-emp3r0r-400 rounded-lg transition-colors text-xs md:text-sm"
        >
          <Terminal className="w-3 h-3 md:w-4 md:h-4" />
          <span>Console</span>
        </button>
        <button
          onClick={retestExternalIP}
          disabled={retesting}
          title="Retest external IP (agent queries public echo services)"
          className="flex items-center justify-center gap-2 px-3 py-2 bg-gray-800 hover:bg-gray-700 disabled:opacity-50 text-gray-300 rounded-lg transition-colors text-xs md:text-sm"
        >
          <RefreshCw className={`w-3 h-3 md:w-4 md:h-4 ${retesting ? 'animate-spin' : ''}`} />
          <span className="hidden md:inline">{retesting ? 'Testing' : 'Ext IP'}</span>
        </button>
      </div>
    </div>
  )
}

function AgentDetail({ agent, onClose }: { agent: Emp3r0rAgent; onClose: () => void }) {
  return (
    <div className="fixed inset-0 md:relative md:inset-auto bg-black/70 md:bg-transparent z-50 md:z-auto">
      <div className="absolute right-0 top-0 bottom-0 w-full md:w-auto md:relative bg-gray-900 md:bg-transparent border-l border-gray-800 overflow-y-auto">
        {/* Mobile close button */}
        <div className="md:hidden flex items-center justify-between p-4 border-b border-gray-800">
          <span className="text-sm font-medium text-gray-300">Agent Details</span>
          <button
            onClick={onClose}
            className="p-2 hover:bg-gray-800 rounded-lg transition-colors"
          >
            <X className="w-5 h-5 text-gray-400" />
          </button>
        </div>

        <div className="p-4">
          <div className="bg-gray-900 border border-gray-800 rounded-xl p-4">
            <h3 className="text-base font-semibold text-gray-100 mb-4">Agent Details</h3>
            
            <div className="grid grid-cols-2 gap-4 text-sm">
              <div>
                <p className="text-gray-500 mb-1 text-xs">UUID</p>
                <p className="text-gray-300 font-mono text-xs break-all">{agent.UUID}</p>
              </div>
              <div>
                <p className="text-gray-500 mb-1 text-xs">Version</p>
                <p className="text-gray-300 text-sm">{agent.Version}</p>
              </div>
              <div>
                <p className="text-gray-500 mb-1 text-xs">Transport</p>
                <p className="text-gray-300 text-sm">{agent.Transport}</p>
              </div>
              <div>
                <p className="text-gray-500 mb-1 text-xs">C2 Host</p>
                <p className="text-gray-300 text-sm truncate">{agent.C2Host}</p>
              </div>
              <div>
                <p className="text-gray-500 mb-1 text-xs">IPs</p>
                <p className="text-gray-300 text-sm">{agent.IPs?.join(', ') || 'N/A'}</p>
              </div>
              <div>
                <p className="text-gray-500 mb-1 text-xs">CPU</p>
                <p className="text-gray-300 text-sm truncate">{agent.CPU || 'N/A'}</p>
              </div>
              <div>
                <p className="text-gray-500 mb-1 text-xs">Memory</p>
                <p className="text-gray-300 text-sm">{agent.Mem || 'N/A'}</p>
              </div>
              <div>
                <p className="text-gray-500 mb-1 text-xs">Kernel</p>
                <p className="text-gray-300 text-sm truncate">{agent.Kernel || 'N/A'}</p>
              </div>
            </div>

            {agent.Process && (
              <div className="mt-4 pt-4 border-t border-gray-800">
                <p className="text-gray-500 mb-2 text-xs">Process</p>
                <div className="bg-gray-950 rounded-lg p-3 font-mono text-xs text-gray-400">
                  <p>PID: {agent.Process.PID} (PPID: {agent.Process.PPID})</p>
                  <p className="text-gray-300 mt-1">{agent.Process.Cmdline}</p>
                </div>
              </div>
            )}

            {agent.Files && agent.Files.length > 0 && (
              <div className="mt-4 pt-4 border-t border-gray-800">
                <p className="text-gray-500 mb-2 text-xs">Files ({agent.Files.length})</p>
                <div className="flex flex-wrap gap-2">
                  {agent.Files.slice(0, 5).map((file, i) => (
                    <span key={i} className="px-2 py-1 bg-gray-800 rounded text-xs text-gray-400">
                      {file}
                    </span>
                  ))}
                  {agent.Files.length > 5 && (
                    <span className="px-2 py-1 text-xs text-gray-500">
                      +{agent.Files.length - 5} more
                    </span>
                  )}
                </div>
              </div>
            )}
          </div>
        </div>
      </div>
    </div>
  )
}



export function AgentList() {
  const { 
    agents, 
    activeAgent, 
    loadingAgents, 
    fetchAgents, 
    setActiveAgent,
    forgetAgent
  } = useStore()
  const [selectedAgent, setSelectedAgent] = useState<string | null>(null)
  const [filter, setFilter] = useState('')

  useEffect(() => {
    fetchAgents()
    const interval = setInterval(fetchAgents, 5000)
    return () => clearInterval(interval)
  }, [fetchAgents])

  const handleSelect = async (agent: Emp3r0rAgent) => {
    setSelectedAgent(agent.Tag)
    await setActiveAgent(agent.Tag)
  }

  const handleConsole = async (agent: Emp3r0rAgent) => {
    await setActiveAgent(agent.Tag)
    // 触发自定义事件导航到 console
    window.dispatchEvent(new CustomEvent('navigate-to', { detail: 'console' }))
  }

  const handleForget = async (uuid: string) => {
    if (confirm('Are you sure you want to forget this agent?')) {
      await forgetAgent(uuid)
      setSelectedAgent(null)
    }
  }

  const filteredAgents = agents.filter(agent => 
    !filter || 
    agent.Tag.toLowerCase().includes(filter.toLowerCase()) ||
    agent.Hostname?.toLowerCase().includes(filter.toLowerCase()) ||
    agent.User?.toLowerCase().includes(filter.toLowerCase())
  )

  const selectedAgentData = agents.find(a => a.Tag === selectedAgent)

  return (
    <div className="h-full flex flex-col md:flex-row">
      {/* Agent List */}
      <div className={`flex flex-col ${selectedAgent ? 'hidden md:flex md:w-1/2' : 'w-full'}`}>
        <div className="p-3 md:p-4 border-b border-gray-800">
          <div className="flex items-center justify-between mb-3">
            <div>
              <h2 className="text-base md:text-lg font-semibold text-gray-100">Agents</h2>
              <p className="text-xs text-gray-500">
                {filteredAgents.length} of {agents.length} connected
              </p>
            </div>
            <button
              onClick={fetchAgents}
              disabled={loadingAgents}
              className="p-2 hover:bg-gray-800 rounded-lg transition-colors"
            >
              <RefreshCw
                className={`w-4 h-4 md:w-5 md:h-5 text-gray-400 ${
                  loadingAgents ? 'animate-spin' : ''
                }`}
              />
            </button>
          </div>

          <input
            type="text"
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            placeholder="Filter agents..."
            className="w-full bg-gray-900 border border-gray-800 rounded-lg px-3 py-2.5 text-sm text-gray-100 placeholder-gray-600 focus:outline-none focus:border-emp3r0r-500"
          />
        </div>

        <div className="flex-1 overflow-y-auto p-3 md:p-4 space-y-3 pb-24 md:pb-4">
          {filteredAgents.length === 0 ? (
            <div className="text-center py-12 text-gray-500">
              <Monitor className="w-8 h-8 md:w-12 md:h-12 mx-auto mb-4 opacity-30" />
              <p className="text-sm">{agents.length === 0 ? 'No agents connected' : 'No agents match filter'}</p>
            </div>
          ) : (
            filteredAgents.map((agent) => (
              <AgentCard
                key={agent.UUID}
                agent={agent}
                isActive={agent.Tag === activeAgent?.Tag}
                onSelect={() => handleSelect(agent)}
                onConsole={() => handleConsole(agent)}
              />
            ))
          )}
        </div>
      </div>

      {/* Agent Detail Panel - Desktop */}
      {selectedAgent && selectedAgentData && (
        <div className="hidden md:block md:w-1/2 border-l border-gray-800 overflow-y-auto p-4">
          <div className="flex items-center justify-between mb-4">
            <span className="text-sm text-gray-400">Agent Details</span>
            <button
              onClick={() => handleForget(selectedAgentData.UUID)}
              className="flex items-center gap-1 px-3 py-1.5 text-sm text-red-400 hover:bg-red-500/20 rounded-lg transition-colors"
            >
              <Trash2 className="w-4 h-4" />
              Forget
            </button>
          </div>
          <div className="bg-gray-900 border border-gray-800 rounded-xl p-4">
            {/* Desktop agent detail content */}
            <h3 className="text-base font-semibold text-gray-100 mb-4">Agent Details</h3>
            <div className="grid grid-cols-2 gap-4 text-sm">
              <div>
                <p className="text-gray-500 mb-1 text-xs">UUID</p>
                <p className="text-gray-300 font-mono text-xs break-all">{selectedAgentData.UUID}</p>
              </div>
              <div>
                <p className="text-gray-500 mb-1 text-xs">Version</p>
                <p className="text-gray-300">{selectedAgentData.Version}</p>
              </div>
              <div>
                <p className="text-gray-500 mb-1 text-xs">Transport</p>
                <p className="text-gray-300">{selectedAgentData.Transport}</p>
              </div>
              <div>
                <p className="text-gray-500 mb-1 text-xs">C2 Host</p>
                <p className="text-gray-300 truncate">{selectedAgentData.C2Host}</p>
              </div>
              <div>
                <p className="text-gray-500 mb-1 text-xs">IPs</p>
                <p className="text-gray-300">{selectedAgentData.IPs?.join(', ') || 'N/A'}</p>
              </div>
              <div>
                <p className="text-gray-500 mb-1 text-xs">User</p>
                <p className="text-gray-300">{selectedAgentData.User}</p>
              </div>
            </div>
          </div>
        </div>
      )}

      {/* Agent Detail Panel - Mobile */}
      {selectedAgent && selectedAgentData && (
        <AgentDetail 
          agent={selectedAgentData} 
          onClose={() => setSelectedAgent(null)} 
        />
      )}
    </div>
  )
}
