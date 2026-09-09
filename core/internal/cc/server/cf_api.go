package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gorilla/mux"
	"github.com/jm33-m0/emp3r0r/core/lib/logging"
)

// CF worker fleet management API.
//
//   GET    /api/cf/accounts             list accounts + per-account quota
//   POST   /api/cf/accounts             add an account
//   PUT    /api/cf/accounts/{id}        edit an account
//   DELETE /api/cf/accounts/{id}        remove (never the active one)
//   POST   /api/cf/accounts/{id}/activate   hot-migrate to this account
//   PUT    /api/cf/settings             migrate_threshold / auto_migrate / shared_secret
//   GET    /api/cf/status               active account + migration history

// cfAccountView is an account row for the panel (token masked).
type cfAccountView struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	TokenMasked string `json:"token_masked"`
	Domain      string `json:"domain"`
	ZoneID      string `json:"zone_id"`
	Active      bool   `json:"active"`
	// quota for this account (nil on fetch error)
	Quota *cfQuotaResponse `json:"quota,omitempty"`
	// DO usage percentage used for the migration bar
	DOUsedPct float64 `json:"do_used_pct"`
}

type cfFleetResponse struct {
	Accounts         []*cfAccountView           `json:"accounts"`
	ActiveAccountID  string                     `json:"active_account_id"`
	WorkerName       string                     `json:"worker_name"`
	SharedSecret     string                     `json:"shared_secret,omitempty"`
	MigrateThreshold int                        `json:"migrate_threshold"`
	AutoMigrate      bool                       `json:"auto_migrate"`
	History          []*CFMigrationHistoryEntry `json:"history"`
}

func cfFleetView(cfg *cfAccountsConfig, withQuota bool) *cfFleetResponse {
	out := &cfFleetResponse{
		Accounts:         make([]*cfAccountView, 0, len(cfg.Accounts)),
		ActiveAccountID:  cfg.ActiveAccountID,
		WorkerName:       cfg.WorkerName,
		SharedSecret:     cfg.SharedSecret,
		MigrateThreshold: cfg.MigrateThreshold,
		AutoMigrate:      cfg.AutoMigrate,
		History:          cfg.sortedHistory(),
	}
	for _, a := range cfg.Accounts {
		v := &cfAccountView{
			ID:          a.ID,
			Label:       a.Label,
			TokenMasked: maskToken(a.APIToken),
			Domain:      a.Domain,
			ZoneID:      a.ZoneID,
			Active:      a.ID == cfg.ActiveAccountID,
		}
		if withQuota && a.APIToken != "" {
			q, err := fetchCFQuota(&cfRelayConfig{APIToken: a.APIToken, AccountID: a.ID})
			if err == nil {
				v.Quota = q
				v.DOUsedPct = 100.0 * float64(q.DurableObjects.RequestsMonth) / float64(q.Limits.DORequestsPerMonth)
			}
		}
		out.Accounts = append(out.Accounts, v)
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func handleWebCFAccounts(w http.ResponseWriter, r *http.Request) {
	cfg, err := loadCFAccounts()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, cfFleetView(cfg, true))
	case http.MethodPost:
		var req struct {
			ID       string `json:"id"`
			APIToken string `json:"api_token"`
			Label    string `json:"label"`
			Domain   string `json:"domain"`
			ZoneID   string `json:"zone_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		req.ID = strings.TrimSpace(req.ID)
		req.APIToken = strings.TrimSpace(req.APIToken)
		if req.ID == "" || req.APIToken == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id and api_token required"})
			return
		}
		err := mutateCFAccounts(func(c *cfAccountsConfig) error {
			if c.findByID(req.ID) != nil {
				return &cfFleetError{"account already exists"}
			}
			c.Accounts = append(c.Accounts, &CFAccount{
				ID: req.ID, APIToken: req.APIToken, Label: req.Label,
				Domain: req.Domain, ZoneID: req.ZoneID,
			})
			return nil
		})
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		logging.Infof("cf accounts: added %s (%s)", req.Label, req.ID)
		cfg, _ = loadCFAccounts()
		writeJSON(w, http.StatusOK, cfFleetView(cfg, false))
	}
}

type cfFleetError struct{ msg string }

func (e *cfFleetError) Error() string { return e.msg }

func handleWebCFAccountByID(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	cfg, err := loadCFAccounts()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	switch r.Method {
	case http.MethodDelete:
		if cfg.ActiveAccountID == id {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cannot delete the active account; migrate away first"})
			return
		}
		_ = mutateCFAccounts(func(c *cfAccountsConfig) error {
			for i, a := range c.Accounts {
				if a.ID == id {
					c.Accounts = append(c.Accounts[:i], c.Accounts[i+1:]...)
					return nil
				}
			}
			return &cfFleetError{"account not found"}
		})
		cfg, _ = loadCFAccounts()
		writeJSON(w, http.StatusOK, cfFleetView(cfg, false))
	case http.MethodPut:
		var req struct {
			Label    string `json:"label"`
			APIToken string `json:"api_token"`
			Domain   string `json:"domain"`
			ZoneID   string `json:"zone_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		err := mutateCFAccounts(func(c *cfAccountsConfig) error {
			a := c.findByID(id)
			if a == nil {
				return &cfFleetError{"account not found"}
			}
			if req.Label != "" {
				a.Label = req.Label
			}
			if req.APIToken != "" {
				a.APIToken = req.APIToken
			}
			a.Domain = req.Domain
			a.ZoneID = req.ZoneID
			return nil
		})
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		cfg, _ = loadCFAccounts()
		writeJSON(w, http.StatusOK, cfFleetView(cfg, false))
	}
}

func handleWebCFActivate(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	reason := "manual activation from panel"
	go func() {
		if err := MigrateRelayTo(id, reason); err != nil {
			logging.Errorf("cf activate: %v", err)
			BroadcastToWebClients("cf_migrate_error", map[string]any{
				"account": id, "error": err.Error(),
			})
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "migration started", "account": id})
}

func handleWebCFSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MigrateThreshold *int    `json:"migrate_threshold"`
		AutoMigrate      *bool   `json:"auto_migrate"`
		SharedSecret     *string `json:"shared_secret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	err := mutateCFAccounts(func(c *cfAccountsConfig) error {
		if req.MigrateThreshold != nil {
			t := *req.MigrateThreshold
			if t <= 0 || t > 100 {
				return &cfFleetError{"migrate_threshold must be 1-100"}
			}
			c.MigrateThreshold = t
		}
		if req.AutoMigrate != nil {
			c.AutoMigrate = *req.AutoMigrate
		}
		if req.SharedSecret != nil && strings.TrimSpace(*req.SharedSecret) != "" {
			c.SharedSecret = strings.TrimSpace(*req.SharedSecret)
		}
		return nil
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	cfg, _ := loadCFAccounts()
	writeJSON(w, http.StatusOK, cfFleetView(cfg, false))
}

func handleWebCFStatus(w http.ResponseWriter, r *http.Request) {
	cfg, err := loadCFAccounts()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, cfFleetView(cfg, false))
}
