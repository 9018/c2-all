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
	if err := checkWorkerHealth(base, secret, 30e9); err != nil {
		t.Fatalf("health check: %v", err)
	}
	t.Logf("health OK for %s", base)
}

// TestDeployRelayWorkerToAllAccounts pushes the currently embedded worker
// source to EVERY account that has an API token (active included — an
// upload never switches listeners, and DO rooms survive script updates).
// Use it to roll out worker changes (e.g. a new /extip route) without a
// fleet migration.
func TestDeployRelayWorkerToAllAccounts(t *testing.T) {
	live.EmpWorkSpace = os.Getenv("EMP3R0R_WORKSPACE")
	cfg, err := loadCFAccounts()
	if err != nil {
		t.Skipf("no cf_accounts.json: %v", err)
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
	deployed := 0
	for _, a := range cfg.Accounts {
		if a.APIToken == "" {
			continue
		}
		base, err := deployRelayWorker(a, cfg.WorkerName, secret)
		if err != nil {
			t.Errorf("deploy to %s (%s): %v", a.ID, a.Label, err)
			continue
		}
		deployed++
		t.Logf("deployed to %s (%s) at %s", a.ID, a.Label, base)
		// health via custom domain when available; skip workers.dev checks
		// from networks where workers.dev is blocked (the deploy already
		// validated the upload, DO binding and route wiring)
		if a.Domain != "" {
			if err := checkWorkerHealth(base, secret, 30e9); err != nil {
				t.Errorf("health on %s: %v", base, err)
			} else {
				t.Logf("health OK for %s", base)
			}
		}
	}
	if deployed == 0 {
		t.Skip("no account with an API token")
	}
}
