package server

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jm33-m0/emp3r0r/core/lib/logging"
)

// Relay Worker deployment via the Cloudflare API (no wrangler needed).
//
// Mirrors what `wrangler deploy` does for cf-relay/wrangler.toml:
//   - PUT /accounts/{id}/workers/subdomain         (workers.dev activation,
//     only needed as a fallback endpoint when no custom domain is set)
//   - PUT /accounts/{id}/workers/scripts/{name}     (module upload, multipart:
//     metadata + worker.js + relay_do.js, DO binding, EMP_SHARED_SECRET)
//   - PUT /zones/{zone}/dns_records + /workers/routes (custom domain, when
//     the account has domain+zone_id configured)
//
// Migration pointer: re-uploading the script with a MIGRATE_URL plain-text
// binding makes the old deployment hand out close(4000, newURL) to every
// connected socket and 409 to new connections (see relay_do.js).

//go:embed cf_worker_src/worker.js cf_worker_src/relay_do.js
var cfWorkerSrc embed.FS

const (
	cfCompatDate    = "2025-03-01"
	cfDNSProxiedIP  = "192.0.2.1" // CF docs' placeholder origin for proxied records
	cfMigrateURLVar = "MIGRATE_URL"
)

// cfAPIError is the {success:false, errors:[...]} envelope CF returns.
type cfAPIError struct {
	Success bool `json:"success"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
	Result json.RawMessage `json:"result"`
}

func cfDo(method, token, apiURL string, body io.Reader, contentType string) ([]byte, int, error) {
	req, err := http.NewRequest(method, apiURL, body)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return data, resp.StatusCode, nil
}

// cfCheck decodes the CF error envelope; returns nil when success=true.
func cfCheck(data []byte, status int, what string) error {
	var e cfAPIError
	if err := json.Unmarshal(data, &e); err != nil {
		// some endpoints return bare results; only fail on HTTP error
		if status >= 400 {
			return fmt.Errorf("%s: HTTP %d: %.200s", what, status, data)
		}
		return nil
	}
	if !e.Success {
		msg := ""
		if len(e.Errors) > 0 {
			msg = fmt.Sprintf("code %d: %s", e.Errors[0].Code, e.Errors[0].Message)
		}
		return fmt.Errorf("%s: HTTP %d: %s", what, status, msg)
	}
	return nil
}

// ensureWorkersDevSubdomain activates the workers.dev subdomain for the
// account (idempotent: reads the existing one first, falls back to a
// deterministic emp-relay-XXXX name when none is registered yet).
func ensureWorkersDevSubdomain(acc *CFAccount, workerName string) (string, error) {
	apiURL := "https://api.cloudflare.com/client/v4/accounts/" + acc.ID + "/workers/subdomain"

	// Existing subdomain? Reuse it (10036 on PUT means one already exists).
	data, status, err := cfDo(http.MethodGet, acc.APIToken, apiURL, nil, "")
	if err == nil && status < 400 {
		var r struct {
			Result struct {
				Subdomain string `json:"subdomain"`
			} `json:"result"`
		}
		if json.Unmarshal(data, &r) == nil && r.Result.Subdomain != "" {
			return r.Result.Subdomain + ".workers.dev", nil
		}
	}

	sub := "emp-relay-" + acc.ID[:4]
	body, _ := json.Marshal(map[string]string{"subdomain": sub})
	data, status, err = cfDo(http.MethodPut, acc.APIToken, apiURL, bytes.NewReader(body), "application/json")
	if err != nil {
		return "", err
	}
	var e cfAPIError
	_ = json.Unmarshal(data, &e)
	if status >= 400 && len(e.Errors) > 0 && (e.Errors[0].Code == 10016 || e.Errors[0].Code == 10036) {
		// another name is already registered — fetch and reuse it
		data2, _, err2 := cfDo(http.MethodGet, acc.APIToken, apiURL, nil, "")
		if err2 == nil {
			var r struct {
				Result struct {
					Subdomain string `json:"subdomain"`
				} `json:"result"`
			}
			if json.Unmarshal(data2, &r) == nil && r.Result.Subdomain != "" {
				return r.Result.Subdomain + ".workers.dev", nil
			}
		}
		return "", fmt.Errorf("workers.dev subdomain exists but could not be read")
	}
	if err := cfCheck(data, status, "workers.dev subdomain"); err != nil {
		return "", err
	}
	return sub + ".workers.dev", nil
}

// uploadRelayWorker uploads (or replaces) the relay Worker script on the
// account. migrateURL, when non-empty, adds the MIGRATE_URL binding that
// points clients at the new deployment.
func uploadRelayWorker(acc *CFAccount, workerName, sharedSecret, migrateURL string) error {
	workerSrc, err := cfWorkerSrc.ReadFile("cf_worker_src/worker.js")
	if err != nil {
		return fmt.Errorf("embedded worker.js: %w", err)
	}
	doSrc, err := cfWorkerSrc.ReadFile("cf_worker_src/relay_do.js")
	if err != nil {
		return fmt.Errorf("embedded relay_do.js: %w", err)
	}

	type plainTextBinding struct {
		Type string `json:"type"`
		Name string `json:"name"`
		Text string `json:"text"`
	}
	type doBinding struct {
		Type      string `json:"type"`
		Name      string `json:"name"`
		ClassName string `json:"class_name"`
	}
	bindings := []any{
		plainTextBinding{Type: "plain_text", Name: "EMP_SHARED_SECRET", Text: sharedSecret},
		doBinding{Type: "durable_object_namespace", Name: "RELAY_DO", ClassName: "RelayDO"},
	}
	if migrateURL != "" {
		bindings = append(bindings, plainTextBinding{Type: "plain_text", Name: cfMigrateURLVar, Text: migrateURL})
	}

	// The RelayDO SQLite class needs a migration only the first time an
	// account hosts the worker. On later uploads:
	//   - new_sqlite_classes + new_tag  -> 10074 (class already depended on)
	//   - no migrations                 -> 10079 (tag '' vs expected 'v1')
	//   - new_tag only                  -> correct for an existing class
	// Try new-class first, then new_tag-only, then bare as a last resort.
	attempts := []map[string]any{
		{
			"main_module":        "worker.js",
			"compatibility_date": cfCompatDate,
			"bindings":           bindings,
			"migrations": map[string]any{
				"new_tag":            "v1",
				"new_sqlite_classes": []string{"RelayDO"},
			},
			"observability": map[string]any{"enabled": false},
		},
		{
			"main_module":        "worker.js",
			"compatibility_date": cfCompatDate,
			"bindings":           bindings,
			"migrations":         map[string]any{"new_tag": "v1"},
			"observability":      map[string]any{"enabled": false},
		},
		{
			"main_module":        "worker.js",
			"compatibility_date": cfCompatDate,
			"bindings":           bindings,
			"observability":      map[string]any{"enabled": false},
		},
	}

	apiURL := "https://api.cloudflare.com/client/v4/accounts/" + acc.ID + "/workers/scripts/" + workerName
	for i, metadata := range attempts {
		metaJSON, _ := json.Marshal(metadata)
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)

		mh, err := mw.CreatePart(map[string][]string{
			"Content-Disposition": []string{`form-data; name="metadata"`},
			"Content-Type":        []string{"application/json"},
		})
		if err != nil {
			return err
		}
		if _, err := mh.Write(metaJSON); err != nil {
			return err
		}
		wh, err := mw.CreatePart(map[string][]string{
			"Content-Disposition": []string{`form-data; name="worker.js"; filename="worker.js"`},
			"Content-Type":        []string{"application/javascript+module"},
		})
		if err != nil {
			return err
		}
		if _, err := wh.Write(workerSrc); err != nil {
			return err
		}
		dh, err := mw.CreatePart(map[string][]string{
			"Content-Disposition": []string{`form-data; name="relay_do.js"; filename="relay_do.js"`},
			"Content-Type":        []string{"application/javascript+module"},
		})
		if err != nil {
			return err
		}
		if _, err := dh.Write(doSrc); err != nil {
			return err
		}
		if err := mw.Close(); err != nil {
			return err
		}

		data, status, err := cfDo(http.MethodPut, acc.APIToken, apiURL, &buf, mw.FormDataContentType())
		if err != nil {
			return err
		}
		if err := cfCheck(data, status, "upload worker "+workerName); err != nil {
			if i == 0 && strings.Contains(err.Error(), "10074") {
				logging.Infof("cf deploy: RelayDO class exists on %s, retrying with tag-only migration", acc.ID)
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("upload worker %s: all attempts failed", workerName)
}

// ensureCustomDomain wires relay.<domain> to the worker: a proxied A record
// (placeholder origin) plus a zone workers route. Both are idempotent.
func ensureCustomDomain(acc *CFAccount, workerName string) (relayHost string, err error) {
	if acc.Domain == "" || acc.ZoneID == "" {
		return "", nil
	}
	host := "relay." + acc.Domain
	zoneBase := "https://api.cloudflare.com/client/v4/zones/" + acc.ZoneID

	// DNS A record, proxied. Look for an existing record first.
	data, status, err := cfDo(http.MethodGet, acc.APIToken,
		zoneBase+"/dns_records?type=A&name="+url.QueryEscape(host)+ "&per_page=50", nil, "")
	if err != nil {
		return "", err
	}
	var dnsResp struct {
		Success bool `json:"success"`
		Result  []struct {
			ID       string `json:"id"`
			Content  string `json:"content"`
			Proxied  bool   `json:"proxied"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &dnsResp); err != nil || !dnsResp.Success || status >= 400 {
		// fall through to create
		dnsResp.Result = nil
	}
	exists := false
	for _, r := range dnsResp.Result {
		if r.Proxied {
			exists = true
			break
		}
	}
	if !exists {
		body, _ := json.Marshal(map[string]any{
			"type": "A", "name": host, "content": cfDNSProxiedIP, "proxied": true, "ttl": 1,
		})
		data, status, err = cfDo(http.MethodPost, acc.APIToken, zoneBase+"/dns_records",
			bytes.NewReader(body), "application/json")
		if err != nil {
			return "", err
		}
		// a duplicate record (81057) is fine — someone created it in the dash
		var e cfAPIError
		_ = json.Unmarshal(data, &e)
		if err := cfCheck(data, status, "dns record "+host); err != nil {
			if !(len(e.Errors) > 0 && e.Errors[0].Code == 81057) {
				return "", err
			}
		}
	}

	// workers route on the zone
	pattern := host + "/*"
	data, status, err = cfDo(http.MethodGet, acc.APIToken, zoneBase+"/workers/routes", nil, "")
	if err != nil {
		return "", err
	}
	var routesResp struct {
		Result []struct {
			ID      string `json:"id"`
			Pattern string `json:"pattern"`
			Script  string `json:"script"`
		} `json:"result"`
	}
	_ = json.Unmarshal(data, &routesResp)
	routeOK := false
	for _, r := range routesResp.Result {
		if r.Pattern == pattern && r.Script == workerName {
			routeOK = true
			break
		}
	}
	if !routeOK {
		body, _ := json.Marshal(map[string]string{"pattern": pattern, "script": workerName})
		data, status, err = cfDo(http.MethodPost, acc.APIToken, zoneBase+"/workers/routes",
			bytes.NewReader(body), "application/json")
		if err != nil {
			return "", err
		}
		if err := cfCheck(data, status, "workers route "+pattern); err != nil {
			return "", err
		}
	}
	return host, nil
}

// deployRelayWorker deploys the relay on the account and returns the relay
// base URL (wss target) — custom domain when configured, workers.dev
// otherwise.
func deployRelayWorker(acc *CFAccount, workerName, sharedSecret string) (relayBase string, err error) {
	// 1. workers.dev subdomain must exist BEFORE the first script upload
	//    (error 10063 otherwise); the call is idempotent.
	devSub, err := ensureWorkersDevSubdomain(acc, workerName)
	if err != nil {
		return "", fmt.Errorf("workers.dev subdomain: %w", err)
	}
	// 2. upload the script (with secret baked as a plain-text binding)
	if err := uploadRelayWorker(acc, workerName, sharedSecret, ""); err != nil {
		return "", err
	}
	// 3. custom domain route (optional): relay.<domain> -> the worker
	if acc.Domain != "" && acc.ZoneID != "" {
		host, err := ensureCustomDomain(acc, workerName)
		if err != nil {
			return "", fmt.Errorf("custom domain: %w", err)
		}
		if host != "" {
			return "wss://" + host, nil
		}
	}
	return "wss://" + workerName + "." + devSub, nil
}

// pointWorkerAtMigration re-uploads the worker on the given (old) account
// with a MIGRATE_URL binding: every connected socket gets
// close(4000, migrateURL) and new connections are rejected with 409.
func pointWorkerAtMigration(acc *CFAccount, workerName, sharedSecret, migrateURL string) error {
	return uploadRelayWorker(acc, workerName, sharedSecret, migrateURL)
}

// waitDNSReady polls until the relay hostname resolves from the CC's own
// resolver. Fresh proxied records can hit negative caches (NXDOMAIN) for a
// few minutes; the health check that follows would otherwise race them.
func waitDNSReady(host string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if addrs, err := net.LookupHost(host); err == nil && len(addrs) > 0 {
			logging.Infof("cf deploy: %s resolves to %v", host, addrs)
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("%s not resolvable after %s: %v", host, timeout, lastErr)
}

// checkWorkerHealth polls the /health endpoint of the deployed worker.
func checkWorkerHealth(baseURL string, timeout time.Duration) error {
	healthURL := strings.Replace(baseURL, "wss://", "https://", 1) + "/health"
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Get(healthURL)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("health check HTTP %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(3 * time.Second)
	}
	logging.Warningf("worker health check failed for %s: %v", baseURL, lastErr)
	return lastErr
}

// removeRelayRoute drops the zone workers route for relay.<domain>, making
// the old domain fail (522) so agents fail over to the embedded next
// endpoint. Fallback when the MIGRATE_URL re-upload is rejected (e.g. 10079
// on wrangler-deployed classes with a migration tag the API cannot match).
func removeRelayRoute(acc *CFAccount, workerName string) error {
	if acc.Domain == "" || acc.ZoneID == "" {
		return fmt.Errorf("account has no domain/zone_id")
	}
	host := "relay." + acc.Domain
	pattern := host + "/*"
	zoneBase := "https://api.cloudflare.com/client/v4/zones/" + acc.ZoneID

	data, status, err := cfDo(http.MethodGet, acc.APIToken, zoneBase+"/workers/routes", nil, "")
	if err != nil {
		return err
	}
	var routesResp struct {
		Result []struct {
			ID      string `json:"id"`
			Pattern string `json:"pattern"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &routesResp); err != nil {
		return fmt.Errorf("parse routes: %w", err)
	}
	removed := 0
	for _, r := range routesResp.Result {
		if r.Pattern == pattern {
			_, status, err = cfDo(http.MethodDelete, acc.APIToken, zoneBase+"/workers/routes/"+r.ID, nil, "")
			if err != nil {
				return err
			}
			if status < 400 {
				removed++
			}
		}
	}
	if removed == 0 {
		return fmt.Errorf("no route %s found to remove", pattern)
	}
	logging.Warningf("cf migrate: removed workers route %s on %s (agents will fail over)", pattern, acc.ID)
	return nil
}
