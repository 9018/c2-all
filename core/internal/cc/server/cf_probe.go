package server

// cf_probe.go — REAL functional probe for a deployed relay worker.
//
// A GET /health 200 only proves the Worker answered. It says nothing about
// the machinery production depends on: the WebSocket upgrade, the Durable
// Object room state (a CC presence flag), and the agent->CC binary pipe.
// probeRelayFunction exercises all of it in an isolated probe room:
//
//	1. CC leg joins        -> hello {"t":"hello","role":"cc"}
//	2. agent leg joins     -> hello {"t":"hello","role":"agent","cc":true}
//	                           (proves the DO knows the CC is present)
//	3. agent sends binary  -> CC leg receives [tag][payload]
//	                           (proves the DO relay pipe end to end)
//
// Every failure names the leg that broke, so the panel's health_error
// points at the actual defect instead of a bare HTTP status.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/jm33-m0/emp3r0r/core/internal/live"
	"github.com/jm33-m0/emp3r0r/core/internal/transport"
	"github.com/jm33-m0/emp3r0r/core/lib/logging"
)

// probeRelayFunction runs the CC+agent round-trip against baseURL
// (wss://host) in an isolated probe room. Returns nil only when the
// whole pipeline works.
func probeRelayFunction(baseURL, sharedSecret string) error {
	if sharedSecret == "" {
		return fmt.Errorf("no shared secret — cannot exercise the WS pipeline")
	}
	u, err := url.Parse(strings.Replace(baseURL, "wss://", "https://", 1))
	if err != nil || u.Host == "" {
		return fmt.Errorf("bad relay base %q", baseURL)
	}
	room := fmt.Sprintf("health-probe-%d", time.Now().UnixNano()%1_000_000)

	dial := func(role string) (*websocket.Conn, error) {
		wsURL := fmt.Sprintf("wss://%s/ws/%s?role=%s&secret=%s",
			u.Host, room, role, url.QueryEscape(sharedSecret))
		header := http.Header{}
		header.Set("X-Relay-Role", role)
		header.Set("Authorization", "Bearer "+sharedSecret)
		dialer := &websocket.Dialer{
			HandshakeTimeout: 10 * time.Second,
			NetDialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return transport.BrowserLikeTLSDial(ctx, addr, nil)
			},
			ReadBufferSize: 1 << 16,
		}
		conn, resp, err := dialer.Dial(wsURL, header)
		if err != nil {
			if resp != nil {
				return nil, fmt.Errorf("%s leg: WS dial HTTP %d", role, resp.StatusCode)
			}
			return nil, fmt.Errorf("%s leg: WS dial: %w", role, err)
		}
		return conn, nil
	}

	readHello := func(conn *websocket.Conn, wantRole string) (ccFlag bool, err error) {
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return false, fmt.Errorf("%s leg: hello read: %w", wantRole, err)
		}
		var hello struct {
			T    string `json:"t"`
			Role string `json:"role"`
			CC   bool   `json:"cc"`
		}
		if err := json.Unmarshal(raw, &hello); err != nil {
			return false, fmt.Errorf("%s leg: hello not JSON: %s", wantRole, string(raw[:sliceMax(len(raw), 64)]))
		}
		if hello.T != "hello" || hello.Role != wantRole {
			return false, fmt.Errorf("%s leg: unexpected hello %+v", wantRole, hello)
		}
		return hello.CC, nil
	}

	// 1. CC leg
	ccConn, err := dial("cc")
	if err != nil {
		return err
	}
	defer ccConn.Close()
	if _, err := readHello(ccConn, "cc"); err != nil {
		return err
	}

	// 2. agent leg — hello must report a CC in the room (DO state)
	agentConn, err := dial("agent")
	if err != nil {
		return err
	}
	defer agentConn.Close()
	ccSeen, err := readHello(agentConn, "agent")
	if err != nil {
		return err
	}
	if !ccSeen {
		return fmt.Errorf("agent leg: hello says cc=false — DO room state broken (CC leg is up)")
	}

	// 3. agent -> CC binary pipe
	agentConn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := agentConn.WriteMessage(websocket.BinaryMessage, []byte("probe-ping")); err != nil {
		return fmt.Errorf("agent leg: send: %w", err)
	}
	// the CC leg interleaves control frames (agent-joined etc.) with the
	// binary pipe — skip text until the piped payload arrives
	ccConn.SetReadDeadline(time.Now().Add(8 * time.Second))
	var mt int
	var data []byte
	for {
		mt, data, err = ccConn.ReadMessage()
		if err != nil {
			return fmt.Errorf("cc leg: pipe read: %w", err)
		}
		if mt == websocket.BinaryMessage {
			break
		}
	}
	if len(data) < len("probe-ping") {
		return fmt.Errorf("cc leg: pipe delivered len=%d — DO pipe broken", len(data))
	}
	if !strings.Contains(string(data), "probe-ping") {
		return fmt.Errorf("cc leg: pipe payload mismatch: %q", string(data[:sliceMax(len(data), 32)]))
	}

	logging.Warningf("relay probe %s: WS + DO room + agent->CC pipe all OK (%s)", room, baseURL)
	return nil
}

func sliceMax(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// handleWebCFTest runs the functional probe against an account's deployed
// relay on demand (panel "测试" button). The probe brings its own CC+agent
// legs, so it works for standby deployments too — no live CC needed there.
func handleWebCFTest(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	cfg, err := loadCFAccounts()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	target := cfg.findByID(id)
	if target == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown CF account"})
		return
	}
	if target.Domain == "" || target.ZoneID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account has no custom domain — nothing deployed to test"})
		return
	}
	relayBase := "wss://relay." + target.Domain
	if target.ID == cfg.ActiveAccountID && len(live.RuntimeConfig.RelayURLs) > 0 {
		// the active host is serving on its live URL — test exactly that
		relayBase = "wss://" + relayHostOf(live.RuntimeConfig.RelayURLs[0])
	}
	secret := cfg.SharedSecret
	if secret == "" {
		secret = relaySecretOf(live.RuntimeConfig.RelayURLs)
	}
	start := time.Now()
	err = probeRelayFunction(relayBase, secret)
	res := map[string]interface{}{
		"relay_base":  relayBase,
		"duration_ms": time.Since(start).Milliseconds(),
		"ok":          err == nil,
	}
	if err != nil {
		res["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, res)
}
