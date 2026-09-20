package server

import (
	"os"
	"testing"
	"time"
)

func TestProbeRelayFunctionLive(t *testing.T) {
	base := os.Getenv("PROBE_RELAY_URL")
	secret := os.Getenv("EMP_SHARED_SECRET")
	if base == "" || secret == "" {
		t.Skip("set PROBE_RELAY_URL + EMP_SHARED_SECRET")
	}
	start := time.Now()
	if err := probeRelayFunction(base, secret); err != nil {
		t.Fatalf("functional probe FAILED: %v", err)
	}
	t.Logf("functional probe PASSED in %v", time.Since(start))
}
