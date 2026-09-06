#!/bin/bash

# emp3r0r Web Panel 构建脚本

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

echo "🔨 Building emp3r0r Web Panel..."

# 安装依赖
if [ ! -d "node_modules" ]; then
    echo "📦 Installing dependencies..."
    npm install
fi

# 构建
echo "📦 Building production bundle..."
npm run build

echo ""
echo "✅ Build complete!"
echo "📁 Output directory: dist/"
echo ""
echo "To serve the built files:"
echo "  - Copy dist/* to your web server"
echo "  - Or use: npx serve dist"
