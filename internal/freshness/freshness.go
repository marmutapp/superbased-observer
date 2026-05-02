package freshness

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// Classifier computes content_hash, mtime, size and a freshness classification
// for a file action by comparing against the file_state table.
//
// The zero value is unusable — call New.
type Classifier struct {
	db               *sql.DB
	maxHashSizeMB    int
	ignorePatterns   []string
	fastPathStatOnly bool
}

// Options configures New.
type Options struct {
	// MaxHashSizeMB is the cap on file size to hash. Files larger than this
	// are classified FreshnessUnknown. Default: 10MB.
	MaxHashSizeMB int
	// IgnorePatterns is a list of glob-ish path fragments. Files whose
	// absolute path contains any of these are skipped (hash=="", freshness=unknown).
	IgnorePatterns []string
	// FastPathStatOnly, when true, skips hashing entirely if the mtime+size
	// match the most recent file_state row — mirroring spec §7.2 step 2.
	FastPathStatOnly bool
}

// New returns a Classifier bound to db. The db must have been opened through
// internal/db.Open so the file_state table exists.
func New(db *sql.DB, opts Options) *Classifier {
	if opts.MaxHashSizeMB <= 0 {
		opts.MaxHashSizeMB = 10
	}
	return &Classifier{
		db:               db,
		maxHashSizeMB:    opts.MaxHashSizeMB,
		ignorePatterns:   opts.IgnorePatterns,
		fastPathStatOnly: opts.FastPathStatOnly,
	}
}

// FileObservation is the per-action file snapshot produced by Classify.
// Any field may be its zero value — callers should plumb the non-zero ones
// into actions and file_state.
type FileObservation struct {
	ContentHash   string
	FileMtime     time.Time
	FileSizeBytes int64
	Freshness     string
	PriorActionID int64
	// ChangeDetected reports whether the current hash differs from any
	// previously observed hash for this file (cross-session).
	ChangeDetected bool
}

// Classify computes the observation for (projectID, absPath) inside session
// sessionID. actionType is the current action (used to decide self-edit
// semantics).
//
// Non-file targets should not call this — it returns FreshnessUnknown for
// empty paths.
//
// Classification rules (spec §7):
//   - No prior file_state row              → fresh
//   - Hash matches and same session + prior read/write → stale
//   - Hash matches and cross-session       → fresh (content unchanged but
//     the prior action was in a different session)
//   - Hash differs AND intervening self-edit in this session → changed_by_self
//   - Hash differs AND no intervening self-edit → changed_externally
//   - File missing / too large / ignored   → unknown
func (c *Classifier) Classify(
	ctx context.Context,
	projectID int64,
	sessionID string,
	actionType string,
	absPath string,
) (FileObservation, error) {
	if absPath == "" {
		return FileObservation{Freshness: models.FreshnessUnknown}, nil
	}
	if c.isIgnored(absPath) {
		return FileObservation{Freshness: models.FreshnessUnknown}, nil
	}

	fi, err := os.Stat(absPath)
	if errors.Is(err, os.ErrNotExist) {
		return FileObservation{Freshness: models.FreshnessUnknown}, nil
	}
	if err != nil {
		return FileObservation{Freshness: models.FreshnessUnknown}, nil
	}
	if fi.IsDir() {
		return FileObservation{Freshness: models.FreshnessUnknown}, nil
	}

	maxBytes := int64(c.maxHashSizeMB) * 1024 * 1024
	if fi.Size() > maxBytes {
		return FileObservation{
			FileMtime:     fi.ModTime(),
			FileSizeBytes: fi.Size(),
			Freshness:     models.FreshnessUnknown,
		}, nil
	}

	obs := FileObservation{
		FileMtime:     fi.ModTime(),
		FileSizeBytes: fi.Size(),
	}

	prior, err := c.lookupFileState(ctx, projectID, absPath)
	if err != nil {
		return obs, err
	}

	// Fast path: stat-only when mtime+size match and we already have a hash.
	if c.fastPathStatOnly && prior != nil &&
		prior.FileSizeBytes == fi.Size() &&
		priorMtimeMatches(prior.FileMtime, fi.ModTime()) &&
		prior.ContentHash != "" {
		obs.ContentHash = prior.ContentHash
	} else {
		hash, err := hashFile(absPath)
		if err != nil {
			obs.Freshness = models.FreshnessUnknown
			return obs, nil
		}
		obs.ContentHash = hash
	}

	switch {
	case prior == nil:
		obs.Freshness = models.FreshnessFresh
	case prior.ContentHash == obs.ContentHash:
		// Identical content. If the prior action was in the same session,
		// this is a redundant re-read. Otherwise it's the first access in
		// this session — mark fresh (the insight that "content unchanged"
		// is captured by ChangeDetected = false).
		if priorSession, err := c.priorSession(ctx, prior.LastActionID); err == nil && priorSession == sessionID {
			obs.Freshness = models.FreshnessStale
		} else {
			obs.Freshness = models.FreshnessFresh
		}
		obs.PriorActionID = prior.LastActionID
	default:
		// Hash differs. Was there an intervening self-edit?
		self, err := c.hadInterveningSelfEdit(ctx, projectID, absPath, prior.LastActionID)
		if err != nil {
			return obs, err
		}
		if self {
			obs.Freshness = models.FreshnessChangedBySelf
		} else {
			obs.Freshness = models.FreshnessChangedExternally
		}
		obs.PriorActionID = prior.LastActionID
		obs.ChangeDetected = true
	}
	return obs, nil
}

// UpsertFileState records a new observation in the file_state table. Safe to
// call after the owning action has been inserted (so LastActionID is
// available). Pass 0 for lastActionID if unknown; the row still updates.
func (c *Classifier) UpsertFileState(
	ctx context.Context,
	projectID int64,
	absPath string,
	obs FileObservation,
	lastActionID int64,
	lastActionType string,
	lastModifiedBy string,
) error {
	if absPath == "" || obs.ContentHash == "" {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var mtime any
	if !obs.FileMtime.IsZero() {
		mtime = obs.FileMtime.UTC().Format(time.RFC3339Nano)
	} else {
		mtime = now
	}
	_, err := c.db.ExecContext(ctx,
		`INSERT INTO file_state (
			project_id, file_path, content_hash, file_mtime, file_size_bytes,
			last_action_id, last_action_type, last_seen_at, last_modified_by
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(project_id, file_path) DO UPDATE SET
			content_hash      = excluded.content_hash,
			file_mtime        = excluded.file_mtime,
			file_size_bytes   = excluded.file_size_bytes,
			last_action_id    = COALESCE(NULLIF(excluded.last_action_id, 0), file_state.last_action_id),
			last_action_type  = excluded.last_action_type,
			last_seen_at      = excluded.last_seen_at,
			last_modified_by  = COALESCE(NULLIF(excluded.last_modified_by, ''), file_state.last_modified_by)
		`,
		projectID, absPath, obs.ContentHash, mtime, obs.FileSizeBytes,
		nullableInt64(lastActionID), lastActionType, now, lastModifiedBy,
	)
	if err != nil {
		return fmt.Errorf("freshness.UpsertFileState: %w", err)
	}
	return nil
}

// priorFileState is the internal form used for classification; not exported.
type priorFileState struct {
	ContentHash   string
	FileMtime     time.Time
	FileSizeBytes int64
	LastActionID  int64
}

func (c *Classifier) lookupFileState(
	ctx context.Context, projectID int64, absPath string,
) (*priorFileState, error) {
	var (
		hash     string
		mtimeRaw sql.NullString
		size     int64
		actID    sql.NullInt64
	)
	err := c.db.QueryRowContext(ctx,
		`SELECT content_hash, file_mtime, file_size_bytes, last_action_id
		 FROM file_state WHERE project_id = ? AND file_path = ?`,
		projectID, absPath,
	).Scan(&hash, &mtimeRaw, &size, &actID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("freshness.lookupFileState: %w", err)
	}
	var mtime time.Time
	if mtimeRaw.Valid {
		mtime, _ = time.Parse(time.RFC3339Nano, mtimeRaw.String)
	}
	return &priorFileState{
		ContentHash:   hash,
		FileMtime:     mtime,
		FileSizeBytes: size,
		LastActionID:  actID.Int64,
	}, nil
}

func (c *Classifier) priorSession(ctx context.Context, actionID int64) (string, error) {
	if actionID == 0 {
		return "", nil
	}
	var sid string
	err := c.db.QueryRowContext(ctx,
		`SELECT session_id FROM actions WHERE id = ?`, actionID,
	).Scan(&sid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("freshness.priorSession: %w", err)
	}
	return sid, nil
}

// hadInterveningSelfEdit returns true if the AI tool performed a write/edit
// on absPath in this project after priorActionID. "Self" here means any
// AI-tool action — not scoped to a particular tool — because a file
// modification by any connected AI tool counts as self.
func (c *Classifier) hadInterveningSelfEdit(
	ctx context.Context, projectID int64, absPath string, priorActionID int64,
) (bool, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT 1 FROM actions
		 WHERE project_id = ? AND target = ?
		   AND action_type IN (?, ?)
		   AND id > ?
		 LIMIT 1`,
		projectID, absPath, models.ActionWriteFile, models.ActionEditFile, priorActionID,
	)
	if err != nil {
		return false, fmt.Errorf("freshness.hadInterveningSelfEdit: %w", err)
	}
	defer rows.Close()
	return rows.Next(), nil
}

func (c *Classifier) isIgnored(absPath string) bool {
	for _, p := range c.ignorePatterns {
		if p == "" {
			continue
		}
		if strings.HasSuffix(p, "/") {
			if strings.Contains(absPath, string(os.PathSeparator)+strings.TrimSuffix(p, "/")+string(os.PathSeparator)) ||
				strings.HasSuffix(absPath, string(os.PathSeparator)+strings.TrimSuffix(p, "/")) {
				return true
			}
			continue
		}
		if strings.HasPrefix(p, "*.") {
			if strings.EqualFold(filepath.Ext(absPath), p[1:]) {
				return true
			}
			continue
		}
		if strings.Contains(absPath, p) {
			return true
		}
	}
	return false
}

// priorMtimeMatches compares mtimes at second granularity. Filesystems report
// ModTime with varying precision; we want stat-only equivalence, not an exact
// nanosecond match.
func priorMtimeMatches(a, b time.Time) bool {
	return a.Unix() == b.Unix()
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func nullableInt64(v int64) any {
	if v == 0 {
		return 0
	}
	return v
}
