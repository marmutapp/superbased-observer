package diag

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
)

func routingGapDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := db.Open(context.Background(), db.Options{Path: filepath.Join(t.TempDir(), "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

func writeClaudeRoute(t *testing.T, home string, baseURL string) {
	t.Helper()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"env":{"ANTHROPIC_BASE_URL":%q}}`, baseURL)
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeCodexRoute(t *testing.T, home string, baseURL string) {
	t.Helper()
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("model_provider = \"openai-observer\"\n\n[model_providers.openai-observer]\nbase_url = %q\n", baseURL)
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func insertAction(t *testing.T, database *sql.DB, tool string, ts time.Time) {
	t.Helper()
	stamp := ts.UTC().Format(time.RFC3339Nano)
	_, err := database.Exec(
		`INSERT INTO projects (root_path, name, created_at) VALUES ('/p', 'p', ?)
		 ON CONFLICT DO NOTHING`, stamp,
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.Exec(
		`INSERT INTO sessions (id, project_id, tool, started_at) VALUES ('s1', 1, ?, ?)
		 ON CONFLICT DO NOTHING`, tool, stamp,
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.Exec(
		`INSERT INTO actions (session_id, project_id, tool, timestamp, action_type)
		 VALUES ('s1', 1, ?, ?, 'tool_call')`,
		tool, stamp,
	)
	if err != nil {
		t.Fatal(err)
	}
}

func insertTurn(t *testing.T, database *sql.DB, provider string, ts time.Time) {
	t.Helper()
	_, err := database.Exec(
		`INSERT INTO api_turns (session_id, timestamp, provider, model, input_tokens, output_tokens)
		 VALUES ('s1', ?, ?, 'm', 1, 1)`,
		ts.UTC().Format(time.RFC3339Nano), provider,
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestProxyRoutingGap(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name       string
		routeHome  func(t *testing.T, home string)
		seed       func(t *testing.T, database *sql.DB)
		wantStatus Status
	}{
		{
			name:       "nothing routed is ok",
			routeHome:  func(t *testing.T, home string) {},
			seed:       func(t *testing.T, database *sql.DB) {},
			wantStatus: StatusOK,
		},
		{
			name:      "routed idle is ok",
			routeHome: func(t *testing.T, home string) { writeClaudeRoute(t, home, "http://127.0.0.1:8820") },
			seed:      func(t *testing.T, database *sql.DB) {},

			wantStatus: StatusOK,
		},
		{
			name:      "routed with activity and turns is ok",
			routeHome: func(t *testing.T, home string) { writeClaudeRoute(t, home, "http://127.0.0.1:8820") },
			seed: func(t *testing.T, database *sql.DB) {
				insertAction(t, database, "claude-code", now.Add(-time.Hour))
				insertTurn(t, database, "anthropic", now.Add(-time.Hour))
			},
			wantStatus: StatusOK,
		},
		{
			name:      "routed with activity and zero turns warns (the daemon-down gap)",
			routeHome: func(t *testing.T, home string) { writeClaudeRoute(t, home, "http://127.0.0.1:8820") },
			seed: func(t *testing.T, database *sql.DB) {
				insertAction(t, database, "claude-code", now.Add(-time.Hour))
			},
			wantStatus: StatusWarn,
		},
		{
			name:      "old activity outside the window is ok",
			routeHome: func(t *testing.T, home string) { writeClaudeRoute(t, home, "http://127.0.0.1:8820") },
			seed: func(t *testing.T, database *sql.DB) {
				insertAction(t, database, "claude-code", now.Add(-48*time.Hour))
			},
			wantStatus: StatusOK,
		},
		{
			name:      "codex gap warns via provider pairing",
			routeHome: func(t *testing.T, home string) { writeCodexRoute(t, home, "http://127.0.0.1:8820/v1") },
			seed: func(t *testing.T, database *sql.DB) {
				insertAction(t, database, "codex", now.Add(-time.Hour))
			},
			wantStatus: StatusWarn,
		},
		{
			name:      "third-party route is not ours to judge",
			routeHome: func(t *testing.T, home string) { writeClaudeRoute(t, home, "https://corp-gateway.example.com") },
			seed: func(t *testing.T, database *sql.DB) {
				insertAction(t, database, "claude-code", now.Add(-time.Hour))
			},
			wantStatus: StatusOK,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			tc.routeHome(t, home)
			database := routingGapDB(t)
			tc.seed(t, database)
			got := checkProxyRoutingGap(context.Background(), database, home)
			if got.Status != tc.wantStatus {
				t.Errorf("status: got %s want %s (msg=%q details=%v)",
					got.Status.String(), tc.wantStatus.String(), got.Message, got.Details)
			}
		})
	}
}
