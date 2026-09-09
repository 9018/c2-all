package server

import (
	"os"
	"testing"

	"github.com/jm33-m0/emp3r0r/core/internal/live"
)

// TestDeployRelayWorkerToStandby deploys the relay Worker to the first
// standby account via the CF Scripts API (no CC listener switch). Skips when
// the account file has no standby account or no API token.
func TestDeployRelayWorkerToStandby(t *testing.T) {
	live.EmpWorkSpace = os.Getenv("EMP3R0R_WORKSPACE")
	cfg, err := loadCFAccounts()
	if err != nil {
		t.Skipf("no cf_accounts.json: %v", err)
	}
	var target *CFAccount
	for _, a := range cfg.Accounts {
		if a.ID != cfg.ActiveAccountID && a.APIToken != "" {
			target = a
			break
		}
	}
	if target == nil {
		t.Skip("no standby account configured")
	}
	if os.Getenv("CF_DEPLOY_E2E") == "" {
		t.Skip("set CF_DEPLOY_E2E=1 to actually deploy to Cloudflare")
	}

	secret := cfg.SharedSecret
	if secret == "" {
		secret = os.Getenv("EMP_SHARED_SECRET")
	}
	if secret == "" {
		t.Fatal("no shared secret")
	}

	base, err := deployRelayWorker(target, cfg.WorkerName, secret)
	if err != nil {
		t.Fatalf("deployRelayWorker: %v", err)
	}
	t.Logf("deployed relay at %s", base)
	if err := checkWorkerHealth(base, 30e9); err != nil {
		t.Fatalf("health check: %v", err)
	}
	t.Logf("health OK for %s", base)
}
