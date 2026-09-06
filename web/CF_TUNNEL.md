# Cloudflare Tunnel Integration (emp3r0r)

Expose the Web UI and Agent H2 endpoints through Cloudflare Tunnel.

Services:
- Web UI: https://localhost:9443 (self-signed cert)
- Agent H2: https://localhost:8888 (agent transport)

Constraints:
- Do NOT require client mTLS for Web UI (Cloudflare does not forward client certs to origin)
- Keep self-signed TLS on origins and set noTLSVerify: true in cloudflared originRequest
- For WebSocket on Web UI, force http2Origin: false to allow WS upgrade over HTTP/1.1

Example config.yml:

  tunnel: emp3r0r-c2
  credentials-file: /home/USER/.cloudflared/emp3r0r-c2.json
  ingress:
    - hostname: c2-web.example.com
      service: https://localhost:9443
      originRequest:
        noTLSVerify: true
        http2Origin: false
    - hostname: c2-agent.example.com
      service: https://localhost:8888
      originRequest:
        noTLSVerify: true
        http2Origin: true
    - service: http_status:404

Steps:
1) cloudflared tunnel login
2) cloudflared tunnel create emp3r0r-c2
3) cloudflared tunnel route dns emp3r0r-c2 c2-web.example.com
4) cloudflared tunnel route dns emp3r0r-c2 c2-agent.example.com
5) cloudflared --config config.yml tunnel run emp3r0r-c2

Verify:
- curl -kI https://c2-web.example.com/api/health
- WebSocket: wss://c2-web.example.com/api/ws?session=TOKEN
- Agent connects to https://c2-agent.example.com with pinned CA

Notes:
- P2P/KCP (UDP) is not supported by CF Tunnel. Use H2 mode.
- Ensure services bind to 0.0.0.0 for LAN access prior to tunneling.
