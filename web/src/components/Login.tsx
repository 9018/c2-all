import { useState } from 'react'
import { useStore } from '@/stores/useStore'
import { api } from '@/lib/api'
import { LogIn, Loader2 } from 'lucide-react'

export function Login() {
  const { setToken } = useStore()
  const [tokenInput, setTokenInput] = useState('')
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')

  const handleLogin = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!tokenInput.trim()) return

    setLoading(true)
    setError('')

    try {
      api.setToken(tokenInput)
      const valid = await api.healthCheck()
      if (valid) {
        setToken(tokenInput)
      } else {
        setError('Invalid token or server unreachable')
        api.setToken('')
      }
    } catch (err) {
      setError('Connection failed')
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="min-h-screen flex items-center justify-center bg-gray-950">
      <div className="w-full max-w-md p-8">
        {/* Logo */}
        <div className="text-center mb-8">
          <div className="w-16 h-16 bg-emp3r0r-500 rounded-2xl flex items-center justify-center mx-auto mb-4">
            <span className="text-white text-2xl font-bold">e</span>
          </div>
          <h1 className="text-2xl font-bold text-gray-100">emp3r0r</h1>
          <p className="text-gray-500 mt-2">Command & Control Panel</p>
        </div>

        {/* Login Form */}
        <form onSubmit={handleLogin} className="space-y-4">
          <div>
            <label className="block text-sm text-gray-400 mb-2">
              Access Token
            </label>
            <input
              type="password"
              value={tokenInput}
              onChange={(e) => setTokenInput(e.target.value)}
              placeholder="Enter your token..."
              className="w-full bg-gray-900 border border-gray-800 rounded-lg px-4 py-3 text-gray-100 placeholder-gray-600 focus:outline-none focus:border-emp3r0r-500 transition-colors"
            />
          </div>

          {error && (
            <p className="text-sm text-red-400">{error}</p>
          )}

          <button
            type="submit"
            disabled={!tokenInput.trim() || loading}
            className="w-full flex items-center justify-center gap-2 bg-emp3r0r-500 hover:bg-emp3r0r-600 disabled:opacity-50 disabled:cursor-not-allowed text-white font-medium py-3 rounded-lg transition-colors"
          >
            {loading ? (
              <Loader2 className="w-5 h-5 animate-spin" />
            ) : (
              <LogIn className="w-5 h-5" />
            )}
            Connect
          </button>
        </form>

        {/* Info */}
        <div className="mt-8 p-4 bg-gray-900/50 rounded-lg border border-gray-800">
          <p className="text-sm text-gray-500">
            <strong className="text-gray-400">Token:</strong> The access token
            is configured on the C2 server. It's used to authenticate your
            session with the operator API.
          </p>
        </div>
      </div>
    </div>
  )
}
