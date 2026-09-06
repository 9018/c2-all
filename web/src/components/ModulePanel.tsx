import { useEffect, useState } from 'react'
import { useStore } from '@/stores/useStore'
import { api } from '@/lib/api'
import { 
  Boxes, 
  Play, 
  Loader2,
  Monitor,
  Terminal,
  Wind,
  Search,
  Info,
  X,
  Check
} from 'lucide-react'
import { ModuleConfig, ModuleOption } from '@/types'

function ModuleCard({ module: mod, onSelect }: { 
  module: ModuleConfig
  onSelect: () => void
}) {
  const platformColors = {
    Linux: 'bg-orange-500/20 text-orange-400',
    Windows: 'bg-blue-500/20 text-blue-400',
    Generic: 'bg-gray-700 text-gray-400',
  }

  const platformIcons = {
    Linux: Terminal,
    Windows: Wind,
    Generic: Monitor,
  }

  const PlatformIcon = platformIcons[mod.Platform as keyof typeof platformIcons] || Monitor
  const platformColor = platformColors[mod.Platform as keyof typeof platformColors] || platformColors.Generic

  return (
    <div 
      onClick={onSelect}
      className="border border-gray-800 rounded-xl p-3 md:p-4 cursor-pointer hover:border-gray-700 hover:bg-gray-900/50 transition-all active:scale-[0.98]"
    >
      <div className="flex items-start gap-3">
        <div className="w-10 h-10 bg-emp3r0r-500/20 rounded-lg flex items-center justify-center flex-shrink-0">
          <Boxes className="w-5 h-5 text-emp3r0r-500" />
        </div>
        <div className="flex-1 min-w-0">
          <div className="flex items-start justify-between gap-2">
            <h3 className="font-medium text-gray-100 truncate">{mod.Name}</h3>
            <span className={`px-2 py-0.5 rounded-full text-xs flex-shrink-0 ${platformColor}`}>
              <PlatformIcon className="w-3 h-3 inline mr-1" />
              {mod.Platform}
            </span>
          </div>
          <p className="text-xs md:text-sm text-gray-500 mt-1 line-clamp-2">{mod.Comment}</p>
          <div className="mt-2 flex items-center justify-between text-xs text-gray-500">
            <span>{mod.Author}</span>
            <span>{Object.keys(mod.Options).length} options</span>
          </div>
        </div>
      </div>
    </div>
  )
}

function ModuleExecute({ module: mod, onClose }: { 
  module: ModuleConfig
  onClose: () => void
}) {
  const { activeAgent } = useStore()
  const [params, setParams] = useState<Record<string, string>>({})
  const [executing, setExecuting] = useState(false)
  const [result, setResult] = useState<{ success: boolean; message: string } | null>(null)

  const handleSubmit = async () => {
    if (!activeAgent) {
      setResult({ success: false, message: 'No agent selected' })
      return
    }

    setExecuting(true)
    setResult(null)

    try {
      const cmdParts = [mod.Name]
      Object.entries(params).forEach(([key, value]) => {
        if (value) {
          cmdParts.push(`--${key} "${value}"`)
        }
      })

      const jobId = `module-${Date.now()}`
      await api.sendCommand({
        AgentTag: activeAgent.Tag,
        Action: 'command',
        Command: cmdParts.join(' '),
        JobID: jobId,
      })

      setResult({ success: true, message: `Module ${mod.Name} executed successfully` })
    } catch (error) {
      setResult({ success: false, message: `Failed: ${error}` })
    } finally {
      setExecuting(false)
    }
  }

  return (
    <div className="fixed inset-0 bg-black/70 flex items-end md:items-center justify-center z-50">
      <div className="bg-gray-900 border border-gray-800 rounded-t-2xl md:rounded-2xl w-full max-w-lg mx-0 md:mx-4 overflow-hidden max-h-[90vh] flex flex-col">
        {/* Header */}
        <div className="flex items-center justify-between p-4 border-b border-gray-800">
          <div className="flex items-center gap-3">
            <div className="w-10 h-10 bg-emp3r0r-500/20 rounded-lg flex items-center justify-center">
              <Boxes className="w-5 h-5 text-emp3r0r-500" />
            </div>
            <div>
              <h3 className="font-semibold text-gray-100">{mod.Name}</h3>
              <p className="text-xs text-gray-500">{mod.Platform}</p>
            </div>
          </div>
          <button
            onClick={onClose}
            className="p-2 hover:bg-gray-800 rounded-lg transition-colors"
          >
            <X className="w-5 h-5 text-gray-400" />
          </button>
        </div>

        {/* Description */}
        <div className="p-4 bg-gray-950 border-b border-gray-800">
          <p className="text-sm text-gray-400">{mod.Comment}</p>
          {mod.Author && (
            <p className="text-xs text-gray-500 mt-1">Author: {mod.Author}</p>
          )}
        </div>

        {/* Parameters - Scrollable */}
        <div className="flex-1 overflow-y-auto p-4">
          <h4 className="text-sm font-medium text-gray-300 mb-3">Parameters</h4>
          
          {Object.keys(mod.Options).length === 0 ? (
            <p className="text-sm text-gray-500">No parameters required</p>
          ) : (
            <div className="space-y-4">
              {Object.entries(mod.Options).map(([name, option]) => (
                <ParameterInput
                  key={name}
                  name={name}
                  option={option}
                  value={params[name] || ''}
                  onChange={(value) => setParams({ ...params, [name]: value })}
                />
              ))}
            </div>
          )}
        </div>

        {/* Result */}
        {result && (
          <div className={`mx-4 p-3 rounded-lg ${
            result.success ? 'bg-green-500/20 text-green-400' : 'bg-red-500/20 text-red-400'
          }`}>
            <div className="flex items-center gap-2">
              {result.success ? <Check className="w-4 h-4" /> : <X className="w-4 h-4" />}
              <span className="text-sm">{result.message}</span>
            </div>
          </div>
        )}

        {/* Actions */}
        <div className="p-4 border-t border-gray-800 flex justify-end gap-2">
          <button
            onClick={onClose}
            className="px-4 py-2.5 text-sm text-gray-400 hover:bg-gray-800 rounded-lg transition-colors"
          >
            Cancel
          </button>
          <button
            onClick={handleSubmit}
            disabled={executing || !activeAgent}
            className="flex items-center gap-2 px-4 py-2.5 bg-emp3r0r-500 hover:bg-emp3r0r-600 disabled:opacity-50 disabled:cursor-not-allowed text-white rounded-lg transition-colors"
          >
            {executing ? (
              <Loader2 className="w-4 h-4 animate-spin" />
            ) : (
              <Play className="w-4 h-4" />
            )}
            Execute
          </button>
        </div>
      </div>
    </div>
  )
}

function ParameterInput({ 
  name, 
  option, 
  value, 
  onChange 
}: { 
  name: string
  option: ModuleOption
  value: string
  onChange: (value: string) => void
}) {
  return (
    <div>
      <label className="block text-sm text-gray-400 mb-1">
        {option.Desc}
        {option.Required && <span className="text-red-500 ml-1">*</span>}
      </label>
      {option.Vals && option.Vals.length > 0 ? (
        <select
          value={value || option.Val}
          onChange={(e) => onChange(e.target.value)}
          className="w-full bg-gray-800 border border-gray-700 rounded-lg px-3 py-2.5 text-sm text-gray-100 focus:outline-none focus:border-emp3r0r-500"
        >
          <option value="">Select...</option>
          {option.Vals.map((val) => (
            <option key={val} value={val}>
              {val}
            </option>
          ))}
        </select>
      ) : (
        <input
          type={option.Type === 'int' || option.Type === 'uint' ? 'number' : option.Secret ? 'password' : 'text'}
          value={value || option.Val || ''}
          onChange={(e) => onChange(e.target.value)}
          placeholder={option.Val || `Enter ${name}...`}
          className="w-full bg-gray-800 border border-gray-700 rounded-lg px-3 py-2.5 text-sm text-gray-100 placeholder-gray-600 focus:outline-none focus:border-emp3r0r-500"
        />
      )}
    </div>
  )
}

export function ModulePanel() {
  const { modules, loadingModules, fetchModules, activeAgent } = useStore()
  const [filter, setFilter] = useState('')
  const [selectedModule, setSelectedModule] = useState<ModuleConfig | null>(null)
  const [platformFilter, setPlatformFilter] = useState<string>('all')

  useEffect(() => {
    fetchModules()
  }, [fetchModules])

  const filteredModules = Object.values(modules).filter((mod) => {
    const matchesFilter = 
      mod.Name.toLowerCase().includes(filter.toLowerCase()) ||
      mod.Comment.toLowerCase().includes(filter.toLowerCase())
    
    const matchesPlatform = 
      platformFilter === 'all' || 
      mod.Platform.toLowerCase() === platformFilter.toLowerCase()

    return matchesFilter && matchesPlatform
  })

  return (
    <div className="h-full flex flex-col">
      {/* Header */}
      <div className="p-3 md:p-4 border-b border-gray-800">
        <div className="flex items-center justify-between mb-3">
          <div>
            <h2 className="text-base md:text-lg font-semibold text-gray-100">Modules</h2>
            <p className="text-xs text-gray-500">
              {filteredModules.length} of {Object.keys(modules).length} available
            </p>
          </div>
          {!activeAgent && (
            <div className="flex items-center gap-2 px-2 md:px-3 py-1.5 bg-yellow-500/20 text-yellow-400 rounded-lg text-xs">
              <Info className="w-4 h-4" />
              <span className="hidden md:inline">Select an agent first</span>
              <span className="md:hidden">No agent</span>
            </div>
          )}
        </div>

        {/* Filters */}
        <div className="flex items-center gap-2">
          <div className="relative flex-1">
            <Search className="absolute left-3 top-1/2 -translate-y-1/2 w-4 h-4 text-gray-500" />
            <input
              type="text"
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
              placeholder="Search..."
              className="w-full bg-gray-900 border border-gray-800 rounded-lg pl-10 pr-4 py-2.5 text-sm text-gray-100 placeholder-gray-600 focus:outline-none focus:border-emp3r0r-500"
            />
          </div>
          <select
            value={platformFilter}
            onChange={(e) => setPlatformFilter(e.target.value)}
            className="bg-gray-900 border border-gray-800 rounded-lg px-3 py-2.5 text-sm text-gray-100 focus:outline-none focus:border-emp3r0r-500"
          >
            <option value="all">All</option>
            <option value="linux">Linux</option>
            <option value="windows">Windows</option>
            <option value="generic">Generic</option>
          </select>
        </div>
      </div>

      {/* Module List */}
      <div className="flex-1 overflow-y-auto p-3 md:p-4 pb-24 md:pb-4">
        {loadingModules ? (
          <div className="text-center py-12">
            <Loader2 className="w-8 h-8 mx-auto animate-spin text-gray-500" />
            <p className="text-gray-500 mt-2 text-sm">Loading modules...</p>
          </div>
        ) : filteredModules.length === 0 ? (
          <div className="text-center py-12 text-gray-500">
            <Boxes className="w-8 h-8 md:w-12 md:h-12 mx-auto mb-4 opacity-30" />
            <p className="text-sm">No modules found</p>
            <p className="text-xs mt-1">Try adjusting your search</p>
          </div>
        ) : (
          <div className="grid grid-cols-1 md:grid-cols-2 gap-3 md:gap-4">
            {filteredModules.map((mod) => (
              <ModuleCard
                key={mod.Name}
                module={mod}
                onSelect={() => setSelectedModule(mod)}
              />
            ))}
          </div>
        )}
      </div>

      {/* Module Execute Modal */}
      {selectedModule && (
        <ModuleExecute
          module={selectedModule}
          onClose={() => setSelectedModule(null)}
        />
      )}
    </div>
  )
}
