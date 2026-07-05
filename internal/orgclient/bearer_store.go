package orgclient

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	keyring "github.com/zalando/go-keyring"
)

// keyring record names stored under the configured service (KeychainID).
const (
	recBearer   = "bearer"            // the signed bearer envelope (string)
	recAgentKey = "agent-signing-key" // base64(std) of the Ed25519 private key
	recProbe    = "__sbo_probe__"     // backend-availability probe
)

// ErrNoSecret is returned by Load* when nothing has been stored yet (a
// not-enrolled agent). Callers treat it as "not enrolled", not as a failure.
var ErrNoSecret = errors.New("orgclient: secret not present")

// BearerStore persists the two agent secrets — the server-issued bearer and
// the agent's Ed25519 signing key — outside the agent DB. Implementations are
// either OS-keychain-backed or a 0600-mode file fallback.
type BearerStore interface {
	SaveBearer(bearer string) error
	LoadBearer() (string, error)
	SaveAgentKey(key ed25519.PrivateKey) error
	LoadAgentKey() (ed25519.PrivateKey, error)
	// Clear removes both secrets (used by unenrol). Absence is not an error.
	Clear() error
	// Backend names the active store ("keychain" or "file") for diagnostics.
	Backend() string
}

// OpenBearerStore selects a backend for the given keychain service id. It
// probes the OS keychain once; if a round-trip succeeds it returns a
// keychain-backed store, otherwise it falls back to a 0600-mode file store
// rooted at fallbackDir. A nil logger is tolerated (slog.Default is used).
//
// Warning hygiene (M4.4 of the 2026-06-02 teams test follow-ups): on the
// FIRST observed downgrade we log a WARN naming the fallback; we then
// touch a sentinel at <fallbackDir>/org-bearer/.keychain-unavailable so
// subsequent CLI invocations (which can be many per session) stay silent.
// If the keychain later becomes available (e.g. the user starts gnome-
// keyring-daemon), keychainUsable() returns true on the next probe, we
// remove the sentinel, and the WARN can fire again on a future downgrade.
func OpenBearerStore(service, fallbackDir string, logger *slog.Logger) BearerStore {
	if logger == nil {
		logger = slog.Default()
	}
	storeDir := filepath.Join(fallbackDir, "org-bearer")
	sentinel := filepath.Join(storeDir, ".keychain-unavailable")
	if keychainUsable(service) {
		// Recovered from a previous downgrade — clear the sentinel so a
		// future downgrade warns again.
		_ = os.Remove(sentinel)
		return &keychainStore{service: service}
	}
	if _, err := os.Stat(sentinel); err != nil {
		logger.Warn(
			"org bearer: OS keychain unavailable, falling back to 0600 file store",
			"service", service, "dir", fallbackDir,
			"note", "this warning is suppressed on subsequent invocations; remove ~/.observer/org-bearer/.keychain-unavailable to re-arm",
		)
		_ = os.MkdirAll(storeDir, 0o700)
		_ = os.WriteFile(sentinel, []byte("1"), 0o600)
	}
	return &fileStore{dir: storeDir, service: service}
}

// keychainUsable probes the keyring with a throwaway record. A full
// set/get/delete cycle is the only reliable cross-platform availability test
// (ErrUnsupportedPlatform alone misses headless Linux without a Secret
// Service). The probe record is always deleted.
func keychainUsable(service string) bool {
	if err := keyring.Set(service, recProbe, "1"); err != nil {
		return false
	}
	got, err := keyring.Get(service, recProbe)
	_ = keyring.Delete(service, recProbe)
	return err == nil && got == "1"
}

// --- keychain-backed store -------------------------------------------------

type keychainStore struct{ service string }

func (s *keychainStore) Backend() string { return "keychain" }

func (s *keychainStore) SaveBearer(bearer string) error {
	if err := keyring.Set(s.service, recBearer, bearer); err != nil {
		return fmt.Errorf("orgclient.SaveBearer: %w", err)
	}
	return nil
}

func (s *keychainStore) LoadBearer() (string, error) {
	v, err := keyring.Get(s.service, recBearer)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrNoSecret
	}
	if err != nil {
		return "", fmt.Errorf("orgclient.LoadBearer: %w", err)
	}
	return v, nil
}

func (s *keychainStore) SaveAgentKey(key ed25519.PrivateKey) error {
	if err := keyring.Set(s.service, recAgentKey, base64.StdEncoding.EncodeToString(key)); err != nil {
		return fmt.Errorf("orgclient.SaveAgentKey: %w", err)
	}
	return nil
}

func (s *keychainStore) LoadAgentKey() (ed25519.PrivateKey, error) {
	v, err := keyring.Get(s.service, recAgentKey)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil, ErrNoSecret
	}
	if err != nil {
		return nil, fmt.Errorf("orgclient.LoadAgentKey: %w", err)
	}
	return decodeAgentKey(v)
}

func (s *keychainStore) Clear() error {
	var errs []error
	for _, rec := range []string{recBearer, recAgentKey} {
		if err := keyring.Delete(s.service, rec); err != nil && !errors.Is(err, keyring.ErrNotFound) {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("orgclient.Clear: %w", errors.Join(errs...))
	}
	return nil
}

// --- 0600-file fallback store ----------------------------------------------

type fileStore struct {
	dir     string
	service string
}

func (s *fileStore) Backend() string { return "file" }

func (s *fileStore) path(rec string) string {
	return filepath.Join(s.dir, s.service+"."+rec)
}

func (s *fileStore) write(rec, val string) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("orgclient.fileStore: mkdir: %w", err)
	}
	if err := os.WriteFile(s.path(rec), []byte(val), 0o600); err != nil {
		return fmt.Errorf("orgclient.fileStore: write %s: %w", rec, err)
	}
	return nil
}

func (s *fileStore) read(rec string) (string, error) {
	b, err := os.ReadFile(s.path(rec))
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNoSecret
	}
	if err != nil {
		return "", fmt.Errorf("orgclient.fileStore: read %s: %w", rec, err)
	}
	return string(b), nil
}

func (s *fileStore) SaveBearer(bearer string) error { return s.write(recBearer, bearer) }
func (s *fileStore) LoadBearer() (string, error)    { return s.read(recBearer) }

func (s *fileStore) SaveAgentKey(key ed25519.PrivateKey) error {
	return s.write(recAgentKey, base64.StdEncoding.EncodeToString(key))
}

func (s *fileStore) LoadAgentKey() (ed25519.PrivateKey, error) {
	v, err := s.read(recAgentKey)
	if err != nil {
		return nil, err
	}
	return decodeAgentKey(v)
}

func (s *fileStore) Clear() error {
	var errs []error
	for _, rec := range []string{recBearer, recAgentKey} {
		if err := os.Remove(s.path(rec)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("orgclient.fileStore.Clear: %w", errors.Join(errs...))
	}
	return nil
}

// decodeAgentKey validates a stored Ed25519 private key.
func decodeAgentKey(v string) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return nil, fmt.Errorf("orgclient: decode agent key: %w", err)
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("orgclient: agent key wrong size: got %d want %d", len(raw), ed25519.PrivateKeySize)
	}
	return ed25519.PrivateKey(raw), nil
}
