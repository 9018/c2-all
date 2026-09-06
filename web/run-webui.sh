#!/bin/sh
set -e
export EMP3R0R_PREFIX=/home/a9017/.local
CC_BIN=${1:-/tmp/emp3r0r-cc-v18}
if pgrep -f emp3r0r-cc >/dev/null 2>&1; then
  echo "Killing existing emp3r0r-cc..."
  pkill -9 -f emp3r0r-cc || true
  sleep 1
fi
setsid "$CC_BIN" server --h2-port 8888 --http-port 7000 </dev/null >/tmp/c2_webui.log 2>&1 &
sleep 2
echo "Logs: tail -f /tmp/c2_webui.log"
ss -ltnp | grep -E '9443|8888|7000' || true
