import { useEffect, useState } from 'react'
import { X } from 'lucide-react'
import { api } from '@/lib/api'
import { useStore } from '@/stores/useStore'

export function FilePropertiesPanel({
  path,
  onClose,
}: {
  path: string
  onClose: () => void
}) {
  const { activeAgent } = useStore()
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [info, setInfo] = useState<{ name: string; permission: string; checksum: string; size: number } | null>(null)

  useEffect(() => {
    let mounted = true
    if (!activeAgent) return
    setLoading(true)
    setError(null)
    api.stat(activeAgent.Tag, path)
      .then((res) => { if (mounted) setInfo(res) })
      .catch((e) => { if (mounted) setError(e instanceof Error ? e.message : String(e)) })
      .finally(() => { if (mounted) setLoading(false) })
    return () => { mounted = false }
  }, [activeAgent, path])

  return (
    <div className="fixed inset-0 z-40 bg-black/50" onClick={onClose}>
      <div
        className="absolute right-0 top-0 h-full w-full sm:w-96 bg-gray-900 border-l border-gray-800 shadow-xl"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="p-3 border-b border-gray-800 flex items-center justify-between">
          <div className="text-sm text-gray-200">Properties</div>
          <button onClick={onClose} className="p-1 rounded hover:bg-gray-800 text-gray-400">
            <X className="w-4 h-4" />
          </button>
        </div>
        <div className="p-4 text-sm text-gray-300 space-y-2">
          <div className="text-xs text-gray-500 break-all">{path}</div>
          {loading ? (
            <div className="text-gray-500">Loading...</div>
          ) : error ? (
            <div className="text-red-400">{error}</div>
          ) : info ? (
            <div className="space-y-2">
              <div>
                <div className="text-gray-500">Name</div>
                <div className="text-gray-200">{info.name}</div>
              </div>
              <div>
                <div className="text-gray-500">Size</div>
                <div className="text-gray-200">{info.size} bytes</div>
              </div>
              <div>
                <div className="text-gray-500">Permission</div>
                <div className="text-gray-200">{info.permission}</div>
              </div>
              <div>
                <div className="text-gray-500">Checksum</div>
                <div className="text-gray-200">{info.checksum || '-'}</div>
              </div>
            </div>
          ) : null}
        </div>
      </div>
    </div>
  )
}
