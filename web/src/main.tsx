import React, { useEffect } from 'react'
import ReactDOM from 'react-dom/client'
import { useStore } from '@/stores/useStore'
import { Layout } from '@/components/Layout'
import { Login } from '@/components/Login'
import { api } from '@/lib/api'
import './index.css'

function App() {
  const { token, setToken } = useStore()

  // 初始化时从 localStorage 恢复 token
  useEffect(() => {
    const savedToken = localStorage.getItem('emp3r0r_token')
    if (savedToken) {
      setToken(savedToken)
      api.setToken(savedToken)
    }
  }, [setToken])

  if (!token) {
    return <Login />
  }

  return <Layout />
}

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>
)
