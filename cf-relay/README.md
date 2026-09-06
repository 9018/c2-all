# emp3r0r-cf-relay

Cloudflare Worker-based rendezvous relay for emp3r0r C2. Both the CC and
agents dial **out** to this Worker — no inbound ports anywhere, and each CF
account/domain you deploy to is an independent, disposable rendezvous point.

```
agent ──WSS(out)──▶ CF Worker/DO ──WSS(out)──▶ CC
                    (zero-trust relay:
                     sees only ciphertext —
                     SecureConn AES-GCM is end-to-end)
```

## Routes

| Route | Purpose | Auth |
|---|---|---|
| `GET /health` | liveness | none |
| `POST /dns`, `GET /dns?dns=...` | RFC 8484 DoH relay (multi-upstream failover) | `EMP_SHARED_SECRET` (Bearer or `?secret=`) |
| `GET /ws/<room>?role=<cc\|agent>&secret=...` | relay WebSocket pair per room | `EMP_SHARED_SECRET` |

## Framing (relay data plane)

- agent → relay: raw binary payload
- relay → CC: `[tag byte][payload]` (tag `a1`→`0x01`, … `a254`→`0xFE`)
- CC → relay: `[tag byte][payload]` targeted, `[0xFF][payload]` broadcast
- relay → agent: raw binary payload
- JSON **text** frames are control: `hello`, `agent-joined`, `agent-left`, `ping`/`pong`

## Deploy

```bash
npx wrangler deploy            # set EMP_SHARED_SECRET as a secret first:
npx wrangler secret put EMP_SHARED_SECRET
```

Then in emp3r0r CC config (`emp3r0r.json`):

```json
"relay_urls": ["wss://your.worker.example/ws/room1?role=cc&secret=SECRET"]
```

Agents embed the matching `ws(s)://.../ws/<room>?role=agent&secret=SECRET` URL
as their `CCAddress` with `c2_channel_mode: "worker_ws"`.

## Multi-account strategy

Each CF account gets its own Worker deployment + room names. A CC can list
several `relay_urls` (one per account/domain) — sessions survive the loss of
any single relay because agent identity lives in the end-to-end MsgAuth UUID
layer, not at the relay.

## Local development

```bash
npx wrangler dev --port 8806 --var EMP_SHARED_SECRET:testsec
```

Note: local `wrangler dev` occasionally drops the first WS handshake
(nondeterministic miniflare quirk, ~20% — retry; production CF is unaffected).

## Tests

- `/tmp/test_relay_ws.py <port>` — Python WS protocol conformance (9 steps)
- `core/internal/transport/relay_e2e_test.go` — Go channel ↔ relay E2E
- `core/internal/transport/relay_secure_test.go` — SecureConn AES-GCM over relay (100KB blocks)
