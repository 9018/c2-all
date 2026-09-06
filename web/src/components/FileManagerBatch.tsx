import { useEffect, useState } from 'react'
import { Download, Trash2 } from 'lucide-react'
import { api } from '@/lib/api'
import { useStore } from '@/stores/useStore'

interface FileEntry { name: string; ftype: string; size: string; date: string; perm: string }

export function FileManagerBatch({
  entries,
  currentPath,
  onDone,
  reload,
}: {
  entries: FileEntry[]
  currentPath: string
  onDone?: () => void
  reload: () => void
}) {
  const { activeAgent } = useStore()
  const [selected, setSelected] = useState<Record<string, boolean>>({})
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    // reset selection when entries change
    setSelected({})
  }, [entries, currentPath])

  const toggle = (name: string) => setSelected(s => ({ ...s, [name]: !s[name] }))
  const selNames = Object.keys(selected).filter(k => selected[k])

  const removeSelected = async () => {
    if (!activeAgent || selNames.length === 0) return
    if (!confirm(`Delete ${selNames.length} item(s)?`)) return
    setBusy(true)
    try {
      for (const name of selNames) {
        const path = currentPath === '/' ? `/${name}` : `${currentPath}/${name}`
        await api.removePath(activeAgent.Tag, path)
      }
      reload()
      onDone?.()
    } catch (e) {
      alert(`Batch delete failed: ${e instanceof Error ? e.message : String(e)}`)
    } finally {
      setBusy(false)
    }
  }

  const downloadSelected = async () => {
    if (!activeAgent || selNames.length === 0) return
    setBusy(true)
    try {
      for (const name of selNames) {
        const entry = entries.find(e => e.name === name)
        if (!entry || entry.ftype === 'dir') continue
        const path = currentPath === '/' ? `/${name}` : `${currentPath}/${name}`
        const blob = await api.downloadFile(activeAgent.Tag, path)
        const url = URL.createObjectURL(blob)
        const a = document.createElement('a')
        a.href = url
        a.download = name
        a.click()
        URL.revokeObjectURL(url)
      }
    } catch (e) {
      alert(`Batch download failed: ${e instanceof Error ? e.message : String(e)}`)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="sticky bottom-0 left-0 right-0 p-2 bg-black/60 backdrop-blur border-t border-gray-800">
      <div className="flex items-center justify-between gap-2">
        <div className="text-xs text-gray-400">Selected: {selNames.length}</div>
        <div className="flex items-center gap-2">
          <button
            onClick={downloadSelected}
            disabled={busy || selNames.length === 0}
            className="px-2 py-1 text-xs bg-gray-800 hover:bg-gray-700 rounded text-gray-200 disabled:opacity-50 flex items-center gap-1"
          >
            <Download className="w-3 h-3" /> Download
          </button>
          <button
            onClick={removeSelected}
            disabled={busy || selNames.length === 0}
            className="px-2 py-1 text-xs bg-red-900/40 hover:bg-red-900/60 rounded text-red-300 disabled:opacity-50 flex items-center gap-1"
          >
            <Trash2 className="w-3 h-3" /> Delete
          </button>
        </div>
      </div>
      <div className="mt-2 grid grid-cols-2 sm:grid-cols-3 md:grid-cols-4 gap-1 text-xs">
        {entries.map((e) => (
          <label key={e.name} className="flex items-center gap-2 p-1 rounded hover:bg-gray-800/50">
            <input
              type="checkbox"
              className="accent-emp3r0r-500 w-3 h-3"
              checked={!!selected[e.name]}
              onChange={() => toggle(e.name)}
            />
            <span className="truncate text-gray-200">{e.name}</span>
            <span className="ml-auto text-gray-500">{e.ftype === 'dir' ? '-' : e.size}</span>
          </label>
        ))}
      </div>
    </div>
  )
}
