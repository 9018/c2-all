# Web UI Testing Guide (File Ops + Cloudflare Tunnel)

This guide covers:
- Automated end-to-end verification of file operations
- Structured logging added to the backend
- Cloudflare Tunnel setup for Web UI (9443) and Agent H2 (8888)

## 1) Automated Verification Script

Path: web/scripts/verify_file_ops.sh

What it does:
- Discovers an agent (or honors AGENT_TAG env)
- Exercises: mkdir, ls, upload (small and 2MB binary), stat, cp, mv, download (hash verify), rm files, rm dir
- Handles agent tags containing backslashes by JSON-escaping

Usage:
- Ensure CC is running with the Web server (run-webui.sh) and at least one agent is online
- TOKEN is read from ~/.emp3r0r/web_token.txt

Run:
  chmod +x web/scripts/verify_file_ops.sh
  ./web/scripts/verify_file_ops.sh

Expected output: all [PASS] lines, ending with “All checks passed.”

## 2) Structured Logging in Backend

File: core/internal/cc/server/web_server.go
Added lifecycle logs and timings for file APIs:
- [LS] start/done/timeout job=..., elapsed=...
- [DOWNLOAD/FTP] start, timeout
- [DOWNLOAD/CAT] start/done/timeout
- [UPLOAD] start size_b64=...
- [RM]/[MKDIR]/[STAT]/[CP]/[MV] start/done/timeout

File: core/internal/cc/server/handler_ftp_cbor.go
- Logs WebFTPSink registration/closure and bridges CBOR stream → HTTP.

## 3) Cloudflare Tunnel (cloudflared)

Goal:
- Expose Web UI (9443, self-signed) and Agent H2 (8888, mTLS to agents) through Cloudflare
- Token-based auth remains; do NOT use mTLS for Web UI since CF does not support client certs for origin auth.

Sample config.yml:

  tunnel: emp3r0r-c2
  credentials-file: /home/USER/.cloudflared/emp3r0r-c2.json
  ingress:
    - hostname: c2-web.example.com
      service: https://localhost:9443
      originRequest:
        noTLSVerify: true
        http2Origin: false  # gorilla/websocket prefers HTTP/1.1 for WS upgrade
    - hostname: c2-agent.example.com
      service: https://localhost:8888
      originRequest:
        noTLSVerify: true
        http2Origin: true
    - service: http_status:404

Notes:
- Use distinct hostnames for Web and Agent to keep routing simple
- WebSocket for Web UI is wss://c2-web.example.com/api/ws?session=TOKEN
- Agents connect to https://c2-agent.example.com with the pinned CA key (TLS still enforced end-to-end)
- P2P/KCP (UDP) cannot traverse CF Tunnel; switch to H2/mTLS mode

Run:
  cloudflared tunnel login
  cloudflared tunnel create emp3r0r-c2
  cloudflared tunnel route dns emp3r0r-c2 c2-web.example.com
  cloudflared tunnel route dns emp3r0r-c2 c2-agent.example.com
  cloudflared --config config.yml tunnel run emp3r0r-c2

Troubleshooting:
- 525/526 errors: set originRequest.noTLSVerify: true for self-signed TLS
- WS issues: ensure http2Origin: false for the Web UI origin to allow WS upgrade
- Verify local services: curl -kI https://localhost:9443/health and https://localhost:8888/

## 4) Frontend UX Enhancements (Next)
- Batch selection with checkboxes, bulk delete/download
- Right-side Properties panel (populate from /api/stat)
- Search/filter: client-side filter by name/ftype; later add server-side if needed

