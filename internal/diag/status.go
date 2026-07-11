package diag

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// StatusSnapshot is a point-in-time view of the observer's state. Exposed so
// callers can format it for both humans (CLI) and machines (JSON export).
type StatusSnapshot struct {
	// Version is the build-stamped binary version (e.g. "1.8.2"). The
	// dashboard surfaces it in the topbar and compares against the npm
	// registry to flag a stale install. Empty in unit tests and on dev
	// builds where main.version is still "dev".
	Version          string         `json:"version,omitempty"`
	DBPath           string         `json:"db_path"`
	DBSizeBytes      int64          `json:"db_size_bytes"`
	SchemaVersion    int            `json:"schema_version"`
	Counts           SnapshotCounts `json:"counts"`
	LastActionAt     time.Time      `json:"last_action_at,omitempty"`
	LastActionTool   string         `json:"last_action_tool,omitempty"`
	PerToolLastSeen  []ToolActivity `json:"per_tool_last_seen"`
	RecentFailures24 int            `json:"recent_failures_24h"`

	// StartedAt / UptimeSeconds describe the serving process (stamped
	// by the dashboard handler, like Version — diag.Snapshot itself
	// leaves them zero). The dashboard's restart-pending banner
	// compares a config-save timestamp against StartedAt to detect
	// that the operator has restarted the daemon since the save.
	StartedAt     time.Time `json:"started_at,omitempty"`
	UptimeSeconds int64     `json:"uptime_seconds,omitempty"`
}

// SnapshotCounts holds the row counts for each table the user cares about.
type SnapshotCounts struct {
	Projects       int `json:"projects"`
	Sessions       int `json:"sessions"`
	Actions        int `json:"actions"`
	APITurns       int `json:"api_turns"`
	FileState      int `json:"file_state"`
	FailureContext int `json:"failure_context"`
	ActionExcerpts int `json:"action_excerpts"`
	TokenUsageRows int `json:"token_usage"`
	// CacheEvents counts rows in the cachetrack cache_events table
	// (migration 036). Drives the dashboard sidebar "Cache" badge so
	// the operator sees at a glance how much prompt-cache activity has
	// been captured. Stays zero on DBs that pre-date migration 036 or
	// on installs that have disabled [cachetrack].enabled — the table
	// count is read tolerantly.
	CacheEvents int `json:"cache_events"`
	// Suggestions is the advisor's active suggestion count, point-read
	// from the daemon-refreshed advisor_digest snapshot (migration 039)
	// via json_extract — no advisor import, no engine run. Drives the
	// sidebar "Suggestions" badge. Zero when the digest hasn't been
	// written yet (daemon just started / advisor disabled).
	Suggestions int `json:"suggestions"`
	// GuardEvents counts rows in the guard_events verdict table
	// (migration 040). Drives the sidebar "Security" badge. Read
	// tolerantly like cache_events — zero on pre-040 DBs.
	GuardEvents int `json:"guard_events"`
	// RouterDecisions counts rows in the router_decisions table
	// (migration 041). Drives the sidebar "Routing" badge. Read
	// tolerantly — zero on pre-041 DBs and while [routing] is off.
	RouterDecisions int `json:"router_decisions"`
	// LiveSessions counts sessions with any activity (actions,
	// api_turns, or token_usage rows) in the last 15 minutes — the
	// same activity definition and default window as the dashboard's
	// /api/live endpoint, without its 8-session display cap. Drives
	// the sidebar "Live" badge.
	LiveSessions int `json:"live_sessions"`
}

// ToolActivity is one tool's most recent action.
type ToolActivity struct {
	Tool        string    `json:"tool"`
	LastSeenAt  time.Time `json:"last_seen_at"`
	ActionCount int       `json:"action_count"`
}

// Snapshot returns a status snapshot of the observer DB. Errors loading
// individual fields surface as zero values; the caller can render whatever's
// available.
func Snapshot(ctx context.Context, database *sql.DB, dbPath string) (StatusSnapshot, error) {
	if database == nil {
		return StatusSnapshot{}, errors.New("diag.Snapshot: nil DB")
	}
	snap := StatusSnapshot{DBPath: dbPath}
	if fi, err := os.Stat(expandTilde(dbPath)); err == nil {
		snap.DBSizeBytes = fi.Size()
	}

	// Schema version — tolerated on error (schema may be partial); the
	// destination stays zero-valued, so the result is discarded explicitly.
	_ = database.QueryRowContext(
		ctx,
		`SELECT CAST(value AS INTEGER) FROM schema_meta WHERE key='version'`,
	).Scan(&snap.SchemaVersion)

	tableCounts := []struct {
		table string
		dest  *int
	}{
		{"projects", &snap.Counts.Projects},
		{"sessions", &snap.Counts.Sessions},
		{"actions", &snap.Counts.Actions},
		{"api_turns", &snap.Counts.APITurns},
		{"file_state", &snap.Counts.FileState},
		{"failure_context", &snap.Counts.FailureContext},
		{"action_excerpts", &snap.Counts.ActionExcerpts},
		{"token_usage", &snap.Counts.TokenUsageRows},
		{"cache_events", &snap.Counts.CacheEvents},
		{"guard_events", &snap.Counts.GuardEvents},
		{"router_decisions", &snap.Counts.RouterDecisions},
	}
	for _, tc := range tableCounts {
		_ = database.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tc.table).Scan(tc.dest)
	}

	// Live sessions: distinct sessions with activity in the last 15
	// minutes across the three activity tables — the /api/live
	// definition (its default window), as a count instead of a feed.
	liveSince := time.Now().UTC().Add(-15 * time.Minute).Format(time.RFC3339Nano)
	_ = database.QueryRowContext(
		ctx, `
		SELECT COUNT(DISTINCT session_id) FROM (
			SELECT session_id FROM actions WHERE timestamp >= ?
			UNION ALL
			SELECT session_id FROM api_turns WHERE timestamp >= ?
			UNION ALL
			SELECT session_id FROM token_usage WHERE timestamp >= ?
		) WHERE session_id IS NOT NULL AND session_id <> ''`,
		liveSince, liveSince, liveSince,
	).Scan(&snap.Counts.LiveSessions)

	// Advisor active-suggestion count from the digest snapshot (tolerant:
	// table may not exist pre-migration-039, payload may lack the field).
	_ = database.QueryRowContext(
		ctx,
		`SELECT CAST(COALESCE(json_extract(payload, '$.total_count'), 0) AS INTEGER)
		 FROM advisor_digest WHERE id = 1`,
	).Scan(&snap.Counts.Suggestions)

	// Last action overall.
	var lastTS, lastTool sql.NullString
	_ = database.QueryRowContext(
		ctx,
		`SELECT timestamp, tool FROM actions ORDER BY timestamp DESC LIMIT 1`,
	).Scan(&lastTS, &lastTool)
	if lastTS.Valid {
		if t, err := time.Parse(time.RFC3339Nano, lastTS.String); err == nil {
			snap.LastActionAt = t
		}
	}
	if lastTool.Valid {
		snap.LastActionTool = lastTool.String
	}

	// Per-tool last seen.
	rows, err := database.QueryContext(ctx,
		`SELECT tool, MAX(timestamp), COUNT(*) FROM actions GROUP BY tool ORDER BY tool`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var ta ToolActivity
			var ts string
			if err := rows.Scan(&ta.Tool, &ts, &ta.ActionCount); err != nil {
				continue
			}
			if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
				ta.LastSeenAt = t
			}
			snap.PerToolLastSeen = append(snap.PerToolLastSeen, ta)
		}
	}

	// Failures in the last 24 hours.
	cutoff := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339Nano)
	_ = database.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM failure_context WHERE timestamp >= ?`, cutoff,
	).Scan(&snap.RecentFailures24)

	return snap, nil
}

// expandTilde turns "~/foo" into "$HOME/foo" so os.Stat resolves correctly.
func expandTilde(p string) string {
	if !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[2:])
}

// FormatStatus renders a StatusSnapshot as a human-readable multi-line
// string suitable for CLI display. The JSON representation comes for free
// via json.Marshal(snap).
func FormatStatus(s StatusSnapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "DB:               %s (%s, schema v%d)\n",
		s.DBPath, humanBytes(s.DBSizeBytes), s.SchemaVersion)
	fmt.Fprintf(&b, "Projects:         %d\n", s.Counts.Projects)
	fmt.Fprintf(&b, "Sessions:         %d\n", s.Counts.Sessions)
	fmt.Fprintf(&b, "Actions:          %d\n", s.Counts.Actions)
	fmt.Fprintf(&b, "API turns:        %d\n", s.Counts.APITurns)
	fmt.Fprintf(&b, "File state:       %d\n", s.Counts.FileState)
	fmt.Fprintf(&b, "Failures (total): %d\n", s.Counts.FailureContext)
	fmt.Fprintf(&b, "Failures (24h):   %d\n", s.RecentFailures24)
	fmt.Fprintf(&b, "Excerpts (FTS5):  %d\n", s.Counts.ActionExcerpts)
	fmt.Fprintf(&b, "Token rows:       %d\n", s.Counts.TokenUsageRows)
	fmt.Fprintf(&b, "Cache events:     %d\n", s.Counts.CacheEvents)
	fmt.Fprintf(&b, "Guard events:     %d\n", s.Counts.GuardEvents)
	fmt.Fprintf(&b, "Router decisions: %d\n", s.Counts.RouterDecisions)
	fmt.Fprintf(&b, "Live sessions:    %d\n", s.Counts.LiveSessions)
	if !s.LastActionAt.IsZero() {
		fmt.Fprintf(&b, "Last action:      %s by %s\n",
			s.LastActionAt.Format(time.RFC3339), s.LastActionTool)
	}
	if len(s.PerToolLastSeen) > 0 {
		fmt.Fprintln(&b, "Per-tool activity:")
		for _, ta := range s.PerToolLastSeen {
			fmt.Fprintf(&b, "  %-12s %d actions, last %s\n",
				ta.Tool, ta.ActionCount, ta.LastSeenAt.Format(time.RFC3339))
		}
	}
	return b.String()
}

// humanBytes formats a byte count as KB/MB/GB.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n2 := n / unit; n2 >= unit; n2 /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
