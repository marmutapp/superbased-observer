package orgserver

import (
	"context"
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
	"github.com/marmutapp/superbased-observer/internal/orgserver/config"
	"github.com/marmutapp/superbased-observer/internal/orgserver/dashboard"
	dashgen "github.com/marmutapp/superbased-observer/internal/orgserver/dashboard/gen"
	"github.com/marmutapp/superbased-observer/internal/orgserver/dashboard/webapp"
	orgdb "github.com/marmutapp/superbased-observer/internal/orgserver/db"
	"github.com/marmutapp/superbased-observer/internal/orgserver/rollup"
	"github.com/marmutapp/superbased-observer/internal/orgserver/scim"
)

// shutdownTimeout bounds graceful drain on SIGINT/SIGTERM.
const shutdownTimeout = 15 * time.Second

// Server is the assembled org server.
type Server struct {
	cfg       config.Config
	db        *sql.DB
	org       orgdb.Org
	logger    *slog.Logger
	handler   http.Handler
	evaluator *budget.Evaluator
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
	orgAPI := dashboard.NewAPI(db, rollupCache, cfg.Dashboard.AdminEmails, logger)

	handler := buildMux(buildDeps{
		handlers:    handlers,
		saml:        saml,
		scimHandler: scimHandler,
		orgAPI:      orgAPI,
		sessions:    sessions,
		scimToken:   scimToken,
		logger:      logger,
		db:          db,
		devAuth:     cfg.Server.DevAuth,
	})

	evaluator := budget.NewEvaluator(db, org,
		time.Duration(cfg.Dashboard.BudgetPollSeconds)*time.Second, logger)

	return &Server{cfg: cfg, db: db, org: org, logger: logger, handler: handler, evaluator: evaluator}, nil
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
	db          *sql.DB // for /readyz
	devAuth     bool    // M3.2: enables /auth/dev/login (compose-only convenience)
}

// buildMux assembles the route table and wraps it with global middleware.
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

	// Admin enrolment-token mint (SAML session, JSON 401 on no session).
	requireSAMLAPI := auth.RequireSAMLSession(d.sessions, auth.JSONUnauthorized())
	mux.Handle("POST /api/org/enrolment-tokens", requireSAMLAPI(http.HandlerFunc(d.handlers.MintEnrolmentToken)))

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
