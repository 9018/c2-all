package transport

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestWorkerWSChannelE2E 需要 local relay (wrangler dev --port 8806, secret=testsec)
// 跳过条件：RELAY_TEST_URL 未设置。
func TestWorkerWSChannelE2E(t *testing.T) {
	relayBase := "ws://127.0.0.1:8806"
	room := fmt.Sprintf("go-test-%d", time.Now().UnixNano()%1_000_000)
	secret := "testsec"

	// ---- CC 侧：RelayListener (role=cc) ----
	ccURL := fmt.Sprintf("%s/ws/%s?role=cc&secret=%s", relayBase, room, secret)
	listener, err := NewRelayListener(ccURL)
	if err != nil {
		t.Fatalf("NewRelayListener: %v", err)
	}
	defer listener.Close()

	// ---- agent 侧：C2ChannelWrapper (role=agent) ----
	w := &WorkerWSChannelWrapper{Role: "agent"}
	agentURL := fmt.Sprintf("%s/ws/%s?role=agent&secret=%s", relayBase, room, secret)
	agentConn, resp, err := w.Dial(context.Background(), &http.Client{}, agentURL)
	if err != nil {
		t.Fatalf("agent dial: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("agent dial status %d", resp.StatusCode)
	}
	defer agentConn.Close()

	// CC 接受虚拟连接（agent 上线后 tag 分配）
	_ = listener
	vc := make(chan net.Conn, 1)
	go func() {
		c, err := listener.Accept()
		if err == nil {
			vc <- c
		}
	}()

	// agent 先发一条，CC 才能得知 tag 并生成虚拟连接
	banner := []byte("hello-i-am-agent")
	if _, err := agentConn.Write(banner); err != nil {
		t.Fatalf("agent write banner: %v", err)
	}

	var ccConn net.Conn
	select {
	case ccConn = <-vc:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for CC virtual conn")
	}

	// CC 读到 banner
	buf := make([]byte, len(banner))
	_ = ccConn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(ccConn, buf); err != nil {
		t.Fatalf("cc read banner: %v", err)
	}
	if !bytes.Equal(buf, banner) {
		t.Fatalf("banner mismatch: %q", buf)
	}

	// CC 回写（定向 tag 已由 virtualConn 记录）
	reply := []byte("hello-agent-this-is-cc")
	if _, err := ccConn.Write(reply); err != nil {
		t.Fatalf("cc write: %v", err)
	}
	rbuf := make([]byte, len(reply))
	_ = agentConn.(interface{ SetReadDeadline(time.Time) error }).SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(agentConn, rbuf); err != nil {
		t.Fatalf("agent read reply: %v", err)
	}
	if !bytes.Equal(rbuf, reply) {
		t.Fatalf("reply mismatch: %q", rbuf)
	}

	// 双向多次往返（顺序性）
	for i := 0; i < 20; i++ {
		msg := binary.BigEndian.AppendUint32(nil, uint32(i))
		if _, err := agentConn.Write(msg); err != nil {
			t.Fatalf("round %d agent write: %v", i, err)
		}
		got := make([]byte, 4)
		if _, err := io.ReadFull(ccConn, got); err != nil {
			t.Fatalf("round %d cc read: %v", i, err)
		}
		if binary.BigEndian.Uint32(got) != uint32(i) {
			t.Fatalf("round %d: got %d", i, binary.BigEndian.Uint32(got))
		}
		if _, err := ccConn.Write([]byte{byte(i)}); err != nil {
			t.Fatalf("round %d cc write: %v", i, err)
		}
		back := make([]byte, 1)
		if _, err := io.ReadFull(agentConn, back); err != nil {
			t.Fatalf("round %d agent read: %v", i, err)
		}
		if back[0] != byte(i) {
			t.Fatalf("round %d back: %d", i, back[0])
		}
	}
	t.Log("E2E relay channel: 20 round-trips OK")
}
