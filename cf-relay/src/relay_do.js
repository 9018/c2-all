const MARKER_AGENT_MSG = 0x01;
const MARKER_CC_BROADCAST = 0xff;

export class RelayDO {
  constructor(state, env) {
    this.state = state;
    this.env = env;
    this.cc = null;
    this.agents = new Map();
    this.nextTag = 1;
  }

  async fetch(request) {
    const url = new URL(request.url);
    const role = url.searchParams.get('role') || 'agent';
    const upgradeHeader = request.headers.get('upgrade');
    if (!upgradeHeader || upgradeHeader.toLowerCase() !== 'websocket') {
      return new Response('expected websocket upgrade', { status: 426 });
    }

    const pair = new WebSocketPair();
    const client = pair['0'];
    const server = pair['1'];
    if (!client || !server) {
      return new Response('WebSocketPair construction failed', { status: 500 });
    }

    const roomId = url.pathname.split('/').filter(Boolean)[1] || 'default';
    const tag = role === 'cc' ? 'cc' : `a${this.nextTag++}`;

    server.accept();
    this.meta = this.meta || new Map();
    this.meta.set(server, { role, tag, roomId });

    if (role === 'cc') {
      if (this.cc) {
        try { this.cc.close(4000, 'superseded'); } catch {}
      }
      this.cc = server;
      this.send(server, { t: 'hello', role: 'cc', tag: 'cc', agents: [...this.agents.keys()] });
    } else {
      this.agents.set(tag, server);
      this.send(server, { t: 'hello', role: 'agent', tag, cc: !!this.cc });
      if (this.cc) this.send(this.cc, { t: 'agent-joined', tag });
    }

    server.addEventListener('message', (e) => this.onMessage(server, e.data));
    server.addEventListener('close', () => this.onClose(server));

    return new Response(null, { status: 101, webSocket: client });
  }

  onMessage(ws, data) {
    if (typeof data === 'string') {
      let msg = null;
      try { msg = JSON.parse(data); } catch { return; }
      if (msg && msg.t === 'ping') this.send(ws, { t: 'pong', time: Date.now() });
      return;
    }
    const meta = (this.meta && this.meta.get(ws)) || {};
    const role = meta.role;
    if (role === 'agent') {
      if (!this.cc) return;
      const tagByte = parseInt(String(meta.tag).slice(1), 10) & 0xff;
      const bytes = new Uint8Array(data instanceof ArrayBuffer ? new Uint8Array(data) : data);
      const framed = new Uint8Array(1 + bytes.length);
      framed[0] = tagByte;
      framed.set(bytes, 1);
      try { this.cc.send(framed); } catch {}
    } else if (role === 'cc') {
      const bytes = new Uint8Array(data instanceof ArrayBuffer ? new Uint8Array(data) : data);
      if (bytes.length < 1) return;
      const marker = bytes[0];
      const payload = bytes.subarray(1);
      if (marker === 0xff) {
        for (const [, aws] of this.agents) { try { aws.send(payload); } catch {} }
      } else {
        const aws = this.agents.get(`a${marker}`);
        if (aws) { try { aws.send(payload); } catch {} }
      }
    }
  }

  onClose(ws) {
    const meta = (this.meta && this.meta.get(ws)) || {};
    if (this.meta) this.meta.delete(ws);
    if (meta.role === 'cc' && this.cc === ws) {
      this.cc = null;
      // CC is gone: agents would otherwise hang forever sending into a void
      // (their sockets stay open, they wait for ACKs that never come).
      // Kill every agent socket so they redial and re-checkin when CC returns.
      for (const [tag, aws] of this.agents) {
        try { aws.close(4001, 'cc-gone'); } catch {}
        this.agents.delete(tag);
      }
      return;
    }
    if (meta.role === 'agent') {
      this.agents.delete(meta.tag);
      if (this.cc) this.send(this.cc, { t: 'agent-left', tag: meta.tag });
    }
  }

  send(ws, obj) {
    try { ws.send(JSON.stringify(obj)); } catch {}
  }
}
