package guard

import (
	"time"

	"github.com/marmutapp/superbased-observer/internal/policy"
)

// Posture integration (guard spec §5.4 / §13, G11). The compiled
// native-dialect drift check is computed at the owner — the cmd-layer
// dialect runner, which holds the guard/compile translation tables
// and the store's pin state — and evaluated here through the REAL
// engine so [guard.rules] disable, overrides and mode apply to R-204
// exactly like every other rule (the EvaluateMCPFindings precedent).

// EvaluatePostureFindings evaluates posture findings (the §13.2
// dialect drift results today) through the engine: one
// KindConfigChange event per finding, so each audit row names exactly
// one subject. The events carry watcher capabilities — the drift scan
// observes config state post-hoc and can never block (the R-204 row
// is flag-in-both-modes regardless; `observer guard compile` is the
// remediation). The native config PATH travels inside the finding,
// never as Event.Target: a config-change event targeting the settings
// path itself would hit R-160's earlier path row, and the scan is not
// an agent write (pinned by the policy posture tests).
//
// Returned verdicts are ready for store.PersistGuardVerdicts; rows
// land session-less and unanchored (no action/api_turn produced them
// — the compiled artifact itself is the subject).
func (g *Guard) EvaluatePostureFindings(findings []policy.PostureFinding, now time.Time) []ActionVerdict {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var out []ActionVerdict
	for i := range findings {
		f := findings[i]
		ev := policy.Event{
			Kind:            policy.KindConfigChange,
			Tool:            f.Client,
			Caps:            watcherCaps,
			PostureFindings: []policy.PostureFinding{f},
			Now:             now,
		}
		verdict, guardErr := g.Evaluate(ev)
		if verdict.Decision < policy.DecisionFlag && guardErr == nil {
			continue
		}
		out = append(out, ActionVerdict{
			Input: ActionInput{
				Tool:      f.Client,
				Target:    f.Target,
				Timestamp: now,
			},
			Kind:       policy.KindConfigChange,
			Category:   g.CategoryFor(verdict.RuleID),
			Verdict:    verdict,
			GuardError: guardErr != nil,
		})
	}
	return out
}
