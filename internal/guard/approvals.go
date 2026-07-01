package guard

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/marmutapp/superbased-observer/internal/policy"
)

// Approvals integration (guard spec §6.3, G8): operator-granted
// scoped exceptions downgrade blocking verdicts so the agent's
// natural retry succeeds. The grants live in guard_approvals (G2);
// the LOOKUP is injected (ApprovalLookup) because guard never imports
// store — cmd composition wires it to store.ApprovalActiveFor.
//
// Cost posture: the lookup only runs for verdicts that would BLOCK
// (ask/deny). Flag/allow verdicts never consult it, so the hook
// latency budget only pays the DB read on the rare blocking path —
// and a blocked call was about to not-happen anyway.

// ApprovalLookup reports whether an active (non-expired) approval
// grant covers the rule for this session/project. projectRootHash is
// the sha256 hex of the project root ("" when unknown). Lookup
// failures must report false (no approval) — fail-safe toward
// enforcement, never toward a silent grant.
type ApprovalLookup func(ruleID, sessionID, projectRootHash string) bool

// SetApprovalLookup wires the grant lookup. Nil disables approvals
// (every blocking verdict enforces). Set once at composition.
func (g *Guard) SetApprovalLookup(fn ApprovalLookup) {
	g.approvals = fn
}

// applyApprovals downgrades an ask/deny verdict to flag when an
// active grant covers it. The downgrade is RECORDED on the verdict
// (reason suffix + the returned bool drives the audit row's
// degraded_from="approved" marker) — an approval is an audited
// exception (§14.4 exception register), never a silent allow.
func (g *Guard) applyApprovals(v policy.Verdict, ev *policy.Event) (policy.Verdict, bool) {
	if g.approvals == nil || v.Decision < policy.DecisionAsk || v.RuleID == "" {
		return v, false
	}
	if !g.approvals(v.RuleID, ev.SessionID, HashProjectRoot(ev.ProjectRoot)) {
		return v, false
	}
	v.Decision = policy.DecisionFlag
	v.Reason += " [approved: an operator grant covers this rule for this scope]"
	return v, true
}

// HashProjectRoot returns the sha256 hex of a project root — the
// guard_approvals.project_root_hash convention (raw paths never land
// in the approvals table, matching the org-wire posture). Empty in,
// empty out.
func HashProjectRoot(root string) string {
	if root == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(root))
	return hex.EncodeToString(sum[:])
}
