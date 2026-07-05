package dashboard

import (
	"net/http"
	"os"
	"time"

	"github.com/marmutapp/superbased-observer/internal/diag"
)

// GET /api/health/doctor (usability arc P4.8 / review row D1) — the
// `observer doctor` checks as JSON. Same diag.Run, same checks
// (schema, DB integrity, sizes, adapter paths, hook checksums +
// binary, MCP registrations, pidbridge, concurrent daemons, codex
// trust, org enrolment); the Details lines carry the remediation
// hints the CLI prints. Read-only and on-demand — the panel runs it
// on open and on the Re-run button, never on a poll loop (the DB
// integrity check is not free on a large observer.db).
func (s *Server) handleHealthDoctor(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	cfg, err := loadConfigForDashboard(s.opts.ConfigPath)
	if err != nil {
		writeErr(w, err)
		return
	}
	exe, _ := os.Executable()
	report := diag.Run(r.Context(), diag.DoctorOptions{
		Config: cfg,
		// s.opts.DB, not s.db(): doctor diagnoses the real install
		// even while demo mode is active (P6.7).
		DB:         s.opts.DB,
		BinaryPath: exe,
		HomeDir:    setupWizardHome, // "" in production; test sandbox override
	})
	type wireCheck struct {
		Name    string   `json:"name"`
		Status  string   `json:"status"`
		Message string   `json:"message"`
		Details []string `json:"details,omitempty"`
	}
	checks := make([]wireCheck, 0, len(report.Checks))
	for _, c := range report.Checks {
		checks = append(checks, wireCheck{
			Name:    c.Name,
			Status:  c.Status.String(),
			Message: c.Message,
			Details: c.Details,
		})
	}
	ok, warn, fail := report.Counts()
	writeJSON(w, map[string]any{
		"checks":       checks,
		"ok":           ok,
		"warn":         warn,
		"fail":         fail,
		"all_ok":       fail == 0 && warn == 0,
		"generated_at": time.Now().UTC().Format(time.RFC3339),
	})
}
