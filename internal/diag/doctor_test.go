package diag

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/mcp"
)

func newTestEnv(t *testing.T) (config.Config, *sql.DB, string, string) {
	t.Helper()
	home := t.TempDir()
	dbPath := filepath.Join(home, ".observer", "observer.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(context.Background(), db.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	cfg := config.Default()
	cfg.Observer.DBPath = dbPath
	cfg.Observer.Retention.MaxDBSizeMB = 500
	return cfg, d, home, "/usr/local/bin/observer"
}

func TestRun_AllOK_AfterInit(t *testing.T) {
	cfg, database, home, binary := newTestEnv(t)

	// Simulate `observer init` having registered MCP for all 3 tools so the
	// MCP check is OK rather than warn.
	registerJSON(t, filepath.Join(home, ".claude.json"), binary)
	registerJSON(t, filepath.Join(home, ".cursor", "mcp.json"), binary)
	registerTOML(t, filepath.Join(home, ".codex", "config.toml"), binary)

	// Simulate hook_checksums.json with a real config file matching it.
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	settingsBody := []byte(`{"hooks":{}}`)
	if err := os.WriteFile(settings, settingsBody, 0o600); err != nil {
		t.Fatal(err)
	}
	writeChecksums(t, home, map[string]checksumEntry{
		settings: hashOf(settingsBody, binary),
	})

	report := Run(context.Background(), DoctorOptions{
		Config:     cfg,
		DB:         database,
		HomeDir:    home,
		BinaryPath: binary,
	})

	for _, c := range report.Checks {
		if c.Status == StatusFail {
			t.Errorf("unexpected fail: %+v", c)
		}
	}
	if report.Failed() {
		t.Error("Failed() reported true")
	}
	// At least these three should be StatusOK.
	requireCheck(t, report, "db.schema", StatusOK)
	requireCheck(t, report, "db.integrity", StatusOK)
	requireCheck(t, report, "hooks.checksums", StatusOK)
	requireCheck(t, report, "mcp.registrations", StatusOK)
}

func TestRun_HookChecksumDriftWarns(t *testing.T) {
	cfg, database, home, binary := newTestEnv(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte(`{"original":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Record the original checksum then mutate the file to simulate drift.
	writeChecksums(t, home, map[string]checksumEntry{
		settings: hashOf([]byte(`{"original":true}`), binary),
	})
	if err := os.WriteFile(settings, []byte(`{"modified":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	report := Run(context.Background(), DoctorOptions{
		Config: cfg, DB: database, HomeDir: home, BinaryPath: binary,
	})
	requireCheck(t, report, "hooks.checksums", StatusWarn)
}

func TestRun_HookFileMissingFails(t *testing.T) {
	cfg, database, home, binary := newTestEnv(t)
	writeChecksums(t, home, map[string]checksumEntry{
		filepath.Join(home, ".claude", "settings.json"): hashOf([]byte(`x`), binary),
	})
	report := Run(context.Background(), DoctorOptions{
		Config: cfg, DB: database, HomeDir: home, BinaryPath: binary,
	})
	requireCheck(t, report, "hooks.checksums", StatusFail)
	if !report.Failed() {
		t.Error("expected report.Failed()")
	}
}

func TestRun_BinaryMismatchWarns(t *testing.T) {
	cfg, database, home, binary := newTestEnv(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{}`)
	if err := os.WriteFile(settings, body, 0o600); err != nil {
		t.Fatal(err)
	}
	writeChecksums(t, home, map[string]checksumEntry{
		settings: {SHA256: hashOf(body, "/different/path").SHA256, BinaryPath: "/different/path"},
	})
	report := Run(context.Background(), DoctorOptions{
		Config: cfg, DB: database, HomeDir: home, BinaryPath: binary,
	})
	requireCheck(t, report, "hooks.binary", StatusWarn)
}

func TestRun_NoMCPYetWarns(t *testing.T) {
	cfg, database, home, binary := newTestEnv(t)
	report := Run(context.Background(), DoctorOptions{
		Config: cfg, DB: database, HomeDir: home, BinaryPath: binary,
	})
	requireCheck(t, report, "mcp.registrations", StatusWarn)
}

func TestRun_MCPBinaryMismatchWarns(t *testing.T) {
	cfg, database, home, binary := newTestEnv(t)
	registerJSON(t, filepath.Join(home, ".claude.json"), "/another/binary")
	report := Run(context.Background(), DoctorOptions{
		Config: cfg, DB: database, HomeDir: home, BinaryPath: binary,
	})
	requireCheck(t, report, "mcp.registrations", StatusWarn)
}

func TestRun_DBSizeAtCapFails(t *testing.T) {
	cfg, database, home, binary := newTestEnv(t)
	cfg.Observer.Retention.MaxDBSizeMB = 1
	// Inflate the DB to >1MB by inserting padded rows. The schema only has
	// projects + sessions populated by tests; inserting many sessions is
	// the simplest path to size growth.
	pid, err := upsertOneProject(database, "/r")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8000; i++ {
		_, _ = database.ExecContext(context.Background(),
			`INSERT INTO sessions (id, project_id, tool, started_at, metadata) VALUES (?, ?, ?, ?, ?)`,
			"sess-"+strings.Repeat("x", 50)+"-"+strconv.Itoa(i), pid, "claude-code", "2026-04-16T10:00:00Z",
			strings.Repeat("padding-padding-padding-padding-", 8),
		)
	}
	// Force a checkpoint so the actual file grows.
	_, _ = database.ExecContext(context.Background(), `PRAGMA wal_checkpoint(TRUNCATE)`)

	report := Run(context.Background(), DoctorOptions{
		Config: cfg, DB: database, HomeDir: home, BinaryPath: binary,
	})
	for _, c := range report.Checks {
		if c.Name == "db.size" && c.Status != StatusFail {
			t.Errorf("expected db.size fail at 1MB cap, got %+v", c)
		}
	}
}

// --- helpers ---

func upsertOneProject(d *sql.DB, root string) (int64, error) {
	res, err := d.ExecContext(context.Background(),
		`INSERT INTO projects (root_path, created_at) VALUES (?, ?)`,
		root, "2026-04-16T10:00:00Z")
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

type checksumEntry struct {
	SHA256     string `json:"sha256"`
	BinaryPath string `json:"binary_path"`
}

func writeChecksums(t *testing.T, home string, entries map[string]checksumEntry) {
	t.Helper()
	dir := filepath.Join(home, ".observer")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(entries)
	if err := os.WriteFile(filepath.Join(dir, "hook_checksums.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func hashOf(body []byte, binary string) checksumEntry {
	return checksumEntry{
		SHA256:     sha256Hex(body),
		BinaryPath: binary,
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func registerJSON(t *testing.T, path, binary string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"mcpServers": map[string]any{
			mcp.ServerName: map[string]any{"command": binary, "args": []string{"serve"}},
		},
	}
	raw, _ := json.Marshal(body)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func registerTOML(t *testing.T, path, binary string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("[mcp_servers." + mcp.ServerName + "]\ncommand = \"" + binary + "\"\nargs = [\"serve\"]\n")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func requireCheck(t *testing.T, r Report, name string, want Status) {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			if c.Status != want {
				t.Errorf("check %s: status %s want %s — message %q", name, c.Status, want, c.Message)
			}
			return
		}
	}
	t.Errorf("check %s missing from report", name)
}
