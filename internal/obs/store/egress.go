package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// egress.go is the ONE owner (CLAUDE.md rule #4) of the node-local
// obs_egress_decisions table (migration 0007, G22 design §7). It records the
// Plane-A egress directive the boundary computed and the outcome the proxy
// realized, hash-chained (SHA-256 over prior row_hash || 0x1e || canonical
// bytes) for a tamper-evident audit story.
//
// SERIALIZED chain (design finding 15): unlike InsertAdmissionEvent's default
// deferred tx — where two writers can read the same predecessor and fork the
// chain — the egress insert holds egressChainMu across the read-tail+insert so
// the tail is stable, AND the table's UNIQUE(prev_hash) constraint is a
// DB-level backstop that refuses a forked write outright. The raw request text
// is never stored — only message_hash.

// egressChainMu serializes the read-tail-then-insert of the egress hash chain
// across every Store instance in the process (Store is a thin, re-created
// wrapper over the shared *sql.DB, so a per-Store mutex would not serialize —
// the chain is process-global state, so its lock is package-level).
var egressChainMu sync.Mutex

// EgressDecisionRow is one egress directive + realized outcome to persist. All
// columns are operator-config or content-free except user (end-user PII, same
// node-local posture as obs_admission_events.user).
type EgressDecisionRow struct {
	TS                   time.Time
	Mode                 string
	RuleName             string
	PolicyHash           string
	Action               string
	UpstreamID           string
	TargetShape          string
	ModelFrom            string
	ModelTo              string
	Effort               string
	ReasonCode           string
	MustUseTarget        bool
	Applied              bool
	FailClosed           bool
	SwitchHeld           bool
	EstCacheForfeitClass string
	Degraded             string
	VerdictDecision      string
	CriterionID          string
	MessageHash          string
	RequestID            string
	SessionID            string
	Tenant               string
	User                 string
}

// canonicalBytes returns the audit-canonical serialization for the hash chain.
// id, prev_hash and row_hash are excluded (id is SQLite-assigned; prev_hash is
// the other half of the preimage; row_hash is the output).
//
// The REALIZED-outcome annotations — applied, fail_closed, realized_at,
// realized_outcome (migration 0008) — are ALSO excluded: the proxy→obs
// realized-outcome callback UPDATEs them in place AFTER the forward (G22 wave
// 2), so folding them into the preimage would invalidate row_hash on every
// update. The chain therefore stays tamper-evident over the immutable DECISION
// (rule/policy/action/target/verdict/message_hash + the decision-time intent
// flags must_use_target and switch_held); the realized outcome is a linked,
// once-updated status. Field order is fixed and every field is 0x1e-separated
// so the bytes describe exactly what the row's DECISION half stores.
func (r EgressDecisionRow) canonicalBytes() []byte {
	var b strings.Builder
	w := func(parts ...string) {
		for _, p := range parts {
			b.WriteString(p)
			b.WriteByte(0x1e)
		}
	}
	w(r.TS.UTC().Format(time.RFC3339Nano))
	w(r.Mode, r.RuleName, r.PolicyHash, r.Action, r.UpstreamID, r.TargetShape)
	w(r.ModelFrom, r.ModelTo, r.Effort, r.ReasonCode)
	w(boolStr(r.MustUseTarget), boolStr(r.SwitchHeld))
	w(r.EstCacheForfeitClass, r.Degraded, r.VerdictDecision, r.CriterionID)
	w(r.MessageHash, r.RequestID, r.SessionID, r.Tenant, r.User)
	return []byte(b.String())
}

// egressChainHash computes SHA-256(prev || 0x1e || canonical row bytes),
// matching the admission chain construction.
func egressChainHash(prev string, canonical []byte) string {
	h := sha256.New()
	h.Write([]byte(prev))
	h.Write([]byte{0x1e})
	h.Write(canonical)
	return hex.EncodeToString(h.Sum(nil))
}

// InsertEgressDecision appends one egress decision, computing its chain links
// against the current table tail. The read-tail+insert is serialized by
// egressChainMu (process-global) and further guarded by UNIQUE(prev_hash) at the
// DB level, so concurrent writers cannot fork the chain. Returns the inserted
// row id — the correlation handle the proxy carries back on the realized-outcome
// callback (UpdateEgressRealized). The row is inserted with the realized
// annotations at their decision-time baseline (applied=false, fail_closed=false);
// the proxy reports the actual outcome after the forward.
func (s *Store) InsertEgressDecision(ctx context.Context, r EgressDecisionRow) (int64, error) {
	if r.TS.IsZero() {
		r.TS = time.Now().UTC()
	}

	egressChainMu.Lock()
	defer egressChainMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("obs/store.InsertEgressDecision: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var prev string
	err = tx.QueryRowContext(ctx, `SELECT row_hash FROM obs_egress_decisions ORDER BY id DESC LIMIT 1`).Scan(&prev)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("obs/store.InsertEgressDecision: tail: %w", err)
	}
	rowHash := egressChainHash(prev, r.canonicalBytes())

	res, err := tx.ExecContext(
		ctx, `
		INSERT INTO obs_egress_decisions (
			ts, prev_hash, row_hash, mode, rule_name, policy_hash, action,
			upstream_id, target_shape, model_from, model_to, effort, reason_code,
			must_use_target, applied, fail_closed, switch_held,
			est_cache_forfeit_class, degraded, verdict_decision, criterion_id,
			message_hash, request_id, session_id, tenant, user
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.TS.UTC().Format(time.RFC3339Nano), prev, rowHash, r.Mode, r.RuleName, r.PolicyHash, r.Action,
		r.UpstreamID, r.TargetShape, r.ModelFrom, r.ModelTo, r.Effort, r.ReasonCode,
		boolInt(r.MustUseTarget), boolInt(r.Applied), boolInt(r.FailClosed), boolInt(r.SwitchHeld),
		r.EstCacheForfeitClass, r.Degraded, r.VerdictDecision, r.CriterionID,
		r.MessageHash, r.RequestID, r.SessionID, r.Tenant, r.User,
	)
	if err != nil {
		return 0, fmt.Errorf("obs/store.InsertEgressDecision: insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("obs/store.InsertEgressDecision: last id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("obs/store.InsertEgressDecision: commit: %w", err)
	}
	return id, nil
}

// UpdateEgressRealized records the outcome the proxy actually realized for a
// previously-inserted egress decision (G22 wave 2, design §7). It UPDATEs the
// mutable realized annotations — applied, fail_closed, realized_outcome — and
// stamps realized_at. These columns are OUTSIDE the hash-chain preimage
// (canonicalBytes), so this in-place update never invalidates row_hash and the
// tamper-evident DECISION chain stays intact. It is a no-op on a zero id (a
// non-persisted / advise-mode decision the proxy never routed).
func (s *Store) UpdateEgressRealized(ctx context.Context, id int64, applied, failClosed bool, outcome string) error {
	if id == 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE obs_egress_decisions
		SET applied = ?, fail_closed = ?, realized_outcome = ?, realized_at = ?
		WHERE id = ?`,
		boolInt(applied), boolInt(failClosed), outcome, time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("obs/store.UpdateEgressRealized: %w", err)
	}
	return nil
}

// EgressDecisionView is one row for the timeline / enrichment reads (no chain
// columns).
type EgressDecisionView struct {
	ID              int64  `json:"id"`
	TS              string `json:"ts"`
	Mode            string `json:"mode"`
	RuleName        string `json:"rule_name"`
	PolicyHash      string `json:"policy_hash"`
	Action          string `json:"action"`
	UpstreamID      string `json:"upstream_id,omitempty"`
	TargetShape     string `json:"target_shape,omitempty"`
	ModelFrom       string `json:"model_from,omitempty"`
	ModelTo         string `json:"model_to,omitempty"`
	Effort          string `json:"effort,omitempty"`
	ReasonCode      string `json:"reason_code"`
	MustUseTarget   bool   `json:"must_use_target"`
	Applied         bool   `json:"applied"`
	FailClosed      bool   `json:"fail_closed"`
	SwitchHeld      bool   `json:"switch_held"`
	RealizedOutcome string `json:"realized_outcome,omitempty"`
	Degraded        string `json:"degraded,omitempty"`
	VerdictDecision string `json:"verdict_decision,omitempty"`
	CriterionID     string `json:"criterion_id,omitempty"`
	RequestID       string `json:"request_id,omitempty"`
	SessionID       string `json:"session_id,omitempty"`
	User            string `json:"user,omitempty"`
}

// ListEgressDecisions returns egress decisions newest-first, capped at limit.
func (s *Store) ListEgressDecisions(ctx context.Context, limit int) ([]EgressDecisionView, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, ts, mode, rule_name, policy_hash, action,
			COALESCE(upstream_id,''), COALESCE(target_shape,''), COALESCE(model_from,''),
			COALESCE(model_to,''), COALESCE(effort,''), reason_code,
			must_use_target, applied, fail_closed, switch_held, COALESCE(realized_outcome,''),
			COALESCE(degraded,''), COALESCE(verdict_decision,''), COALESCE(criterion_id,''),
			COALESCE(request_id,''), COALESCE(session_id,''), COALESCE(user,'')
		FROM obs_egress_decisions ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("obs/store.ListEgressDecisions: query: %w", err)
	}
	defer rows.Close()
	var out []EgressDecisionView
	for rows.Next() {
		var v EgressDecisionView
		var mut, app, fc, sh int
		if err := rows.Scan(&v.ID, &v.TS, &v.Mode, &v.RuleName, &v.PolicyHash, &v.Action,
			&v.UpstreamID, &v.TargetShape, &v.ModelFrom, &v.ModelTo, &v.Effort, &v.ReasonCode,
			&mut, &app, &fc, &sh, &v.RealizedOutcome, &v.Degraded, &v.VerdictDecision, &v.CriterionID,
			&v.RequestID, &v.SessionID, &v.User); err != nil {
			return nil, fmt.Errorf("obs/store.ListEgressDecisions: scan: %w", err)
		}
		v.MustUseTarget, v.Applied, v.FailClosed, v.SwitchHeld = mut != 0, app != 0, fc != 0, sh != 0
		out = append(out, v)
	}
	return out, rows.Err()
}

// EgressActionCounts sums decisions by action since a time bound (for status).
func (s *Store) EgressActionCounts(ctx context.Context, since time.Time) (map[string]int, error) {
	q := `SELECT action, COUNT(*) FROM obs_egress_decisions`
	var args []any
	if !since.IsZero() {
		q += " WHERE ts >= ?"
		args = append(args, since.UTC().Format(time.RFC3339Nano))
	}
	q += " GROUP BY action"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("obs/store.EgressActionCounts: query: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var action string
		var n int
		if err := rows.Scan(&action, &n); err != nil {
			return nil, fmt.Errorf("obs/store.EgressActionCounts: scan: %w", err)
		}
		out[action] = n
	}
	return out, rows.Err()
}

// EgressChainResult reports a verify-walk outcome.
type EgressChainResult struct {
	Rows    int    `json:"rows"`
	OK      bool   `json:"ok"`
	BreakAt int64  `json:"break_at,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// VerifyEgressChain walks obs_egress_decisions in id order and recomputes each
// row_hash from the stored prev_hash + canonical bytes, reporting the first
// divergence (tamper-evidence, mirroring VerifyAdmissionChain).
func (s *Store) VerifyEgressChain(ctx context.Context) (EgressChainResult, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, ts, prev_hash, row_hash, mode, rule_name, policy_hash, action,
			COALESCE(upstream_id,''), COALESCE(target_shape,''), COALESCE(model_from,''),
			COALESCE(model_to,''), COALESCE(effort,''), reason_code,
			must_use_target, applied, fail_closed, switch_held,
			COALESCE(est_cache_forfeit_class,''), COALESCE(degraded,''),
			COALESCE(verdict_decision,''), COALESCE(criterion_id,''),
			COALESCE(message_hash,''), COALESCE(request_id,''), COALESCE(session_id,''),
			COALESCE(tenant,''), COALESCE(user,'')
		FROM obs_egress_decisions ORDER BY id ASC`)
	if err != nil {
		return EgressChainResult{}, fmt.Errorf("obs/store.VerifyEgressChain: query: %w", err)
	}
	defer rows.Close()

	res := EgressChainResult{OK: true}
	prevExpected := ""
	for rows.Next() {
		var (
			id                  int64
			tsStr, prev, stored string
			r                   EgressDecisionRow
			mut, app, fc, sh    int
		)
		if err := rows.Scan(&id, &tsStr, &prev, &stored, &r.Mode, &r.RuleName, &r.PolicyHash, &r.Action,
			&r.UpstreamID, &r.TargetShape, &r.ModelFrom, &r.ModelTo, &r.Effort, &r.ReasonCode,
			&mut, &app, &fc, &sh, &r.EstCacheForfeitClass, &r.Degraded,
			&r.VerdictDecision, &r.CriterionID, &r.MessageHash, &r.RequestID, &r.SessionID,
			&r.Tenant, &r.User); err != nil {
			return EgressChainResult{}, fmt.Errorf("obs/store.VerifyEgressChain: scan: %w", err)
		}
		r.MustUseTarget, r.Applied, r.FailClosed, r.SwitchHeld = mut != 0, app != 0, fc != 0, sh != 0
		if r.TS, err = time.Parse(time.RFC3339Nano, tsStr); err != nil {
			res.OK, res.BreakAt, res.Detail = false, id, "unparseable ts"
			return res, nil
		}
		res.Rows++
		if prev != prevExpected {
			res.OK, res.BreakAt, res.Detail = false, id, "prev_hash does not match prior row_hash"
			return res, nil
		}
		if want := egressChainHash(prev, r.canonicalBytes()); want != stored {
			res.OK, res.BreakAt, res.Detail = false, id, "row_hash mismatch (row altered)"
			return res, nil
		}
		prevExpected = stored
	}
	return res, rows.Err()
}
