package dashboard

// terminal_limits.go — the dashboard-editable [terminal] runtime bounds verb:
// POST /api/terminal/limits flips [terminal].max_concurrent and/or
// [terminal].idle_timeout and LIVE-applies them to the running PTY manager with
// no restart. It mirrors handleRemoteSetRevokeStandingOnTakeover exactly
// (requireConfirmToken → strict POINTER decode → remoteManageMu load-modify-write
// → Validate → WriteToml → recordManageAudit), then adds the live-apply hop.
//
// Unlike the launch policy ([terminal.launch], captured into termsvc at start
// and honestly "restart required"), these two knobs are plain runtime fields the
// Manager reads under its own mutex on every Create / reap tick — so a
// SetLimits call takes effect immediately. The verb type-asserts the injected
// LaunchManager to a SMALL OPTIONAL interface for the live-apply; it never
// widens the LaunchManager seam (dozens of test fakes implement it). When the
// adapter is absent (a test server, or a read-only dashboard with no launcher),
// the write still persists and the verb reports restart_required:true so the
// operator knows the flip binds on the next start.

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
)

// terminalLimitsSetter is the OPTIONAL live-apply interface the verb asserts on
// the dashboard's LaunchManager. The cmd adapter (launchManagerAdapter) supplies
// it; a bare test fake does not — a failed assertion degrades to
// restart_required:true, never a panic. Kept narrow so it does not widen the
// LaunchManager contract every fake must satisfy.
type terminalLimitsSetter interface {
	SetTerminalLimits(maxConcurrent int, idleTimeout time.Duration)
}

// parseIdleTimeout maps a persisted [terminal].idle_timeout string onto the
// live duration the SAME way cmd's applyTerminalBounds does: ""/"0" (and any
// non-positive parse) → 0 = idle reaping DISABLED, a positive Go duration opts
// in. The string was already validated by config.Validate before persist, so a
// parse failure here is only a defensive fall-back to 0.
func parseIdleTimeout(s string) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0
	}
	return d
}

// handleTerminalLimits — POST /api/terminal/limits (Local). Flips
// [terminal].max_concurrent and/or [terminal].idle_timeout and live-applies
// them. Strict pointer decode: each field is optional (nil = leave that key
// unchanged) but at least one must be present — both nil is a 400 so a
// silently-defaulted zero can never rewrite a limit the operator did not touch.
// Persist happens first (config is the source of truth); the live-apply is a
// best-effort hop that flips restart_required only when the launcher seam is
// absent.
func (s *Server) handleTerminalLimits(w http.ResponseWriter, r *http.Request) {
	if !requireConfirmToken(w, r) {
		return
	}
	var body struct {
		MaxConcurrent *int    `json:"max_concurrent"`
		IdleTimeout   *string `json:"idle_timeout"`
	}
	if err := decodeJSONBody(r, &body); err != nil || (body.MaxConcurrent == nil && body.IdleTimeout == nil) {
		http.Error(w, `{"error":"body must be JSON with at least one of max_concurrent (int) or idle_timeout (string)"}`, http.StatusBadRequest)
		return
	}

	remoteManageMu.Lock()
	defer remoteManageMu.Unlock()
	cfg, cfgPath, err := s.loadConfigForManage()
	if err != nil {
		writeErr(w, err)
		return
	}
	if body.MaxConcurrent != nil {
		cfg.Terminal.MaxConcurrent = *body.MaxConcurrent
	}
	if body.IdleTimeout != nil {
		cfg.Terminal.IdleTimeout = strings.TrimSpace(*body.IdleTimeout)
	}
	if err := config.Validate(cfg); err != nil {
		writeErr(w, err)
		return
	}
	if err := config.WriteToml(cfgPath, cfg); err != nil {
		writeErr(w, err)
		return
	}

	// Live-apply from the PERSISTED values (config is the source of truth). The
	// setter is optional: a launcher-less/read-only dashboard or a test server
	// has no adapter, so the flip binds on the next start and we say so.
	restartRequired := true
	if setter, ok := s.opts.LaunchManager.(terminalLimitsSetter); ok {
		setter.SetTerminalLimits(cfg.Terminal.MaxConcurrent, parseIdleTimeout(cfg.Terminal.IdleTimeout))
		restartRequired = false
	}

	detail := "max_concurrent=" + strconv.Itoa(cfg.Terminal.MaxConcurrent) + " idle_timeout=" + quoteOrZero(cfg.Terminal.IdleTimeout)
	s.recordManageAudit(r, "terminal_limits", detail)
	writeJSON(w, map[string]any{
		"ok":               true,
		"restart_required": restartRequired,
		"max_concurrent":   cfg.Terminal.MaxConcurrent,
		"idle_timeout":     cfg.Terminal.IdleTimeout,
	})
}

// quoteOrZero renders an idle_timeout string for the metadata-only audit detail,
// mapping the empty (disabled) case to a stable "0" token.
func quoteOrZero(s string) string {
	if strings.TrimSpace(s) == "" {
		return "0"
	}
	return s
}
