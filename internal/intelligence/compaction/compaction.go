package compaction

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Snapshot is the JSON shape stored in compaction_events.file_state_snapshot.
// Kept compact because the column is indexed by nothing — only read by the
// recovery-context MCP tool.
type Snapshot struct {
	CapturedAt string                      `json:"captured_at"`
	Files      map[string]SnapshotFileInfo `json:"files"`
	FileCount  int                         `json:"file_count"`
}

// SnapshotFileInfo is the per-file entry in Snapshot.Files.
type SnapshotFileInfo struct {
	ContentHash string `json:"content_hash,omitempty"`
	Mtime       string `json:"mtime,omitempty"`
	SizeBytes   int64  `json:"size,omitempty"`
}

// Recorder writes compaction_events rows.
type Recorder struct{ db *sql.DB }

// New wraps db.
func New(db *sql.DB) *Recorder { return &Recorder{db: db} }

// CaptureOptions parameterize Capture.
type CaptureOptions struct {
	SessionID   string
	ProjectRoot string
	Tool        string
	Timestamp   time.Time
	// Trigger is a free-form tag ("manual", "auto") from the hook payload.
	Trigger string
	// PreActionCount is the action count right before compaction, if
	// known. Zero if not provided.
	PreActionCount int
}

// Capture creates a compaction_events row with a file_state snapshot for
// the project that owns ProjectRoot. Returns the new row's id.
func (r *Recorder) Capture(ctx context.Context, opts CaptureOptions) (int64, error) {
	if opts.SessionID == "" {
		return 0, fmt.Errorf("compaction.Capture: SessionID required")
	}
	if opts.Timestamp.IsZero() {
		opts.Timestamp = time.Now().UTC()
	}
	pid, err := r.projectID(ctx, opts.ProjectRoot)
	if err != nil {
		return 0, err
	}
	if pid == 0 {
		// Capture a row anyway so the MCP tool has something to return —
		// the snapshot will be empty. Find the project via the session's
		// existing row.
		if err := r.db.QueryRowContext(ctx,
			`SELECT project_id FROM sessions WHERE id = ?`, opts.SessionID).Scan(&pid); err != nil {
			return 0, fmt.Errorf("compaction.Capture: lookup session project: %w", err)
		}
	}

	snap, err := r.buildSnapshot(ctx, pid, opts.Timestamp)
	if err != nil {
		return 0, err
	}
	snapJSON, err := json.Marshal(snap)
	if err != nil {
		return 0, fmt.Errorf("compaction.Capture: marshal: %w", err)
	}

	res, err := r.db.ExecContext(ctx,
		`INSERT INTO compaction_events
		   (session_id, project_id, timestamp, tool, pre_action_count,
		    file_state_snapshot, ghost_files_after)
		 VALUES (?, ?, ?, ?, ?, ?, NULL)`,
		opts.SessionID, pid,
		opts.Timestamp.UTC().Format(time.RFC3339Nano),
		opts.Tool, opts.PreActionCount, string(snapJSON),
	)
	if err != nil {
		return 0, fmt.Errorf("compaction.Capture: insert: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func (r *Recorder) projectID(ctx context.Context, root string) (int64, error) {
	if root == "" {
		return 0, nil
	}
	var pid int64
	err := r.db.QueryRowContext(ctx,
		`SELECT id FROM projects WHERE root_path = ?`, root).Scan(&pid)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("compaction: project lookup: %w", err)
	}
	return pid, nil
}

func (r *Recorder) buildSnapshot(ctx context.Context, pid int64, capturedAt time.Time) (Snapshot, error) {
	out := Snapshot{
		CapturedAt: capturedAt.UTC().Format(time.RFC3339Nano),
		Files:      map[string]SnapshotFileInfo{},
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT file_path, COALESCE(content_hash, ''),
		        COALESCE(file_mtime, ''), COALESCE(file_size_bytes, 0)
		 FROM file_state WHERE project_id = ?`, pid)
	if err != nil {
		return out, fmt.Errorf("compaction: snapshot: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var path, hash, mtime string
		var size int64
		if err := rows.Scan(&path, &hash, &mtime, &size); err != nil {
			return out, fmt.Errorf("compaction: snapshot scan: %w", err)
		}
		out.Files[path] = SnapshotFileInfo{ContentHash: hash, Mtime: mtime, SizeBytes: size}
	}
	out.FileCount = len(out.Files)
	return out, rows.Err()
}

// ReconcileOptions parameterize Reconcile.
type ReconcileOptions struct {
	SessionID string
	// Window is how long after compaction to look for "remembered" file
	// actions. Zero means 15 minutes.
	Window time.Duration
}

// Reconcile fills in the ghost_files_after column on the most recent
// compaction_events row for the session. Ghost files are files that were
// in the pre-compact snapshot but haven't been re-touched within Window
// after the compaction timestamp — the AI tool "forgot" about them.
func (r *Recorder) Reconcile(ctx context.Context, opts ReconcileOptions) error {
	if opts.SessionID == "" {
		return fmt.Errorf("compaction.Reconcile: SessionID required")
	}
	if opts.Window <= 0 {
		opts.Window = 15 * time.Minute
	}

	var eventID int64
	var snapshotJSON string
	var ts string
	err := r.db.QueryRowContext(ctx,
		`SELECT id, COALESCE(file_state_snapshot, ''), timestamp
		 FROM compaction_events WHERE session_id = ?
		 ORDER BY timestamp DESC LIMIT 1`, opts.SessionID).Scan(&eventID, &snapshotJSON, &ts)
	if err == sql.ErrNoRows {
		return nil // nothing to reconcile
	}
	if err != nil {
		return fmt.Errorf("compaction.Reconcile: find event: %w", err)
	}
	if snapshotJSON == "" {
		return nil
	}
	compactionAt, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return fmt.Errorf("compaction.Reconcile: parse ts: %w", err)
	}
	var snap Snapshot
	if err := json.Unmarshal([]byte(snapshotJSON), &snap); err != nil {
		return fmt.Errorf("compaction.Reconcile: unmarshal: %w", err)
	}

	// Files touched in the window after compaction.
	rows, err := r.db.QueryContext(ctx,
		`SELECT DISTINCT target FROM actions
		 WHERE session_id = ? AND timestamp > ? AND timestamp < ?
		       AND action_type IN ('read_file', 'edit_file', 'write_file')
		       AND target != ''`,
		opts.SessionID,
		compactionAt.UTC().Format(time.RFC3339Nano),
		compactionAt.Add(opts.Window).UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("compaction.Reconcile: touched files: %w", err)
	}
	touched := map[string]bool{}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return fmt.Errorf("compaction.Reconcile: scan: %w", err)
		}
		touched[t] = true
	}
	rows.Close()

	// Ghosts = snapshot files minus touched files.
	var ghosts []string
	for path := range snap.Files {
		if !touched[path] {
			ghosts = append(ghosts, path)
		}
	}
	ghostJSON, err := json.Marshal(ghosts)
	if err != nil {
		return fmt.Errorf("compaction.Reconcile: marshal: %w", err)
	}
	if _, err := r.db.ExecContext(ctx,
		`UPDATE compaction_events SET ghost_files_after = ? WHERE id = ?`,
		string(ghostJSON), eventID); err != nil {
		return fmt.Errorf("compaction.Reconcile: update: %w", err)
	}
	return nil
}
