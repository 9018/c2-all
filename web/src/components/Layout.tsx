import { useState, useEffect } from 'react'
import { Sidebar } from './Sidebar'
import { Dashboard } from './Dashboard'
import { AgentList } from './AgentList'
import { CommandConsole } from './CommandConsole'
import { ModulePanel } from './ModulePanel'
import { FileManager } from './FileManager'
import { useWebSocket } from '@/hooks/useWebSocket'
import { useStore } from '@/stores/useStore'
import { Menu, X, LogOut } from 'lucide-react'

export function Layout() {
  const [activeView, setActiveView] = useState<string>('dashboard')
  const [mobileMenuOpen, setMobileMenuOpen] = useState(false)
  const { activeAgent, setToken } = useStore()

  // 初始化 WebSocket 连接
  useWebSocket()

  // 关闭移动菜单当切换视图时
  const handleNavigate = (view: string) => {
    setActiveView(view)
    setMobileMenuOpen(false)
  }

  // 监听自定义导航事件
  useEffect(() => {
    const handleNavigateTo = (e: CustomEvent) => {
      handleNavigate(e.detail)
    }
    window.addEventListener('navigate-to', handleNavigateTo as EventListener)
    return () => window.removeEventListener('navigate-to', handleNavigateTo as EventListener)
  }, [])

  // 关闭移动菜单当点击内容区域
  useEffect(() => {
    const handleResize = () => {
      if (window.innerWidth >= 768) {
        setMobileMenuOpen(false)
      }
    }
    window.addEventListener('resize', handleResize)
    return () => window.removeEventListener('resize', handleResize)
  }, [])

  const renderMainContent = () => {
    switch (activeView) {
      case 'dashboard':
        return <Dashboard />
      case 'agents':
        return <AgentList />
      case 'console':
        return <CommandConsole />
      case 'files':
        return <FileManager />
      case 'modules':
        return <ModulePanel />
      default:
        return <Dashboard />
    }
  }

  const handleLogout = () => {
    setToken('')
    localStorage.removeItem('emp3r0r_token')
    window.location.reload()
  }

  return (
    <div className="h-screen flex flex-col md:flex-row bg-gray-950">
      {/* Mobile Header */}
      <header className="md:hidden h-14 border-b border-gray-800 flex items-center justify-between px-4 bg-gray-900/95 backdrop-blur-sm sticky top-0 z-40">
        <div className="flex items-center gap-3">
          <button
            onClick={() => setMobileMenuOpen(!mobileMenuOpen)}
            className="p-2 hover:bg-gray-800 rounded-lg transition-colors"
          >
            {mobileMenuOpen ? (
              <X className="w-5 h-5 text-gray-300" />
            ) : (
              <Menu className="w-5 h-5 text-gray-300" />
            )}
          </button>
          <div className="flex items-center gap-2">
            <div className="w-8 h-8 rounded-lg bg-emp3r0r-500/20 flex items-center justify-center">
              <img src="/favicon.svg" alt="emp3r0r" className="w-5 h-5" />
            </div>
            <span className="font-semibold text-gray-100">emp3r0r</span>
          </div>
        </div>
        
        <div className="flex items-center gap-2">
          {activeAgent && (
            <div className="flex items-center gap-1.5 px-2 py-1 bg-gray-800 rounded-full">
              <div className="w-1.5 h-1.5 bg-green-500 rounded-full" />
              <span className="text-xs text-gray-400 max-w-[100px] truncate">
                {activeAgent.Hostname || activeAgent.Tag}
              </span>
            </div>
          )}
          <button
            onClick={handleLogout}
            className="p-2 hover:bg-gray-800 rounded-lg transition-colors text-gray-400 hover:text-gray-200"
          >
            <LogOut className="w-4 h-4" />
          </button>
        </div>
      </header>

      {/* Mobile Menu Overlay */}
      {mobileMenuOpen && (
        <div 
          className="md:hidden fixed inset-0 bg-black/50 z-40"
          onClick={() => setMobileMenuOpen(false)}
        />
      )}

      {/* Mobile Sidebar */}
      <div className={`
        md:hidden fixed inset-y-0 left-0 w-64 z-50 transform transition-transform duration-300 ease-in-out
        ${mobileMenuOpen ? 'translate-x-0' : '-translate-x-full'}
      `}>
        <Sidebar onNavigate={handleNavigate} activeView={activeView} />
      </div>

      {/* Desktop Sidebar - hidden on mobile */}
      <div className="hidden md:block">
        <Sidebar onNavigate={handleNavigate} activeView={activeView} />
      </div>
      
      {/* Main Content */}
      <main className="flex-1 flex flex-col overflow-hidden">
        {/* Desktop Top Bar */}
        <header className="hidden md:flex h-16 border-b border-gray-800 items-center justify-between px-6">
          <div className="flex items-center gap-4">
            <h1 className="text-lg font-semibold text-gray-100 capitalize">
              {activeView}
            </h1>
            {activeAgent && (
              <div className="flex items-center gap-2 px-3 py-1 bg-gray-800 rounded-full">
                <div className="w-2 h-2 bg-green-500 rounded-full" />
                <span className="text-sm text-gray-400">
                  {activeAgent.Hostname || activeAgent.Tag}
                </span>
              </div>
            )}
          </div>
          
          <div className="flex items-center gap-3">
            <span className="text-sm text-gray-500">
              {new Date().toLocaleDateString()}
            </span>
            <button
              onClick={handleLogout}
              className="flex items-center gap-2 px-3 py-1.5 text-sm text-gray-400 hover:bg-gray-800 rounded-lg transition-colors"
            >
              <LogOut className="w-4 h-4" />
              Logout
            </button>
          </div>
        </header>

        {/* Content Area - scrollable on mobile */}
        <div className="flex-1 overflow-y-auto">
          {renderMainContent()}
        </div>

        {/* Mobile Bottom Navigation */}
        <nav className="md:hidden fixed bottom-0 left-0 right-0 h-16 bg-gray-900 border-t border-gray-800 flex items-center justify-around px-2 safe-area-bottom">
          <NavButton
            active={activeView === 'dashboard'}
            onClick={() => handleNavigate('dashboard')}
            label="Home"
            icon="📊"
          />
          <NavButton
            active={activeView === 'agents'}
            onClick={() => handleNavigate('agents')}
            label="Agents"
            icon="🖥️"
            count={(useStore.getState().agents ?? []).length}
          />
          <NavButton
            active={activeView === 'console'}
            onClick={() => handleNavigate('console')}
            label="Console"
            icon="💻"
          />
          <NavButton
            active={activeView === 'files'}
            onClick={() => handleNavigate('files')}
            label="Files"
            icon="📁"
          />
          <NavButton
            active={activeView === 'modules'}
            onClick={() => handleNavigate('modules')}
            label="Modules"
            icon="📦"
          />
        </nav>
      </main>
    </div>
  )
}

// Mobile Bottom Nav Button
function NavButton({ 
  active, 
  onClick, 
  label, 
  icon,
  count 
}: { 
  active: boolean
  onClick: () => void
  label: string
  icon: string
  count?: number
}) {
  return (
    <button
      onClick={onClick}
      className={`flex flex-col items-center justify-center min-w-[60px] py-2 rounded-lg transition-colors relative ${
        active 
          ? 'text-emp3r0r-500' 
          : 'text-gray-500 active:text-gray-300'
      }`}
    >
      <span className="text-xl mb-0.5">{icon}</span>
      <span className="text-[10px] font-medium">{label}</span>
      {count !== undefined && count > 0 && (
        <span className="absolute -top-0.5 -right-0.5 min-w-[18px] h-[18px] flex items-center justify-center bg-emp3r0r-500 text-white text-[10px] font-bold rounded-full px-1">
          {count}
        </span>
      )}
    </button>
  )
}
