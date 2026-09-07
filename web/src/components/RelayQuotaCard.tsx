import { useCallback, useEffect, useState } from 'react'
import { Cloud, RefreshCw, AlertTriangle, Key, Copy } from 'lucide-react'
import { api } from '@/lib/api'

interface RelayQuota {
  configured: boolean
  api_token?: string
  account_id?: string
  workers: {
    requests_today: number
    errors_today: number
    subrequests_today: number
    requests_yesterday: number
  }
  durable_objects: {
    requests_month: number
    errors_month: number
    active_time_us: number
    gb_seconds: number
  }
  limits: {
    workers_requests_per_day: number
    do_requests_per_month: number
    do_gb_seconds_per_month: number
  }
  data_date: string
  fetched_at: string
  error?: string
}

function pctColor(pct: number): string {
  if (pct >= 85) return 'from-red-600 to-red-500'
  if (pct >= 60) return 'from-yellow-600 to-yellow-500'
  return 'from-green-600 to-green-500'
}

function maskToken(token: string): string {
  // cfut_xxxx…xxxx — short form for the row, full value on click-to-copy
  if (token.length <= 14) return token
  return `${token.slice(0, 10)}…${token.slice(-4)}`
}

function Bar({ label, value, limit, unit }: { label: string; value: number; limit: number; unit: string }) {
  const pct = limit > 0 ? Math.min(100, (value / limit) * 100) : 0
  return (
    <div>
      <div className="flex justify-between text-sm mb-1.5">
        <span className="text-gray-400">{label}</span>
        <span className="text-gray-500 font-mono text-xs">
          {value.toLocaleString()} / {limit.toLocaleString()} {unit}
        </span>
      </div>
      <div className="h-2 md:h-2.5 bg-gray-800 rounded-full overflow-hidden">
        <div
          className={`h-full bg-gradient-to-r ${pctColor(pct)} transition-all duration-500`}
          style={{ width: `${pct}%` }}
        />
      </div>
    </div>
  )
}

export function RelayQuotaCard() {
  const [quota, setQuota] = useState<RelayQuota | null>(null)
  const [loading, setLoading] = useState(false)

  const refresh = useCallback(async () => {
    setLoading(true)
    try {
      setQuota(await api.getRelayQuota())
    } catch {
      // card stays on last data; error path is rendered below
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    refresh()
    const t = setInterval(refresh, 60_000)
    return () => clearInterval(t)
  }, [refresh])

  return (
    <div className="bg-gray-900 border border-gray-800 rounded-xl p-4">
      <div className="flex items-center justify-between mb-4">
        <h2 className="text-base md:text-lg font-semibold text-gray-100 flex items-center gap-2">
          <Cloud className="w-5 h-5 text-gray-400" />
          Relay Worker Quota
        </h2>
        <button
          onClick={refresh}
          disabled={loading}
          className="p-2 hover:bg-gray-800 rounded-lg transition-colors disabled:opacity-50"
          title="Refresh usage from Cloudflare"
        >
          <RefreshCw className={`w-4 h-4 text-gray-400 ${loading ? 'animate-spin' : ''}`} />
        </button>
      </div>

      {!quota ? (
        <p className="text-sm text-gray-500">Loading…</p>
      ) : !quota.configured ? (
        <div className="text-sm text-gray-500 space-y-1">
          <p className="flex items-center gap-2 text-yellow-500">
            <AlertTriangle className="w-4 h-4" /> Not configured
          </p>
          <p className="font-mono text-xs">
            Write ~/.emp3r0r/cf_relay.json on the C2 server:
          </p>
          <pre className="text-xs bg-gray-800 rounded-lg p-2 overflow-x-auto">
{`{
  "api_token": "cfut_...",
  "account_id": "..."
}`}
          </pre>
          {quota.error && <p className="text-xs text-gray-600">{quota.error}</p>}
        </div>
      ) : (
        <div className="space-y-4">
          <Bar
            label="Workers requests (today)"
            value={quota.workers.requests_today}
            limit={quota.limits.workers_requests_per_day}
            unit=""
          />
          <Bar
            label="Durable Objects requests (month)"
            value={quota.durable_objects.requests_month}
            limit={quota.limits.do_requests_per_month}
            unit=""
          />
          <Bar
            label="Durable Objects duration (month)"
            value={Math.round(quota.durable_objects.gb_seconds * 10) / 10}
            limit={quota.limits.do_gb_seconds_per_month}
            unit="GB-s"
          />
          <div className="flex items-center justify-between text-xs text-gray-500 pt-1">
            <span>
              DO errors this month:{' '}
              <span className={quota.durable_objects.errors_month > 0 ? 'text-red-400 font-mono' : 'font-mono'}>
                {quota.durable_objects.errors_month.toLocaleString()}
              </span>
              {quota.durable_objects.requests_month > 0 && (
                <span className="text-gray-600">
                  {' '}of {quota.durable_objects.requests_month.toLocaleString()}
                </span>
              )}
            </span>
            <span title="Cloudflare analytics aggregation lags a few hours">
              data as of {quota.data_date || '—'}
            </span>
          </div>
          {(quota.api_token || quota.account_id) && (
            <div className="flex items-center gap-2 text-xs text-gray-600 pt-1 border-t border-gray-800/60 mt-1 pt-2">
              <Key className="w-3 h-3 shrink-0" />
              <span className="font-mono truncate" title={`account: ${quota.account_id || ''}`}>
                {quota.account_id}
              </span>
              <button
                onClick={() => navigator.clipboard?.writeText(quota.api_token || '')}
                className="font-mono text-gray-500 hover:text-gray-300 truncate transition-colors"
                title={`token: ${quota.api_token}\n(click to copy)`}
              >
                {maskToken(quota.api_token || '')}
              </button>
              <button
                onClick={() => navigator.clipboard?.writeText(quota.api_token || '')}
                className="p-1 hover:bg-gray-800 rounded shrink-0 transition-colors"
                title="Copy API token"
              >
                <Copy className="w-3 h-3" />
              </button>
            </div>
          )}
          {quota.error && (
            <p className="text-xs text-yellow-500 flex items-start gap-1">
              <AlertTriangle className="w-3 h-3 mt-0.5 shrink-0" />
              {quota.error}
            </p>
          )}
        </div>
      )}
    </div>
  )
}
