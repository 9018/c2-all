// relay_do.js — per-room relay Durable Object (WebSocket Hibernation API).
//
// WHY HIBERNATION (billing root cause fix):
//   The old implementation used `server.accept()` + in-memory Maps, which pins
//   the DO in memory 24/7: duration is billed as 128MB x wall-clock, so a free
//   plan's monthly duration quota (~13,000 GB-s, i.e. ~29h of an always-on
//   128MB object) is burned by about a day of idle sockets. Once exhausted,
//   EVERY DO call on the account returns 500 until the month resets.
//
//   The Hibernation API (`ctx.acceptWebSocket`) lets the DO evict from memory
//   while WebSockets stay connected at the edge: idle = zero duration charge,
//   and the DO is woken only by incoming messages.
//
// STATE MODEL (nothing is assumed resident — hibernation throws memory away):
//   - Role/tag metadata rides on each socket via serializeAttachment(), which
//     survives DO eviction (probed on the live runtime: `ws.tags` does NOT
//     exist; serializeAttachment/deserializeAttachment do).
//   - CC presence rides in storage ("ccAlive") to survive the case where a
//     closing socket is already deregistered from the tag index.
//   - The agent tag counter persists in storage so tags never collide across
//     evictions/restarts.
//
// Wire protocol (unchanged on the Go side, see c2channel_workerws.go):
//   join         -> text   {"t":"hello","role":"cc"|"agent","tag":..,"cc":bool,"agents":[..]}
//   agent joins  -> text   {"t":"agent-joined","tag":"a7"}        (to CC)
//   agent left   -> text   {"t":"agent-left","tag":"a7"}          (to CC)
//   CC gone      -> close code 4001 "cc-gone" on every agent socket (they redial)
//   superseded   -> close code 4000 on the previous CC socket
//   agent -> CC   -> binary [tagByte][payload]
//   CC -> agent   -> binary [tagByte][payload] targeted | [0xFF][payload] broadcast
//   keepalive     -> WS protocol ping/pong (edge-level; does not wake the DO)

const TAG_CC = 'role:cc';
const TAG_AGENT = 'role:agent';

export class RelayDO {
    constructor(ctx, env) {
        this.ctx = ctx;
        this.env = env;
        // Fixed request/response pairs are answered by the edge without waking
        // the DO — free keepalive for any client that speaks JSON ping.
        try {
            this.ctx.setWebSocketAutoResponse(
                new WebSocketRequestResponsePair(
                    JSON.stringify({ t: 'ping' }),
                    JSON.stringify({ t: 'pong' })
                )
            );
        } catch { /* runtime without auto-response support: fine */ }
    }

    // ---- membership helpers ---------------------------------------------------

    ccSockets() {
        return this.ctx.getWebSockets(TAG_CC);
    }

    agentSockets() {
        return this.ctx.getWebSockets(TAG_AGENT);
    }

    /** per-socket metadata that survives hibernation ({role, tag} or null) */
    metaOf(ws) {
        try {
            const m = ws.deserializeAttachment();
            if (m && typeof m === 'object') return m;
        } catch { /* no attachment / unsupported runtime */ }
        return null;
    }

    /** "a<number>" tag of a socket, or null */
    tagOf(ws) {
        const m = this.metaOf(ws);
        return m && m.tag ? String(m.tag) : null;
    }

    /** tag strings of connected agents, e.g. ["a1","a3"] */
    agentTags() {
        const out = [];
        for (const ws of this.agentSockets()) {
            const tag = this.tagOf(ws);
            if (tag) out.push(tag);
        }
        return out;
    }

    socketForTag(tag) {
        return this.ctx.getWebSockets(`tag:${tag}`)[0] || null;
    }

    /** numeric tag byte for agent->CC frames (a1 -> 0x01). Allocated tags are
     *  always 1..254 (0x00 = unreadable fallback, 0xFF = broadcast), so the
     *  bijection with socketForTag(`a${byte}`) holds. Agents whose tags are
     *  not readable (or overflow tags) fall back to byte 0, which is never
     *  legitimately assigned — degraded, never misrouted. */
    tagByteOf(ws) {
        const tag = this.tagOf(ws);
        if (!tag) return 0;
        const n = parseInt(String(tag).slice(1), 10);
        return Number.isFinite(n) && n >= 1 && n <= 254 ? n : 0;
    }

    send(ws, obj) {
        try { ws.send(JSON.stringify(obj)); } catch { /* socket already dead */ }
    }

    /** Agent tag allocation, persisted in storage so tags never collide
     *  across DO evictions/restarts.
     *
     *  The wire format carries the tag as ONE byte (agent->CC framing,
     *  CC->agent targeting), so the tag number must stay in 1..254 —
     *  0x00 is the fallback for unreadable tags and 0xFF is broadcast.
     *  The counter therefore wraps at 254 and skips numbers whose tag is
     *  still held by a connected agent (tag strings and tag bytes must
     *  stay in bijection or the CC's targeted frames get dropped on the
     *  floor by socketForTag()). */
    async nextAgentTag() {
        let n = (await this.ctx.storage.get('nextTag')) || 1;
        let probe = n > 254 ? 1 : n;
        for (let i = 0; i < 254; i++) {
            const tag = `a${probe}`;
            if (this.ctx.getWebSockets(`tag:${tag}`).length === 0) {
                await this.ctx.storage.put('nextTag', probe >= 254 ? 1 : probe + 1);
                return tag;
            }
            probe = probe >= 254 ? 1 : probe + 1;
        }
        // 254 concurrent agents in one room: give up on routability rather
        // than blocking the join (tagByteOf falls back to byte 0).
        await this.ctx.storage.put('nextTag', probe >= 254 ? 1 : probe + 1);
        return `overflow-${Date.now()}`;
    }

    // ---- entry point ------------------------------------------------------------

    async fetch(request) {
        const url = new URL(request.url);
        const role = url.searchParams.get('role') || 'agent';
        if (role !== 'cc' && role !== 'agent') {
            return new Response('role must be cc or agent', { status: 400 });
        }
        const upgradeHeader = request.headers.get('upgrade');
        if (!upgradeHeader || upgradeHeader.toLowerCase() !== 'websocket') {
            return new Response('expected websocket upgrade', { status: 426 });
        }

        const roomId = url.pathname.split('/').filter(Boolean)[1] || 'default';
        const pair = new WebSocketPair();
        const client = pair['0'];
        const server = pair['1'];
        if (!client || !server) {
            return new Response('WebSocketPair construction failed', { status: 500 });
        }

        if (role === 'cc') {
            // A new CC supersedes any previous one in this room.
            for (const old of this.ccSockets()) {
                try { old.close(4000, 'superseded'); } catch {}
            }
            this.ctx.acceptWebSocket(server, [TAG_CC, `room:${roomId}`]);
            // Ride role/tag on the socket itself so message/close handlers
            // can recover it after any hibernation.
            try { server.serializeAttachment({ role: 'cc', tag: 'cc' }); } catch {}
            // Remember CC presence in storage: on close events the runtime may
            // already have deregistered the socket from the tag index.
            await this.ctx.storage.put('ccAlive', true);
            this.send(server, { t: 'hello', role: 'cc', tag: 'cc', agents: this.agentTags() });
        } else {
            const tag = await this.nextAgentTag();
            this.ctx.acceptWebSocket(server, [TAG_AGENT, `tag:${tag}`, `room:${roomId}`]);
            try { server.serializeAttachment({ role: 'agent', tag }); } catch {}
            const cc = this.ccSockets();
            this.send(server, { t: 'hello', role: 'agent', tag, cc: cc.length > 0 });
            for (const c of cc) this.send(c, { t: 'agent-joined', tag });
        }

        return new Response(null, { status: 101, webSocket: client });
    }

    // ---- hibernation event handlers -----------------------------------------------

    async webSocketMessage(ws, message) {
        if (typeof message === 'string') {
            // Inbound text is a legacy JSON ping; the auto-responder normally
            // eats it at the edge, answer manually if it got through.
            let msg = null;
            try { msg = JSON.parse(message); } catch { return; }
            if (msg && msg.t === 'ping') this.send(ws, { t: 'pong' });
            return;
        }

        const bytes = message instanceof ArrayBuffer
            ? new Uint8Array(message)
            : new Uint8Array(message.buffer, message.byteOffset, message.byteLength);

        const meta = this.metaOf(ws);
        if (meta && meta.role === 'agent') {
            // agent -> CC: prepend the agent's tag byte for CC-side demux
            const tagByte = this.tagByteOf(ws);
            const framed = new Uint8Array(1 + bytes.length);
            framed[0] = tagByte;
            framed.set(bytes, 1);
            for (const cc of this.ccSockets()) {
                try { cc.send(framed); } catch {}
            }
            return;
        }

        // CC -> agent(s): [target tag byte | 0xFF broadcast][payload]
        if (bytes.length < 1) return;
        const marker = bytes[0];
        const payload = bytes.subarray(1);
        if (marker === 0xff) {
            for (const aws of this.agentSockets()) {
                try { aws.send(payload); } catch {}
            }
            return;
        }
        const target = this.socketForTag(`a${marker}`);
        if (target) {
            try { target.send(payload); } catch {}
        }
        // Unknown tag byte: frame dropped (agent may have left between CC's
        // routing-table view and this send). CC re-syncs on reconnect.
    }

    async webSocketClose(ws, code, reason, wasClean) {
        await this.handleLeave(ws);
    }

    async webSocketError(ws, error) {
        try { ws.close(1011, 'error'); } catch {}
        await this.handleLeave(ws);
    }

    async handleLeave(ws) {
        const meta = this.metaOf(ws);

        if (!meta || meta.role !== 'cc') {
            // departing agent: notify CC (best effort)
            const tag = meta && meta.tag;
            if (tag) {
                for (const cc of this.ccSockets()) this.send(cc, { t: 'agent-left', tag });
            }
            return;
        }

        // Departing CC (identified by its attachment, or — when the runtime
        // has already deregistered the closing socket — by the fact that no
        // CC socket remains while storage says one was present).
        const ccAlive = await this.ctx.storage.get('ccAlive');
        if (!meta && (this.ccSockets().length > 0 || !ccAlive)) {
            // A CC is still connected (this was a superseded/unknown socket)
            // or no CC was ever present — nothing to sweep.
            return;
        }
        if (this.ccSockets().some(c => c !== ws)) {
            // another CC already took over (supersede) — no sweep
            return;
        }
        await this.ctx.storage.delete('ccAlive');

        // CC is gone: agents would otherwise hang forever sending into a void
        // (their sockets stay open, they wait for ACKs that never come).
        // Kill every agent socket so they redial and re-checkin when CC returns.
        for (const aws of this.agentSockets()) {
            try { aws.close(4001, 'cc-gone'); } catch {}
        }
    }
}
