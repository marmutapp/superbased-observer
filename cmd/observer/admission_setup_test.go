package main

import (
	"bufio"
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/config"
)

// TestApplyAdmissionSetup_GoldenLints pins the §7 acceptance: applying the
// on-scope + jailbreak templates with a purpose yields a config whose
// admission policy lints clean, with the purpose seeded into the definition.
func TestApplyAdmissionSetup_GoldenLints(t *testing.T) {
	cfg := config.Default()
	cfg.Observability.Enabled = true
	ch := admissionSetupChoices{
		SetJudge:       true,
		JudgeBaseURL:   "http://127.0.0.1:11434/v1",
		JudgeModel:     "llama3.1:8b-instruct",
		AdoptTemplates: []string{"on_scope", "jailbreak"},
		Purpose:        "scheduling assistant for a booking app",
		Mode:           "observe",
	}
	got := applyAdmissionSetup(cfg, ch)
	if !got.Observability.Admission.Enabled || got.Observability.Admission.Mode != "observe" {
		t.Fatalf("admission not enabled/observe: %+v", got.Observability.Admission)
	}
	if len(got.Observability.Admission.Criterion) != 2 {
		t.Fatalf("want 2 criteria, got %d", len(got.Observability.Admission.Criterion))
	}
	var onScope config.AdmissionCriterionConfig
	for _, c := range got.Observability.Admission.Criterion {
		if c.ID == "on_scope" {
			onScope = c
		}
	}
	if !strings.Contains(onScope.Definition, "booking app") {
		t.Errorf("purpose not seeded into on_scope definition: %q", onScope.Definition)
	}
	if issues, fatal := obsAdmissionLintCLI(got); fatal {
		t.Errorf("golden config lints fatal: %v", issues)
	}
}

// TestApplyAdmissionSetup_RemergesByID pins that re-adopting a template
// replaces its row rather than duplicating it.
func TestApplyAdmissionSetup_RemergesByID(t *testing.T) {
	cfg := config.Default()
	cfg.Observability.Enabled = true
	ch := admissionSetupChoices{AdoptTemplates: []string{"jailbreak"}, Mode: "observe"}
	cfg = applyAdmissionSetup(cfg, ch)
	cfg = applyAdmissionSetup(cfg, ch)
	n := 0
	for _, c := range cfg.Observability.Admission.Criterion {
		if c.ID == "jailbreak" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("jailbreak criterion count = %d, want 1 (merge by ID)", n)
	}
}

// TestRunBatchAdmissionSetup_DryRunThenWrite pins batch mode: without --yes it
// only prints; with --yes it writes a config that reloads + lints clean.
func TestRunBatchAdmissionSetup_DryRunThenWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	cfg := config.Default()
	cfg.Observability.Enabled = true
	ch := admissionSetupChoices{
		SetJudge:       true,
		JudgeBaseURL:   "http://127.0.0.1:11434/v1",
		JudgeModel:     "qwen2.5:7b",
		AdoptTemplates: []string{"on_scope"},
		Purpose:        "customer-support triage",
		Mode:           "observe",
	}

	var dry bytes.Buffer
	if err := runBatchAdmissionSetup(&dry, cfg, path, ch, false); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !strings.Contains(dry.String(), "dry run") {
		t.Errorf("dry-run output missing the dry-run notice: %q", dry.String())
	}
	if _, err := loadConfigForSetup(path); err != nil {
		t.Fatalf("load after dry run: %v", err)
	}
	// Dry run must NOT have written the file.
	if got, _ := loadConfigForSetup(path); got.Observability.Admission.Enabled {
		t.Errorf("dry run wrote the config")
	}

	var wrote bytes.Buffer
	if err := runBatchAdmissionSetup(&wrote, cfg, path, ch, true); err != nil {
		t.Fatalf("write run: %v", err)
	}
	reloaded, err := config.Load(config.LoadOptions{GlobalPath: path})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reloaded.Observability.Admission.Enabled || len(reloaded.Observability.Admission.Criterion) != 1 {
		t.Errorf("written config wrong: %+v", reloaded.Observability.Admission)
	}
	if issues, fatal := obsAdmissionLintCLI(reloaded); fatal {
		t.Errorf("written config lints fatal: %v", issues)
	}
}

// TestCollectAdmissionChoices_Scripted drives the interactive collector with a
// scripted stdin and asserts the resolved choices.
func TestCollectAdmissionChoices_Scripted(t *testing.T) {
	cfg := config.Default()
	cfg.Observability.Enabled = true // skip the precondition prompt
	script := strings.Join([]string{
		"y",                                    // configure the judge now?
		"1",                                    // hosting: local
		"",                                     // base_url: default
		"llama3.1:8b-instruct",                 // model
		"y",                                    // adopt on_scope
		"scheduling assistant for booking app", // purpose
		"n",                                    // adopt denied_topics
		"y",                                    // adopt jailbreak
		"n",                                    // start in enforce?
	}, "\n") + "\n"

	p := &setupPrompter{in: bufio.NewReader(strings.NewReader(script)), out: &bytes.Buffer{}}
	ch, err := collectAdmissionChoices(context.Background(), p, cfg)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if !ch.SetJudge || ch.JudgeModel != "llama3.1:8b-instruct" {
		t.Errorf("judge not collected: %+v", ch)
	}
	if ch.JudgeBaseURL == "" {
		t.Errorf("local base_url default not applied")
	}
	if !contains(ch.AdoptTemplates, "on_scope") || !contains(ch.AdoptTemplates, "jailbreak") {
		t.Errorf("adopted templates = %v, want on_scope + jailbreak", ch.AdoptTemplates)
	}
	if contains(ch.AdoptTemplates, "denied_topics") {
		t.Errorf("denied_topics adopted despite n")
	}
	if ch.Purpose == "" || ch.Mode != "observe" {
		t.Errorf("purpose/mode wrong: %+v", ch)
	}

	// The collected choices must apply into a clean-linting config.
	if _, fatal := obsAdmissionLintCLI(applyAdmissionSetup(cfg, ch)); fatal {
		t.Errorf("scripted choices produced a fatal-linting config")
	}
}
