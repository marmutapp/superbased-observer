package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/marmutapp/superbased-observer/internal/config"
)

// egress_persist.go implements the write-through persistence seam for the obs
// egress-routing policy editor (POST /api/obs/egress/policy?persist=1). It is
// the REVERSE of obs_wire.go's egressPolicyInput ([observability.egress] config
// → egress.PolicyInput): it maps an edited policy back onto the config block and
// writes it durably so a daemon restart keeps the admin's edit.
//
// Symmetric to admission_persist.go. The seam crosses as raw JSON bytes so this
// file needs NO internal/obs import (the reverse-import separability boundary,
// tests/invariant/obs_boundary_test.go): it decodes the editor wire shape into a
// LOCAL mirror struct and never references an internal/obs type.

// egressEditorPolicy mirrors internal/obs/httpapi's egressPolicyDTO JSON wire
// shape (the editor GET/POST body).
type egressEditorPolicy struct {
	Mode            string               `json:"mode"`
	CooldownSeconds int                  `json:"cooldown_seconds"`
	Targets         []egressEditorTarget `json:"targets"`
	Rules           []egressEditorRule   `json:"rules"`
	Cohorts         map[string]string    `json:"cohorts"`
}

type egressEditorTarget struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	Shape string `json:"shape"`
}

type egressEditorRule struct {
	Name          string             `json:"name"`
	When          egressEditorWhen   `json:"when"`
	Action        egressEditorAction `json:"action"`
	OnUnavailable string             `json:"on_unavailable"`
	Reason        string             `json:"reason"`
	ReasonCode    string             `json:"reason_code"`
}

type egressEditorWhen struct {
	VerdictAtLeast    string   `json:"verdict_at_least"`
	Criterion         string   `json:"criterion"`
	SeverityAtLeast   string   `json:"severity_at_least"`
	ContentClass      string   `json:"content_class"`
	ModelGlob         string   `json:"model_glob"`
	Provider          string   `json:"provider"`
	User              string   `json:"user"`
	UserCohort        string   `json:"user_cohort"`
	BudgetBandAtLeast *float64 `json:"budget_band_at_least"`
	MinPromptTokens   int      `json:"min_prompt_tokens"`
}

type egressEditorAction struct {
	RouteToUpstream string `json:"route_to_upstream"`
	RouteToModel    string `json:"route_to_model"`
	SetEffort       string `json:"set_effort"`
	Deny            bool   `json:"deny"`
	NoRoute         bool   `json:"no_route"`
}

// egressPolicyPersister returns the httpapi write-through persistence seam for
// the egress editor. configPath is the daemon's OWN loaded config path (the
// `--config` value, "" ⇒ default ~/.observer/config.toml). Each call serializes
// against every other in-process config writer via config.WithConfigLock,
// RELOADS the on-disk config (so concurrent unrelated edits are preserved),
// overlays [observability.egress], and writes through config.WriteToml (the one
// config-write owner — atomic temp+rename, prior file kept as .bak). CLAUDE.md
// #4: one owner per config write.
func egressPolicyPersister(configPath string) func(ctx context.Context, policyJSON []byte) error {
	return func(_ context.Context, policyJSON []byte) error {
		var p egressEditorPolicy
		if err := json.Unmarshal(policyJSON, &p); err != nil {
			return fmt.Errorf("egressPolicyPersister: decode policy: %w", err)
		}
		return config.WithConfigLock(func() error {
			path, err := resolveAdmissionConfigPath(configPath)
			if err != nil {
				return fmt.Errorf("egressPolicyPersister: resolve config path: %w", err)
			}
			cfg, err := loadConfigForSetup(path)
			if err != nil {
				return fmt.Errorf("egressPolicyPersister: load config: %w", err)
			}
			cfg = applyEgressEditorPolicy(cfg, p)
			if err := config.WriteToml(path, cfg); err != nil {
				return fmt.Errorf("egressPolicyPersister: write config: %w", err)
			}
			return nil
		})
	}
}

// applyEgressEditorPolicy overlays the editor's egress policy onto cfg's
// [observability.egress] block and returns the result. PURE (no I/O) so it is
// unit-testable. It writes the whole egress block (mode, cooldown, targets,
// rules, cohorts) and forces Enabled=true: the editor is authoring an egress
// policy, so the block stays enabled (mode="off" is how the admin turns routing
// off while keeping the authored rules). Everything OUTSIDE [observability.egress]
// is preserved untouched from the reloaded on-disk config.
func applyEgressEditorPolicy(cfg config.Config, p egressEditorPolicy) config.Config {
	ec := &cfg.Observability.Egress
	ec.Enabled = true
	ec.Mode = p.Mode
	ec.CooldownSeconds = p.CooldownSeconds
	ec.Cohorts = p.Cohorts

	ec.Targets = make([]config.EgressTargetConfig, 0, len(p.Targets))
	for _, t := range p.Targets {
		ec.Targets = append(ec.Targets, config.EgressTargetConfig{ID: t.ID, URL: t.URL, Shape: t.Shape})
	}

	ec.Rules = make([]config.EgressRuleConfig, 0, len(p.Rules))
	for _, r := range p.Rules {
		ec.Rules = append(ec.Rules, config.EgressRuleConfig{
			Name:          r.Name,
			OnUnavailable: r.OnUnavailable,
			Reason:        r.Reason,
			ReasonCode:    r.ReasonCode,
			When: config.EgressWhenConfig{
				VerdictAtLeast:    r.When.VerdictAtLeast,
				Criterion:         r.When.Criterion,
				SeverityAtLeast:   r.When.SeverityAtLeast,
				ContentClass:      r.When.ContentClass,
				ModelGlob:         r.When.ModelGlob,
				Provider:          r.When.Provider,
				User:              r.When.User,
				UserCohort:        r.When.UserCohort,
				BudgetBandAtLeast: r.When.BudgetBandAtLeast,
				MinPromptTokens:   r.When.MinPromptTokens,
			},
			Action: config.EgressActionConfig{
				RouteToUpstream: r.Action.RouteToUpstream,
				RouteToModel:    r.Action.RouteToModel,
				SetEffort:       r.Action.SetEffort,
				Deny:            r.Action.Deny,
				NoRoute:         r.Action.NoRoute,
			},
		})
	}
	return cfg
}
