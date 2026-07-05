package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// admission.go is the ONE owner (CLAUDE.md rule #4) of the node-local
// obs_admission_* tables (migration 0005). obs_admission_events is
// hash-chained (SHA-256 over the prior row_hash || 0x1e || canonical bytes)
// for a tamper-evident audit story, mirroring the guard_events precedent; a
// VerifyAdmissionChain walk recomputes and reports divergence. The raw request
// text is never stored — only MessageHash — and the one content-bearing field
// (ReasonExcerpt) arrives already gated by the caller's ContentGate.

// AdmissionEventRow is one admission verdict to persist. ReasonExcerpt must be
// gated by the caller (empty when the node's ContentGate denies raw content).
type AdmissionEventRow struct {
	TS            time.Time
	Mode          string
	Decision      string
	Severity      string
	CriterionID   string
	PolicyHash    string
	JudgeUsed     bool
	JudgeHosting  string
	Degraded      string
	LatencyMS     int
	TraceID       string
	SessionID     string
	Tenant        string
	User          string
	RequestID     string
	MessageHash   string
	ReasonExcerpt string
}

// canonicalBytes returns the audit-canonical serialization of the row for the
// hash chain. id, prev_hash and row_hash are excluded (id is SQLite-assigned;
// prev_hash is the other half of the preimage; row_hash is the output). Field
// order is fixed and every field is length-tagged via the 0x1e separator so
// the bytes describe exactly what the row stores.
func (r AdmissionEventRow) canonicalBytes() []byte {
	var b strings.Builder
	w := func(parts ...string) {
		for _, p := range parts {
			b.WriteString(p)
			b.WriteByte(0x1e)
		}
	}
	w(r.TS.UTC().Format(time.RFC3339Nano))
	w(r.Mode, r.Decision, r.Severity, r.CriterionID, r.PolicyHash)
	w(boolStr(r.JudgeUsed), r.JudgeHosting, r.Degraded, strconv.Itoa(r.LatencyMS))
	w(r.TraceID, r.SessionID, r.Tenant, r.User, r.RequestID, r.MessageHash, r.ReasonExcerpt)
	return []byte(b.String())
}

// admissionChainHash computes SHA-256(prev || 0x1e || canonical row bytes),
// matching the guard chain construction.
func admissionChainHash(prev string, canonical []byte) string {
	h := sha256.New()
	h.Write([]byte(prev))
	h.Write([]byte{0x1e})
	h.Write(canonical)
	return hex.EncodeToString(h.Sum(nil))
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// InsertAdmissionEvent appends one verdict, computing its chain links against
// the current table tail in the same transaction. Returns the new row_hash.
func (s *Store) InsertAdmissionEvent(ctx context.Context, r AdmissionEventRow) (string, error) {
	if r.TS.IsZero() {
		r.TS = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("obs/store.InsertAdmissionEvent: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var prev string
	err = tx.QueryRowContext(ctx, `SELECT row_hash FROM obs_admission_events ORDER BY id DESC LIMIT 1`).Scan(&prev)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("obs/store.InsertAdmissionEvent: tail: %w", err)
	}
	rowHash := admissionChainHash(prev, r.canonicalBytes())

	_, err = tx.ExecContext(
		ctx, `
		INSERT INTO obs_admission_events (
			ts, prev_hash, row_hash, mode, decision, severity, criterion_id,
			policy_hash, judge_used, judge_hosting, degraded, latency_ms,
			trace_id, session_id, tenant, user, request_id, message_hash, reason_excerpt
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.TS.UTC().Format(time.RFC3339Nano), prev, rowHash, r.Mode, r.Decision, r.Severity, r.CriterionID,
		r.PolicyHash, boolInt(r.JudgeUsed), r.JudgeHosting, r.Degraded, r.LatencyMS,
		r.TraceID, r.SessionID, r.Tenant, r.User, r.RequestID, r.MessageHash, r.ReasonExcerpt,
	)
	if err != nil {
		return "", fmt.Errorf("obs/store.InsertAdmissionEvent: insert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("obs/store.InsertAdmissionEvent: commit: %w", err)
	}
	return rowHash, nil
}

// AdmissionEventView is one row for the timeline / enrichment reads. It carries
// only what the surfaces render (no chain columns).
type AdmissionEventView struct {
	ID            int64  `json:"id"`
	TS            string `json:"ts"`
	Mode          string `json:"mode"`
	Decision      string `json:"decision"`
	Severity      string `json:"severity"`
	CriterionID   string `json:"criterion_id"`
	PolicyHash    string `json:"policy_hash"`
	JudgeUsed     bool   `json:"judge_used"`
	JudgeHosting  string `json:"judge_hosting"`
	Degraded      string `json:"degraded"`
	LatencyMS     int    `json:"latency_ms"`
	TraceID       string `json:"trace_id"`
	SessionID     string `json:"session_id"`
	Tenant        string `json:"tenant"`
	User          string `json:"user"`
	RequestID     string `json:"request_id"`
	ReasonExcerpt string `json:"reason_excerpt,omitempty"`
}

// AdmissionListOptions filters the timeline read.
type AdmissionListOptions struct {
	Decision  string // exact match, empty = any
	Criterion string // exact match, empty = any
	Since     time.Time
	Limit     int
	Offset    int
}

// ListAdmissionEvents returns verdicts newest-first under the filter.
func (s *Store) ListAdmissionEvents(ctx context.Context, opts AdmissionListOptions) ([]AdmissionEventView, error) {
	var (
		where []string
		args  []any
	)
	if opts.Decision != "" {
		where = append(where, "decision = ?")
		args = append(args, opts.Decision)
	}
	if opts.Criterion != "" {
		where = append(where, "criterion_id = ?")
		args = append(args, opts.Criterion)
	}
	if !opts.Since.IsZero() {
		where = append(where, "ts >= ?")
		args = append(args, opts.Since.UTC().Format(time.RFC3339Nano))
	}
	q := `SELECT id, ts, mode, decision, severity, COALESCE(criterion_id,''), policy_hash,
		judge_used, COALESCE(judge_hosting,''), COALESCE(degraded,''), latency_ms,
		COALESCE(trace_id,''), COALESCE(session_id,''), COALESCE(tenant,''), COALESCE(user,''),
		COALESCE(request_id,''), COALESCE(reason_excerpt,'')
		FROM obs_admission_events`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY id DESC"
	limit := opts.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	q += " LIMIT ? OFFSET ?"
	args = append(args, limit, opts.Offset)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("obs/store.ListAdmissionEvents: query: %w", err)
	}
	defer rows.Close()
	return scanAdmissionViews(rows)
}

// AdmissionEventsForRequest returns verdicts soft-joined to a proxy turn by
// request_id (trajectory enrichment). Newest first.
func (s *Store) AdmissionEventsForRequest(ctx context.Context, requestID string) ([]AdmissionEventView, error) {
	if strings.TrimSpace(requestID) == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, ts, mode, decision, severity, COALESCE(criterion_id,''), policy_hash,
		judge_used, COALESCE(judge_hosting,''), COALESCE(degraded,''), latency_ms,
		COALESCE(trace_id,''), COALESCE(session_id,''), COALESCE(tenant,''), COALESCE(user,''),
		COALESCE(request_id,''), COALESCE(reason_excerpt,'')
		FROM obs_admission_events WHERE request_id = ? ORDER BY id DESC`, requestID)
	if err != nil {
		return nil, fmt.Errorf("obs/store.AdmissionEventsForRequest: query: %w", err)
	}
	defer rows.Close()
	return scanAdmissionViews(rows)
}

// AdmissionReplaySample is one captured request body to replay against the
// current policy (admission spec §9 `simulate`). Text is the raw prompt body,
// present ONLY for spans the node's content posture retained (obs_span_content
// stores content solely when the ContentGate allowed it); TraceID is a
// content-free reference for the report.
type AdmissionReplaySample struct {
	Text    string
	TraceID string
}

// LoadAdmissionReplaySamples returns the most-recent captured prompt bodies to
// replay through the admission pipeline. It reads the SAME content source the
// eval plane uses (obs_span_content, kind='prompt'), skipping rows with no
// retained content (content-gated off) — those aren't replayable. Newest
// first, capped at limit. Node-local read; no wire exposure.
func (s *Store) LoadAdmissionReplaySamples(ctx context.Context, limit int) ([]AdmissionReplaySample, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT content, COALESCE(trace_id,'')
		  FROM obs_span_content
		 WHERE kind = 'prompt' AND content IS NOT NULL AND content <> ''
		 ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("obs/store.LoadAdmissionReplaySamples: query: %w", err)
	}
	defer rows.Close()
	var out []AdmissionReplaySample
	for rows.Next() {
		var sm AdmissionReplaySample
		if err := rows.Scan(&sm.Text, &sm.TraceID); err != nil {
			return nil, fmt.Errorf("obs/store.LoadAdmissionReplaySamples: scan: %w", err)
		}
		out = append(out, sm)
	}
	return out, rows.Err()
}

func scanAdmissionViews(rows *sql.Rows) ([]AdmissionEventView, error) {
	var out []AdmissionEventView
	for rows.Next() {
		var v AdmissionEventView
		var judgeUsed int
		if err := rows.Scan(&v.ID, &v.TS, &v.Mode, &v.Decision, &v.Severity, &v.CriterionID, &v.PolicyHash,
			&judgeUsed, &v.JudgeHosting, &v.Degraded, &v.LatencyMS,
			&v.TraceID, &v.SessionID, &v.Tenant, &v.User, &v.RequestID, &v.ReasonExcerpt); err != nil {
			return nil, fmt.Errorf("obs/store.scanAdmissionViews: scan: %w", err)
		}
		v.JudgeUsed = judgeUsed != 0
		out = append(out, v)
	}
	return out, rows.Err()
}

// AdmissionDecisionCounts sums verdicts by decision since a time bound.
func (s *Store) AdmissionDecisionCounts(ctx context.Context, since time.Time) (map[string]int, error) {
	q := `SELECT decision, COUNT(*) FROM obs_admission_events`
	var args []any
	if !since.IsZero() {
		q += " WHERE ts >= ?"
		args = append(args, since.UTC().Format(time.RFC3339Nano))
	}
	q += " GROUP BY decision"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("obs/store.AdmissionDecisionCounts: query: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var dec string
		var n int
		if err := rows.Scan(&dec, &n); err != nil {
			return nil, fmt.Errorf("obs/store.AdmissionDecisionCounts: scan: %w", err)
		}
		out[dec] = n
	}
	return out, rows.Err()
}

// AdmissionChainResult reports a verify-walk outcome.
type AdmissionChainResult struct {
	Rows    int    `json:"rows"`
	OK      bool   `json:"ok"`
	BreakAt int64  `json:"break_at,omitempty"` // id of the first divergent row
	Detail  string `json:"detail,omitempty"`
}

// VerifyAdmissionChain walks obs_admission_events in id order and recomputes
// each row_hash from the stored prev_hash + canonical bytes, reporting the
// first divergence (tamper-evidence, admission spec §12).
func (s *Store) VerifyAdmissionChain(ctx context.Context) (AdmissionChainResult, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, ts, prev_hash, row_hash, mode, decision, severity,
		COALESCE(criterion_id,''), policy_hash, judge_used, COALESCE(judge_hosting,''), COALESCE(degraded,''),
		latency_ms, COALESCE(trace_id,''), COALESCE(session_id,''), COALESCE(tenant,''), COALESCE(user,''),
		COALESCE(request_id,''), message_hash, COALESCE(reason_excerpt,'')
		FROM obs_admission_events ORDER BY id ASC`)
	if err != nil {
		return AdmissionChainResult{}, fmt.Errorf("obs/store.VerifyAdmissionChain: query: %w", err)
	}
	defer rows.Close()

	res := AdmissionChainResult{OK: true}
	prevExpected := ""
	for rows.Next() {
		var (
			id                  int64
			tsStr, prev, stored string
			r                   AdmissionEventRow
			judgeUsed           int
		)
		if err := rows.Scan(&id, &tsStr, &prev, &stored, &r.Mode, &r.Decision, &r.Severity,
			&r.CriterionID, &r.PolicyHash, &judgeUsed, &r.JudgeHosting, &r.Degraded,
			&r.LatencyMS, &r.TraceID, &r.SessionID, &r.Tenant, &r.User,
			&r.RequestID, &r.MessageHash, &r.ReasonExcerpt); err != nil {
			return AdmissionChainResult{}, fmt.Errorf("obs/store.VerifyAdmissionChain: scan: %w", err)
		}
		r.JudgeUsed = judgeUsed != 0
		if r.TS, err = time.Parse(time.RFC3339Nano, tsStr); err != nil {
			res.OK, res.BreakAt, res.Detail = false, id, "unparseable ts"
			return res, nil
		}
		res.Rows++
		if prev != prevExpected {
			res.OK, res.BreakAt, res.Detail = false, id, "prev_hash does not match prior row_hash"
			return res, nil
		}
		if want := admissionChainHash(prev, r.canonicalBytes()); want != stored {
			res.OK, res.BreakAt, res.Detail = false, id, "row_hash mismatch (row altered)"
			return res, nil
		}
		prevExpected = stored
	}
	return res, rows.Err()
}

// UpsertPolicyVersion records a content-addressed policy snapshot. body is the
// admin's authored policy (config, not end-user content). Idempotent on
// policy_hash.
func (s *Store) UpsertPolicyVersion(ctx context.Context, policyHash, mode, scope string, criteriaCount int, body string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO obs_admission_policy_versions (policy_hash, created_at, mode, scope, criteria_count, body)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(policy_hash) DO NOTHING`,
		policyHash, time.Now().UTC().Format(time.RFC3339Nano), mode, scope, criteriaCount, body)
	if err != nil {
		return fmt.Errorf("obs/store.UpsertPolicyVersion: %w", err)
	}
	return nil
}
