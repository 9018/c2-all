import { useCallback, useEffect, useState } from 'react'
import { Cloud, Plus, Trash2, Pencil, Rocket, RefreshCw, ShieldCheck, Globe, History, Loader2 } from 'lucide-react'
import { api } from '@/lib/api'
import type { CFFleet, CFAccount, CFMigrationHistory } from '@/types'

// Cloudflare relay Worker fleet management:
//   - preset multiple CF accounts (API token + account id + optional domain)
//   - live per-account quota bars (same GraphQL source as RelayQuotaCard)
//   - hot migration: deploy the worker on another account, repoint the old
//     one, switch the CC's relay listeners; agents follow close(4002)

function pctColor(pct: number): string {
  if (pct >= 85) return 'bg-red-600'
  if (pct >= 60) return 'bg-yellow-600'
  return 'bg-green-600'
}

function QuotaBar({ pct, label }: { pct: number; label: string }) {
  const clamped = Math.min(100, Math.max(0, pct))
  return (
    <div className="flex items-center gap-2">
      <div className="flex-1 h-2 bg-gray-800 rounded-full overflow-hidden">
        <div className={`h-full ${pctColor(clamped)} transition-all`} style={{ width: `${clamped}%` }} />
      </div>
      <span className="text-xs text-gray-400 font-mono w-12 text-right">{clamped.toFixed(0)}%</span>
      <span className="text-xs text-gray-500 w-20">{label}</span>
    </div>
  )
}

interface AddForm {
  id: string
  api_token: string
  label: string
  domain: string
  zone_id: string
}

export function WorkerManagePage() {
  const [fleet, setFleet] = useState<CFFleet | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [showAdd, setShowAdd] = useState(false)
  const [editing, setEditing] = useState<CFAccount | null>(null)
  const [form, setForm] = useState<AddForm>({ id: '', api_token: '', label: '', domain: '', zone_id: '' })
  const [migrating, setMigrating] = useState('')
  const [threshold, setThreshold] = useState(90)
  const [autoMigrate, setAutoMigrate] = useState(true)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const f = await api.getCFAccounts()
      setFleet(f)
      setThreshold(f.migrate_threshold)
      setAutoMigrate(f.auto_migrate)
      setError('')
    } catch (e: any) {
      setError(e.message || String(e))
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => { load() }, [load])

  const submitAdd = async () => {
    setNotice('')
    try {
      await api.addCFAccount(form)
      setShowAdd(false)
      setForm({ id: '', api_token: '', label: '', domain: '', zone_id: '' })
      await load()
      setNotice('账号已添加')
    } catch (e: any) {
      setError(e.message || String(e))
    }
  }

  const submitEdit = async () => {
    if (!editing) return
    setNotice('')
    try {
      await api.updateCFAccount(editing.id, {
        label: form.label, api_token: form.api_token, domain: form.domain, zone_id: form.zone_id,
      })
      setEditing(null)
      setForm({ id: '', api_token: '', label: '', domain: '', zone_id: '' })
      await load()
      setNotice('账号已更新')
    } catch (e: any) {
      setError(e.message || String(e))
    }
  }

  const remove = async (acct: CFAccount) => {
    if (!confirm(`删除账号 ${acct.label || acct.id}？`)) return
    setNotice('')
    try {
      await api.deleteCFAccount(acct.id)
      await load()
      setNotice('账号已删除')
    } catch (e: any) {
      setError(e.message || String(e))
    }
  }

  const migrate = async (acct: CFAccount) => {
    if (!confirm(`热迁移到 ${acct.label || acct.id}？\n\n1. 在新账号部署 Worker\n2. 旧 Worker 通知 agent 迁移\n3. CC 切换监听\n\n迁移过程中 agent 会自动跟随。`)) return
    setMigrating(acct.id)
    setNotice('迁移已启动（后台执行），约 30-60s 完成')
    try {
      await api.activateCFAccount(acct.id)
      setTimeout(load, 5000)
    } catch (e: any) {
      setError(e.message || String(e))
      setMigrating('')
    }
  }

  const saveSettings = async () => {
    setNotice('')
    try {
      const f = await api.updateCFSettings({ migrate_threshold: threshold, auto_migrate: autoMigrate })
      setFleet(f)
      setNotice('设置已保存')
    } catch (e: any) {
      setError(e.message || String(e))
    }
  }

  const fmtTime = (t: string) => {
    try { return new Date(t).toLocaleString() } catch { return t }
  }

  return (
    <div className="p-6 max-w-6xl mx-auto space-y-6">
      {/* header */}
      <div className="flex items-center justify-between">
        <div className="flex items-center gap-3">
          <Cloud className="w-7 h-7 text-emp3r0r-400" />
          <div>
            <h1 className="text-xl font-bold text-gray-100">Relay Worker 管理</h1>
            <p className="text-sm text-gray-500">多账号热迁移 · 配额到达阈值自动切换</p>
          </div>
        </div>
        <div className="flex gap-2">
          <button onClick={load} className="flex items-center gap-2 px-4 py-2 bg-gray-800 hover:bg-gray-700 rounded-lg text-gray-300 text-sm transition-colors">
            <RefreshCw className={`w-4 h-4 ${loading ? 'animate-spin' : ''}`} /> 刷新
          </button>
          <button onClick={() => { setEditing(null); setForm({ id: '', api_token: '', label: '', domain: '', zone_id: '' }); setShowAdd(true) }} className="flex items-center gap-2 px-4 py-2 bg-emp3r0r-500 hover:bg-emp3r0r-600 rounded-lg text-white text-sm font-medium transition-colors">
            <Plus className="w-4 h-4" /> 添加账号
          </button>
        </div>
      </div>

      {error && <div className="p-3 bg-red-500/10 border border-red-500/30 rounded-lg text-red-400 text-sm">{error}</div>}
      {notice && <div className="p-3 bg-green-500/10 border border-green-500/30 rounded-lg text-green-400 text-sm">{notice}</div>}

      {/* settings bar */}
      {fleet && (
        <div className="grid grid-cols-1 md:grid-cols-3 gap-4 p-4 bg-gray-900 border border-gray-800 rounded-xl">
          <div className="col-span-1 md:col-span-2 flex items-center gap-4">
            <div className="flex-1">
              <label className="text-sm text-gray-400">迁移阈值（DO 用量 %）</label>
              <div className="flex items-center gap-3 mt-1">
                <input type="range" min={50} max={99} value={threshold}
                  onChange={(e) => setThreshold(Number(e.target.value))}
                  className="flex-1 accent-emp3r0r-500" />
                <span className="text-lg font-bold text-emp3r0r-400 w-10">{threshold}%</span>
              </div>
            </div>
            <div className="flex items-center gap-2 pt-5">
              <button
                onClick={() => setAutoMigrate(!autoMigrate)}
                className={`relative w-11 h-6 rounded-full transition-colors ${autoMigrate ? 'bg-emp3r0r-500' : 'bg-gray-700'}`}>
                <span className={`absolute top-0.5 w-5 h-5 rounded-full bg-white transition-all ${autoMigrate ? 'left-5.5' : 'left-0.5'}`} style={{ left: autoMigrate ? '22px' : '2px' }} />
              </button>
              <span className="text-sm text-gray-300">自动迁移</span>
            </div>
          </div>
          <div className="flex items-end justify-end">
            <button onClick={saveSettings} className="px-4 py-2 bg-gray-800 hover:bg-gray-700 rounded-lg text-gray-300 text-sm">保存设置</button>
          </div>
        </div>
      )}

      {/* account cards */}
      <div className="grid grid-cols-1 lg:grid-cols-2 gap-4">
        {fleet?.accounts.map((acct) => (
          <div key={acct.id} className={`p-4 rounded-xl border ${acct.active ? 'border-emp3r0r-500/40 bg-emp3r0r-500/5' : 'border-gray-800 bg-gray-900'}`}>
            <div className="flex items-center justify-between mb-3">
              <div className="flex items-center gap-2">
                {acct.active && <ShieldCheck className="w-5 h-5 text-emp3r0r-400" />}
                <span className="font-medium text-gray-200 truncate max-w-[220px]">{acct.label || acct.id.slice(0, 12)}</span>
                {acct.active && <span className="px-2 py-0.5 bg-emp3r0r-500/30 text-emp3r0r-400 text-xs rounded-full">活跃</span>}
              </div>
              <div className="flex gap-1">
                <button onClick={() => { setEditing(acct); setForm({ id: acct.id, api_token: '', label: acct.label, domain: acct.domain, zone_id: acct.zone_id }); setShowAdd(true) }} className="p-2 hover:bg-gray-800 rounded-lg text-gray-400"><Pencil className="w-4 h-4" /></button>
                {!acct.active && (
                  <button onClick={() => remove(acct)} className="p-2 hover:bg-red-500/10 rounded-lg text-red-400"><Trash2 className="w-4 h-4" /></button>
                )}
              </div>
            </div>

            <div className="space-y-2 text-xs text-gray-500 mb-3">
              <div className="flex gap-2"><span className="w-16 shrink-0">账号</span><span className="font-mono text-gray-400 truncate">{acct.id}</span></div>
              <div className="flex gap-2"><span className="w-16 shrink-0">Token</span><span className="font-mono text-gray-400">{acct.token_masked}</span></div>
              {acct.domain && (
                <div className="flex gap-2"><Globe className="w-3.5 h-3.5 shrink-0" /><span className="font-mono text-gray-400">{acct.domain}</span></div>
              )}
            </div>

            {acct.quota ? (
              <div className="space-y-1.5">
                <QuotaBar pct={acct.do_used_pct} label="DO 请求/月" />
                <div className="text-[11px] text-gray-600">数据日期 {acct.quota.data_date} · DO 错误 {acct.quota.durable_objects.errors_month}</div>
              </div>
            ) : (
              <div className="text-xs text-gray-600">无配额数据（token 未配置或查询失败）</div>
            )}

            {!acct.active && (
              <button
                onClick={() => migrate(acct)}
                disabled={migrating === acct.id}
                className="mt-3 w-full flex items-center justify-center gap-2 px-4 py-2 bg-emp3r0r-500/20 hover:bg-emp3r0r-500/30 border border-emp3r0r-500/40 text-emp3r0r-400 text-sm rounded-lg transition-colors disabled:opacity-50">
                {migrating === acct.id ? <Loader2 className="w-4 h-4 animate-spin" /> : <Rocket className="w-4 h-4" />}
                {migrating === acct.id ? '迁移中…' : '迁移到此账号'}
              </button>
            )}
          </div>
        ))}
      </div>

      {/* history */}
      {fleet && fleet.history.length > 0 && (
        <div className="p-4 bg-gray-900 border border-gray-800 rounded-xl">
          <h2 className="flex items-center gap-2 text-sm font-medium text-gray-300 mb-3"><History className="w-4 h-4" /> 迁移历史</h2>
          <div className="space-y-2">
            {fleet.history.map((h: CFMigrationHistory, i: number) => (
              <div key={i} className="flex items-center gap-3 text-xs text-gray-500">
                <span className="font-mono">{fmtTime(h.time)}</span>
                <span className="font-mono text-gray-400">{h.from_host || '(初始)'}</span>
                <span className="text-emp3r0r-400">→</span>
                <span className="font-mono text-gray-300">{h.to_host}</span>
                <span className="text-gray-600">{h.reason}</span>
              </div>
            ))}
          </div>
        </div>
      )}

      {/* add/edit dialog */}
      {showAdd && (
        <div className="fixed inset-0 bg-black/60 flex items-center justify-center z-50 p-4" onClick={() => setShowAdd(false)}>
          <div className="bg-gray-900 border border-gray-700 rounded-xl p-5 w-full max-w-md space-y-4" onClick={(e) => e.stopPropagation()}>
            <h2 className="text-lg font-bold text-gray-100">{editing ? '编辑账号' : '添加 CF 账号'}</h2>
            <div className="space-y-3">
              <Field label="Account ID" value={form.id} disabled={!!editing} onChange={(v) => setForm({ ...form, id: v })} placeholder="32 位十六进制" />
              <Field label="API Token" value={form.api_token} onChange={(v) => setForm({ ...form, api_token: v })} placeholder={editing ? '留空则不变' : 'cfut_…'} />
              <Field label="标签（如邮箱）" value={form.label} onChange={(v) => setForm({ ...form, label: v })} placeholder="label@example.com" />
              <Field label="域名（可选）" value={form.domain} onChange={(v) => setForm({ ...form, domain: v })} placeholder="xxx.kdns.fr" />
              <Field label="Zone ID（可选）" value={form.zone_id} onChange={(v) => setForm({ ...form, zone_id: v })} placeholder="32 位十六进制" />
            </div>
            <div className="flex justify-end gap-2">
              <button onClick={() => setShowAdd(false)} className="px-4 py-2 bg-gray-800 hover:bg-gray-700 rounded-lg text-gray-300 text-sm">取消</button>
              <button onClick={editing ? submitEdit : submitAdd} className="px-4 py-2 bg-emp3r0r-500 hover:bg-emp3r0r-600 rounded-lg text-white text-sm font-medium">
                {editing ? '保存' : '添加'}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}

function Field({ label, value, onChange, placeholder, disabled }: {
  label: string; value: string; onChange: (v: string) => void; placeholder?: string; disabled?: boolean
}) {
  return (
    <div>
      <label className="text-xs text-gray-500">{label}</label>
      <input
        type="text"
        value={value}
        disabled={disabled}
        onChange={(e) => onChange(e.target.value)}
        placeholder={placeholder}
        className="mt-1 w-full px-3 py-2 bg-gray-800 border border-gray-700 rounded-lg text-gray-200 text-sm font-mono focus:outline-none focus:border-emp3r0r-500 disabled:opacity-50"
      />
    </div>
  )
}
