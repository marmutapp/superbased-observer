package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/processobs"
)

// Process-observability persistence (docs/process-observability.md §10).
//
// Two tables back the feature (migration 044):
//
//   process_runs   — one row per observed process, keyed by the
//                    PID-reuse-proof process_key. Upserted: inserted at
//                    exec, updated at exit (and refined to action_id by the
//                    §9.2.4 correlation pass in P3).
//   process_events — one row per high-signal event (network/file/privilege),
//                    populated from P5 onward.
//
// PRIVACY INVARIANT — both tables are NODE-LOCAL. They MUST NOT appear in
// internal/store/orgpush.go::SelectUnpushedSince; pinned by the sentinel in
// tests/invariant/privacy_test.go alongside the cachetrack / router tables.
//
// MODULE-BOUNDARY NOTE — this file is the seam where processobs DOMAIN
// types (processobs.ProcessRun) are translated into the store's own SQL
// shape. The processobs package never imports internal/store; it persists
// through the processobs.Sink interface, which *(*Store).PersistRuns
// implements. Domain types do not leak into the SQL, exactly as the
// cachetrack engine's ObserveResult is translated in cachetrack.go.

// ProcessRunRow is the SQL-shaped read model for a process_runs row,
// returned by the query helpers (the CLI tree/list/explain surfaces in P3
// and the doctor count). Nullable columns use pointers/zero values.
type ProcessRunRow struct {
	ID                    int64
	ProcessKey            string
	BootID                string
	PID                   int
	PPID                  int
	StartTimeTicks        int64
	ParentProcessKey      string
	SessionID             string
	ProjectID             int64
	Tool                  string
	ActionID              *int64
	TurnIndex             *int
	AttributionSource     string
	AttributionConfidence string
	ExePath               string
	ExeBasename           string
	CWD                   string
	ArgvPreview           string
	ArgvHash              string
	ArgvArgc              int
	UID                   int
	GID                   int
	Username              string
	EnvPostureJSON        string
	StartedAt             time.Time
	LastSeenAt            time.Time
	ExitedAt              time.Time
	Exited                bool
	ExitCode              int
	ExitSignal            int
	DurationMs            int64
	IsBoundary            bool
	// Security / isolation posture (P4), empty until captured.
	SeccompMode     string
	CapabilitiesEff string
	AppArmorLabel   string
	SELinuxLabel    string
	CgroupHash      string
	ContainerID     string
	PIDNamespace    string
	MountNamespace  string
	NetNamespace    string
	// Resource metrics (migration 045), 0 until captured.
	CPUUserMs       int64
	CPUSystemMs     int64
	MaxRSSBytes     int64
	WorkingSetBytes int64
	ReadBytes       int64
	WriteBytes      int64
	ReadOps         int64
	WriteOps        int64
	ThreadCount     int32
	HandleCount     int32
	MetricSamples   []processobs.MetricSample
}

const upsertProcessRunSQL = `
INSERT INTO process_runs (
    process_key, boot_id, pid, ppid, start_time_ticks, parent_process_key,
    session_id, project_id, tool, action_id, turn_index,
    attribution_source, attribution_confidence,
    exe_path, exe_basename, exe_device, exe_inode, exe_hash,
    cwd, argv_preview, argv_hash, argv_argc,
    uid, gid, euid, egid, username,
    cgroup_hash, container_id, pid_namespace, mount_namespace, net_namespace,
    seccomp_mode, apparmor_label, selinux_label, capabilities_eff,
    env_posture_json,
    started_at, last_seen_at, exited_at, exit_code, exit_signal, duration_ms,
    cpu_user_ms, cpu_system_ms, max_rss_bytes, working_set_bytes,
    read_bytes, write_bytes, read_ops, write_ops, thread_count, handle_count,
    metric_samples_json,
    metadata_json
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
          ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
          ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(process_key) DO UPDATE SET
    session_id             = excluded.session_id,
    project_id             = excluded.project_id,
    tool                   = excluded.tool,
    action_id              = COALESCE(excluded.action_id, process_runs.action_id),
    turn_index             = COALESCE(excluded.turn_index, process_runs.turn_index),
    attribution_source     = excluded.attribution_source,
    attribution_confidence = excluded.attribution_confidence,
    exe_path               = COALESCE(excluded.exe_path, process_runs.exe_path),
    exe_basename           = COALESCE(excluded.exe_basename, process_runs.exe_basename),
    cwd                    = COALESCE(excluded.cwd, process_runs.cwd),
    argv_preview           = COALESCE(excluded.argv_preview, process_runs.argv_preview),
    argv_hash              = COALESCE(excluded.argv_hash, process_runs.argv_hash),
    argv_argc              = excluded.argv_argc,
    env_posture_json       = COALESCE(excluded.env_posture_json, process_runs.env_posture_json),
    last_seen_at           = excluded.last_seen_at,
    exited_at              = COALESCE(excluded.exited_at, process_runs.exited_at),
    exit_code              = COALESCE(excluded.exit_code, process_runs.exit_code),
    exit_signal            = COALESCE(excluded.exit_signal, process_runs.exit_signal),
    duration_ms            = COALESCE(excluded.duration_ms, process_runs.duration_ms),
    cpu_user_ms            = COALESCE(excluded.cpu_user_ms, process_runs.cpu_user_ms),
    cpu_system_ms          = COALESCE(excluded.cpu_system_ms, process_runs.cpu_system_ms),
    max_rss_bytes          = COALESCE(excluded.max_rss_bytes, process_runs.max_rss_bytes),
    working_set_bytes      = COALESCE(excluded.working_set_bytes, process_runs.working_set_bytes),
    read_bytes             = COALESCE(excluded.read_bytes, process_runs.read_bytes),
    write_bytes            = COALESCE(excluded.write_bytes, process_runs.write_bytes),
    read_ops               = COALESCE(excluded.read_ops, process_runs.read_ops),
    write_ops              = COALESCE(excluded.write_ops, process_runs.write_ops),
    thread_count           = COALESCE(excluded.thread_count, process_runs.thread_count),
    handle_count           = COALESCE(excluded.handle_count, process_runs.handle_count),
    metric_samples_json    = COALESCE(excluded.metric_samples_json, process_runs.metric_samples_json)`

// PersistRuns implements [processobs.Sink]. It upserts a batch of process
// runs by process_key in one transaction: the exec snapshot inserts the
// row, the exit snapshot (same key) updates the runtime/resource columns.
// Immutable identity (process_key, pid, start_time, started_at, parent) is
// preserved by not listing it in the DO UPDATE set; the COALESCE-guarded
// columns keep an earlier non-NULL value when a later snapshot omits it.
//
// Returns the count of rows processed. A failure aborts the whole batch
// (txn rollback); the observer records this as a non-fatal sink-error drop.
func (s *Store) PersistRuns(ctx context.Context, runs []processobs.ProcessRun) (int, error) {
	if len(runs) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store.PersistRuns: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, upsertProcessRunSQL)
	if err != nil {
		return 0, fmt.Errorf("store.PersistRuns: prepare: %w", err)
	}
	defer stmt.Close()

	for i := range runs {
		r := &runs[i]
		if _, err := stmt.ExecContext(ctx, processRunArgs(r)...); err != nil {
			return 0, fmt.Errorf("store.PersistRuns: exec[%d]: %w", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store.PersistRuns: commit: %w", err)
	}
	return len(runs), nil
}

// processRunArgs flattens a ProcessRun into the upsert parameter list. The
// scrubbing/capping already happened in processobs; here we only map domain
// values to nullable SQL values. P4 envelope groups (security/isolation/
// resources) are zero in P1 and land as NULL.
func processRunArgs(r *processobs.ProcessRun) []any {
	a := r.Attribution

	var exitedAt, exitCode, exitSignal, durationMs any
	if r.Exited {
		exitedAt = nullableTimestamp(r.ExitedAt)
		exitCode = r.ExitCode
		exitSignal = r.ExitSignal
		durationMs = r.DurationMs
	}

	return []any{
		r.ProcessKey,
		nullableString(r.BootID),
		r.PID,
		r.PPID,
		r.StartTimeTicks,
		nullableString(r.ParentProcessKey),
		nullableString(a.SessionID),
		nullableInt64(a.ProjectID),
		nullableString(a.Tool),
		nullableInt64Ptr(a.ActionID),
		nullableIntPtr(a.TurnIndex),
		string(orNone(a.Source)),
		string(orNoneConf(a.Confidence)),
		nullableString(r.ExePath),
		nullableString(r.ExeBasename),
		nullableString(r.ExeDevice),
		nullableString(r.ExeInode),
		nullableString(r.ExeHash),
		nullableString(r.CWD),
		nullableString(r.ArgvPreview),
		nullableString(r.ArgvHash),
		r.ArgvArgc,
		r.UID,
		r.GID,
		r.EUID,
		r.EGID,
		nullableString(r.Username),
		nullableString(r.CgroupHash),
		nullableString(r.ContainerID),
		nullableString(r.PIDNamespace),
		nullableString(r.MountNamespace),
		nullableString(r.NetNamespace),
		nullableString(r.SeccompMode),
		nullableString(r.AppArmorLabel),
		nullableString(r.SELinuxLabel),
		nullableString(r.CapabilitiesEff),
		nullableString(marshalEnvPosture(r.EnvPosture)),
		timestamp(r.StartedAt),
		timestamp(r.LastSeenAt),
		exitedAt,
		exitCode,
		exitSignal,
		durationMs,
		nullableInt64(r.CPUUserMs),
		nullableInt64(r.CPUSystemMs),
		nullableInt64(r.MaxRSSBytes),
		nullableInt64(r.WorkingSetBytes),
		nullableInt64(r.ReadBytes),
		nullableInt64(r.WriteBytes),
		nullableInt64(r.ReadOps),
		nullableInt64(r.WriteOps),
		nullableInt64(int64(r.ThreadCount)),
		nullableInt64(int64(r.HandleCount)),
		nullableString(marshalMetricSamples(r.MetricSamples)),
		nil, // metadata_json (unused)
	}
}

// selectProcessRunCols is qualified with the `pr` alias so it composes with
// a JOIN (LongRunningChildRuns joins sessions, which also has id/tool/
// project_id/started_at). Every query selecting these MUST alias
// process_runs AS pr.
const selectProcessRunCols = `
    pr.id, pr.process_key, COALESCE(pr.boot_id, ''), pr.pid, COALESCE(pr.ppid, 0),
    COALESCE(pr.start_time_ticks, 0), COALESCE(pr.parent_process_key, ''),
    COALESCE(pr.session_id, ''), COALESCE(pr.project_id, 0), COALESCE(pr.tool, ''),
    pr.action_id, pr.turn_index, pr.attribution_source, pr.attribution_confidence,
    COALESCE(pr.exe_path, ''), COALESCE(pr.exe_basename, ''), COALESCE(pr.cwd, ''),
    COALESCE(pr.argv_preview, ''), COALESCE(pr.argv_hash, ''), COALESCE(pr.argv_argc, 0),
    COALESCE(pr.uid, 0), COALESCE(pr.gid, 0), COALESCE(pr.username, ''),
    COALESCE(pr.env_posture_json, ''),
    pr.started_at, pr.last_seen_at, pr.exited_at,
    pr.exit_code, pr.exit_signal, COALESCE(pr.duration_ms, 0),
    COALESCE(pr.seccomp_mode, ''), COALESCE(pr.capabilities_eff, ''),
    COALESCE(pr.apparmor_label, ''), COALESCE(pr.selinux_label, ''),
    COALESCE(pr.cgroup_hash, ''), COALESCE(pr.container_id, ''),
    COALESCE(pr.pid_namespace, ''), COALESCE(pr.mount_namespace, ''),
    COALESCE(pr.net_namespace, ''),
    COALESCE(pr.cpu_user_ms, 0), COALESCE(pr.cpu_system_ms, 0),
    COALESCE(pr.max_rss_bytes, 0), COALESCE(pr.working_set_bytes, 0),
    COALESCE(pr.read_bytes, 0), COALESCE(pr.write_bytes, 0),
    COALESCE(pr.read_ops, 0), COALESCE(pr.write_ops, 0),
    COALESCE(pr.thread_count, 0), COALESCE(pr.handle_count, 0),
    COALESCE(pr.metric_samples_json, '')`

// ProcessRunsForSession returns every process run attributed to a session,
// ordered by start time then pid — the substrate for the session process
// tree (P3 dashboard/CLI). Empty result is not an error.
func (s *Store) ProcessRunsForSession(ctx context.Context, sessionID string) ([]ProcessRunRow, error) {
	q := `SELECT ` + selectProcessRunCols + `
		FROM process_runs pr WHERE pr.session_id = ?
		ORDER BY pr.started_at ASC, pr.pid ASC`
	rows, err := s.db.QueryContext(ctx, q, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store.ProcessRunsForSession: query: %w", err)
	}
	defer rows.Close()
	return scanProcessRunRows(rows, "ProcessRunsForSession")
}

// CorrelateProcessActions runs the §9.2.4 deferred pass for one session: it
// links each attributed process_run to the run_command action that spawned
// it (command/argv match + time window + subtree propagation), filling
// action_id/turn_index. This is the "OS side-effects join back to the
// message/action that caused them" seam — the watcher may ingest the action
// AFTER the process event, so it runs deferred (lazily before a tree render,
// or on the maintenance tick), not at capture time.
//
// Idempotent: the pure correlator skips already-linked runs and the UPDATE is
// further guarded by `action_id IS NULL`, so a second pass is a no-op.
// Returns the number of rows newly linked.
func (s *Store) CorrelateProcessActions(ctx context.Context, sessionID string) (int, error) {
	if sessionID == "" {
		return 0, nil
	}
	runs, err := s.loadProcessRunRefs(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	if len(runs) == 0 {
		return 0, nil
	}
	actions, err := s.loadRunCommandActions(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	if len(actions) == 0 {
		return 0, nil
	}

	links := processobs.CorrelateActions(runs, actions, 0)
	if len(links) == 0 {
		return 0, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store.CorrelateProcessActions: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx,
		`UPDATE process_runs SET action_id = ?, turn_index = ? WHERE process_key = ? AND action_id IS NULL`)
	if err != nil {
		return 0, fmt.Errorf("store.CorrelateProcessActions: prepare: %w", err)
	}
	defer stmt.Close()

	var n int
	for _, l := range links {
		res, err := stmt.ExecContext(ctx, l.ActionID, nullableIntPtr(l.TurnIndex), l.ProcessKey)
		if err != nil {
			return 0, fmt.Errorf("store.CorrelateProcessActions: exec: %w", err)
		}
		c, _ := res.RowsAffected()
		n += int(c)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store.CorrelateProcessActions: commit: %w", err)
	}
	return n, nil
}

// CorrelateCrossOS runs the §5.5 deferred cross-OS pass for one session: it
// matches the Windows AI-tool root process (exe basename in the tool's set, cwd
// == the session's project root, started within a bounded window) to the
// session and attributes it + its subtree (source cross_os_correlation,
// confidence medium). The bridge stores Windows process rows UNATTRIBUTED (the
// pidbridge holds WSL-side pids, so no direct hit across the OS boundary), so
// this pass is how those rows join a session.
//
// Idempotent: the UPDATE is guarded so a row already attributed to this session
// is untouched (re-run returns 0) and a high-confidence attribution is never
// downgraded. Returns the number of rows newly attributed (or re-homed).
func (s *Store) CorrelateCrossOS(ctx context.Context, sessionID string) (int, error) {
	if sessionID == "" {
		return 0, nil
	}
	sess, ok, err := s.loadCrossOSSession(ctx, sessionID)
	if err != nil || !ok {
		return 0, err
	}
	// Bound the candidate set to an anchor window around the session start (a
	// generous ±CrossOSWindow superset of the correlator's own match window);
	// loadCrossOSProcRefs adds this session's existing rows and pulls the full
	// subtree of every seed. This keeps the load to the AI subtree instead of
	// scanning every unattributed row in the table (the drawer-slowdown cause).
	lo := timestamp(sess.StartedAt.Add(-processobs.CrossOSWindow))
	hi := timestamp(sess.StartedAt.Add(processobs.CrossOSWindow))
	runs, err := s.loadCrossOSProcRefs(ctx, sessionID, lo, hi)
	if err != nil {
		return 0, err
	}
	if len(runs) == 0 {
		return 0, nil
	}

	attrs := processobs.CorrelateCrossOS([]processobs.CrossOSSessionRef{sess}, runs, nil, 0)
	if len(attrs) == 0 {
		return 0, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store.CorrelateCrossOS: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Attribute an unattributed row, or re-home one a previous pass placed in a
	// different session; never touch a row already in this session (idempotent)
	// or one at high confidence (authoritative).
	stmt, err := tx.PrepareContext(ctx,
		`UPDATE process_runs
		    SET session_id = ?, project_id = ?, tool = ?, attribution_source = ?, attribution_confidence = ?
		  WHERE process_key = ? AND attribution_confidence != ? AND (session_id IS NULL OR session_id != ?)`)
	if err != nil {
		return 0, fmt.Errorf("store.CorrelateCrossOS: prepare: %w", err)
	}
	defer stmt.Close()

	var n int
	for _, a := range attrs {
		res, err := stmt.ExecContext(ctx,
			a.SessionID, a.ProjectID, a.Tool, string(a.Source), string(a.Confidence),
			a.ProcessKey, string(processobs.ConfHigh), a.SessionID)
		if err != nil {
			return 0, fmt.Errorf("store.CorrelateCrossOS: exec: %w", err)
		}
		c, _ := res.RowsAffected()
		n += int(c)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store.CorrelateCrossOS: commit: %w", err)
	}
	return n, nil
}

// loadCrossOSSession reads one session's (tool, project_id, project root_path,
// started_at) for the cross-OS match. The root_path is the Windows-shaped
// project root recorded from the hook payload's cwd.
func (s *Store) loadCrossOSSession(ctx context.Context, sessionID string) (processobs.CrossOSSessionRef, bool, error) {
	const q = `
		SELECT s.tool, s.project_id, COALESCE(p.root_path, ''), s.started_at
		FROM sessions s JOIN projects p ON s.project_id = p.id
		WHERE s.id = ?`
	var ref processobs.CrossOSSessionRef
	var started string
	err := s.db.QueryRowContext(ctx, q, sessionID).Scan(&ref.Tool, &ref.ProjectID, &ref.ProjectRoot, &started)
	if errors.Is(err, sql.ErrNoRows) {
		return processobs.CrossOSSessionRef{}, false, nil
	}
	if err != nil {
		return processobs.CrossOSSessionRef{}, false, fmt.Errorf("store.loadCrossOSSession: %w", err)
	}
	ref.SessionID = sessionID
	ref.StartedAt = parseStamp(started)
	return ref, true, nil
}

// loadCrossOSProcRefs reads the candidate pool for the cross-OS pass via three
// recursive walks rooted at the same SEEDS — this session's own rows plus the
// UNATTRIBUTED rows started within [lo, hi] (the anchor window around session
// start):
//   - DOWN parent_process_key to pull every DESCENDANT of a seed (so a
//     long-running session's late children are correlated regardless of their
//     own start time);
//   - UP parent_process_key to pull every ANCESTOR CHAIN of a seed (so a branded
//     IDE/desktop launcher — Cursor.exe / Code.exe / OpenCode.exe — that was
//     started long BEFORE the window is in the pool; the correlator's
//     launcher-ancestor anchoring needs it to identify a generic project-cwd
//     worker's subtree). The up-walk adds only the chain to the root, NOT the
//     launcher's other-project descendants, so the pool stays bounded.
//
// This keeps the load to the AI subtree(s) + this session's tree + the seed
// ancestor chains instead of scanning every unattributed row in the table — the
// old `session_id IS NULL` candidate set grew with system-process churn and
// slowed the Processes drawer. Both walks are indexed: the child-walk by
// idx_process_runs_parent (migration 046), the ancestor-walk by the process_key
// primary key.
func (s *Store) loadCrossOSProcRefs(ctx context.Context, sessionID, lo, hi string) ([]processobs.CrossOSProcRef, error) {
	const q = `
		WITH RECURSIVE seeds(process_key, parent_process_key) AS (
			SELECT process_key, parent_process_key FROM process_runs
			 WHERE session_id = ?
			    OR (session_id IS NULL AND started_at BETWEEN ? AND ?)
		),
		down(process_key) AS (
			SELECT process_key FROM seeds
			UNION
			SELECT pr.process_key FROM process_runs pr
			  JOIN down d ON pr.parent_process_key = d.process_key
		),
		up(process_key, parent_process_key) AS (
			SELECT process_key, parent_process_key FROM seeds
			UNION
			SELECT pr.process_key, pr.parent_process_key FROM process_runs pr
			  JOIN up u ON pr.process_key = u.parent_process_key
		)
		SELECT process_key, COALESCE(parent_process_key, ''), COALESCE(exe_basename, ''),
		       COALESCE(cwd, ''), started_at, attribution_source, attribution_confidence
		  FROM process_runs
		 WHERE process_key IN (SELECT process_key FROM down)
		    OR process_key IN (SELECT process_key FROM up)`
	rows, err := s.db.QueryContext(ctx, q, sessionID, lo, hi)
	if err != nil {
		return nil, fmt.Errorf("store.loadCrossOSProcRefs: query: %w", err)
	}
	defer rows.Close()
	var out []processobs.CrossOSProcRef
	for rows.Next() {
		var r processobs.CrossOSProcRef
		var started, src, conf string
		if err := rows.Scan(&r.ProcessKey, &r.ParentProcessKey, &r.ExeBasename, &r.CWD, &started, &src, &conf); err != nil {
			return nil, fmt.Errorf("store.loadCrossOSProcRefs: scan: %w", err)
		}
		r.StartedAt = parseStamp(started)
		r.Source = processobs.AttributionSource(src)
		r.Confidence = processobs.Confidence(conf)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) loadProcessRunRefs(ctx context.Context, sessionID string) ([]processobs.ProcRunRef, error) {
	const q = `
		SELECT process_key, COALESCE(parent_process_key, ''), started_at,
		       COALESCE(argv_preview, ''), COALESCE(exe_basename, ''),
		       (action_id IS NOT NULL)
		FROM process_runs WHERE session_id = ?`
	rows, err := s.db.QueryContext(ctx, q, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store.loadProcessRunRefs: query: %w", err)
	}
	defer rows.Close()
	var out []processobs.ProcRunRef
	for rows.Next() {
		var r processobs.ProcRunRef
		var started string
		var linked int
		if err := rows.Scan(&r.ProcessKey, &r.ParentProcessKey, &started, &r.ArgvPreview, &r.ExeBasename, &linked); err != nil {
			return nil, fmt.Errorf("store.loadProcessRunRefs: scan: %w", err)
		}
		r.StartedAt = parseStamp(started)
		r.Linked = linked != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) loadRunCommandActions(ctx context.Context, sessionID string) ([]processobs.ActionRef, error) {
	const q = `
		SELECT id, turn_index, COALESCE(target, ''), timestamp, COALESCE(duration_ms, 0), COALESCE(success, 1)
		FROM actions
		WHERE session_id = ? AND action_type = ? AND target IS NOT NULL AND target != ''`
	rows, err := s.db.QueryContext(ctx, q, sessionID, models.ActionRunCommand)
	if err != nil {
		return nil, fmt.Errorf("store.loadRunCommandActions: query: %w", err)
	}
	defer rows.Close()
	var out []processobs.ActionRef
	for rows.Next() {
		var a processobs.ActionRef
		var turn sql.NullInt64
		var ts string
		var durationMs int64
		var success int64
		if err := rows.Scan(&a.ActionID, &turn, &a.Command, &ts, &durationMs, &success); err != nil {
			return nil, fmt.Errorf("store.loadRunCommandActions: scan: %w", err)
		}
		if turn.Valid {
			v := int(turn.Int64)
			a.TurnIndex = &v
		}
		a.Timestamp = parseStamp(ts)
		// codex logs the action at command END with a duration; widen the
		// link back-skew by it (see processobs.ActionRef.Duration).
		if durationMs > 0 {
			a.Duration = time.Duration(durationMs) * time.Millisecond
		}
		a.Success = success != 0
		out = append(out, a)
	}
	return out, rows.Err()
}

// loadOSAnchoredActionIDs returns the set of action ids for this session that
// already have a CAPTURED OS process anchored to them — a process_runs row
// carrying that action_id that is NOT itself a derived row. Derived rows
// (attribution_source = action_correlation, the synthesized command rows) are
// excluded so a derived row never counts as its own OS anchor; otherwise the
// derivation pass would treat a command as captured the moment it synthesized
// its row and never refresh it. This is the dedup key that keeps OS-or-derived
// mutually exclusive: exactly one anchor per command.
func (s *Store) loadOSAnchoredActionIDs(ctx context.Context, sessionID string) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT action_id FROM process_runs
		 WHERE session_id = ? AND action_id IS NOT NULL AND attribution_source != ?`,
		sessionID, string(processobs.AttrActionCorrelation))
	if err != nil {
		return nil, fmt.Errorf("store.loadOSAnchoredActionIDs: query: %w", err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store.loadOSAnchoredActionIDs: scan: %w", err)
		}
		out[id] = true
	}
	return out, rows.Err()
}

// DeriveProcessRunsFromActions materializes the §9.2.4 action-derived command
// rows for one session: every run_command action that the OS-capture backend
// never observed gets a synthesized process_runs row carrying its deterministic
// message link (see processobs.DeriveCommandRuns for the why). It is the second
// feed path into the single process_runs owner (PersistRuns) — capture is the
// first — and runs in the same lazy/debounced pass as the correlators, AFTER
// CorrelateProcessActions has had a chance to anchor any captured OS process to
// its action.
//
// Self-healing and idempotent. Two writes, both keyed on the action↔OS-anchor
// relation:
//  1. Upsert a derived row for each action with no OS anchor (re-running just
//     refreshes it — the derived process_key is stable per action id).
//  2. Delete any derived row whose action has SINCE gained an OS anchor (a
//     later poll caught the real process and CorrelateProcessActions linked
//     it) — the real row, with pid + metrics + subtree, wins.
//
// Returns the number of derived rows upserted.
func (s *Store) DeriveProcessRunsFromActions(ctx context.Context, sessionID string) (int, error) {
	if sessionID == "" {
		return 0, nil
	}
	actions, err := s.loadRunCommandActions(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	if len(actions) == 0 {
		return 0, nil
	}
	osLinked, err := s.loadOSAnchoredActionIDs(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	tool, projectID, err := s.sessionToolProject(ctx, sessionID)
	if err != nil {
		return 0, err
	}

	runs := processobs.DeriveCommandRuns(sessionID, tool, projectID, actions, osLinked)
	if len(runs) > 0 {
		if _, err := s.PersistRuns(ctx, runs); err != nil {
			return 0, fmt.Errorf("store.DeriveProcessRunsFromActions: persist: %w", err)
		}
	}

	// Reap derived rows whose action now has an OS anchor (the real process
	// superseded the synthesized one). Cheap no-op when osLinked is empty.
	if len(osLinked) > 0 {
		if err := s.deleteSupersededDerivedRuns(ctx, sessionID, osLinked); err != nil {
			return 0, err
		}
	}
	return len(runs), nil
}

// deleteSupersededDerivedRuns removes derived command rows for actions that
// have gained an OS anchor. Scoped to the session's derived rows (the prefix +
// attribution_source guard) so it can never touch a captured process.
func (s *Store) deleteSupersededDerivedRuns(ctx context.Context, sessionID string, osLinked map[int64]bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store.deleteSupersededDerivedRuns: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx,
		`DELETE FROM process_runs
		 WHERE session_id = ? AND attribution_source = ? AND action_id = ?`)
	if err != nil {
		return fmt.Errorf("store.deleteSupersededDerivedRuns: prepare: %w", err)
	}
	defer stmt.Close()
	for id := range osLinked {
		if _, err := stmt.ExecContext(ctx, sessionID, string(processobs.AttrActionCorrelation), id); err != nil {
			return fmt.Errorf("store.deleteSupersededDerivedRuns: exec: %w", err)
		}
	}
	return tx.Commit()
}

// sessionToolProject resolves a session id to its tool + project_id for
// stamping derived rows. A missing session yields ("", 0) — the derivation
// still produces valid rows attributed to the session id.
func (s *Store) sessionToolProject(ctx context.Context, sessionID string) (string, int64, error) {
	var tool string
	var projectID sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(tool, ''), project_id FROM sessions WHERE id = ?`, sessionID).
		Scan(&tool, &projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, fmt.Errorf("store.sessionToolProject: %w", err)
	}
	return tool, projectID.Int64, nil
}

// SessionSeedByID resolves a session id to its attribution seed for the §5.5
// P-B6 env-token (EV) path: the Windows capturer recovered an allowlisted
// session-id env var, and this confirms a session with that id exists (direct
// equality — verified 2026-06-17: CLAUDE_CODE_SESSION_ID == sessions.id) and
// returns its tool + project so the Attributor can attribute the whole subtree
// at HIGH confidence. ok=false (clean miss) when no such session exists — the
// run then falls back to the medium CorrelateCrossOS pass. This is the token
// counterpart of the pidbridge SeedLookup, keyed on the session id directly so
// it is namespace-independent across the OS boundary.
func (s *Store) SessionSeedByID(ctx context.Context, id string) (processobs.Seed, bool, error) {
	if id == "" {
		return processobs.Seed{}, false, nil
	}
	var tool string
	var projectID sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(tool, ''), project_id FROM sessions WHERE id = ?`, id).
		Scan(&tool, &projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return processobs.Seed{}, false, nil
	}
	if err != nil {
		return processobs.Seed{}, false, fmt.Errorf("store.SessionSeedByID: %w", err)
	}
	return processobs.Seed{
		SessionID:  id,
		Tool:       tool,
		ProjectID:  projectID.Int64,
		Source:     processobs.AttrEnvToken,
		Confidence: processobs.ConfHigh,
	}, true, nil
}

// CountProcessRuns returns the total number of process_runs rows — fed to
// `observer doctor` ("process rows retained", spec §13.2).
func (s *Store) CountProcessRuns(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM process_runs`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store.CountProcessRuns: %w", err)
	}
	return n, nil
}

// ProcessStats summarizes persisted process_runs for `observer process status`.
type ProcessStats struct {
	Total        int64
	Attributed   int64
	Unattributed int64
	// Derived counts the action-derived command rows (§9.2.4): rows
	// synthesized from the tool's own exec record for commands the OS-capture
	// backend missed. They are real, message-linked rows but carry no pid /
	// resource metrics / subtree, so they are tallied separately from
	// OS-observed captures rather than inflating the capture total.
	Derived int64
	ByTool  map[string]int64
}

// ProcessStats returns the persisted process-run tallies (total, attributed
// split, derived split, per-tool) for the status surface.
func (s *Store) ProcessStats(ctx context.Context) (ProcessStats, error) {
	st := ProcessStats{ByTool: map[string]int64{}}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(session_id IS NOT NULL), 0),
		        COALESCE(SUM(attribution_source = ?), 0)
		 FROM process_runs`, string(processobs.AttrActionCorrelation)).
		Scan(&st.Total, &st.Attributed, &st.Derived); err != nil {
		return st, fmt.Errorf("store.ProcessStats: totals: %w", err)
	}
	st.Unattributed = st.Total - st.Attributed

	rows, err := s.db.QueryContext(ctx,
		`SELECT tool, COUNT(*) FROM process_runs WHERE tool IS NOT NULL GROUP BY tool ORDER BY tool`)
	if err != nil {
		return st, fmt.Errorf("store.ProcessStats: by tool: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var tool string
		var n int64
		if err := rows.Scan(&tool, &n); err != nil {
			return st, fmt.Errorf("store.ProcessStats: scan: %w", err)
		}
		st.ByTool[tool] = n
	}
	return st, rows.Err()
}

// ActionCommandsForSession returns action_id → command for the session's
// run_command actions, for labeling the action links in `observer process
// tree` / the dashboard drawer.
func (s *Store) ActionCommandsForSession(ctx context.Context, sessionID string) (map[int64]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, COALESCE(target, '') FROM actions WHERE session_id = ? AND action_type = ?`,
		sessionID, models.ActionRunCommand)
	if err != nil {
		return nil, fmt.Errorf("store.ActionCommandsForSession: query: %w", err)
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var cmd string
		if err := rows.Scan(&id, &cmd); err != nil {
			return nil, fmt.Errorf("store.ActionCommandsForSession: scan: %w", err)
		}
		out[id] = cmd
	}
	return out, rows.Err()
}

// ActionMessageIDsForSession returns action_id → message_id for the session's
// run_command actions — the §9.2.4 link from a correlated process_run to the
// assistant MESSAGE that issued the command (actions.message_id, e.g. an
// Anthropic "msg_…" id). Empty message_ids are skipped. Kept separate from
// ActionCommandsForSession so the latter's other callers (the CLI tree) are
// unaffected (additive). The deferred correlation only links run_command
// actions, so the action_id on a process_run always resolves here.
func (s *Store) ActionMessageIDsForSession(ctx context.Context, sessionID string) (map[int64]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, message_id FROM actions
		 WHERE session_id = ? AND action_type = ? AND message_id IS NOT NULL AND message_id != ''`,
		sessionID, models.ActionRunCommand)
	if err != nil {
		return nil, fmt.Errorf("store.ActionMessageIDsForSession: query: %w", err)
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var msgID string
		if err := rows.Scan(&id, &msgID); err != nil {
			return nil, fmt.Errorf("store.ActionMessageIDsForSession: scan: %w", err)
		}
		out[id] = msgID
	}
	return out, rows.Err()
}

// ProcessFindingRow is a derived observe-only process finding for the CLI /
// dashboard (docs/process-observability.md §14). It is computed ON READ from
// the process_runs envelope via the pure processobs.DeriveFindings engine —
// NOT stored: a finding is a pure function of the (post-exec immutable) run
// facts, so deriving keeps it always consistent with the rows and needs no
// write path or idempotency. Backend-emitted high-signal events
// (network/file) are a separate substrate (process_events) for a future
// side-effect-capable backend.
type ProcessFindingRow struct {
	RuleID      string    `json:"rule_id"`
	Severity    string    `json:"severity"`
	ProcessKey  string    `json:"process_key"`
	SessionID   string    `json:"session_id"`
	Tool        string    `json:"tool,omitempty"`
	ExeBasename string    `json:"exe_basename,omitempty"`
	TargetKind  string    `json:"target_kind,omitempty"`
	Target      string    `json:"target,omitempty"`
	Detail      string    `json:"detail,omitempty"`
	Timestamp   time.Time `json:"timestamp"`
	ActionID    *int64    `json:"action_id,omitempty"`
	TurnIndex   *int      `json:"turn_index,omitempty"`
}

// findingRunMeta carries the display columns a finding row records that aren't
// part of the pure RunFacts the engine reasons over (keyed by process_key).
type findingRunMeta struct {
	tool        string
	exeBasename string
	startedAt   time.Time
	actionID    *int64
	turnIndex   *int
}

// findingFactsCols is the column list loaded into processobs.RunFacts (+ the
// display meta). euid is read directly here because selectProcessRunCols (the
// tree/list reader) does not carry it.
const findingFactsCols = `
	process_key, COALESCE(session_id, ''), COALESCE(attribution_source, ''),
	COALESCE(exe_path, ''), COALESCE(exe_basename, ''),
	COALESCE(uid, 0), COALESCE(euid, 0), COALESCE(tool, ''),
	started_at, action_id, turn_index`

func scanFindingFacts(rows *sql.Rows, who string) ([]processobs.RunFacts, map[string]findingRunMeta, error) {
	var facts []processobs.RunFacts
	meta := make(map[string]findingRunMeta)
	for rows.Next() {
		var f processobs.RunFacts
		var source, tool, started string
		var actionID, turnIndex sql.NullInt64
		if err := rows.Scan(&f.ProcessKey, &f.SessionID, &source,
			&f.ExePath, &f.ExeBasename, &f.UID, &f.EUID, &tool,
			&started, &actionID, &turnIndex); err != nil {
			return nil, nil, fmt.Errorf("store.%s: scan: %w", who, err)
		}
		f.Attributed = source != "" && source != string(processobs.AttrNone)
		m := findingRunMeta{tool: tool, exeBasename: f.ExeBasename, startedAt: parseStamp(started)}
		if actionID.Valid {
			v := actionID.Int64
			m.actionID = &v
		}
		if turnIndex.Valid {
			v := int(turnIndex.Int64)
			m.turnIndex = &v
		}
		facts = append(facts, f)
		meta[f.ProcessKey] = m
	}
	return facts, meta, rows.Err()
}

func buildFindingRows(facts []processobs.RunFacts, meta map[string]findingRunMeta) []ProcessFindingRow {
	findings := processobs.DeriveFindings(facts)
	out := make([]ProcessFindingRow, 0, len(findings))
	for _, f := range findings {
		m := meta[f.ProcessKey]
		out = append(out, ProcessFindingRow{
			RuleID:      string(f.RuleID),
			Severity:    f.Severity,
			ProcessKey:  f.ProcessKey,
			SessionID:   f.SessionID,
			Tool:        m.tool,
			ExeBasename: m.exeBasename,
			TargetKind:  f.TargetKind,
			Target:      f.Target,
			Detail:      f.Detail,
			Timestamp:   m.startedAt,
			ActionID:    m.actionID,
			TurnIndex:   m.turnIndex,
		})
	}
	return out
}

// ProcessFindingsForSession derives the observe-only findings for one
// session's process runs (§14). Empty result is not an error.
func (s *Store) ProcessFindingsForSession(ctx context.Context, sessionID string) ([]ProcessFindingRow, error) {
	if sessionID == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+findingFactsCols+` FROM process_runs WHERE session_id = ? ORDER BY started_at ASC, pid ASC`,
		sessionID)
	if err != nil {
		return nil, fmt.Errorf("store.ProcessFindingsForSession: query: %w", err)
	}
	defer rows.Close()
	facts, meta, err := scanFindingFacts(rows, "ProcessFindingsForSession")
	if err != nil {
		return nil, err
	}
	return buildFindingRows(facts, meta), nil
}

// RecentProcessFindings derives observe-only findings across all attributed
// runs started within the window — the substrate for the dashboard anomaly
// rollup (§13.1 Security/Observability tab). since ≤ 0 uses 24h.
func (s *Store) RecentProcessFindings(ctx context.Context, since time.Duration) ([]ProcessFindingRow, error) {
	if since <= 0 {
		since = 24 * time.Hour
	}
	cutoff := timestamp(time.Now().UTC().Add(-since))
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+findingFactsCols+` FROM process_runs
		 WHERE session_id IS NOT NULL AND started_at >= ?
		 ORDER BY started_at DESC, pid ASC`,
		cutoff)
	if err != nil {
		return nil, fmt.Errorf("store.RecentProcessFindings: query: %w", err)
	}
	defer rows.Close()
	facts, meta, err := scanFindingFacts(rows, "RecentProcessFindings")
	if err != nil {
		return nil, err
	}
	return buildFindingRows(facts, meta), nil
}

func scanProcessRunRows(rows *sql.Rows, who string) ([]ProcessRunRow, error) {
	var out []ProcessRunRow
	for rows.Next() {
		var r ProcessRunRow
		var actionID sql.NullInt64
		var turnIndex sql.NullInt64
		var startedAt, lastSeenAt string
		var exitedAt sql.NullString
		var exitCode, exitSignal sql.NullInt64
		var metricSamplesJSON string
		if err := rows.Scan(
			&r.ID, &r.ProcessKey, &r.BootID, &r.PID, &r.PPID,
			&r.StartTimeTicks, &r.ParentProcessKey,
			&r.SessionID, &r.ProjectID, &r.Tool,
			&actionID, &turnIndex, &r.AttributionSource, &r.AttributionConfidence,
			&r.ExePath, &r.ExeBasename, &r.CWD,
			&r.ArgvPreview, &r.ArgvHash, &r.ArgvArgc,
			&r.UID, &r.GID, &r.Username,
			&r.EnvPostureJSON,
			&startedAt, &lastSeenAt, &exitedAt,
			&exitCode, &exitSignal, &r.DurationMs,
			&r.SeccompMode, &r.CapabilitiesEff,
			&r.AppArmorLabel, &r.SELinuxLabel,
			&r.CgroupHash, &r.ContainerID,
			&r.PIDNamespace, &r.MountNamespace, &r.NetNamespace,
			&r.CPUUserMs, &r.CPUSystemMs, &r.MaxRSSBytes, &r.WorkingSetBytes,
			&r.ReadBytes, &r.WriteBytes, &r.ReadOps, &r.WriteOps,
			&r.ThreadCount, &r.HandleCount, &metricSamplesJSON,
		); err != nil {
			return nil, fmt.Errorf("store.%s: scan: %w", who, err)
		}
		r.MetricSamples = parseMetricSamples(metricSamplesJSON)
		if actionID.Valid {
			v := actionID.Int64
			r.ActionID = &v
		}
		if turnIndex.Valid {
			v := int(turnIndex.Int64)
			r.TurnIndex = &v
		}
		r.StartedAt = parseStamp(startedAt)
		r.LastSeenAt = parseStamp(lastSeenAt)
		if exitedAt.Valid && exitedAt.String != "" {
			r.ExitedAt = parseStamp(exitedAt.String)
			r.Exited = true
		}
		if exitCode.Valid {
			r.ExitCode = int(exitCode.Int64)
		}
		if exitSignal.Valid {
			r.ExitSignal = int(exitSignal.Int64)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.%s: rows: %w", who, err)
	}
	return out, nil
}

// LongRunningChildRuns returns attributed process runs that are still alive
// (no exit observed) whose session ended more than `threshold` ago — the
// §14 `process.long_running_child` signal (a child that outlived its AI
// session). Ordered oldest-session-end first. threshold <= 0 uses 1h.
func (s *Store) LongRunningChildRuns(ctx context.Context, threshold time.Duration) ([]ProcessRunRow, error) {
	if threshold <= 0 {
		threshold = time.Hour
	}
	cutoff := timestamp(time.Now().UTC().Add(-threshold))
	q := `SELECT ` + selectProcessRunCols + `
		FROM process_runs pr
		JOIN sessions sn ON sn.id = pr.session_id
		WHERE pr.exited_at IS NULL
		  AND sn.ended_at IS NOT NULL
		  AND sn.ended_at < ?
		ORDER BY sn.ended_at ASC, pr.started_at ASC`
	rows, err := s.db.QueryContext(ctx, q, cutoff)
	if err != nil {
		return nil, fmt.Errorf("store.LongRunningChildRuns: query: %w", err)
	}
	defer rows.Close()
	return scanProcessRunRows(rows, "LongRunningChildRuns")
}

// PruneProcessRows removes process_runs and process_events older than
// retentionDays, swept from the daemon maintenance tick alongside the
// cachetrack/guard/routing sweeps (cmd/observer/prune.go::runRetention).
// process_runs is pruned on started_at; process_events on timestamp.
// Returns the total rows removed. retentionDays ≤ 0 is a no-op (returns 0),
// so the caller can pass the config value straight through.
//
// Idempotent: a second run within the same horizon removes nothing.
func (s *Store) PruneProcessRows(ctx context.Context, retentionDays int) (int, error) {
	if retentionDays <= 0 {
		return 0, nil
	}
	cutoff := timestamp(time.Now().UTC().Add(-time.Duration(retentionDays) * 24 * time.Hour))

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store.PruneProcessRows: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var removed int
	// Events first (they reference process_runs by id; remove the children
	// before the parents to keep the intent clear even without enforced FKs).
	r, err := tx.ExecContext(ctx, `DELETE FROM process_events WHERE timestamp < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store.PruneProcessRows: events: %w", err)
	}
	n, _ := r.RowsAffected()
	removed += int(n)

	r, err = tx.ExecContext(ctx, `DELETE FROM process_runs WHERE started_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store.PruneProcessRows: runs: %w", err)
	}
	n, _ = r.RowsAffected()
	removed += int(n)

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store.PruneProcessRows: commit: %w", err)
	}
	return removed, nil
}

// marshalMetricSamples serializes the sparkline ring buffer to compact JSON, or
// "" when empty (mapped to a NULL column). parseMetricSamples is the inverse
// for the read path.
func marshalMetricSamples(s []processobs.MetricSample) string {
	if len(s) == 0 {
		return ""
	}
	b, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	return string(b)
}

func parseMetricSamples(s string) []processobs.MetricSample {
	if s == "" {
		return nil
	}
	var out []processobs.MetricSample
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

// marshalEnvPosture serializes the env-posture map to compact JSON, or ""
// when empty (which the caller maps to a NULL column).
func marshalEnvPosture(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}

// orNone defaults an empty attribution source to "none" (the column is
// NOT NULL and an unattributed run still has a source of record).
func orNone(s processobs.AttributionSource) processobs.AttributionSource {
	if s == "" {
		return processobs.AttrNone
	}
	return s
}

func orNoneConf(c processobs.Confidence) processobs.Confidence {
	if c == "" {
		return processobs.ConfNone
	}
	return c
}

// ActiveSessionRoots returns the distinct project root paths of sessions with
// activity (an action / token row, or a fresh start) inside the last
// windowMinutes. The process observer uses this to capture UNATTRIBUTED
// processes that run in an active session's project directory — the
// generic-interpreter tools (hermes-as-python, pi, roo-code/cline-in-VS-Code,
// Copilot) that present no distinctive launcher and so can't be caught by the
// AI-subtree signal. The deferred CorrelateCrossOS pass then joins those
// project-cwd processes to the session (cwd == project_root). Bounded to recent
// activity so the captured set stays scoped to live work, not every project
// dir ever seen. Empty/blank roots are skipped.
func (s *Store) ActiveSessionRoots(ctx context.Context, windowMinutes int) ([]string, error) {
	if windowMinutes <= 0 {
		windowMinutes = 60
	}
	since := timestamp(time.Now().UTC().Add(-time.Duration(windowMinutes) * time.Minute))
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT p.root_path
		  FROM sessions s JOIN projects p ON p.id = s.project_id
		 WHERE p.root_path IS NOT NULL AND p.root_path <> ''
		   AND (s.started_at >= ?
		     OR EXISTS (SELECT 1 FROM actions a WHERE a.session_id = s.id AND a.timestamp >= ?)
		     OR EXISTS (SELECT 1 FROM token_usage u WHERE u.session_id = s.id AND u.timestamp >= ?))`,
		since, since, since)
	if err != nil {
		return nil, fmt.Errorf("store.ActiveSessionRoots: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var root string
		if err := rows.Scan(&root); err != nil {
			return nil, fmt.Errorf("store.ActiveSessionRoots: scan: %w", err)
		}
		if root != "" {
			out = append(out, root)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ActiveSessionRoots: rows: %w", err)
	}
	return out, nil
}
