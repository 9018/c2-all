#!/bin/bash

# emp3r0r Web Panel 启动脚本

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

echo "🚀 Starting emp3r0r Web Panel..."

# 检查 Node.js
if ! command -v node &> /dev/null; then
    echo "❌ Node.js is required. Please install it first."
    exit 1
fi

# 安装依赖
if [ ! -d "node_modules" ]; then
    echo "📦 Installing dependencies..."
    npm install
fi

# 启动开发服务器
echo "🌐 Starting development server..."
echo "📝 Panel will be available at: http://localhost:5173"
echo "📝 API proxy configured to: https://localhost:9443"
echo ""
echo "Press Ctrl+C to stop"
echo ""

npm run dev
