import { useState, useEffect, useRef } from 'react'
import { useStore } from '@/stores/useStore'
import { api } from '@/lib/api'
import { Folder, File, Download, Upload, RefreshCw, Home, ChevronRight, ArrowLeft, Plus, Trash2, Pencil, Copy, Info, CheckSquare } from 'lucide-react'

// Optional: simple prompt for new folder on mobile
function promptFolderName(defaultName = ''): Promise<string | null> {
  return new Promise((resolve) => {
    const name = window.prompt('New folder name', defaultName)
    resolve(name && name.trim() ? name.trim() : null)
  })
}

interface FileEntry {
  name: string
  ftype: string
  size: string
  date: string
  perm: string
}

import { FileManagerBatch } from './FileManagerBatch'
import { FilePropertiesPanel } from './FilePropertiesPanel'

export function FileManager() {
  const { activeAgent } = useStore()
  const [files, setFiles] = useState<FileEntry[]>([])
  const [multi, setMulti] = useState(false)
  const [currentPath, setCurrentPath] = useState('/')
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [newFolderName, setNewFolderName] = useState('')
  const [query, setQuery] = useState('')
  const fileInputRef = useRef<HTMLInputElement | null>(null)

  const loadFiles = async (path: string) => {
    if (!activeAgent) return
    
    setLoading(true)
    setError(null)
    
    try {
      const result: any = await api.listFiles(activeAgent.Tag, path)
      if (Array.isArray(result)) {
        setFiles(result as FileEntry[])
      } else if (result && typeof result === 'object' && 'text' in result) {
        throw new Error((result as { text: string }).text || 'Unexpected result from server')
      } else {
        throw new Error('Unexpected response format')
      }
      setCurrentPath(path)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load files')
      setFiles([])
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    if (activeAgent) {
      loadFiles('/')
    }
  }, [activeAgent])

  const handleRefresh = () => {
    loadFiles(currentPath)
  }

  const toggleMulti = () => setMulti(v => !v)

  const handleNavigate = (entry: FileEntry) => {
    if (entry.ftype === 'dir') {
      const newPath = currentPath === '/' ? `/${entry.name}` : `${currentPath}/${entry.name}`
      loadFiles(newPath)
    }
  }

  const handleGoUp = () => {
    if (currentPath === '/') return
    const parts = currentPath.split('/').filter(Boolean)
    parts.pop()
    const parentPath = parts.length === 0 ? '/' : `/${parts.join('/')}`
    loadFiles(parentPath)
  }

  const handleGoHome = () => {
    loadFiles('/')
  }

  const handleDownload = async (entry: FileEntry) => {
    if (!activeAgent || entry.ftype === 'dir') return
    
    const filePath = currentPath === '/' ? `/${entry.name}` : `${currentPath}/${entry.name}`
    
    try {
      const blob = await api.downloadFile(activeAgent.Tag, filePath)
      const url = URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = url
      a.download = entry.name
      a.click()
      URL.revokeObjectURL(url)
    } catch (err) {
      alert(`Download failed: ${err instanceof Error ? err.message : 'Unknown error'}`)
    }
  }

  const handleUploadClick = () => {
    fileInputRef.current?.click()
  }

  const handleCreateFolder = async () => {
    if (!activeAgent) return
    let name = newFolderName.trim()
    if (!name) {
      const fromPrompt = await promptFolderName('New Folder')
      if (!fromPrompt) return
      name = fromPrompt
    }
    const dest = currentPath === '/' ? `/${name}` : `${currentPath}/${name}`
    try {
      setLoading(true)
      await api.makeDir(activeAgent.Tag, dest)
      setNewFolderName('')
      await loadFiles(currentPath)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to create folder')
    } finally {
      setLoading(false)
    }
  }

  const handleRemove = async (entry: FileEntry) => {
    if (!activeAgent) return
    const dest = currentPath === '/' ? `/${entry.name}` : `${currentPath}/${entry.name}`
    if (!confirm(`Delete ${dest}?`)) return
    try {
      setLoading(true)
      await api.removePath(activeAgent.Tag, dest)
      await loadFiles(currentPath)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Delete failed')
    } finally {
      setLoading(false)
    }
  }

  const handleRename = async (entry: FileEntry) => {
    if (!activeAgent) return
    const src = currentPath === '/' ? `/${entry.name}` : `${currentPath}/${entry.name}`
    const newName = window.prompt('Rename to', entry.name)
    if (!newName || newName.trim() === entry.name) return
    const dst = currentPath === '/' ? `/${newName.trim()}` : `${currentPath}/${newName.trim()}`
    try {
      setLoading(true)
      await api.move(activeAgent.Tag, src, dst)
      await loadFiles(currentPath)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Rename failed')
    } finally {
      setLoading(false)
    }
  }

  const handleCopy = async (entry: FileEntry) => {
    if (!activeAgent) return
    const src = currentPath === '/' ? `/${entry.name}` : `${currentPath}/${entry.name}`
    const suggested = currentPath === '/' ? `/${entry.name}.copy` : `${currentPath}/${entry.name}.copy`
    const dst = window.prompt('Copy to path', suggested)
    if (!dst || dst.trim() === src) return
    try {
      setLoading(true)
      await api.copy(activeAgent.Tag, src, dst.trim())
      await loadFiles(currentPath)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Copy failed')
    } finally {
      setLoading(false)
    }
  }

  const [propPath, setPropPath] = useState<string | null>(null)
  const handleInfo = (entry: FileEntry) => {
    const path = currentPath === '/' ? `/${entry.name}` : `${currentPath}/${entry.name}`
    setPropPath(path)
  }

  const handleFileSelected = async (e: React.ChangeEvent<HTMLInputElement>) => {
    try {
      const file = e.target.files?.[0]
      if (!file || !activeAgent) return
      // Read as base64 (without data: prefix)
      const toBase64 = (f: File) => new Promise<string>((resolve, reject) => {
        const reader = new FileReader()
        reader.onload = () => {
          const res = reader.result as string
          const idx = res.indexOf(',')
          resolve(idx >= 0 ? res.slice(idx + 1) : res)
        }
        reader.onerror = () => reject(reader.error)
        reader.readAsDataURL(f)
      })
      const b64 = await toBase64(file)
      const destPath = currentPath === '/' ? `/${file.name}` : `${currentPath}/${file.name}`
      setLoading(true)
      await api.uploadFile(activeAgent.Tag, destPath, b64)
      await loadFiles(currentPath)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Upload failed')
    } finally {
      setLoading(false)
      if (fileInputRef.current) fileInputRef.current.value = ''
    }
  }

  const formatSize = (sizeStr: string) => {
    const m = /([0-9]+)/.exec(sizeStr)
    const size = m ? parseInt(m[1], 10) : NaN
    if (isNaN(size)) return sizeStr
    if (size < 1024) return `${size} B`
    if (size < 1024 * 1024) return `${(size / 1024).toFixed(1)} KB`
    if (size < 1024 * 1024 * 1024) return `${(size / (1024 * 1024)).toFixed(1)} MB`
    return `${(size / (1024 * 1024 * 1024)).toFixed(1)} GB`
  }

  if (!activeAgent) {
    return (
      <div className="h-full flex items-center justify-center text-gray-500">
        <div className="text-center">
          <Folder className="w-12 h-12 mx-auto mb-4 opacity-30" />
          <p className="mb-4">Select an agent to browse files</p>
          <button
            onClick={() => window.dispatchEvent(new CustomEvent('navigate-to', { detail: 'agents' }))}
            className="px-4 py-2 bg-emp3r0r-500/20 hover:bg-emp3r0r-500/30 text-emp3r0r-400 rounded-lg transition-colors text-sm"
          >
            去选择 Agent
          </button>
        </div>
      </div>
    )
  }

  return (
    <div className="h-full flex flex-col">
      {/* Header */}
      <div className="p-3 md:p-4 border-b border-gray-800">
        <div className="flex items-center justify-between mb-2">
          <h2 className="text-base md:text-lg font-semibold text-gray-100">File Manager</h2>
          <div className="flex items-center gap-1.5">
            <div className="hidden md:flex items-center gap-1 bg-gray-900/40 border border-gray-800 rounded-lg px-2 py-1">
              <input
                value={query}
                onChange={(e) => setQuery(e.target.value)}
                placeholder="Search"
                className="bg-transparent text-xs text-gray-300 outline-none placeholder:text-gray-500 w-28 md:w-44"
              />
            </div>
            <div className="hidden md:flex items-center gap-1.5 bg-gray-900/40 border border-gray-800 rounded-lg px-2 py-1">
              <Plus className="w-4 h-4 text-gray-500" />
              <input
                value={newFolderName}
                onChange={(e) => setNewFolderName(e.target.value)}
                placeholder="New folder"
                className="bg-transparent text-xs text-gray-300 outline-none placeholder:text-gray-500 w-28 md:w-40"
                onKeyDown={(e) => { if (e.key === 'Enter') handleCreateFolder() }}
              />
              <button
                onClick={handleCreateFolder}
                className="px-2 py-0.5 text-xs bg-emp3r0r-600 hover:bg-emp3r0r-500 rounded"
              >Create</button>
            </div>
            <button
              onClick={handleCreateFolder}
              className="md:hidden p-2 hover:bg-gray-800 rounded-lg transition-colors text-gray-400"
              title="New Folder"
            >
              <Plus className="w-4 h-4" />
            </button>
            <button
              onClick={handleUploadClick}
              className="p-2 hover:bg-gray-800 rounded-lg transition-colors text-gray-400"
              title="Upload"
            >
              <Upload className="w-4 h-4" />
            </button>
            <button
              onClick={handleRefresh}
              disabled={loading}
              className="p-2 hover:bg-gray-800 rounded-lg transition-colors text-gray-400 disabled:opacity-50"
            >
              <RefreshCw className={`w-4 h-4 ${loading ? 'animate-spin' : ''}`} />
            </button>
            <button
              onClick={toggleMulti}
              className={`p-2 rounded-lg transition-colors ${multi ? 'bg-emp3r0r-700 text-emp3r0r-100' : 'hover:bg-gray-800 text-gray-400'}`}
              title="Batch select"
            >
              <CheckSquare className="w-4 h-4" />
            </button>
          </div>
        </div>
        
        {/* Breadcrumb */}
        <div className="flex items-center gap-1 text-xs text-gray-400 overflow-x-auto">
          <button
            onClick={handleGoHome}
            className="hover:text-emp3r0r-400 transition-colors"
          >
            <Home className="w-3 h-3" />
          </button>
          
          {currentPath !== '/' && (
            <>
              <ChevronRight className="w-3 h-3" />
              <button
                onClick={handleGoUp}
                className="hover:text-emp3r0r-400 transition-colors"
                title="Up"
              >
                <ArrowLeft className="w-3 h-3" />
              </button>
            </>
          )}
          
          {currentPath.split('/').filter(Boolean).map((part, index, arr) => (
            <span key={index} className="flex items-center gap-1">
              <ChevronRight className="w-3 h-3" />
              <span className={index === arr.length - 1 ? 'text-gray-200' : ''}>
                {part}
              </span>
            </span>
          ))}
        </div>
      </div>

      {/* Hidden file input for upload */}
      <input
        ref={fileInputRef}
        type="file"
        className="hidden"
        onChange={handleFileSelected}
      />

      {/* File List */}
      <div className="flex-1 overflow-y-auto p-2 md:p-4 pb-24">
        {error && (
          <div className="mb-4 p-3 bg-red-900/20 border border-red-800 rounded-lg text-red-400 text-sm">
            {error}
          </div>
        )}
        
        {loading ? (
          <div className="flex items-center justify-center h-32 text-gray-500">
            <RefreshCw className="w-6 h-6 animate-spin" />
          </div>
        ) : files.length === 0 ? (
          <div className="flex flex-col items-center justify-center h-32 text-gray-500">
            <Folder className="w-8 h-8 mb-2 opacity-30" />
            <p className="text-sm">No files found</p>
          </div>
        ) : (
          <div className="space-y-1">
            {(query ? files.filter(f => f.name.toLowerCase().includes(query.toLowerCase())) : files).map((entry, index) => (
              <div
                key={index}
                className={`flex items-center gap-3 p-2 md:p-3 rounded-lg transition-colors ${
                  entry.ftype === 'dir'
                    ? 'hover:bg-gray-800/50 cursor-pointer'
                    : 'hover:bg-gray-800/30'
                }`}
                onClick={() => handleNavigate(entry)}
              >
                {multi && (
                  <input
                    type="checkbox"
                    className="accent-emp3r0r-500 w-4 h-4"
                    onClick={(e) => e.stopPropagation()}
                    onChange={() => { /* selection handled in batch panel */ }}
                  />
                )}
                {/* Icon */}
                <div className={`flex-shrink-0 ${
                  entry.ftype === 'dir' ? 'text-emp3r0r-400' : 'text-gray-400'
                }`}>
                  {entry.ftype === 'dir' ? (
                    <Folder className="w-5 h-5" />
                  ) : (
                    <File className="w-5 h-5" />
                  )}
                </div>
                
                {/* Name */}
                <div className="flex-1 min-w-0">
                  <div className="text-sm text-gray-200 truncate">{entry.name}</div>
                  <div className="text-xs text-gray-500">{entry.perm}</div>
                </div>
                
                {/* Size */}
                <div className="text-xs text-gray-500 w-20 text-right">
                  {entry.ftype === 'dir' ? '-' : formatSize(entry.size)}
                </div>
                
                {/* Date */}
                <div className="text-xs text-gray-500 w-32 text-right hidden md:block">
                  {entry.date}
                </div>
                
                {/* Actions */}
                <button
                  onClick={(e) => { e.stopPropagation(); handleInfo(entry) }}
                  className="p-1.5 hover:bg-gray-700 rounded-lg transition-colors text-gray-400"
                  title="Properties"
                >
                  <Info className="w-4 h-4" />
                </button>
                {entry.ftype === 'file' && (
                  <button
                    onClick={(e) => {
                      e.stopPropagation()
                      handleDownload(entry)
                    }}
                    className="p-1.5 hover:bg-gray-700 rounded-lg transition-colors text-gray-400"
                    title="Download"
                  >
                    <Download className="w-4 h-4" />
                  </button>
                )}
                <button
                  onClick={(e) => { e.stopPropagation(); handleCopy(entry) }}
                  className="p-1.5 hover:bg-gray-700 rounded-lg transition-colors text-gray-400"
                  title="Copy"
                >
                  <Copy className="w-4 h-4" />
                </button>
                <button
                  onClick={(e) => { e.stopPropagation(); handleRename(entry) }}
                  className="p-1.5 hover:bg-gray-700 rounded-lg transition-colors text-gray-400"
                  title="Rename / Move"
                >
                  <Pencil className="w-4 h-4" />
                </button>
                <button
                  onClick={(e) => { e.stopPropagation(); handleRemove(entry) }}
                  className="p-1.5 hover:bg-red-900/40 rounded-lg transition-colors text-red-400"
                  title="Delete"
                >
                  <Trash2 className="w-4 h-4" />
                </button>
              </div>
            ))}
          </div>
        )}
      </div>
      
      {/* Footer */}
      <div className="p-3 border-t border-gray-800 text-xs text-gray-500">
        {files.length} items | {currentPath}
      </div>
      {multi && (
        <FileManagerBatch
          entries={files}
          currentPath={currentPath}
          reload={() => loadFiles(currentPath)}
        />
      )}
      {propPath && (
        <FilePropertiesPanel path={propPath} onClose={() => setPropPath(null)} />
      )}
    </div>
  )
}
