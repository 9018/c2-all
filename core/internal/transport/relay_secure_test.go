package transport

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"
	"time"
)

// TestSecureConnOverRelay 验证 SecureConn(AES-GCM) 能跑在 worker_ws 通道上。
// 需要 local relay: wrangler dev --port 8806 --var EMP_SHARED_SECRET=testsec
func TestSecureConnOverRelay(t *testing.T) {
	room := fmt.Sprintf("sec-test-%d", time.Now().UnixNano()%1_000_000)
	base := "ws://127.0.0.1:8806"
	secret := "testsec"

	// CC 侧
	listener, err := NewRelayListener(fmt.Sprintf("%s/ws/%s?role=cc&secret=%s", base, room, secret))
	if err != nil {
		t.Fatalf("relay listener: %v", err)
	}
	defer listener.Close()

	type accepted struct {
		conn io.ReadWriteCloser
		err  error
	}
	ccCh := make(chan accepted, 1)
	go func() {
		c, err := listener.Accept()
		ccCh <- accepted{c, err}
	}()

	// agent 侧：走正式 channel 注册表路径
	wrapper, err := GetC2ChannelWrapper("worker_ws")
	if err != nil {
		t.Fatalf("GetC2ChannelWrapper: %v", err)
	}
	agentConn, resp, err := wrapper.Dial(context.Background(), nil,
		fmt.Sprintf("%s/ws/%s?role=agent&secret=%s", base, room, secret))
	if err != nil {
		t.Fatalf("agent dial: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}

	// agent 先写一条触发 CC 侧 Accept
	if _, err := agentConn.Write([]byte("trigger")); err != nil {
		t.Fatalf("trigger write: %v", err)
	}
	var ccConn io.ReadWriteCloser
	select {
	case a := <-ccCh:
		ccConn, a = a.conn, a
		if a.err != nil {
			t.Fatalf("accept: %v", a.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("accept timeout")
	}
	defer ccConn.Close()

	// 消费 agent 的原始触发字节（不进入 SecureConn 流）
	trigger := make([]byte, len("trigger"))
	if _, err := io.ReadFull(ccConn, trigger); err != nil {
		t.Fatalf("read trigger: %v", err)
	}

	// 双向多轮大块数据（SecureConn 用 32KB 块，验证分块与重组）
	scA := NewSecureConn(agentConn)
	scB := NewSecureConn(ccConn)
	payload := bytes.Repeat([]byte("A"), 100_000)
	go func() { _, _ = scA.Write(payload) }()
	got := make([]byte, len(payload))
	readDone := make(chan error, 1)
	go func() { _, err := io.ReadFull(scB, got); readDone <- err }()
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("read 100KB: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("read 100KB timeout")
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("payload mismatch after SecureConn")
	}
	t.Log("SecureConn over relay: 100KB roundtrip OK")

	// 反向
	payload2 := bytes.Repeat([]byte("B"), 64_000)
	go func() { _, _ = scB.Write(payload2) }()
	got2 := make([]byte, len(payload2))
	readDone2 := make(chan error, 1)
	go func() { _, err := io.ReadFull(scA, got2); readDone2 <- err }()
	select {
	case err := <-readDone2:
		if err != nil {
			t.Fatalf("read reverse: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("read reverse timeout")
	}
	if !bytes.Equal(got2, payload2) {
		t.Fatal("reverse payload mismatch")
	}
	t.Log("reverse: 64KB OK")
}
