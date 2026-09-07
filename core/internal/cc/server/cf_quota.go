package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jm33-m0/emp3r0r/core/internal/live"
	"github.com/jm33-m0/emp3r0r/core/lib/logging"
)

// Cloudflare Worker relay quota panel.
//
// Shows free-plan burn-down for the account hosting the relay Worker:
//   - Workers requests (per day, free: 100k/day)
//   - Durable Objects requests/errors (per month, free: 100k/month)
//   - Durable Objects duration in GB-s (per month, free: 13k GB-s)
//
// The always-on DO bug we fixed with the Hibernation API burned the whole
// monthly duration quota in ~a day; this panel makes that visible before
// agents start getting 500s account-wide.
//
// Credentials live in <workspace>/cf_relay.json (NOT committed):
//
//	{"api_token": "cfut_...", "account_id": "f8b31..."}

// cfRelayConfig is the on-disk credential file for the quota panel.
type cfRelayConfig struct {
	APIToken  string `json:"api_token"`
	AccountID string `json:"account_id"`
}

// cfQuotaResponse is what GET /api/relay-quota returns.
type cfQuotaResponse struct {
	// false when cf_relay.json is missing/incomplete
	Configured bool `json:"configured"`

	Workers struct {
		RequestsToday     int64 `json:"requests_today"`
		ErrorsToday       int64 `json:"errors_today"`
		SubrequestsToday  int64 `json:"subrequests_today"`
		RequestsYesterday int64 `json:"requests_yesterday"`
	} `json:"workers"`

	DurableObjects struct {
		RequestsMonth int64   `json:"requests_month"`
		ErrorsMonth   int64   `json:"errors_month"`
		ActiveTimeUS  int64   `json:"active_time_us"`
		GBSeconds     float64 `json:"gb_seconds"`
	} `json:"durable_objects"`

	// free-plan limits for the progress bars
	Limits struct {
		WorkersRequestsPerDay int64   `json:"workers_requests_per_day"`
		DORequestsPerMonth    int64   `json:"do_requests_per_month"`
		DOGBSecondsPerMonth   float64 `json:"do_gb_seconds_per_month"`
	} `json:"limits"`

	// latest date present in CF's analytics (aggregation lags a few hours)
	DataDate string `json:"data_date"`

	FetchedAt time.Time `json:"fetched_at"`
	Error     string    `json:"error,omitempty"`
}

var (
	cfQuotaCacheMu sync.Mutex
	cfQuotaCache   *cfQuotaResponse
	cfQuotaCacheAt time.Time
)

// free-plan limits (Workers Free)
const (
	cfLimitWorkersRequestsPerDay = 100_000
	cfLimitDORequestsPerMonth    = 100_000
	cfLimitDOGBSecondsPerMonth   = 13_000.0
)

func loadCFRelayConfig() (*cfRelayConfig, error) {
	path := filepath.Join(live.EmpWorkSpace, "cf_relay.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg cfRelayConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.APIToken == "" || cfg.AccountID == "" {
		return nil, fmt.Errorf("%s needs api_token and account_id", path)
	}
	return &cfg, nil
}

// cfGraphqlResponse mirrors the combined analytics query.
type cfGraphqlResponse struct {
	Data struct {
		Viewer struct {
			Accounts []struct {
				WorkersInvocationsAdaptive []struct {
					Sum struct {
						Requests     int64 `json:"requests"`
						Errors       int64 `json:"errors"`
						Subrequests  int64 `json:"subrequests"`
					} `json:"sum"`
					Dimensions struct {
						Date string `json:"date"`
					} `json:"dimensions"`
				} `json:"workersInvocationsAdaptive"`
				DurableObjectsInvocationsAdaptiveGroups []struct {
					Sum struct {
						Requests int64 `json:"requests"`
						Errors   int64 `json:"errors"`
					} `json:"sum"`
					Dimensions struct {
						Date string `json:"date"`
					} `json:"dimensions"`
				} `json:"durableObjectsInvocationsAdaptiveGroups"`
				DurableObjectsPeriodicGroups []struct {
					Sum struct {
						ActiveTime int64 `json:"activeTime"`
					} `json:"sum"`
					Dimensions struct {
						Date string `json:"date"`
					} `json:"dimensions"`
				} `json:"durableObjectsPeriodicGroups"`
			} `json:"accounts"`
		} `json:"viewer"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// fetchCFQuota queries Cloudflare's GraphQL analytics for the relay Worker.
func fetchCFQuota(cfg *cfRelayConfig) (*cfQuotaResponse, error) {
	now := time.Now().UTC()
	today := now.Format("2006-01-02")
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)

	query := `query($account: AccountTag!, $today: Time!, $month: Time!) {
		viewer {
			accounts(filter: {accountTag: $account}) {
				workersInvocationsAdaptive(filter: {datetime_geq: $today}, limit: 10000, orderBy: [date_ASC]) {
					sum { requests errors subrequests }
					dimensions { date }
				}
				durableObjectsInvocationsAdaptiveGroups(filter: {datetimeHour_geq: $month}, limit: 10000) {
					sum { requests errors }
					dimensions { date }
				}
				durableObjectsPeriodicGroups(filter: {datetimeHour_geq: $month}, limit: 10000) {
					sum { activeTime }
					dimensions { date }
				}
			}
		}
	}`
	payload, err := json.Marshal(map[string]any{
		"query": query,
		"variables": map[string]string{
			"account": cfg.AccountID,
			"today":   today + "T00:00:00Z",
			"month":   monthStart,
		},
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, "https://api.cloudflare.com/client/v4/graphql", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIToken)
	req.Header.Set("Content-Type", "application/json")
	req.Body = io.NopCloser(bytes.NewReader(payload))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cloudflare graphql HTTP %d: %.200s", resp.StatusCode, body)
	}

	var g cfGraphqlResponse
	if err := json.Unmarshal(body, &g); err != nil {
		return nil, err
	}
	if len(g.Errors) > 0 {
		return nil, fmt.Errorf("graphql: %s", g.Errors[0].Message)
	}

	out := &cfQuotaResponse{Configured: true, FetchedAt: time.Now()}
	out.Limits.WorkersRequestsPerDay = cfLimitWorkersRequestsPerDay
	out.Limits.DORequestsPerMonth = cfLimitDORequestsPerMonth
	out.Limits.DOGBSecondsPerMonth = cfLimitDOGBSecondsPerMonth

	accounts := g.Data.Viewer.Accounts
	if len(accounts) == 0 {
		return out, nil
	}
	acct := accounts[0]

	// workers: split today / yesterday by date dimension
	for _, row := range acct.WorkersInvocationsAdaptive {
		switch row.Dimensions.Date {
		case today:
			out.Workers.RequestsToday += row.Sum.Requests
			out.Workers.ErrorsToday += row.Sum.Errors
			out.Workers.SubrequestsToday += row.Sum.Subrequests
		case yesterday:
			out.Workers.RequestsYesterday += row.Sum.Requests
		}
		if row.Dimensions.Date > out.DataDate {
			out.DataDate = row.Dimensions.Date
		}
	}

	for _, row := range acct.DurableObjectsInvocationsAdaptiveGroups {
		out.DurableObjects.RequestsMonth += row.Sum.Requests
		out.DurableObjects.ErrorsMonth += row.Sum.Errors
		if row.Dimensions.Date > out.DataDate {
			out.DataDate = row.Dimensions.Date
		}
	}

	for _, row := range acct.DurableObjectsPeriodicGroups {
		out.DurableObjects.ActiveTimeUS += row.Sum.ActiveTime
		if row.Dimensions.Date > out.DataDate {
			out.DataDate = row.Dimensions.Date
		}
	}
	// DO duration billing: GB-s = active wall time (seconds) x 128MB
	out.DurableObjects.GBSeconds = float64(out.DurableObjects.ActiveTimeUS) / 1e6 * 0.125

	return out, nil
}

// handleWebRelayQuota serves GET /api/relay-quota (cached 60s).
func handleWebRelayQuota(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	cfQuotaCacheMu.Lock()
	if cfQuotaCache != nil && time.Since(cfQuotaCacheAt) < time.Minute {
		cfQuotaCacheMu.Unlock()
		json.NewEncoder(w).Encode(cfQuotaCache)
		return
	}
	cfQuotaCacheMu.Unlock()

	cfg, err := loadCFRelayConfig()
	if err != nil {
		resp := &cfQuotaResponse{Configured: false, FetchedAt: time.Now()}
		resp.Error = err.Error()
		cfQuotaCacheMu.Lock()
		cfQuotaCache, cfQuotaCacheAt = resp, time.Now()
		cfQuotaCacheMu.Unlock()
		json.NewEncoder(w).Encode(resp)
		return
	}

	quota, err := fetchCFQuota(cfg)
	if err != nil {
		logging.Warningf("relay-quota: %v", err)
		resp := &cfQuotaResponse{Configured: true, FetchedAt: time.Now()}
		resp.Limits.WorkersRequestsPerDay = cfLimitWorkersRequestsPerDay
		resp.Limits.DORequestsPerMonth = cfLimitDORequestsPerMonth
		resp.Limits.DOGBSecondsPerMonth = cfLimitDOGBSecondsPerMonth
		resp.Error = err.Error()
		quota = resp
	}

	cfQuotaCacheMu.Lock()
	cfQuotaCache, cfQuotaCacheAt = quota, time.Now()
	cfQuotaCacheMu.Unlock()
	json.NewEncoder(w).Encode(quota)
}
