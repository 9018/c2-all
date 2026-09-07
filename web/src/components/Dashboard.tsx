import { useEffect } from 'react'
import { useStore } from '@/stores/useStore'
import { 
  Monitor, 
  Shield, 
  Activity,
  Clock,
  Globe,
  Terminal,
  Server
} from 'lucide-react'

function StatCard({ 
  icon: Icon, 
  label, 
  value, 
  color = 'text-gray-400'
}: { 
  icon: any
  label: string
  value: string | number
  color?: string
}) {
  return (
    <div className="p-4 bg-gray-900 border border-gray-800 rounded-xl">
      <div className="flex items-center gap-3">
        <div className={`p-2 bg-gray-800 rounded-lg ${color}`}>
          <Icon className="w-5 h-5" />
        </div>
        <div>
          <p className="text-xs text-gray-500">{label}</p>
          <p className="text-xl font-semibold text-gray-100">{value}</p>
        </div>
      </div>
    </div>
  )
}

function AgentStatusBadge({ online }: { online: boolean }) {
  return (
    <span className={`inline-flex items-center gap-1 px-2 py-0.5 rounded-full text-xs ${
      online ? 'bg-green-500/20 text-green-400' : 'bg-gray-700 text-gray-400'
    }`}>
      <span className={`w-1.5 h-1.5 rounded-full ${online ? 'bg-green-500' : 'bg-gray-500'}`} />
      {online ? 'Online' : 'Offline'}
    </span>
  )
}

export function Dashboard() {
  const { agents, logs, fetchAgents } = useStore()

  useEffect(() => {
    fetchAgents()
    const interval = setInterval(fetchAgents, 5000)
    return () => clearInterval(interval)
  }, [fetchAgents])

  const onlineAgents = agents.filter((a) => {
    const lastSeen = new Date(a.LastSeen)
    return Date.now() - lastSeen.getTime() < 60000
  })

  const windowsAgents = agents.filter((a) => a.GOOS === 'windows')
  const linuxAgents = agents.filter((a) => a.GOOS === 'linux')
  const rootAgents = agents.filter((a) => a.HasRoot)

  const recentLogs = logs.slice(-10).reverse()

  return (
    <div className="h-full overflow-y-auto p-4 md:p-6 pb-24 md:pb-6">
      {/* Header */}
      <div className="flex items-center justify-between mb-4 md:mb-6">
        <h1 className="text-xl md:text-2xl font-bold text-gray-100">Dashboard</h1>
        <div className="flex items-center gap-2 text-xs md:text-sm text-gray-500">
          <Clock className="w-4 h-4" />
          <span className="hidden sm:inline">{new Date().toLocaleString()}</span>
          <span className="sm:hidden">{new Date().toLocaleTimeString()}</span>
        </div>
      </div>

      {/* Stats Grid - 2x2 on mobile, 4 cols on desktop */}
      <div className="grid grid-cols-2 md:grid-cols-4 gap-3 md:gap-4 mb-4 md:mb-6">
        <StatCard
          icon={Monitor}
          label="Total"
          value={agents.length}
          color="text-emp3r0r-500"
        />
        <StatCard
          icon={Activity}
          label="Online"
          value={onlineAgents.length}
          color="text-green-500"
        />
        <StatCard
          icon={Shield}
          label="Root"
          value={rootAgents.length}
          color="text-yellow-500"
        />
        <StatCard
          icon={Globe}
          label="Platforms"
          value={`${linuxAgents.length}L/${windowsAgents.length}W`}
          color="text-blue-500"
        />
      </div>

      {/* Two Column Layout */}
      <div className="grid grid-cols-1 lg:grid-cols-2 gap-4 md:gap-6">
        {/* OS Distribution */}
        <div className="bg-gray-900 border border-gray-800 rounded-xl p-4">
          <h2 className="text-base md:text-lg font-semibold text-gray-100 mb-4 flex items-center gap-2">
            <Server className="w-5 h-5 text-gray-400" />
            OS Distribution
          </h2>
          <div className="space-y-4">
            <div>
              <div className="flex justify-between text-sm mb-2">
                <span className="text-gray-400 flex items-center gap-2">
                  <Terminal className="w-4 h-4" />
                  Linux
                </span>
                <span className="text-gray-500">
                  {linuxAgents.length} ({agents.length ? Math.round(linuxAgents.length / agents.length * 100) : 0}%)
                </span>
              </div>
              <div className="h-2 md:h-3 bg-gray-800 rounded-full overflow-hidden">
                <div
                  className="h-full bg-gradient-to-r from-orange-500 to-orange-400 transition-all duration-500"
                  style={{
                    width: `${agents.length ? (linuxAgents.length / agents.length) * 100 : 0}%`,
                  }}
                />
              </div>
            </div>
            <div>
              <div className="flex justify-between text-sm mb-2">
                <span className="text-gray-400 flex items-center gap-2">
                  <Monitor className="w-4 h-4" />
                  Windows
                </span>
                <span className="text-gray-500">
                  {windowsAgents.length} ({agents.length ? Math.round(windowsAgents.length / agents.length * 100) : 0}%)
                </span>
              </div>
              <div className="h-2 md:h-3 bg-gray-800 rounded-full overflow-hidden">
                <div
                  className="h-full bg-gradient-to-r from-blue-500 to-blue-400 transition-all duration-500"
                  style={{
                    width: `${agents.length ? (windowsAgents.length / agents.length) * 100 : 0}%`,
                  }}
                />
              </div>
            </div>
          </div>
        </div>

        {/* Recent Activity */}
        <div className="bg-gray-900 border border-gray-800 rounded-xl p-4">
          <h2 className="text-base md:text-lg font-semibold text-gray-100 mb-4 flex items-center gap-2">
            <Activity className="w-5 h-5 text-gray-400" />
            Recent Activity
          </h2>
          <div className="space-y-2 max-h-32 md:max-h-48 overflow-y-auto">
            {recentLogs.length === 0 ? (
              <div className="text-center py-6 md:py-8 text-gray-500">
                <Activity className="w-6 h-6 md:w-8 md:h-8 mx-auto mb-2 opacity-30" />
                <p className="text-sm">No recent activity</p>
              </div>
            ) : (
              recentLogs.map((log, i) => (
                <div
                  key={i}
                  className="flex items-start gap-2 text-sm py-1"
                >
                  <span className="text-gray-600 whitespace-nowrap text-xs">
                    {new Date(log.time).toLocaleTimeString()}
                  </span>
                  <span
                    className={`text-xs ${
                      log.level === 'error'
                        ? 'text-red-400'
                        : log.level === 'warning'
                        ? 'text-yellow-400'
                        : log.level === 'success'
                        ? 'text-green-400'
                        : 'text-gray-400'
                    }`}
                  >
                    {log.message}
                  </span>
                </div>
              ))
            )}
          </div>
        </div>

        {/* Connected Agents Table - Responsive */}
        <div className="bg-gray-900 border border-gray-800 rounded-xl p-4 lg:col-span-2">
          <h2 className="text-base md:text-lg font-semibold text-gray-100 mb-4 flex items-center gap-2">
            <Monitor className="w-5 h-5 text-gray-400" />
            Connected Agents
          </h2>
          {agents.length === 0 ? (
            <div className="text-center py-8 md:py-12 text-gray-500">
              <Monitor className="w-8 h-8 md:w-12 md:h-12 mx-auto mb-4 opacity-30" />
              <p>No agents connected</p>
              <p className="text-sm mt-1">Waiting for agents to check in...</p>
            </div>
          ) : (
            <>
              {/* Desktop Table */}
              <div className="hidden md:block overflow-x-auto">
                <table className="w-full text-sm">
                  <thead>
                    <tr className="text-left text-gray-500 border-b border-gray-800">
                      <th className="pb-3 font-medium">Status</th>
                      <th className="pb-3 font-medium">Tag</th>
                      <th className="pb-3 font-medium">Hostname</th>
                      <th className="pb-3 font-medium">OS</th>
                      <th className="pb-3 font-medium">User</th>
                      <th className="pb-3 font-medium">From</th>
                      <th className="pb-3 font-medium">Last Seen</th>
                    </tr>
                  </thead>
                  <tbody>
                    {agents.map((agent) => {
                      const isOnline = Date.now() - new Date(agent.LastSeen).getTime() < 60000
                      return (
                        <tr 
                          key={agent.UUID} 
                          className="border-b border-gray-800/50 hover:bg-gray-800/30 transition-colors"
                        >
                          <td className="py-3">
                            <AgentStatusBadge online={isOnline} />
                          </td>
                          <td className="py-3 font-mono text-emp3r0r-400">{agent.Tag}</td>
                          <td className="py-3 text-gray-300">{agent.Hostname || '-'}</td>
                          <td className="py-3">
                            <span className={`px-2 py-0.5 rounded text-xs ${
                              agent.GOOS === 'linux' 
                                ? 'bg-orange-500/20 text-orange-400' 
                                : agent.GOOS === 'windows'
                                ? 'bg-blue-500/20 text-blue-400'
                                : 'bg-gray-700 text-gray-400'
                            }`}>
                              {agent.GOOS}/{agent.Arch}
                            </span>
                          </td>
                          <td className="py-3">
                            <span className={`flex items-center gap-1 ${
                              agent.HasRoot ? 'text-yellow-400' : 'text-gray-400'
                            }`}>
                              {agent.HasRoot && <Shield className="w-3 h-3" />}
                              {agent.User}
                            </span>
                          </td>
                          <td className="py-3 text-gray-500 font-mono text-xs">
                            {agent.ExternalIP || agent.From}
                          </td>
                          <td className="py-3 text-gray-500 text-xs">
                            {new Date(agent.LastSeen).toLocaleTimeString()}
                          </td>
                        </tr>
                      )
                    })}
                  </tbody>
                </table>
              </div>

              {/* Mobile Agent Cards */}
              <div className="md:hidden space-y-3">
                {agents.map((agent) => {
                  const isOnline = Date.now() - new Date(agent.LastSeen).getTime() < 60000
                  return (
                    <div
                      key={agent.UUID}
                      className="bg-gray-800/50 rounded-lg p-3 border border-gray-700/50"
                    >
                      <div className="flex items-center justify-between mb-2">
                        <div className="flex items-center gap-2">
                          <AgentStatusBadge online={isOnline} />
                          <span className="font-mono text-sm text-emp3r0r-400">{agent.Tag}</span>
                        </div>
                        <span className={`px-2 py-0.5 rounded text-xs ${
                          agent.GOOS === 'linux' 
                            ? 'bg-orange-500/20 text-orange-400' 
                            : agent.GOOS === 'windows'
                            ? 'bg-blue-500/20 text-blue-400'
                            : 'bg-gray-700 text-gray-400'
                        }`}>
                          {agent.GOOS}
                        </span>
                      </div>
                      <div className="grid grid-cols-2 gap-2 text-xs">
                        <div className="text-gray-500">Host: <span className="text-gray-300">{agent.Hostname || '-'}</span></div>
                        <div className="text-gray-500">User: <span className={agent.HasRoot ? 'text-yellow-400' : 'text-gray-300'}>{agent.User}</span></div>
                        <div className="text-gray-500">From: <span className="text-gray-300 font-mono">{agent.From}</span></div>
                        <div className="text-gray-500">Seen: <span className="text-gray-300">{new Date(agent.LastSeen).toLocaleTimeString()}</span></div>
                      </div>
                    </div>
                  )
                })}
              </div>
            </>
          )}
        </div>
      </div>
    </div>
  )
}
