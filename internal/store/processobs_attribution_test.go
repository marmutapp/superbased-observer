package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/processobs"
)

// attrState is the attribution unit as it lands in / reads back from
// process_runs: the five columns processobs.Attribution owns, which the
// §9.2.5 MAX-upgrade guard moves together or not at all.
type attrState struct {
	sessionID  string
	projectID  int64
	tool       string
	source     processobs.AttributionSource
	confidence processobs.Confidence
}

// readAttr reads the stored attribution unit for one process_key. A NULL
// session_id/project_id/tool reads back as the zero value, exactly as an
// unattributed row should.
func readAttr(t *testing.T, s *Store, key string) attrState {
	t.Helper()
	var (
		got       attrState
		sessionID sql.NullString
		projectID sql.NullInt64
		tool      sql.NullString
		source    string
		conf      string
	)
	err := s.db.QueryRowContext(context.Background(),
		`SELECT session_id, project_id, tool, attribution_source, attribution_confidence
		   FROM process_runs WHERE process_key = ?`, key).
		Scan(&sessionID, &projectID, &tool, &source, &conf)
	if err != nil {
		t.Fatalf("readAttr(%s): %v", key, err)
	}
	got.sessionID = sessionID.String
	got.projectID = projectID.Int64
	got.tool = tool.String
	got.source = processobs.AttributionSource(source)
	got.confidence = processobs.Confidence(conf)
	return got
}

// runWithAttr builds a minimal persistable run carrying one attribution unit.
func runWithAttr(key string, a attrState, started time.Time) processobs.ProcessRun {
	return processobs.ProcessRun{
		ProcessKey:     key,
		BootID:         "boot",
		PID:            4242,
		StartTimeTicks: 1000,
		ExePath:        "/usr/bin/node",
		ExeBasename:    "node",
		StartedAt:      started,
		LastSeenAt:     started,
		Attribution: processobs.Attribution{
			SessionID:  a.sessionID,
			ProjectID:  a.projectID,
			Tool:       a.tool,
			Source:     a.source,
			Confidence: a.confidence,
		},
	}
}

// mustSession seeds one extra session row (mustProjectAndSession only makes
// one) so a re-home attempt has a second, real FK target to aim at.
func mustSession(t *testing.T, s *Store, id string, projectID int64, tool string) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(),
		`INSERT INTO sessions (id, project_id, tool, started_at) VALUES (?, ?, ?, ?)`,
		id, projectID, tool, timestamp(time.Now().UTC())); err != nil {
		t.Fatalf("seed session %s: %v", id, err)
	}
}

// TestPersistRunsAttributionMaxUpgrade is the table-driven pin for the §9.2.5
// guard: on conflict the attribution unit is adopted ONLY when the incoming
// confidence STRICTLY outranks the stored one (none < low < medium < high, the
// SQL mirror of processobs.confidenceRank/outranks). Every row asserts all five
// columns, so a partial (mixed-state) write fails just as loudly as a clobber.
func TestPersistRunsAttributionMaxUpgrade(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newTestStore(t)
	sessA, projectID := mustProjectAndSession(t, s) // "sess-proc-1", tool claude-code
	const sessB = "sess-proc-2"
	mustSession(t, s, sessB, projectID, "codex")

	unattributed := attrState{source: processobs.AttrNone, confidence: processobs.ConfNone}
	low := attrState{sessionID: sessA, projectID: projectID, tool: "codex", source: processobs.AttrHeuristic, confidence: processobs.ConfLow}
	mediumA := attrState{sessionID: sessA, projectID: projectID, tool: "codex", source: processobs.AttrCrossOSCorrelation, confidence: processobs.ConfMedium}
	mediumB := attrState{sessionID: sessB, projectID: projectID, tool: "codex", source: processobs.AttrCrossOSCorrelation, confidence: processobs.ConfMedium}
	highA := attrState{sessionID: sessA, projectID: projectID, tool: "claude-code", source: processobs.AttrBridge, confidence: processobs.ConfHigh}
	highB := attrState{sessionID: sessB, projectID: projectID, tool: "claude-code", source: processobs.AttrEnvToken, confidence: processobs.ConfHigh}

	tests := []struct {
		name     string
		stored   attrState // written by the first persist (the INSERT)
		incoming attrState // written by the second persist (the CONFLICT)
		want     attrState
	}{
		// --- upgrades: strictly stronger evidence wins ---
		{"none upgrades to low", unattributed, low, low},
		{"none upgrades to medium", unattributed, mediumA, mediumA},
		{"none upgrades to high", unattributed, highA, highA},
		{"low upgrades to medium", low, mediumA, mediumA},
		{"low upgrades to high", low, highA, highA},
		{"medium upgrades to high", mediumA, highA, highA},

		// --- downgrades: NEVER (the shipped data-loss bug) ---
		{"medium is not blanked by none", mediumA, unattributed, mediumA},
		{"high is not blanked by none", highA, unattributed, highA},
		{"high is not downgraded by medium", highA, mediumA, highA},
		{"high is not downgraded by low", highA, low, highA},
		{"medium is not downgraded by low", mediumA, low, mediumA},

		// --- equal rank: keep the stored row (strict >, mirroring
		//     processobs.outranks — no churn, and an equal-rank RE-HOME must go
		//     through the dedicated CorrelateCrossOS UPDATE, not a metrics/exit
		//     upsert riding in behind it) ---
		{"equal medium keeps the stored session", mediumA, mediumB, mediumA},
		{"equal high keeps the stored session", highA, highB, highA},
		{"equal none stays unattributed", unattributed, unattributed, unattributed},
	}

	started := time.Unix(1_700_000_000, 0).UTC()
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// One key per case so the sub-cases are independent.
			key := "pk-upgrade-" + tt.name
			if _, err := s.PersistRuns(ctx, []processobs.ProcessRun{runWithAttr(key, tt.stored, started)}); err != nil {
				t.Fatalf("PersistRuns insert: %v", err)
			}
			if got := readAttr(t, s, key); got != tt.stored {
				t.Fatalf("first INSERT did not store the attribution as given: got %+v want %+v", got, tt.stored)
			}
			in := runWithAttr(key, tt.incoming, started)
			in.LastSeenAt = started.Add(time.Duration(i+1) * time.Second)
			if _, err := s.PersistRuns(ctx, []processobs.ProcessRun{in}); err != nil {
				t.Fatalf("PersistRuns conflict: %v", err)
			}
			if got := readAttr(t, s, key); got != tt.want {
				t.Errorf("after conflict: got %+v want %+v", got, tt.want)
			}
		})
	}
}

// TestPersistRunsFirstInsertAcceptsNone pins requirement 5: the guard is on the
// CONFLICT path only. A process nobody has attributed must still land as a
// genuine unattributed row (NULL session, source/confidence 'none') — that row
// is what the deferred CorrelateCrossOS pass later picks up.
func TestPersistRunsFirstInsertAcceptsNone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newTestStore(t)

	run := runWithAttr("pk-first-none", attrState{}, time.Unix(1_700_000_100, 0).UTC())
	// Deliberately EMPTY source/confidence (not even "none") — orNone /
	// orNoneConf default them at the boundary.
	run.Attribution = processobs.Attribution{}
	if _, err := s.PersistRuns(ctx, []processobs.ProcessRun{run}); err != nil {
		t.Fatalf("PersistRuns: %v", err)
	}
	var sessionID sql.NullString
	var source, conf string
	if err := s.db.QueryRowContext(ctx,
		`SELECT session_id, attribution_source, attribution_confidence
		   FROM process_runs WHERE process_key = ?`, "pk-first-none").
		Scan(&sessionID, &source, &conf); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if sessionID.Valid {
		t.Errorf("session_id: got %q, want NULL on a genuinely unattributed first insert", sessionID.String)
	}
	if source != string(processobs.AttrNone) || conf != string(processobs.ConfNone) {
		t.Errorf("got source=%q confidence=%q, want none/none", source, conf)
	}
}

// TestPersistRunsDoesNotClobberCorrelatedAttribution reproduces the exact
// three-step sequence from docs/process-observability.md §9.2.5 END TO END,
// through the REAL CorrelateCrossOS pass (not a hand-written UPDATE), and
// asserts step 3 no longer erases step 2:
//
//  1. captured unattributed at exec        session=<NULL> source=none  confidence=none
//  2. after CorrelateCrossOS (medium)      session=S      cross_os_correlation/medium
//  3. after the unattributed exit persist  session=S      cross_os_correlation/medium  <-- was ERASED
//
// This is the regression the fix exists for: CorrelateCrossOS is the only
// attribution path for every non-claude-code tool, so the clobber silently
// undid attribution for exactly those tools.
func TestPersistRunsDoesNotClobberCorrelatedAttribution(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newTestStore(t)

	const (
		root      = "/home/dev/crossos-proj"
		sessionID = "sess-crossos"
		key       = "pk-crossos-root"
	)
	projectID, err := s.UpsertProject(ctx, root, "")
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	sessionStart := time.Now().UTC().Add(-time.Minute)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, tool, started_at) VALUES (?, ?, 'codex', ?)`,
		sessionID, projectID, timestamp(sessionStart)); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	// STEP 1 — the process observer captures the codex root UNATTRIBUTED
	// (the cross-OS bridge holds no seed for it), running in the project cwd.
	exec := processobs.ProcessRun{
		ProcessKey:     key,
		BootID:         "boot",
		PID:            9100,
		StartTimeTicks: 500,
		ExePath:        "/usr/bin/codex",
		ExeBasename:    "codex",
		CWD:            root,
		StartedAt:      sessionStart.Add(5 * time.Second),
		LastSeenAt:     sessionStart.Add(5 * time.Second),
		Attribution:    processobs.Attribution{Source: processobs.AttrNone, Confidence: processobs.ConfNone},
		MetricSamples: []processobs.MetricSample{
			{T: sessionStart.Add(5 * time.Second), CPUMs: 10, WorkingSet: 2048},
		},
	}
	if _, err := s.PersistRuns(ctx, []processobs.ProcessRun{exec}); err != nil {
		t.Fatalf("step 1 PersistRuns: %v", err)
	}
	if got := readAttr(t, s, key); got.sessionID != "" || got.confidence != processobs.ConfNone {
		t.Fatalf("step 1: expected an unattributed row, got %+v", got)
	}

	// STEP 2 — the deferred cross-OS join attributes it at medium.
	n, err := s.CorrelateCrossOS(ctx, sessionID)
	if err != nil {
		t.Fatalf("step 2 CorrelateCrossOS: %v", err)
	}
	if n == 0 {
		t.Fatal("step 2: CorrelateCrossOS attributed nothing — the fixture no longer exercises the bug")
	}
	correlated := readAttr(t, s, key)
	want := attrState{
		sessionID:  sessionID,
		projectID:  projectID,
		tool:       "codex",
		source:     processobs.AttrCrossOSCorrelation,
		confidence: processobs.ConfMedium,
	}
	if correlated != want {
		t.Fatalf("step 2: got %+v want %+v", correlated, want)
	}

	// STEP 3 — the process exits. The in-memory run is STILL unattributed
	// (nothing ever tells the live tree what the deferred pass resolved), so
	// the exit snapshot carries none/none. It must not erase step 2.
	exit := exec
	exit.Exited = true
	exit.ExitedAt = sessionStart.Add(90 * time.Second)
	exit.LastSeenAt = exit.ExitedAt
	exit.ExitCode = 0
	exit.DurationMs = 85_000
	exit.CPUUserMs = 1234
	exit.MetricSamples = append(exit.MetricSamples,
		processobs.MetricSample{T: exit.ExitedAt, CPUMs: 1234, WorkingSet: 4096})
	if _, err := s.PersistRuns(ctx, []processobs.ProcessRun{exit}); err != nil {
		t.Fatalf("step 3 PersistRuns: %v", err)
	}
	if got := readAttr(t, s, key); got != want {
		t.Fatalf("step 3 ERASED the correlated attribution: got %+v want %+v", got, want)
	}

	// ...and the exit/resource columns of that same upsert still landed: the
	// guard is scoped to attribution, it does not freeze the row.
	rows, err := s.ProcessRunsForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("ProcessRunsForSession: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected the correlated run to be visible to its session, got %d rows", len(rows))
	}
	r := rows[0]
	if !r.Exited || r.DurationMs != 85_000 || r.CPUUserMs != 1234 {
		t.Errorf("exit/resource columns did not update: exited=%v duration=%d cpu=%d", r.Exited, r.DurationMs, r.CPUUserMs)
	}
	if len(r.MetricSamples) != 2 {
		t.Errorf("metric_samples_json: got %d samples, want 2 (the exit ring must replace the exec ring)", len(r.MetricSamples))
	}
}

// TestPersistRunsMetricSamplesUnaffectedByAttributionGuard pins that the
// COALESCE-upsert semantics of metric_samples_json are unchanged by the
// attribution guard: a later persist WITH a ring replaces the stored one, and a
// later persist WITHOUT one (empty ring → NULL) keeps what is stored — in both
// the confidence-upgrade and the confidence-blocked direction.
func TestPersistRunsMetricSamplesUnaffectedByAttributionGuard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newTestStore(t)
	sessionID, projectID := mustProjectAndSession(t, s)
	started := time.Unix(1_700_000_200, 0).UTC()

	ring := func(n int64) []processobs.MetricSample {
		return []processobs.MetricSample{{T: started, CPUMs: n, WorkingSet: 1024}}
	}
	medium := attrState{sessionID: sessionID, projectID: projectID, tool: "codex", source: processobs.AttrCrossOSCorrelation, confidence: processobs.ConfMedium}
	high := attrState{sessionID: sessionID, projectID: projectID, tool: "codex", source: processobs.AttrBridge, confidence: processobs.ConfHigh}
	none := attrState{source: processobs.AttrNone, confidence: processobs.ConfNone}

	tests := []struct {
		name        string
		stored      attrState
		storedRing  []processobs.MetricSample
		incoming    attrState
		incomingRng []processobs.MetricSample
		wantCPU     int64 // CPUMs of the single sample expected afterwards
	}{
		{"blocked downgrade still refreshes the ring", medium, ring(1), none, ring(2), 2},
		{"blocked downgrade with no ring keeps the stored ring", medium, ring(1), none, nil, 1},
		{"upgrade refreshes the ring", medium, ring(1), high, ring(3), 3},
		{"upgrade with no ring keeps the stored ring", medium, ring(1), high, nil, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := "pk-ring-" + tt.name
			first := runWithAttr(key, tt.stored, started)
			first.MetricSamples = tt.storedRing
			if _, err := s.PersistRuns(ctx, []processobs.ProcessRun{first}); err != nil {
				t.Fatalf("PersistRuns insert: %v", err)
			}
			second := runWithAttr(key, tt.incoming, started)
			second.MetricSamples = tt.incomingRng
			if _, err := s.PersistRuns(ctx, []processobs.ProcessRun{second}); err != nil {
				t.Fatalf("PersistRuns conflict: %v", err)
			}
			var raw string
			if err := s.db.QueryRowContext(ctx,
				`SELECT COALESCE(metric_samples_json, '') FROM process_runs WHERE process_key = ?`, key).
				Scan(&raw); err != nil {
				t.Fatalf("read ring: %v", err)
			}
			got := parseMetricSamples(raw)
			if len(got) != 1 {
				t.Fatalf("ring: got %d samples, want 1 (%q)", len(got), raw)
			}
			if got[0].CPUMs != tt.wantCPU {
				t.Errorf("ring: got CPUMs=%d want %d", got[0].CPUMs, tt.wantCPU)
			}
		})
	}
}
