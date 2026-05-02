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
	DBPath           string         `json:"db_path"`
	DBSizeBytes      int64          `json:"db_size_bytes"`
	SchemaVersion    int            `json:"schema_version"`
	Counts           SnapshotCounts `json:"counts"`
	LastActionAt     time.Time      `json:"last_action_at,omitempty"`
	LastActionTool   string         `json:"last_action_tool,omitempty"`
	PerToolLastSeen  []ToolActivity `json:"per_tool_last_seen"`
	RecentFailures24 int            `json:"recent_failures_24h"`
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

	// Schema version
	if err := database.QueryRowContext(ctx,
		`SELECT CAST(value AS INTEGER) FROM schema_meta WHERE key='version'`,
	).Scan(&snap.SchemaVersion); err != nil && !errors.Is(err, sql.ErrNoRows) {
		// Tolerated — schema may be partial.
	}

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
	}
	for _, tc := range tableCounts {
		_ = database.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tc.table).Scan(tc.dest)
	}

	// Last action overall.
	var lastTS, lastTool sql.NullString
	_ = database.QueryRowContext(ctx,
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
	_ = database.QueryRowContext(ctx,
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
