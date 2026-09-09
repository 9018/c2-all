import React from 'react'
import ReactDOM from 'react-dom/client'
import { Layout } from '@/components/Layout'
import './index.css'

function App() {
  return <Layout />
}

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>
)
