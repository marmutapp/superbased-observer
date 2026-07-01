package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/routing"
	"github.com/marmutapp/superbased-observer/internal/routingconfig"
)

// Routing rules editor backend (R2.2, operator checkpoint Q3 FULL):
// the dashboard's read/lint surface over [[routing.rules]].
//
// One write owner — deliberately NOT a second writer: custom routing
// rules live inside [routing] in config.toml, and the ONLY path that
// writes that section is PUT /api/config/section/routing
// (applySectionUpdate case "routing"). The R1.1 contract stands: a
// body WITHOUT RulesTOML preserves [[routing.rules]] wholesale from
// the prior file; a body WITH RulesTOML replaces them — after the
// same gate the lint endpoint runs (TOML parse, no stray keys, the
// exact config.Load shape checks via config.ValidateRouting, and the
// compiler's error-severity lint). A fragment that fails the gate
// refuses the save with the file untouched.
//
// The TOML dialect served and accepted here is the encoder's own
// (config.WriteToml round-trips the whole file through BurntSushi on
// every dashboard save), so what the editor shows is exactly what the
// file holds.

// routingRulesDoc is the TOML round-trip envelope: a fragment is the
// [[routing.rules]] blocks exactly as they appear in config.toml.
type routingRulesDoc struct {
	Routing struct {
		Rules []config.RoutingRuleConfig `toml:"rules"`
	} `toml:"routing"`
}

// parseRoutingRulesTOML decodes a [[routing.rules]] fragment.
// Returned problems are blocking: a TOML syntax error, or any key
// outside routing.rules — silently dropping pasted content the save
// would not carry is the failure mode this guard exists for.
func parseRoutingRulesTOML(text string) ([]config.RoutingRuleConfig, []string) {
	var doc routingRulesDoc
	md, err := toml.Decode(text, &doc)
	if err != nil {
		return nil, []string{fmt.Sprintf("TOML parse: %v", err)}
	}
	var problems []string
	for _, k := range md.Undecoded() {
		problems = append(problems, fmt.Sprintf("key %q is outside [[routing.rules]] — the editor saves rules only; edit other [routing] shapes in the section form or the config file", k.String()))
	}
	if strings.TrimSpace(text) != "" && len(doc.Routing.Rules) == 0 && len(problems) == 0 {
		problems = append(problems, "no [[routing.rules]] blocks found — the fragment must use the [[routing.rules]] table-array shape (see the recipe gallery in docs/model-routing.md)")
	}
	return doc.Routing.Rules, problems
}

// encodeRoutingRulesTOML renders rules in the same dialect
// config.WriteToml persists.
func encodeRoutingRulesTOML(rules []config.RoutingRuleConfig) (string, error) {
	if len(rules) == 0 {
		return "", nil
	}
	var doc routingRulesDoc
	doc.Routing.Rules = rules
	var sb strings.Builder
	if err := toml.NewEncoder(&sb).Encode(doc); err != nil {
		return "", fmt.Errorf("encode routing rules: %w", err)
	}
	return sb.String(), nil
}

// gateRoutingRules runs the rules-replacement save gate against the
// rest of the on-disk section: the exact config.Load shape checks
// (config.ValidateRouting) plus the compiler's lint over the composed
// policy (template + custom rules). Blocking problems are load errors
// and error-severity lint findings; warnings ride along for display
// but never block (the CLI lint contract).
func gateRoutingRules(current config.RoutingConfig, rules []config.RoutingRuleConfig) (issues []routing.LintIssue, blocking []string) {
	next := current
	next.Rules = rules
	if err := config.ValidateRouting(next); err != nil {
		blocking = append(blocking, err.Error())
	}
	_, issues = routing.Compile(routingconfig.Spec(next))
	if routing.LintHasErrors(issues) {
		for _, i := range issues {
			if i.Severity == routing.LintError {
				rule := i.RuleName
				if rule == "" {
					rule = "-"
				}
				blocking = append(blocking, fmt.Sprintf("lint [%s] rule=%s: %s", i.Check, rule, i.Message))
			}
		}
	}
	return issues, blocking
}

// handleRoutingPolicy serves GET /api/routing/policy — the rules
// editor's load payload: the current [[routing.rules]] as TOML (the
// encoder dialect), counts, the active policy identity, and the
// current full-policy lint findings. Read-only; the write path is the
// one config seam (PUT /api/config/section/routing with RulesTOML).
func (s *Server) handleRoutingPolicy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	cfg, err := loadConfigForDashboard(s.opts.ConfigPath)
	if err != nil {
		writeErr(w, fmt.Errorf("load config: %w", err))
		return
	}
	rulesTOML, err := encodeRoutingRulesTOML(cfg.Routing.Rules)
	if err != nil {
		writeErr(w, err)
		return
	}
	policy, issues := routing.Compile(routingconfig.Spec(cfg.Routing))
	writeJSON(w, map[string]any{
		"rules_toml":  rulesTOML,
		"rules":       len(cfg.Routing.Rules),
		"policy":      policy.Name,
		"policy_hash": policy.Hash(),
		"lint":        lintIssueViews(issues),
		"config_path": s.opts.ConfigPath,
		"note": "Custom rules append AFTER template expansion and are walked top-down, first match wins. " +
			"Saving replaces ALL [[routing.rules]] — an empty editor clears them. The save is lint-gated and " +
			"restart-honest; key_pool, tiers, budgets, privacy rules and every other [routing] shape are untouched.",
	})
}

// handleRoutingPolicyLint serves POST /api/routing/policy/lint — the
// editor's Validate button. Always 200 with the findings; the
// refusal-on-problems lives on the section PUT, where a write is at
// stake (the guard policy editor's contract).
func (s *Server) handleRoutingPolicyLint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		RulesTOML string `json:"rules_toml"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	cfg, err := loadConfigForDashboard(s.opts.ConfigPath)
	if err != nil {
		writeErr(w, fmt.Errorf("load config: %w", err))
		return
	}
	rules, problems := parseRoutingRulesTOML(req.RulesTOML)
	var issues []routing.LintIssue
	if len(problems) == 0 {
		var gateProblems []string
		issues, gateProblems = gateRoutingRules(cfg.Routing, rules)
		problems = append(problems, gateProblems...)
	}
	if problems == nil {
		problems = []string{}
	}
	writeJSON(w, map[string]any{
		"ok":       len(problems) == 0,
		"problems": problems,
		"lint":     lintIssueViews(issues),
		"rules":    len(rules),
	})
}

// lintIssueViews maps compiler lint findings to the wire shape the
// Routing page's status handler already serves.
func lintIssueViews(issues []routing.LintIssue) []map[string]string {
	out := make([]map[string]string, 0, len(issues))
	for _, i := range issues {
		out = append(out, map[string]string{
			"check":    i.Check,
			"rule":     i.RuleName,
			"severity": string(i.Severity),
			"message":  i.Message,
		})
	}
	return out
}
