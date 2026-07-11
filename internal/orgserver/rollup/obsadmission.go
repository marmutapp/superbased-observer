// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 SuperBased

package rollup

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// obsadmission.go is the READ-ONLY org input-admission monitoring surface
// (Plane-A admission org tier, gap-audit 2026-07-10 §2.1 / #1b). It reports the
// posture (mode / judge / criteria / audit-chain), the verdict timeline, the
// would-block/flag overlay by end-user, and the shared policy version history
// that enrolled nodes push under [org_client.share.obs].admission. Authoring is
// deliberately node-side ONLY — there is NO remote policy write here, mirroring
// the "never server-forced" posture the rest of the org surfaces hold.
//
// Display mapping (owned HERE, not on the wire): the node ships its verdict
// vocabulary VERBATIM (allow | flag | ask | deny). The node engine orders
// decisions by strictness allow < flag < ask < deny and gates every request at
// `>= ask` (internal/obs/admission/pipeline.go: a criterion decision `>= ask`
// is terminal, i.e. the request is HELD for confirmation rather than admitted),
// so ask and deny are the two BLOCKING-class outcomes. The web2 posture tile
// carries exactly three 24h buckets {allow, flag, would_block}; this rollup
// folds ask+deny into would_block for those summary counts and for the
// per-end-user overlay. The verdict ROW list ships the RAW stored decision
// unchanged (allow/flag/ask/deny) because the web2 decision pill renders each
// distinctly (ask→info, deny→danger) — translating there would collapse a
// distinction the UI expects.
//
// Chain rule (mirrors the guard §10.4 chainSegmentsCTE, windowed): per node
// (grouped by the re-pinned user_email) a chain is intact when its events split
// into at most ONE segment — segments = heads (empty prev_hash) + unlinked
// (non-empty prev_hash whose target row_hash is absent from THIS node's window
// row_hash set). A node's oldest in-window row legitimately dangles (its
// predecessor predates the pushed window), so segments <= 1 is tolerant of that
// single boundary; segments > 1 means a genuine mid-window gap (broken /
// truncated).

// maxAdmissionVerdicts caps the verdict timeline payload (newest first). The
// 24h summary counts are computed separately over the full 24h slice.
const maxAdmissionVerdicts = 500

// ObsAdmissionVerdictCounts is the 24h posture summary. would_block folds the
// two blocking-class node decisions (ask + deny).
type ObsAdmissionVerdictCounts struct {
	Allow      int64 `json:"allow"`
	Flag       int64 `json:"flag"`
	WouldBlock int64 `json:"would_block"`
}

// ObsAdmissionChainNode is one node's (user_email's) audit-chain state.
type ObsAdmissionChainNode struct {
	UserEmail string `json:"user_email"`
	Rows      int64  `json:"rows"`
	OK        bool   `json:"ok"`
}

// ObsAdmissionChain is the fleet audit-chain roll-up: ok is true only when
// every node's chain is intact.
type ObsAdmissionChain struct {
	OK    bool                    `json:"ok"`
	Nodes []ObsAdmissionChainNode `json:"nodes"`
}

// ObsAdmissionPolicyVersion is one shared, content-addressed policy snapshot.
type ObsAdmissionPolicyVersion struct {
	PolicyHash    string `json:"policy_hash"`
	UserEmail     string `json:"user_email"` // author = the pushing operator
	CreatedAt     string `json:"created_at"`
	Mode          string `json:"mode"`
	Scope         string `json:"scope"`
	CriteriaCount int64  `json:"criteria_count"`
	Body          string `json:"body"`
}

// ObsAdmissionVerdictRow is one verdict on the timeline (content-free; end_user
// is present only where the node shared full content).
type ObsAdmissionVerdictRow struct {
	TS           string `json:"ts"`
	UserEmail    string `json:"user_email"`
	Mode         string `json:"mode"`
	Decision     string `json:"decision"` // raw node vocabulary (allow|flag|ask|deny)
	Severity     string `json:"severity"`
	CriterionID  string `json:"criterion_id"`
	JudgeUsed    bool   `json:"judge_used"`
	JudgeHosting string `json:"judge_hosting"`
	Degraded     string `json:"degraded"`
	LatencyMS    int64  `json:"latency_ms"`
	TraceID      string `json:"trace_id"`
	RequestID    string `json:"request_id"`
	EndUser      string `json:"end_user"` // '' unless full-content sharing
}

// ObsAdmissionUserBlocks is the per-end-user would-block/flag overlay.
type ObsAdmissionUserBlocks struct {
	EndUser    string `json:"end_user"`
	WouldBlock int64  `json:"would_block"`
	Flag       int64  `json:"flag"`
}

// ObsAdmissionResult is the GET /api/org/obs/admission body. It matches the
// web2 ObsAdmissionResult interface exactly.
type ObsAdmissionResult struct {
	WindowDays       int                         `json:"window_days"`
	Configured       bool                        `json:"configured"`
	Mode             string                      `json:"mode"`          // newest shared policy's mode
	JudgeHosting     string                      `json:"judge_hosting"` // newest judged verdict's hosting
	CriteriaCount    int64                       `json:"criteria_count"`
	Verdicts24h      ObsAdmissionVerdictCounts   `json:"verdicts_24h"`
	Chain            ObsAdmissionChain           `json:"chain"`
	Policies         []ObsAdmissionPolicyVersion `json:"policies"`
	Verdicts         []ObsAdmissionVerdictRow    `json:"verdicts"`
	WouldBlockByUser []ObsAdmissionUserBlocks    `json:"would_block_by_user"`
}

// ObsAdmission builds the admission monitoring surface over the trailing
// window, scoped to the caller (peopleScopeSQL on pushed_by_user_id, identical
// to ObsTraceContent). configured is true when any admission event OR policy
// row is visible in scope. Single-org-per-server convention (no org param).
func ObsAdmission(ctx context.Context, db *sql.DB, w Window, scope Scope, selfUserID string, now time.Time) (ObsAdmissionResult, error) {
	res := ObsAdmissionResult{
		WindowDays:       w.days(),
		Chain:            ObsAdmissionChain{OK: true, Nodes: []ObsAdmissionChainNode{}},
		Policies:         []ObsAdmissionPolicyVersion{},
		Verdicts:         []ObsAdmissionVerdictRow{},
		WouldBlockByUser: []ObsAdmissionUserBlocks{},
	}
	uScope, uArgs := peopleScopeSQL("pushed_by_user_id", scope, selfUserID)
	if uScope == falseScope {
		return res, nil
	}
	since := now.UTC().AddDate(0, 0, -w.days()).Format(time.RFC3339)
	cutoff24h := now.UTC().Add(-24 * time.Hour).Format(time.RFC3339)

	// --- policy version history (unwindowed: the full shared history) ---------
	//nolint:gosec // G201: uScope is a parameterized scope fragment; values bind via ?.
	pq := `
SELECT policy_hash, user_email, created_at, mode, scope, criteria_count, body
  FROM obs_admission_policy_versions
 WHERE ` + uScope + `
 ORDER BY created_at DESC, id DESC`
	if err := eachRow(ctx, db, pq, uArgs, func(rows *sql.Rows) error {
		var p ObsAdmissionPolicyVersion
		if err := rows.Scan(&p.PolicyHash, &p.UserEmail, &p.CreatedAt, &p.Mode, &p.Scope,
			&p.CriteriaCount, &p.Body); err != nil {
			return err
		}
		res.Policies = append(res.Policies, p)
		return nil
	}); err != nil {
		return ObsAdmissionResult{}, fmt.Errorf("rollup.ObsAdmission: policies: %w", err)
	}
	// Posture: mode + criteria_count come from the newest shared policy.
	// judge_hosting is an EVENT-level fact (policies do not carry it), so it is
	// derived below from the newest judged verdict.
	if len(res.Policies) > 0 {
		res.Mode = res.Policies[0].Mode
		res.CriteriaCount = res.Policies[0].CriteriaCount
	}

	// --- 24h posture counts (allow / flag / would_block=ask+deny) -------------
	//nolint:gosec // G201: uScope is a parameterized scope fragment; values bind via ?.
	cq := `
SELECT decision, COUNT(*)
  FROM obs_admission_events
 WHERE ts >= ? AND ` + uScope + `
 GROUP BY decision`
	if err := eachRow(ctx, db, cq, append([]any{cutoff24h}, uArgs...), func(rows *sql.Rows) error {
		var decision string
		var n int64
		if err := rows.Scan(&decision, &n); err != nil {
			return err
		}
		switch decision {
		case "allow":
			res.Verdicts24h.Allow += n
		case "flag":
			res.Verdicts24h.Flag += n
		case "ask", "deny":
			res.Verdicts24h.WouldBlock += n
		}
		return nil
	}); err != nil {
		return ObsAdmissionResult{}, fmt.Errorf("rollup.ObsAdmission: verdicts_24h: %w", err)
	}

	// --- audit-chain continuity per node (windowed heads+unlinked <= 1) -------
	//nolint:gosec // G201: uScope is a parameterized scope fragment; values bind via ?.
	chq := `
WITH scoped AS (
  SELECT user_email, prev_hash, row_hash
    FROM obs_admission_events
   WHERE ts >= ? AND ` + uScope + `
)
SELECT s.user_email, COUNT(*) AS rows,
       SUM(CASE WHEN s.prev_hash = '' THEN 1 ELSE 0 END) AS heads,
       SUM(CASE WHEN s.prev_hash != '' AND NOT EXISTS (
              SELECT 1 FROM scoped p WHERE p.user_email = s.user_email AND p.row_hash = s.prev_hash
           ) THEN 1 ELSE 0 END) AS unlinked
  FROM scoped s
 GROUP BY s.user_email
 ORDER BY s.user_email`
	if err := eachRow(ctx, db, chq, append([]any{since}, uArgs...), func(rows *sql.Rows) error {
		var node ObsAdmissionChainNode
		var heads, unlinked int64
		if err := rows.Scan(&node.UserEmail, &node.Rows, &heads, &unlinked); err != nil {
			return err
		}
		node.OK = heads+unlinked <= 1
		if !node.OK {
			res.Chain.OK = false
		}
		res.Chain.Nodes = append(res.Chain.Nodes, node)
		return nil
	}); err != nil {
		return ObsAdmissionResult{}, fmt.Errorf("rollup.ObsAdmission: chain: %w", err)
	}

	// --- verdict timeline (newest first, capped; raw decision verbatim) -------
	//nolint:gosec // G201: uScope is a parameterized scope fragment; values bind via ?.
	vq := `
SELECT ts, user_email, mode, decision, severity, criterion_id,
       judge_used, judge_hosting, degraded, latency_ms,
       trace_id, request_id, COALESCE(end_user,'')
  FROM obs_admission_events
 WHERE ts >= ? AND ` + uScope + `
 ORDER BY ts DESC, id DESC
 LIMIT ?`
	vArgs := append([]any{since}, uArgs...)
	vArgs = append(vArgs, maxAdmissionVerdicts)
	if err := eachRow(ctx, db, vq, vArgs, func(rows *sql.Rows) error {
		var v ObsAdmissionVerdictRow
		var judgeUsed int64
		if err := rows.Scan(&v.TS, &v.UserEmail, &v.Mode, &v.Decision, &v.Severity, &v.CriterionID,
			&judgeUsed, &v.JudgeHosting, &v.Degraded, &v.LatencyMS,
			&v.TraceID, &v.RequestID, &v.EndUser); err != nil {
			return err
		}
		v.JudgeUsed = judgeUsed != 0
		// The newest judged verdict fixes the posture judge_hosting tile.
		if res.JudgeHosting == "" && v.JudgeHosting != "" {
			res.JudgeHosting = v.JudgeHosting
		}
		res.Verdicts = append(res.Verdicts, v)
		return nil
	}); err != nil {
		return ObsAdmissionResult{}, fmt.Errorf("rollup.ObsAdmission: verdicts: %w", err)
	}

	// --- per-end-user would-block/flag overlay (non-empty end_user only) ------
	// HAVING drops all-allow end-users: a would-block/flag OVERLAY listing a
	// user with zero of either would be misleading noise on the budget card.
	//nolint:gosec // G201: uScope is a parameterized scope fragment; values bind via ?.
	bq := `
SELECT end_user,
       SUM(CASE WHEN decision IN ('ask','deny') THEN 1 ELSE 0 END) AS would_block,
       SUM(CASE WHEN decision = 'flag' THEN 1 ELSE 0 END) AS flag
  FROM obs_admission_events
 WHERE ts >= ? AND COALESCE(end_user,'') != '' AND ` + uScope + `
 GROUP BY end_user
HAVING would_block > 0 OR flag > 0`
	if err := eachRow(ctx, db, bq, append([]any{since}, uArgs...), func(rows *sql.Rows) error {
		var b ObsAdmissionUserBlocks
		if err := rows.Scan(&b.EndUser, &b.WouldBlock, &b.Flag); err != nil {
			return err
		}
		res.WouldBlockByUser = append(res.WouldBlockByUser, b)
		return nil
	}); err != nil {
		return ObsAdmissionResult{}, fmt.Errorf("rollup.ObsAdmission: would_block_by_user: %w", err)
	}
	sort.SliceStable(res.WouldBlockByUser, func(i, j int) bool {
		if res.WouldBlockByUser[i].WouldBlock != res.WouldBlockByUser[j].WouldBlock {
			return res.WouldBlockByUser[i].WouldBlock > res.WouldBlockByUser[j].WouldBlock
		}
		return res.WouldBlockByUser[i].EndUser < res.WouldBlockByUser[j].EndUser
	})

	res.Configured = len(res.Policies) > 0 || len(res.Verdicts) > 0 ||
		res.Verdicts24h.Allow+res.Verdicts24h.Flag+res.Verdicts24h.WouldBlock > 0
	return res, nil
}

// ObsAdmissionReason is one shared verdict-reason excerpt (a DEEPER, audited
// disclosure than the content-free verdict metadata).
type ObsAdmissionReason struct {
	TS            string `json:"ts"`
	RequestID     string `json:"request_id"`
	ReasonExcerpt string `json:"reason_excerpt"`
}

// ObsAdmissionReasonsResult is the GET /api/org/obs/admission/reasons body.
type ObsAdmissionReasonsResult struct {
	Reasons []ObsAdmissionReason `json:"reasons"`
}

// ObsAdmissionReasons returns the non-empty verdict-reason excerpts in the
// window + scope. Excerpts arrive only from nodes that opted into full-content
// sharing; hash-only nodes contribute nothing. The handler MUST write a
// view_admission_reasons audit row BEFORE calling this (deeper disclosure).
func ObsAdmissionReasons(ctx context.Context, db *sql.DB, w Window, scope Scope, selfUserID string) (ObsAdmissionReasonsResult, error) {
	res := ObsAdmissionReasonsResult{Reasons: []ObsAdmissionReason{}}
	uScope, uArgs := peopleScopeSQL("pushed_by_user_id", scope, selfUserID)
	if uScope == falseScope {
		return res, nil
	}
	since := time.Now().UTC().AddDate(0, 0, -w.days()).Format(time.RFC3339)
	//nolint:gosec // G201: uScope is a parameterized scope fragment; values bind via ?.
	q := `
SELECT ts, request_id, reason_excerpt
  FROM obs_admission_events
 WHERE ts >= ? AND reason_excerpt IS NOT NULL AND reason_excerpt != '' AND ` + uScope + `
 ORDER BY ts DESC, id DESC
 LIMIT ?`
	args := append([]any{since}, uArgs...)
	args = append(args, maxAdmissionVerdicts)
	if err := eachRow(ctx, db, q, args, func(rows *sql.Rows) error {
		var r ObsAdmissionReason
		if err := rows.Scan(&r.TS, &r.RequestID, &r.ReasonExcerpt); err != nil {
			return err
		}
		res.Reasons = append(res.Reasons, r)
		return nil
	}); err != nil {
		return ObsAdmissionReasonsResult{}, fmt.Errorf("rollup.ObsAdmissionReasons: %w", err)
	}
	return res, nil
}
