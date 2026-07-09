package orgserver

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/orgserver/api"
	gen "github.com/marmutapp/superbased-observer/internal/orgserver/api/gen"
	"github.com/marmutapp/superbased-observer/internal/orgserver/auth"
	"github.com/marmutapp/superbased-observer/internal/orgserver/budget"
	"github.com/marmutapp/superbased-observer/internal/orgserver/ccanalytics"
	"github.com/marmutapp/superbased-observer/internal/orgserver/codexanalytics"
	"github.com/marmutapp/superbased-observer/internal/orgserver/config"
	"github.com/marmutapp/superbased-observer/internal/orgserver/copilotanalytics"
	"github.com/marmutapp/superbased-observer/internal/orgserver/dashboard"
	dashgen "github.com/marmutapp/superbased-observer/internal/orgserver/dashboard/gen"
	"github.com/marmutapp/superbased-observer/internal/orgserver/dashboard/webapp"
	orgdb "github.com/marmutapp/superbased-observer/internal/orgserver/db"
	"github.com/marmutapp/superbased-observer/internal/orgserver/obsalert"
	"github.com/marmutapp/superbased-observer/internal/orgserver/retention"
	"github.com/marmutapp/superbased-observer/internal/orgserver/rollup"
	"github.com/marmutapp/superbased-observer/internal/orgserver/scim"
)

// shutdownTimeout bounds graceful drain on SIGINT/SIGTERM.
const shutdownTimeout = 15 * time.Second

// Server is the assembled org server.
type Server struct {
	cfg          config.Config
	db           *sql.DB
	org          orgdb.Org
	logger       *slog.Logger
	handler      http.Handler
	evaluator    *budget.Evaluator
	obsAlertEval *obsalert.Evaluator
	// ccScheduler is the OPTIONAL Claude Code Analytics poller (native-console
	// Workstream C). Non-nil only when [cc_analytics].enabled and a key
	// resolved; runs alongside the HTTP server like the evaluator.
	ccScheduler *ccanalytics.Scheduler
	// codexScheduler is the OPTIONAL OpenAI Codex analytics poller (native-console
	// instance #2, Rail C). Non-nil only when [codex_analytics].enabled and a
	// poller built; runs alongside the HTTP server like ccScheduler.
	codexScheduler *codexanalytics.Scheduler
	// copilotSchedulers are the OPTIONAL GitHub Copilot analytics pollers
	// (native-console instance #3, Rail C) — ONE per configured surface
	// (engagement | seats | billing). Empty unless [copilot_analytics].enabled
	// and a token resolved; each runs alongside the HTTP server.
	copilotSchedulers []*copilotanalytics.Scheduler
	// retentionSweeper is the OPTIONAL content-retention prune (Phase 7). Non-nil
	// only when [dashboard.content_retention].otel_content_days > 0 (default OFF,
	// keep forever); runs alongside the HTTP server like the schedulers.
	retentionSweeper *retention.Sweeper
}

// New validates the config and performs all fallible setup: it opens the
// server DB, seeds the org singleton, loads the bearer/session keys and SCIM
// token, fetches the IdP metadata, and assembles the HTTP handler. The
// returned Server owns the DB handle and closes it in Run.
func New(ctx context.Context, cfg config.Config, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if err := config.Validate(cfg); err != nil {
		return nil, err
	}

	db, err := orgdb.Open(ctx, orgdb.Options{Path: cfg.Server.DBPath})
	if err != nil {
		return nil, err
	}
	org, err := orgdb.EnsureOrg(ctx, db, cfg.Server.ExternalURL)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	// Surface a one-time WARN if any pre-v1.8.0 leaked-content columns
	// still hold raw values. Operators can clean up via
	// `observer-org scrub-content --all --confirm`. The probe is a
	// single fast COUNT against one indexed column per table; missing
	// the warning is preferable to a startup hang on a corrupt DB so
	// we swallow errors.
	warnLeakedContent(ctx, db, logger)

	// Secrets — all from configured file paths, never embedded.
	signingKey, err := auth.LoadSigningKey(cfg.Bearer.SigningKeyPath)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	sessionKey, err := auth.LoadSessionKey(cfg.Server.SessionKeyPath)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	scimToken, err := loadSCIMToken(cfg.SCIM.AuthTokenPath)
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	issuer, err := auth.NewIssuer(signingKey, cfg.Server.ExternalURL,
		time.Duration(cfg.Bearer.DefaultLifetimeDays)*24*time.Hour)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	secure := strings.HasPrefix(strings.ToLower(cfg.Server.ExternalURL), "https://")
	sessions, err := auth.NewSessionManager(sessionKey, auth.DefaultSessionTTL, secure)
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	handlers := api.New(db, issuer, org,
		time.Duration(cfg.Enrolment.DefaultTokenLifetimeDays)*24*time.Hour, logger)

	// Org policy channel (guard spec §14.2): when a policy signing key
	// is configured, deliver its public half at enrolment so agents
	// pin it. Configured-but-unreadable is a hard config error (the
	// bearer-key posture); absent config means the channel is off and
	// agents see 404 on the bundle endpoint.
	if cfg.Policy.SigningKeyPath != "" {
		policyKey, err := auth.LoadSigningKey(cfg.Policy.SigningKeyPath)
		if err != nil {
			_ = db.Close()
			return nil, err
		}
		handlers.SetOrgPolicyPublicKey(auth.EncodePublicKey(policyKey.Public().(ed25519.PublicKey)))
	}

	saml, err := auth.NewSAML(ctx, auth.SAMLOptions{
		ExternalURL:    cfg.Server.ExternalURL,
		EntityID:       cfg.SAML.SPEntityID,
		SPCertPath:     cfg.SAML.SPCertPath,
		SPKeyPath:      cfg.SAML.SPKeyPath,
		IDPMetadataURL: cfg.SAML.IDPMetadataURL,
		Mapping: auth.AttributeMapping{
			Email:       cfg.SAML.AttributeMapping["email"],
			DisplayName: cfg.SAML.AttributeMapping["display_name"],
			Groups:      cfg.SAML.AttributeMapping["groups"],
		},
	}, sessions, handlers, logger)
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	scimHandler, err := scim.NewHandler(db, cfg.Server.ExternalURL)
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	rollupCache := rollup.NewCache(0)
	// Loud startup warning when [dashboard].admin_emails is unset — a
	// silently-empty allow-list means every dashboard request 403s on
	// admin-only routes and aggregates over an empty scope (HTTP 200
	// but all zeros). N6 in docs/teams-test-regression-v1.8.2-2026-06-04.md
	// traced an org bring-up to a misplaced TOML section header where
	// the key landed under [org] instead of [dashboard]; making the
	// empty case loud here turns that class of misconfig into a
	// boot-time signal instead of a silent dashboard-zeros mystery.
	if len(cfg.Dashboard.AdminEmails) == 0 {
		logger.Warn("orgserver: [dashboard].admin_emails is empty — no SAML user can see whole-org aggregates; every /api/org/* admin route will 403, every aggregate will be zero. Did the key land under the wrong section?")
	}
	// Dashboard policy publishing (guard spec §14.5, G14): when the policy
	// channel is configured, a policy_admin's publish loads the signing key
	// from disk PER REQUEST through this closure — the long-running process
	// never retains the private half, and on-disk rotation takes effect
	// without a restart (the documented G14 key-exposure design call; see
	// config.PolicyConfig). No key path → nil signer → dashboard publish 409s
	// and the authoring panel renders read-only, exactly the G13 posture.
	var policySigner dashboard.PolicySigner
	if cfg.Policy.SigningKeyPath != "" {
		keyPath := cfg.Policy.SigningKeyPath
		policySigner = func() (ed25519.PrivateKey, error) { return auth.LoadSigningKey(keyPath) }
	}
	orgAPI := dashboard.NewAPI(db, rollupCache, dashboard.Options{
		AdminEmails:          cfg.Dashboard.AdminEmails,
		PolicyAdminEmails:    cfg.Dashboard.PolicyAdminEmails,
		SecurityViewerEmails: cfg.Dashboard.SecurityViewerEmails,
		PolicySigner:         policySigner,
	}, logger)

	handler := buildMux(buildDeps{
		handlers:    handlers,
		saml:        saml,
		scimHandler: scimHandler,
		orgAPI:      orgAPI,
		sessions:    sessions,
		scimToken:   scimToken,
		logger:      logger,
		db:          db,
		adminEmails: cfg.Dashboard.AdminEmails,
		devAuth:     cfg.Server.DevAuth,
	})

	evaluator := budget.NewEvaluator(db, org,
		time.Duration(cfg.Dashboard.BudgetPollSeconds)*time.Second, logger)

	// Obs-alert evaluator (obs-org-tier OP6b): polls obs_alert_rules and fires
	// webhooks on trajectory error-rate / cost / latency threshold crossings.
	// Always constructed (it no-ops when no rules exist), runs alongside the
	// budget evaluator.
	obsAlertEval := obsalert.NewEvaluator(db, org,
		time.Duration(cfg.Dashboard.BudgetPollSeconds)*time.Second, logger)

	// CC Analytics poller (native-console Workstream C) — built only when
	// enabled AND a key resolves; a key-resolution failure is WARN-only so a
	// misconfigured analytics block never blocks server startup.
	var ccScheduler *ccanalytics.Scheduler
	if cfg.CCAnalytics.Enabled {
		key, kerr := ccanalytics.ResolveKey(cfg.CCAnalytics.APIKeyFile)
		if kerr != nil {
			logger.Warn("cc-analytics disabled: no API key", "err", kerr)
		} else if _, verr := ccanalytics.ValidateKind(cfg.CCAnalytics.ApiKind); verr != nil {
			logger.Warn("cc-analytics disabled: bad api_kind", "err", verr)
		} else {
			poller := &ccanalytics.Poller{
				DB: db, BaseURL: cfg.CCAnalytics.BaseURL, APIKey: key, OrgID: org.OrgID,
			}
			ccScheduler = ccanalytics.NewScheduler(poller,
				cfg.CCAnalytics.PollIntervalHours, cfg.CCAnalytics.LagToleranceHours, logger)
		}
	}

	// Codex Analytics poller (native-console instance #2, Rail C) — same posture
	// as CC: built only when enabled AND a key resolves AND the poller
	// constructs; any failure is WARN-only so it never blocks startup.
	var codexScheduler *codexanalytics.Scheduler
	if cfg.CodexAnalytics.Enabled {
		key, kerr := codexanalytics.ResolveKey(cfg.CodexAnalytics.APIKeyFile)
		if kerr != nil {
			logger.Warn("codex-analytics disabled: no API key", "err", kerr)
		} else if poller, perr := codexanalytics.NewPoller(db, cfg.CodexAnalytics.Surface,
			cfg.CodexAnalytics.BaseURL, key, cfg.CodexAnalytics.WorkspaceID, org.OrgID); perr != nil {
			logger.Warn("codex-analytics disabled: bad config", "err", perr)
		} else {
			codexScheduler = codexanalytics.NewScheduler(poller,
				cfg.CodexAnalytics.PollIntervalHours, cfg.CodexAnalytics.LagToleranceHours, logger)
		}
	}

	// Copilot Analytics pollers (native-console instance #3, Rail C) — same
	// posture as CC/Codex, but ONE scheduler per configured surface. A bad
	// surface or a missing owner is WARN-only and skips that surface; the others
	// still run, and nothing blocks startup.
	var copilotSchedulers []*copilotanalytics.Scheduler
	if cfg.CopilotAnalytics.Enabled {
		key, kerr := copilotanalytics.ResolveKey(cfg.CopilotAnalytics.APIKeyFile)
		if kerr != nil {
			logger.Warn("copilot-analytics disabled: no GitHub token", "err", kerr)
		} else {
			for _, surface := range cfg.CopilotAnalytics.Surfaces {
				poller, perr := copilotanalytics.NewPoller(db, surface, cfg.CopilotAnalytics.BaseURL,
					key, cfg.CopilotAnalytics.Owner, copilotanalytics.OwnerType(cfg.CopilotAnalytics.OwnerType),
					cfg.CopilotAnalytics.Report, org.OrgID)
				if perr != nil {
					logger.Warn("copilot-analytics surface skipped", "surface", surface, "err", perr)
					continue
				}
				copilotSchedulers = append(copilotSchedulers, copilotanalytics.NewScheduler(poller,
					cfg.CopilotAnalytics.PollIntervalHours, cfg.CopilotAnalytics.LagToleranceHours, logger))
			}
		}
	}

	// Content-retention sweeper (Phase 7) — nil unless the admin set a positive
	// horizon (default OFF, keep forever). NewSweeper returns nil for ≤0.
	retentionSweeper := retention.NewSweeper(db, cfg.Dashboard.ContentRetention.OTelContentDays, logger)

	return &Server{cfg: cfg, db: db, org: org, logger: logger, handler: handler, evaluator: evaluator, obsAlertEval: obsAlertEval, ccScheduler: ccScheduler, codexScheduler: codexScheduler, copilotSchedulers: copilotSchedulers, retentionSweeper: retentionSweeper}, nil
}

// warnLeakedContent surfaces a single WARN line at startup for each
// content-bearing column that still holds raw values from a pre-v1.8.0
// push. Operators clean up via `observer-org scrub-content --all
// --confirm` (which preserves the *_hash counterparts so rollups
// continue to work). Errors in the probe are swallowed so a corrupt or
// in-flight DB doesn't fail startup; the warning is advisory.
func warnLeakedContent(ctx context.Context, db *sql.DB, logger *slog.Logger) {
	checks := []struct {
		table  string
		column string
	}{
		{"actions", "target"},
		{"actions", "source_file"},
		{"sessions", "project_root"},
		{"sessions", "git_remote"},
		{"api_turns", "project_root"},
		{"token_usage", "project_root"},
		{"token_usage", "source_file"},
	}
	for _, c := range checks {
		var n int64
		// Table/column names are in-package literals; gosec G201 is a false positive.
		q := fmt.Sprintf( //nolint:gosec // G201: in-package allowlist of (table, column) tuples
			`SELECT COUNT(*) FROM %s WHERE %s IS NOT NULL AND %s != ''`, c.table, c.column, c.column,
		)
		if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
			continue
		}
		if n == 0 {
			continue
		}
		logger.Warn("orgserver: pre-v1.8.0 content found in store; consider `observer-org scrub-content`",
			"table", c.table, "column", c.column, "rows", n)
	}
}

// Handler returns the assembled HTTP handler (for in-process E2E tests).
func (s *Server) Handler() http.Handler { return s.handler }

// Close releases the server's owned resources — currently the DB handle.
// Run already closes the DB on return, so Close is for callers that drive
// the server through Handler() (in-process E2E tests) and never call Run;
// without it the open DB handle blocks temp-dir cleanup on Windows. Safe to
// call once; a redundant close of the underlying *sql.DB returns an error
// that the caller may ignore.
func (s *Server) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Org returns the org identity this server represents.
func (s *Server) Org() orgdb.Org { return s.org }

// Run serves until ctx is cancelled, then drains gracefully. It closes the
// DB on return.
func (s *Server) Run(ctx context.Context) error {
	defer func() { _ = s.db.Close() }()

	srv := &http.Server{
		Addr:              s.cfg.Server.Listen,
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Budget evaluator runs alongside the HTTP server; it stops when ctx is
	// cancelled (and drains in-flight webhook deliveries before returning).
	evalDone := make(chan struct{})
	go func() {
		defer close(evalDone)
		s.evaluator.Run(ctx)
	}()

	// Obs-alert evaluator runs alongside the budget evaluator; stops on ctx cancel.
	if s.obsAlertEval != nil {
		go s.obsAlertEval.Run(ctx)
	}

	// CC Analytics poller (optional) runs alongside; stops on ctx cancel.
	if s.ccScheduler != nil {
		go s.ccScheduler.Run(ctx)
	}

	// Codex Analytics poller (optional) runs alongside; stops on ctx cancel.
	if s.codexScheduler != nil {
		go s.codexScheduler.Run(ctx)
	}

	// Copilot Analytics pollers (optional; one per surface) run alongside; each
	// stops on ctx cancel.
	for _, sched := range s.copilotSchedulers {
		go sched.Run(ctx)
	}

	// Content-retention sweeper (optional) runs alongside; stops on ctx cancel.
	if s.retentionSweeper != nil {
		go s.retentionSweeper.Run(ctx)
	}

	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("org server listening", "addr", s.cfg.Server.Listen, "org", s.org.OrgName)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		s.logger.Info("org server shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		err := srv.Shutdown(shutCtx)
		<-evalDone // wait for the evaluator to drain deliveries
		return err
	case err := <-errCh:
		return fmt.Errorf("orgserver.Run: %w", err)
	}
}

type buildDeps struct {
	handlers    *api.Handlers
	saml        *auth.SAML
	scimHandler http.Handler
	orgAPI      *dashboard.API
	sessions    *auth.SessionManager
	scimToken   string
	logger      *slog.Logger
	db          *sql.DB  // for /readyz
	adminEmails []string // [dashboard].admin_emails — gates privileged /api/org/* routes
	devAuth     bool     // M3.2: enables /auth/dev/login (compose-only convenience)
}

// buildMux assembles the route table and wraps it with global middleware.
// adminCheckerFor builds an auth.AdminChecker closure that resolves a session
// user id to its SCIM-provisioned email and reports whether that email is in
// the [dashboard].admin_emails allow-list. This mirrors
// dashboard.API.resolveScope's admin path so the standalone /api/org/* routes
// enforce the same authority as the generated dashboard mutation handlers,
// without threading database/sql into the auth package.
func adminCheckerFor(db *sql.DB, adminEmails []string) auth.AdminChecker {
	allow := make(map[string]bool, len(adminEmails))
	for _, e := range adminEmails {
		allow[strings.ToLower(strings.TrimSpace(e))] = true
	}
	return func(ctx context.Context, userID string) (bool, error) {
		if userID == "" {
			return false, nil
		}
		var email string
		err := db.QueryRowContext(ctx, `SELECT email FROM org_members WHERE user_id = ?`, userID).Scan(&email)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return allow[strings.ToLower(email)], nil
	}
}

func buildMux(d buildDeps) http.Handler {
	mux := http.NewServeMux()

	// Liveness + readiness probes. /healthz returns 200 whenever the
	// process is up and serving HTTP (used by k8s/compose liveness
	// checks). /readyz reports application-level readiness: DB
	// reachable + migrations applied + SAML metadata loaded. Both are
	// public — they expose no secrets, only run-state. /healthz also
	// reports `dev_auth: true` so a monitor catches a server
	// accidentally started with the dev bypass enabled.
	devAuthFlag := "false"
	if d.devAuth {
		devAuthFlag = "true"
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","dev_auth":` + devAuthFlag + `}`))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var schemaVer int
		if err := d.db.QueryRowContext(r.Context(), `SELECT value FROM schema_meta WHERE key='version'`).Scan(&schemaVer); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"not-ready","reason":"db-unreachable"}`))
			return
		}
		_, _ = w.Write([]byte(fmt.Sprintf(`{"status":"ok","schema_version":%d}`, schemaVer)))
	})

	// Dev-auth bypass (M3.2). Issues a session for any provisioned
	// SCIM user without going through SAML — for local evaluation
	// only. Disabled by default; enabled by [server].dev_auth=true.
	// The server logs a WARN at startup when on; /healthz reports
	// dev_auth:true so monitoring catches a misconfigured production
	// instance.
	if d.devAuth {
		d.logger.Warn("orgserver: DEV-AUTH ENABLED — /auth/dev/login bypasses SAML; do NOT use this in production")
		mux.HandleFunc("POST /auth/dev/login", func(w http.ResponseWriter, r *http.Request) {
			email := strings.TrimSpace(r.FormValue("email"))
			if email == "" {
				http.Error(w, "missing email", http.StatusBadRequest)
				return
			}
			var userID string
			if err := d.db.QueryRowContext(r.Context(),
				`SELECT user_id FROM org_members WHERE email = ? AND active = 1`,
				email).Scan(&userID); err != nil {
				http.Error(w, "no such active user", http.StatusUnauthorized)
				return
			}
			if err := d.sessions.Issue(w, userID); err != nil {
				http.Error(w, "session issue failed", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(fmt.Sprintf(`{"status":"ok","user_id":%q,"email":%q}`, userID, email)))
		})
	}

	// Agent protocol via the generated registrar. The scope-aware security
	// middleware enforces the bearer only on operations the spec marks with
	// bearerAuth (push), leaving enroll public.
	gen.HandlerWithOptions(d.handlers, gen.StdHTTPServerOptions{
		BaseRouter:  mux,
		Middlewares: []gen.MiddlewareFunc{bearerSecurity(d.handlers)},
	})

	// SAML SP endpoints (public by protocol).
	mux.HandleFunc("GET /saml/metadata", d.saml.Metadata)
	mux.HandleFunc("POST /saml/acs", d.saml.ACS)
	mux.HandleFunc("GET /saml/sso", d.saml.SSO)
	mux.HandleFunc("GET /saml/slo", d.saml.SLO)

	// Admin gate for privileged /api/org/* mutations + exports. These were
	// previously behind RequireSAMLSession (session-presence only), which
	// admitted any SSO-capable member: SAML JIT-creates a member on first
	// login, so a non-admin could mint enrolment tokens for arbitrary users
	// (ingest impersonation), publish a fleet-wide routing policy, or export
	// every developer's email + spend. requireAdminSAML now also requires the
	// session user's email to be in [dashboard].admin_emails (403 otherwise),
	// matching the dashboard mutation surface (dashboard.API.requireAdmin).
	requireAdminSAML := auth.RequireAdminSAML(
		d.sessions, adminCheckerFor(d.db, d.adminEmails),
		auth.JSONUnauthorized(), auth.JSONForbidden(),
	)
	mux.Handle("POST /api/org/enrolment-tokens", requireAdminSAML(http.HandlerFunc(d.handlers.MintEnrolmentToken)))

	// Model-routing org policy registry (model-routing spec §R19.1/
	// §R19.2): admin publish behind the SAML session (RBAC: the same
	// admin surface that mints enrolment tokens; every publish lands
	// an audit row); agent fetch behind the enrolment bearer. The
	// served body NEVER carries an effective enforce switch — the
	// agent composer structurally ignores enabled/mode keys (§R23: no
	// remote enforce toggle exists).
	mux.Handle("POST /api/org/routing-policy", requireAdminSAML(routingPolicyPublishHandler(d.db)))
	mux.Handle("GET /api/agent/routing-policy", auth.RequireBearer(d.handlers)(routingPolicyGetHandler(d.db)))
	// §R19.5 org rollup audit export (CSV/JSON) — admin surface.
	mux.Handle("GET /api/org/routing-summaries/export", requireAdminSAML(routingSummariesExportHandler(d.db)))
	// T5 per-end-user spend rollup (org-budget guardrails plan §2.1) — admin
	// surface, JSON only (manual route, no OpenAPI codegen).
	mux.Handle("GET /api/org/obs/enduser-spend", requireAdminSAML(obsEndUserSpendHandler(d.db)))

	// Org dashboard data endpoints (/api/org/*) via the generated registrar.
	// The scope-aware samlSecurity middleware enforces the SAML session on the
	// dashboard-tagged operations (all of them); role scoping is per-handler.
	dashgen.HandlerWithOptions(d.orgAPI, dashgen.StdHTTPServerOptions{
		BaseRouter:  mux,
		Middlewares: []dashgen.MiddlewareFunc{samlSecurity(d.sessions)},
	})

	// SCIM provisioning (static token).
	mux.Handle("/scim/", auth.RequireSCIMToken(d.scimToken)(d.scimHandler))

	// Org dashboard SPA (web2/) at root, behind the SAML session. Registered
	// methodless ("/") rather than "GET /" so the methodless "/scim/" prefix is
	// unambiguously more specific (Go 1.22 ServeMux rejects "GET /" vs "/scim/"
	// as a conflict — neither dominates). The more-specific /api/*, /saml/*,
	// /scim/* routes still win; everything else (the SPA shell, /assets/*, and
	// client-side routes) falls through here. A browser without a session is
	// redirected to SSO before the app loads.
	requireSAMLWeb := auth.RequireSAMLSession(d.sessions, auth.RedirectToSSO("/saml/sso"))
	mux.Handle("/", requireSAMLWeb(webapp.Handler()))

	// Global middleware: request id (outermost) → logging → rate limit → mux.
	var h http.Handler = mux
	h = api.RateLimit(50, 100)(h)
	h = api.Logging(d.logger)(h)
	h = api.RequestID()(h)
	return h
}

// bearerSecurity returns a gen middleware that enforces the Ed25519 bearer
// only on operations carrying the generated BearerAuthScopes marker (i.e.
// push). For unmarked operations (enroll) it is a pass-through. This is the
// oapi-codegen security-middleware pattern: the generated per-operation
// wrapper sets the scope marker in the request context before the middleware
// runs.
func bearerSecurity(v auth.BearerVerifier) gen.MiddlewareFunc {
	enforce := auth.RequireBearer(v)
	return func(next http.Handler) http.Handler {
		protected := enforce(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := r.Context().Value(gen.BearerAuthScopes).([]string); ok {
				protected.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// samlSecurity returns a dashboard-gen middleware that enforces the SAML
// session only on operations carrying the generated SamlSessionScopes marker
// (i.e. every /api/org/* dashboard op). It mirrors bearerSecurity: the
// generated per-operation wrapper sets the scope marker in the request context
// before this middleware runs, and a missing session yields a JSON 401.
func samlSecurity(sessions *auth.SessionManager) dashgen.MiddlewareFunc {
	enforce := auth.RequireSAMLSession(sessions, auth.JSONUnauthorized())
	return func(next http.Handler) http.Handler {
		protected := enforce(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := r.Context().Value(dashgen.SamlSessionScopes).([]string); ok {
				protected.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// loadSCIMToken reads the static SCIM token from a file, trimming trailing
// whitespace/newline. An empty token is rejected.
func loadSCIMToken(path string) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("orgserver: read SCIM token %s: %w", path, err)
	}
	tok := strings.TrimSpace(string(body))
	if tok == "" {
		return "", fmt.Errorf("orgserver: SCIM token file %s is empty", path)
	}
	return tok, nil
}
