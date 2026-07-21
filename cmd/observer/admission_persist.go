package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/marmutapp/superbased-observer/internal/config"
)

// admission_persist.go implements the write-through persistence seam for the
// obs admission policy editor (gap-audit item #11: POST
// /api/obs/admission/policy?persist=1). It is the REVERSE of obs_wire.go's
// admissionPolicyInput ([observability.admission] config → PolicyInput): it
// maps an edited policy back onto the config block and writes it durably so a
// daemon restart keeps the admin's edit instead of silently reverting it.
//
// The seam crosses as raw JSON bytes (httpapi.PolicyPersistFunc) precisely so
// this file needs NO internal/obs import — only cmd/observer/obs_wire.go may
// import internal/obs (the reverse-import separability boundary,
// tests/invariant/obs_boundary_test.go). This file therefore decodes the
// editor wire shape into a LOCAL mirror struct and never references an
// internal/obs type.

// admissionEditorPolicy mirrors internal/obs/httpapi's admissionPolicyDTO JSON
// wire shape (the editor GET/POST body). It carries only the operator-editable
// fields; sizing knobs (judge_chunk_bytes, etc.), the judge, and the budget
// are intentionally NOT part of the editor shape and are preserved untouched
// from the on-disk config by applyAdmissionEditorPolicy.
type admissionEditorPolicy struct {
	Mode              string                     `json:"mode"`
	Strict            bool                       `json:"strict"`
	Scope             string                     `json:"scope"`
	SecretRemoteJudge string                     `json:"secret_remote_judge"`
	Prefilter         admissionEditorPrefilter   `json:"prefilter"`
	Criteria          []admissionEditorCriterion `json:"criteria"`
}

// admissionEditorPrefilter mirrors httpapi.admissionPrefilterDTO.
type admissionEditorPrefilter struct {
	Allow           []string `json:"allow"`
	Deny            []string `json:"deny"`
	MaxMessageBytes int      `json:"max_message_bytes"`
}

// admissionEditorCriterion mirrors httpapi.admissionCriterionDTO.
type admissionEditorCriterion struct {
	ID         string   `json:"id"`
	Type       string   `json:"type"`
	Name       string   `json:"name"`
	Definition string   `json:"definition"`
	Topics     []string `json:"topics"`
	Decision   string   `json:"decision"`
	Severity   string   `json:"severity"`
}

// admissionPolicyPersister returns the httpapi write-through persistence seam
// (typed as func(context.Context, []byte) error — the underlying type of
// httpapi.PolicyPersistFunc, kept plain here to avoid importing internal/obs).
// obs_wire.go adapts it onto httpapi.PolicyPersistFunc at the single wiring
// point. Each call resolves the node's global config path fresh, RELOADS the
// on-disk config (so concurrent unrelated edits are preserved), overlays the
// edited [observability.admission] policy, and writes through config.WriteToml
// — the one config-write owner (atomic temp-file + rename, prior file kept as
// .bak). CLAUDE.md #4: one owner per config write.
func admissionPolicyPersister() func(ctx context.Context, policyJSON []byte) error {
	return func(_ context.Context, policyJSON []byte) error {
		var p admissionEditorPolicy
		if err := json.Unmarshal(policyJSON, &p); err != nil {
			return fmt.Errorf("admissionPolicyPersister: decode policy: %w", err)
		}
		path, err := resolveAdmissionConfigPath("")
		if err != nil {
			return fmt.Errorf("admissionPolicyPersister: resolve config path: %w", err)
		}
		cfg, err := loadConfigForSetup(path)
		if err != nil {
			return fmt.Errorf("admissionPolicyPersister: load config: %w", err)
		}
		cfg = applyAdmissionEditorPolicy(cfg, p)
		if err := config.WriteToml(path, cfg); err != nil {
			return fmt.Errorf("admissionPolicyPersister: write config: %w", err)
		}
		return nil
	}
}

// applyAdmissionEditorPolicy overlays the editor's policy fields onto cfg's
// [observability.admission] block and returns the result. It is PURE (no I/O)
// so it is unit-testable. It writes ONLY the editor-editable fields (mode,
// strict, scope, secret_remote_judge, the pre-filter, and the criterion
// table), leaving the judge, budget, retention, cache TTL, and chunk-sizing
// config exactly as loaded from disk — those are not part of the editor wire
// shape, so a policy apply must never clobber them. Enabled is forced true:
// the editor is only reachable when admission is already running, and applying
// a live policy implies the block stays enabled.
func applyAdmissionEditorPolicy(cfg config.Config, p admissionEditorPolicy) config.Config {
	ac := &cfg.Observability.Admission
	ac.Enabled = true
	ac.Mode = p.Mode
	ac.Strict = p.Strict
	ac.Scope = p.Scope
	ac.SecretRemoteJudge = p.SecretRemoteJudge
	ac.Prefilter = config.AdmissionPrefilterConfig{
		Allow:           p.Prefilter.Allow,
		Deny:            p.Prefilter.Deny,
		MaxMessageBytes: p.Prefilter.MaxMessageBytes,
	}
	ac.Criterion = make([]config.AdmissionCriterionConfig, 0, len(p.Criteria))
	for _, c := range p.Criteria {
		ac.Criterion = append(ac.Criterion, config.AdmissionCriterionConfig{
			ID:         c.ID,
			Type:       c.Type,
			Name:       c.Name,
			Definition: c.Definition,
			Topics:     c.Topics,
			Decision:   c.Decision,
			Severity:   c.Severity,
		})
	}
	return cfg
}
