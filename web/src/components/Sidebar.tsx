import { useStore } from '@/stores/useStore'
import { Monitor, Boxes, Terminal, Shield, Folder } from 'lucide-react'

interface SidebarProps {
  onNavigate: (view: string) => void
  activeView: string
}

export function Sidebar({ onNavigate, activeView }: SidebarProps) {
  const { agents } = useStore()
  
  const onlineAgents = agents.filter((a) => {
    const lastSeen = new Date(a.LastSeen)
    return Date.now() - lastSeen.getTime() < 60000
  })

  const menuItems = [
    {
      id: 'dashboard',
      label: 'Dashboard',
      icon: Monitor,
      description: 'Overview & stats',
    },
    {
      id: 'agents',
      label: 'Agents',
      icon: Shield,
      description: 'Manage targets',
      badge: onlineAgents.length,
    },
    {
      id: 'console',
      label: 'Console',
      icon: Terminal,
      description: 'Execute commands',
    },
    {
      id: 'files',
      label: 'Files',
      icon: Folder,
      description: 'Browse files',
    },
    {
      id: 'modules',
      label: 'Modules',
      icon: Boxes,
      description: 'Exploit modules',
    },
  ]

  return (
    <div className="w-64 h-full bg-gray-900 border-r border-gray-800 flex flex-col">
      {/* Logo */}
      <div className="h-16 border-b border-gray-800 flex items-center px-5 gap-3">
        <div className="w-10 h-10 rounded-xl bg-emp3r0r-500/20 flex items-center justify-center">
          <img src="/favicon.svg" alt="emp3r0r" className="w-6 h-6" />
        </div>
        <div>
          <h1 className="font-bold text-gray-100">emp3r0r</h1>
          <p className="text-[10px] text-gray-500">C2 Framework</p>
        </div>
      </div>

      {/* Navigation */}
      <nav className="flex-1 p-3 space-y-1">
        {menuItems.map((item) => {
          const Icon = item.icon
          const isActive = activeView === item.id
          return (
            <button
              key={item.id}
              onClick={() => onNavigate(item.id)}
              className={`w-full flex items-center gap-3 px-4 py-3 rounded-xl transition-all ${
                isActive
                  ? 'bg-emp3r0r-500/20 text-emp3r0r-400 border border-emp3r0r-500/30'
                  : 'text-gray-400 hover:bg-gray-800 hover:text-gray-200 border border-transparent'
              }`}
            >
              <Icon className="w-5 h-5" />
              <div className="flex-1 text-left">
                <div className="text-sm font-medium">{item.label}</div>
                <div className="text-[10px] opacity-60">{item.description}</div>
              </div>
              {item.badge !== undefined && item.badge > 0 && (
                <span className="px-2 py-0.5 bg-emp3r0r-500/30 text-emp3r0r-400 text-xs rounded-full">
                  {item.badge}
                </span>
              )}
            </button>
          )
        })}
      </nav>

      {/* Footer */}
      <div className="p-4 border-t border-gray-800">
        <div className="text-xs text-gray-600 text-center">
          emp3r0r C2 Panel
        </div>
      </div>
    </div>
  )
}
