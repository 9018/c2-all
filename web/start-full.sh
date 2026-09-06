#!/bin/bash

# emp3r0r Web Panel 完整启动脚本 (带模拟后端)

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "🚀 Starting emp3r0r Web Panel with Mock API..."
echo ""

# 启动模拟 API 服务器
echo "📦 Starting Mock API Server..."
cd "$SCRIPT_DIR/mock-server"
node server.js &
API_PID=$!

# 等待 API 服务器启动
sleep 2

# 启动前端开发服务器
echo ""
echo "🌐 Starting Frontend Dev Server..."
cd "$SCRIPT_DIR"
npm run dev &
DEV_PID=$!

echo ""
echo "═══════════════════════════════════════════════════════════════"
echo "  🎯 emp3r0r Web Panel is ready!"
echo "═══════════════════════════════════════════════════════════════"
echo ""
echo "  📍 Frontend:  http://localhost:5173"
echo "  📍 Mock API:  http://localhost:9443"
echo "  🔑 Token:     emp3r0r-test-token-12345"
echo ""
echo "  Open http://localhost:5173 in your browser"
echo "  Enter the token when prompted"
echo ""
echo "  Press Ctrl+C to stop all servers"
echo "═══════════════════════════════════════════════════════════════"
echo ""

# 捕获 Ctrl+C
cleanup() {
    echo ""
    echo "🛑 Shutting down..."
    kill $API_PID 2>/dev/null || true
    kill $DEV_PID 2>/dev/null || true
    echo "✅ Stopped"
    exit 0
}

trap cleanup SIGINT SIGTERM

# 等待
wait
