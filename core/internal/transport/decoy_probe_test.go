// Package transport — decoy_probe_test.go
//
// Live probe for the decoy-visit request path: the unauthenticated
// browser-dressed GET must hit the camouflage page (200 + "Notes" HTML),
// and /favicon.ico must 404 like an ordinary site. Skips without
// ECH_PROBE_HOST.
package transport

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestDecoyVisitCamouflagePage(t *testing.T) {
	host := os.Getenv("ECH_PROBE_HOST")
	if host == "" {
		t.Skip("set ECH_PROBE_HOST (relay hostname) to run")
	}
	client := &http.Client{
		Timeout: 12 * time.Second,
		Transport: &http.Transport{
			ForceAttemptHTTP2: false,
			DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return BrowserLikeTLSDial(ctx, addr, nil)
			},
		},
	}
	for _, tc := range []struct {
		path   string
		wantCO int
		wantNC string // substring of body
	}{
		{"/", http.StatusOK, "Notes"},
		{"/favicon.ico", http.StatusNotFound, ""},
	} {
		req, err := http.NewRequest(http.MethodGet, "https://"+host+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		BrowserRequestHeaders(req.Header, "") // no secret — unauthenticated visitor
		req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", tc.path, err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		resp.Body.Close()
		if resp.StatusCode != tc.wantCO {
			t.Fatalf("GET %s: HTTP %d, want %d", tc.path, resp.StatusCode, tc.wantCO)
		}
		if tc.wantNC != "" && !strings.Contains(string(body), tc.wantNC) {
			t.Fatalf("GET %s: body missing %q", tc.path, tc.wantNC)
		}
		if strings.Contains(string(body), "emp3r0r") {
			t.Fatalf("GET %s: camouflage page leaks the framework name", tc.path)
		}
		t.Logf("GET %s -> %d OK (%d bytes)", tc.path, resp.StatusCode, len(body))
	}
}
