package diag

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/mcp"
)

// Status is the outcome of a single check. Three levels — anything other
// than StatusOK is reported on stderr; StatusFail flips the exit code.
type Status int

const (
	StatusOK Status = iota
	StatusWarn
	StatusFail
)

// String renders a status as a one-token symbol for tabular output.
func (s Status) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusWarn:
		return "warn"
	case StatusFail:
		return "fail"
	}
	return "?"
}

// Check is one row of a Report.
type Check struct {
	Name    string   // short id, e.g. "db.integrity"
	Status  Status   // ok | warn | fail
	Message string   // single-line summary
	Details []string // optional bullet points (printed indented)
}

// Report is the result of running all checks.
type Report struct {
	Checks []Check
}

// Failed reports whether any check has Status == StatusFail.
func (r Report) Failed() bool {
	for _, c := range r.Checks {
		if c.Status == StatusFail {
			return true
		}
	}
	return false
}

// Counts returns (ok, warn, fail) tallies across the report.
func (r Report) Counts() (int, int, int) {
	var ok, warn, fail int
	for _, c := range r.Checks {
		switch c.Status {
		case StatusOK:
			ok++
		case StatusWarn:
			warn++
		case StatusFail:
			fail++
		}
	}
	return ok, warn, fail
}

// DoctorOptions parameterizes Doctor.
type DoctorOptions struct {
	// Config is the loaded observer config. Required.
	Config config.Config
	// DB is an opened observer DB (use internal/db.Open). Required.
	DB *sql.DB
	// HomeDir overrides $HOME (for tests).
	HomeDir string
	// BinaryPath is the absolute path of the running observer binary.
	// Required for hook + MCP registration checks.
	BinaryPath string
}

// Run executes every check and returns the aggregated report. It never
// returns an error — every failure is captured as a StatusFail check.
func Run(ctx context.Context, opts DoctorOptions) Report {
	if opts.HomeDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			opts.HomeDir = home
		}
	}
	r := Report{}
	r.add(checkSchema(ctx, opts.DB))
	r.add(checkDBIntegrity(ctx, opts.DB))
	r.add(checkDBSize(opts.Config, opts.HomeDir))
	r.add(checkAdapterPaths(opts.HomeDir))
	r.add(checkHookChecksums(opts.HomeDir))
	r.add(checkHookCommandsBinary(opts.HomeDir, opts.BinaryPath))
	r.add(checkMCPRegistrations(opts.HomeDir, opts.BinaryPath))
	r.add(checkPidBridge(ctx, opts.DB))
	return r
}

// checkPidBridge reports how many {pid → session_id} entries the proxy
// has on hand. Zero is not a failure (the bridge is opt-in via the
// SessionStart hook), but a warn nudge so users remember to re-run
// `observer init` after upgrading.
func checkPidBridge(ctx context.Context, database *sql.DB) Check {
	var count int
	err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_pid_bridge`).Scan(&count)
	if err != nil {
		return Check{Name: "pidbridge.size", Status: StatusFail, Message: "read session_pid_bridge: " + err.Error()}
	}
	if count == 0 {
		return Check{
			Name: "pidbridge.size", Status: StatusWarn,
			Message: "no pid→session entries — host tool hasn't fired SessionStart yet (re-run `observer init` after upgrading)",
		}
	}
	return Check{Name: "pidbridge.size", Status: StatusOK, Message: fmt.Sprintf("%d pid→session entries", count)}
}

func (r *Report) add(c Check) { r.Checks = append(r.Checks, c) }

// checkSchema verifies a non-zero schema version is recorded.
func checkSchema(ctx context.Context, database *sql.DB) Check {
	v, err := db.Version(ctx, database)
	if err != nil {
		return Check{Name: "db.schema", Status: StatusFail, Message: "schema_meta unreadable: " + err.Error()}
	}
	if v == 0 {
		return Check{Name: "db.schema", Status: StatusFail, Message: "no migrations applied (version 0)"}
	}
	return Check{Name: "db.schema", Status: StatusOK, Message: fmt.Sprintf("schema version %d applied", v)}
}

// checkDBIntegrity runs PRAGMA quick_check.
func checkDBIntegrity(ctx context.Context, database *sql.DB) Check {
	var result string
	if err := database.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil {
		return Check{Name: "db.integrity", Status: StatusFail, Message: "quick_check error: " + err.Error()}
	}
	if result != "ok" {
		return Check{Name: "db.integrity", Status: StatusFail, Message: "quick_check failed: " + result}
	}
	return Check{Name: "db.integrity", Status: StatusOK, Message: "PRAGMA quick_check ok"}
}

// checkDBSize warns when the DB exceeds 80% of max_db_size_mb and fails when
// it exceeds 100%. A zero/negative cap disables the check.
func checkDBSize(cfg config.Config, homeDir string) Check {
	cap := cfg.Observer.Retention.MaxDBSizeMB
	if cap <= 0 {
		return Check{Name: "db.size", Status: StatusOK, Message: "max_db_size_mb disabled"}
	}
	path := cfg.Observer.DBPath
	if strings.HasPrefix(path, "~/") && homeDir != "" {
		path = filepath.Join(homeDir, path[2:])
	}
	fi, err := os.Stat(path)
	if err != nil {
		return Check{Name: "db.size", Status: StatusWarn, Message: "stat db: " + err.Error()}
	}
	mb := fi.Size() / (1024 * 1024)
	switch {
	case mb >= int64(cap):
		return Check{Name: "db.size", Status: StatusFail, Message: fmt.Sprintf("%dMB exceeds max_db_size_mb=%d (run pruning)", mb, cap)}
	case mb >= int64(cap)*8/10:
		return Check{Name: "db.size", Status: StatusWarn, Message: fmt.Sprintf("%dMB approaching max_db_size_mb=%d", mb, cap)}
	}
	return Check{Name: "db.size", Status: StatusOK, Message: fmt.Sprintf("%dMB / %dMB", mb, cap)}
}

// checkAdapterPaths reports which adapter watch dirs exist on disk. Always
// StatusOK — missing dirs just mean that tool isn't installed yet.
func checkAdapterPaths(homeDir string) Check {
	candidates := map[string]string{
		"claude-code": filepath.Join(homeDir, ".claude", "projects"),
		"codex":       filepath.Join(homeDir, ".codex", "sessions"),
	}
	var present, missing []string
	for tool, p := range candidates {
		if dirExists(p) {
			present = append(present, tool)
		} else {
			missing = append(missing, tool)
		}
	}
	sort.Strings(present)
	sort.Strings(missing)
	msg := fmt.Sprintf("present: %s", strings.Join(present, ", "))
	if len(present) == 0 {
		msg = "no adapter dirs detected"
	}
	details := []string{}
	if len(missing) > 0 {
		details = append(details, "missing: "+strings.Join(missing, ", "))
	}
	return Check{Name: "adapters.paths", Status: StatusOK, Message: msg, Details: details}
}

// checkHookChecksums reads ~/.observer/hook_checksums.json and verifies
// each recorded config file still hashes to the expected value. Drift is
// reported as StatusWarn so the user can decide whether to re-run init.
func checkHookChecksums(homeDir string) Check {
	csPath := filepath.Join(homeDir, ".observer", "hook_checksums.json")
	raw, err := os.ReadFile(csPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Check{Name: "hooks.checksums", Status: StatusWarn, Message: "no hook_checksums.json yet — run `observer init` first"}
		}
		return Check{Name: "hooks.checksums", Status: StatusFail, Message: "read checksums: " + err.Error()}
	}
	var entries map[string]struct {
		SHA256     string `json:"sha256"`
		Registered string `json:"registered"`
		BinaryPath string `json:"binary_path"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return Check{Name: "hooks.checksums", Status: StatusFail, Message: "parse checksums: " + err.Error()}
	}
	if len(entries) == 0 {
		return Check{Name: "hooks.checksums", Status: StatusWarn, Message: "no hooks registered"}
	}

	var (
		drift   []string
		missing []string
		matched int
	)
	for path, want := range entries {
		body, err := os.ReadFile(path)
		if err != nil {
			missing = append(missing, path+": "+err.Error())
			continue
		}
		sum := sha256.Sum256(body)
		got := hex.EncodeToString(sum[:])
		if got != want.SHA256 {
			drift = append(drift, path)
			continue
		}
		matched++
	}

	switch {
	case len(missing) > 0:
		return Check{
			Name: "hooks.checksums", Status: StatusFail,
			Message: fmt.Sprintf("%d config file(s) missing", len(missing)),
			Details: missing,
		}
	case len(drift) > 0:
		return Check{
			Name: "hooks.checksums", Status: StatusWarn,
			Message: fmt.Sprintf("%d config file(s) modified externally — re-run `observer init` to refresh", len(drift)),
			Details: drift,
		}
	}
	return Check{Name: "hooks.checksums", Status: StatusOK, Message: fmt.Sprintf("%d config(s) match recorded checksums", matched)}
}

// checkHookCommandsBinary looks at every recorded hook config and verifies
// the command it points to is the running observer binary. Mismatch
// usually means the binary was moved after `observer init`.
func checkHookCommandsBinary(homeDir, binaryPath string) Check {
	if binaryPath == "" {
		return Check{Name: "hooks.binary", Status: StatusWarn, Message: "binary path unknown"}
	}
	csPath := filepath.Join(homeDir, ".observer", "hook_checksums.json")
	raw, err := os.ReadFile(csPath)
	if err != nil {
		return Check{Name: "hooks.binary", Status: StatusOK, Message: "no checksums to verify"}
	}
	var entries map[string]struct {
		BinaryPath string `json:"binary_path"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return Check{Name: "hooks.binary", Status: StatusFail, Message: "parse checksums: " + err.Error()}
	}
	var drift []string
	for path, e := range entries {
		if e.BinaryPath != "" && e.BinaryPath != binaryPath {
			drift = append(drift, fmt.Sprintf("%s: registered=%s running=%s", path, e.BinaryPath, binaryPath))
		}
	}
	sort.Strings(drift)
	if len(drift) > 0 {
		return Check{
			Name: "hooks.binary", Status: StatusWarn,
			Message: fmt.Sprintf("%d hook(s) point at a different binary path — re-run `observer init`", len(drift)),
			Details: drift,
		}
	}
	return Check{Name: "hooks.binary", Status: StatusOK, Message: "all hooks point at the running binary"}
}

// checkMCPRegistrations probes the three MCP config locations and reports
// which contain an observer entry pointing at the running binary.
func checkMCPRegistrations(homeDir, binaryPath string) Check {
	if binaryPath == "" {
		return Check{Name: "mcp.registrations", Status: StatusWarn, Message: "binary path unknown"}
	}
	cases := []struct {
		tool string
		path string
		read func(string, string) (registered bool, mismatch bool, err error)
	}{
		{"claude-code", filepath.Join(homeDir, ".claude.json"), readJSONMCPEntry},
		{"cursor", filepath.Join(homeDir, ".cursor", "mcp.json"), readJSONMCPEntry},
		{"codex", filepath.Join(homeDir, ".codex", "config.toml"), readTOMLMCPEntry},
	}
	var registered, mismatched, absent []string
	for _, c := range cases {
		reg, mismatch, err := c.read(c.path, binaryPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			absent = append(absent, c.tool+": "+err.Error())
			continue
		}
		switch {
		case mismatch:
			mismatched = append(mismatched, c.tool)
		case reg:
			registered = append(registered, c.tool)
		default:
			absent = append(absent, c.tool)
		}
	}
	if len(mismatched) > 0 {
		return Check{
			Name: "mcp.registrations", Status: StatusWarn,
			Message: fmt.Sprintf("MCP entries for %s point at a different binary — re-run `observer init`", strings.Join(mismatched, ", ")),
			Details: append([]string{"registered: " + strings.Join(registered, ", ")}, "absent: "+strings.Join(absent, ", ")),
		}
	}
	if len(registered) == 0 {
		return Check{
			Name: "mcp.registrations", Status: StatusWarn,
			Message: "no MCP registrations found — run `observer init` to register",
			Details: []string{"absent: " + strings.Join(absent, ", ")},
		}
	}
	return Check{
		Name: "mcp.registrations", Status: StatusOK,
		Message: fmt.Sprintf("registered: %s", strings.Join(registered, ", ")),
	}
}

func readJSONMCPEntry(path, binary string) (registered, mismatch bool, err error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return false, false, err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return false, false, err
	}
	servers := map[string]struct {
		Command string `json:"command"`
	}{}
	if raw, ok := top["mcpServers"]; ok {
		_ = json.Unmarshal(raw, &servers)
	}
	entry, ok := servers[mcp.ServerName]
	if !ok {
		return false, false, nil
	}
	if entry.Command == binary {
		return true, false, nil
	}
	return true, true, nil
}

func readTOMLMCPEntry(path, binary string) (registered, mismatch bool, err error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return false, false, err
	}
	root := map[string]any{}
	if err := toml.Unmarshal(body, &root); err != nil {
		return false, false, err
	}
	servers, _ := root["mcp_servers"].(map[string]any)
	entry, _ := servers[mcp.ServerName].(map[string]any)
	if entry == nil {
		return false, false, nil
	}
	cmd, _ := entry["command"].(string)
	if cmd == binary {
		return true, false, nil
	}
	return true, true, nil
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
