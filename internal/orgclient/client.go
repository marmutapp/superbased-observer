package orgclient

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/orgclient/gen"
	"github.com/marmutapp/superbased-observer/internal/orgcontract"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// Backoff bounds for the push loop on retryable failures (spec §2.4.2):
// exponential 250ms→30s with ±25% jitter, reset to the floor after a success.
const (
	initialBackoff = 250 * time.Millisecond
	maxBackoff     = 30 * time.Second
	backoffFactor  = 2
	jitterFraction = 0.25
)

// ErrNotEnrolled is returned by push operations when the agent is not enrolled
// (no org_enrolment row, or the bearer/signing key is missing). It is never a
// retryable error — the caller waits for an enrol rather than backing off.
var ErrNotEnrolled = errors.New("orgclient: not enrolled")

// ErrAuthFailed is returned when the server rejects the bearer or the per-push
// signature (401/403). The push loop treats it as terminal: it stops pushing
// and surfaces the failure (dashboard + org_push_log) rather than retrying a
// credential the server has rejected.
var ErrAuthFailed = errors.New("orgclient: authentication failed")

// Client runs the agent side of the Teams enrolment + push protocol: it
// enrols (binding a fresh Ed25519 keypair), reads content-free rollup rows
// above the local push cursor, signs and ships them to the org server, and
// advances the cursor on acceptance. Nothing here runs unless the agent is
// both configured ([org_client] enabled) and enrolled; see package doc.
type Client struct {
	cfg          config.OrgClientConfig
	store        *store.Store
	bearers      BearerStore
	httpClient   *http.Client
	logger       *slog.Logger
	agentVersion string
}

// New constructs a push Client. httpClient may be nil (a default with a sane
// timeout is used); logger may be nil (slog.Default). agentVersion is stamped
// into each push envelope for server-side diagnostics.
func New(cfg config.OrgClientConfig, st *store.Store, bearers BearerStore, agentVersion string, httpClient *http.Client, logger *slog.Logger) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		cfg:          cfg,
		store:        st,
		bearers:      bearers,
		httpClient:   httpClient,
		logger:       logger,
		agentVersion: agentVersion,
	}
}

// EnrolmentState is a read-only snapshot of the agent's enrolment for the CLI
// and dashboard. LastPush is nil when the agent has never pushed.
type EnrolmentState struct {
	Enrolled     bool
	OrgID        string
	OrgName      string
	OrgServerURL string
	UserID       string
	UserEmail    string
	EnrolledAt   string
	Backend      string // bearer-store backend: "keychain" | "file"
	LastPush     *store.PushLogEntry
}

// PushResult summarises one push attempt for the CLI / loop.
type PushResult struct {
	Empty        bool  // nothing above the cursor; no network call was made
	RowCount     int   // rows in the batch
	Bytes        int   // gzip wire bytes shipped
	AcceptedRows int64 // server-reported newly-stored rows
	DedupedRows  int64 // server-reported already-present rows
}

// Enroll exchanges a one-time compound token for a long-lived bearer. It
// generates a fresh Ed25519 keypair, posts the public half, and on success
// persists the bearer + private key (keychain), seeds the push cursor from the
// current high-water ids (so only post-enrolment activity is ever shared), and
// writes the org_enrolment row. The keychain and cursor are written BEFORE the
// enrolment row so that a concurrently-running push loop, which keys off the
// enrolment row, never observes an enrolled state with an un-seeded cursor.
func (c *Client) Enroll(ctx context.Context, orgURL, token string) (*store.Enrolment, error) {
	orgURL = strings.TrimRight(strings.TrimSpace(orgURL), "/")
	if orgURL == "" {
		return nil, errors.New("orgclient.Enroll: org server URL is required")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("orgclient.Enroll: enrolment token is required")
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("orgclient.Enroll: keygen: %w", err)
	}

	gc, err := c.genClient(orgURL)
	if err != nil {
		return nil, fmt.Errorf("orgclient.Enroll: %w", err)
	}
	resp, err := gc.EnrollAgentWithResponse(ctx, gen.EnrollRequest{
		OneTimeToken:   token,
		AgentPublicKey: base64.RawURLEncoding.EncodeToString(pub),
	})
	if err != nil {
		return nil, fmt.Errorf("orgclient.Enroll: post: %w", err)
	}
	switch resp.StatusCode() {
	case http.StatusOK:
		// fall through
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("orgclient.Enroll: %w: invalid or expired enrolment token", ErrAuthFailed)
	default:
		return nil, fmt.Errorf("orgclient.Enroll: server returned %d: %s", resp.StatusCode(), strings.TrimSpace(string(resp.Body)))
	}
	er := resp.JSON200
	if er == nil || er.Bearer == "" {
		return nil, errors.New("orgclient.Enroll: server returned no bearer")
	}

	// Seed the cursor + persist secrets BEFORE the enrolment row (see doc).
	maxIDs, err := c.store.CurrentMaxIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("orgclient.Enroll: seed cursor: %w", err)
	}
	if err := c.store.SavePushCursor(ctx, maxIDs); err != nil {
		return nil, fmt.Errorf("orgclient.Enroll: save cursor: %w", err)
	}
	// Wipe any prior-run last-push state so `observer org status` after
	// a re-enroll reports "(none yet)" instead of a stale timestamp.
	// N5 in docs/teams-test-regression-2026-06-03.md.
	if err := c.store.ClearLastPushState(ctx); err != nil {
		return nil, fmt.Errorf("orgclient.Enroll: clear last-push state: %w", err)
	}
	if err := c.bearers.SaveAgentKey(priv); err != nil {
		return nil, fmt.Errorf("orgclient.Enroll: %w", err)
	}
	if err := c.bearers.SaveBearer(er.Bearer); err != nil {
		return nil, fmt.Errorf("orgclient.Enroll: %w", err)
	}

	enr := store.Enrolment{
		OrgID:        er.OrgID,
		OrgName:      er.OrgName,
		OrgServerURL: orgURL,
		UserID:       er.UserID,
		UserEmail:    er.UserEmail,
		EnrolledAt:   time.Now().UTC().Format(time.RFC3339),
		BearerKeyID:  c.cfg.KeychainID,
	}
	if err := c.store.WriteEnrolment(ctx, enr); err != nil {
		return nil, fmt.Errorf("orgclient.Enroll: write enrolment: %w", err)
	}

	// Pin the org POLICY signing key when the server delivered one
	// (guard spec §14.2 — the pin every fetched bundle's embedded key
	// is checked against; guard_policy_state is the pin home, append-
	// only so rotations stay auditable and a re-enrol records the new
	// key). A pre-G13 server omits the field: the first bundle fetch
	// then pins trust-on-first-fetch instead. A malformed key is a
	// server bug, WARN-only — enrolment's primary job (the push
	// channel) must not fail over it; the fetch path will TOFU-pin.
	if er.OrgPolicyPublicKey != "" {
		if keyHash, perr := pinBase64Key(er.OrgPolicyPublicKey); perr != nil {
			c.logger.Warn("org policy key not pinned", "err", perr)
		} else if _, perr := c.store.RecordGuardPolicyState(ctx, store.GuardPolicyStateRow{
			Layer:       "org",
			Path:        PolicyKeyPinPath(orgURL),
			ContentHash: keyHash,
			LoadedAt:    time.Now().UTC(),
		}); perr != nil {
			c.logger.Warn("org policy key not pinned", "err", perr)
		} else {
			c.logger.Info("org policy signing key pinned at enrolment", "key_sha256", keyHash)
		}
	}

	c.logger.Info("enrolled in org", "org", enr.OrgName, "org_id", enr.OrgID,
		"user_email", enr.UserEmail, "server", orgURL, "store", c.bearers.Backend())
	return &enr, nil
}

// Unenroll deletes the local enrolment row and clears the keychain secrets.
// The enrolment row is removed first so a concurrent push loop, which re-reads
// the row each cycle, stops pushing as soon as it observes the absence. Absent
// state is not an error (idempotent).
func (c *Client) Unenroll(ctx context.Context) error {
	if err := c.store.DeleteEnrolment(ctx); err != nil {
		return fmt.Errorf("orgclient.Unenroll: %w", err)
	}
	if err := c.bearers.Clear(); err != nil {
		return fmt.Errorf("orgclient.Unenroll: %w", err)
	}
	c.logger.Info("unenrolled from org")
	return nil
}

// Status returns a snapshot of the agent's enrolment for the CLI / dashboard.
func (c *Client) Status(ctx context.Context) (EnrolmentState, error) {
	st := EnrolmentState{Backend: c.bearers.Backend()}
	enr, err := c.store.LoadEnrolment(ctx)
	if err != nil {
		return st, fmt.Errorf("orgclient.Status: %w", err)
	}
	if enr == nil {
		return st, nil
	}
	st.Enrolled = true
	st.OrgID, st.OrgName, st.OrgServerURL = enr.OrgID, enr.OrgName, enr.OrgServerURL
	st.UserID, st.UserEmail, st.EnrolledAt = enr.UserID, enr.UserEmail, enr.EnrolledAt
	last, err := c.store.LastPushLog(ctx)
	if err != nil {
		return st, fmt.Errorf("orgclient.Status: %w", err)
	}
	st.LastPush = last
	return st, nil
}

// LastPayload returns the JSON of the most recent successfully-pushed envelope
// (the content-free rollup, byte-for-byte as it went on the wire), or nil when
// the agent has never pushed. The dashboard serves it verbatim so a developer
// can audit exactly what was shared.
func (c *Client) LastPayload(ctx context.Context) ([]byte, error) {
	return c.store.LoadLastPushPayload(ctx)
}

// PushOnce ships at most one batch of content-free rows above the local push
// cursor to the org server. An empty batch makes no network call and writes no
// log row. On HTTP 200 it advances + persists the cursor and records an "ok"
// push-log row; on an auth failure it records "failed" and returns
// ErrAuthFailed; on any other failure (network, 5xx, 429, 4xx) it records
// "retry" and returns a (retryable) error.
func (c *Client) PushOnce(ctx context.Context) (PushResult, error) {
	enr, err := c.store.LoadEnrolment(ctx)
	if err != nil {
		return PushResult{}, fmt.Errorf("orgclient.PushOnce: %w", err)
	}
	if enr == nil {
		return PushResult{}, ErrNotEnrolled
	}
	bearer, err := c.bearers.LoadBearer()
	if errors.Is(err, ErrNoSecret) {
		return PushResult{}, ErrNotEnrolled
	}
	if err != nil {
		return PushResult{}, fmt.Errorf("orgclient.PushOnce: load bearer: %w", err)
	}
	signKey, err := c.bearers.LoadAgentKey()
	if errors.Is(err, ErrNoSecret) {
		return PushResult{}, ErrNotEnrolled
	}
	if err != nil {
		return PushResult{}, fmt.Errorf("orgclient.PushOnce: load signing key: %w", err)
	}

	cur, err := c.store.LoadPushCursor(ctx)
	if err != nil {
		return PushResult{}, fmt.Errorf("orgclient.PushOnce: load cursor: %w", err)
	}
	batch, err := c.store.SelectUnpushedSince(
		ctx, cur, c.maxPushBytes(), enr.OrgID, enr.UserEmail,
		store.ShareOptions{
			FullContent:           c.cfg.Share.FullContent,
			TargetActionAllowlist: c.cfg.Share.TargetActionAllowlist,
			AdminManaged:          c.cfg.Share.AdminManaged,
			RoutingSummary:        c.cfg.Share.RoutingSummary,
			ObsSummary:            c.cfg.Share.ObsSummary,
			ObsTraces:             c.cfg.Share.ObsTraces,
			ObsContent:            c.cfg.Share.ObsContent,
			ObsEvalSummary:        c.cfg.Share.ObsEvalSummary,
		},
		store.ScopeOptions{
			ProjectRootAllowlist: c.cfg.Scope.ProjectRootAllowlist,
			ProjectRootDenylist:  c.cfg.Scope.ProjectRootDenylist,
		},
	)
	if err != nil {
		return PushResult{}, fmt.Errorf("orgclient.PushOnce: select: %w", err)
	}
	if batch.Empty() {
		return PushResult{Empty: true}, nil
	}

	env := orgcontract.PushEnvelope{
		AgentVersion:     c.agentVersion,
		CursorFrom:       maxCursor(cur),
		CursorTo:         maxCursor(batch.Cursor),
		Sessions:         batch.Sessions,
		Actions:          batch.Actions,
		APITurns:         batch.APITurns,
		TokenUsage:       batch.TokenUsage,
		RoutingSummaries: batch.RoutingSummaries,
		GuardEvents:      batch.GuardEvents,
		OTelContent:      batch.OTelContent,
	}
	raw, err := json.Marshal(env)
	if err != nil {
		_ = c.store.RecordPush(ctx, int64(batch.RowCount()), 0, "failed", err.Error())
		return PushResult{}, fmt.Errorf("orgclient.PushOnce: marshal: %w", err)
	}
	wire, err := gzipBytes(raw)
	if err != nil {
		// A marshal/gzip failure is local and not retryable, but recording it
		// keeps the dashboard honest about why nothing shipped.
		_ = c.store.RecordPush(ctx, int64(batch.RowCount()), 0, "failed", err.Error())
		return PushResult{}, fmt.Errorf("orgclient.PushOnce: encode: %w", err)
	}

	ts := time.Now().Unix()
	sig := ed25519.Sign(signKey, orgcontract.PushSigningMessage(ts, wire))

	gc, err := c.genClient(enr.OrgServerURL)
	if err != nil {
		return PushResult{}, fmt.Errorf("orgclient.PushOnce: %w", err)
	}
	params := &gen.PushBatchParams{
		XSBOTimestamp:      &ts,
		XSBOAgentSignature: strPtr(base64.RawURLEncoding.EncodeToString(sig)),
	}
	resp, err := gc.PushBatchWithBodyWithResponse(ctx, params, "application/json", bytes.NewReader(wire),
		bearerEditor(bearer), gzipEncodingEditor)
	if err != nil {
		_ = c.store.RecordPush(ctx, int64(batch.RowCount()), int64(len(wire)), "retry", err.Error())
		return PushResult{}, fmt.Errorf("orgclient.PushOnce: post: %w", err)
	}

	switch resp.StatusCode() {
	case http.StatusOK:
		if err := c.store.SavePushCursor(ctx, batch.Cursor); err != nil {
			return PushResult{}, fmt.Errorf("orgclient.PushOnce: save cursor: %w", err)
		}
		// Persist the exact rollup that was shared so the dashboard can show it
		// (best-effort: a failure here never fails an accepted push).
		_ = c.store.SaveLastPushPayload(ctx, raw)
		_ = c.store.RecordPush(ctx, int64(batch.RowCount()), int64(len(wire)), "ok", "")
		res := PushResult{RowCount: batch.RowCount(), Bytes: len(wire)}
		if resp.JSON200 != nil {
			res.AcceptedRows = resp.JSON200.AcceptedRows
			res.DedupedRows = resp.JSON200.DedupedRows
		}
		return res, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		msg := serverError(resp.Body, resp.StatusCode())
		_ = c.store.RecordPush(ctx, int64(batch.RowCount()), int64(len(wire)), "failed", msg)
		return PushResult{}, fmt.Errorf("orgclient.PushOnce: %w: %s", ErrAuthFailed, msg)
	default:
		msg := serverError(resp.Body, resp.StatusCode())
		_ = c.store.RecordPush(ctx, int64(batch.RowCount()), int64(len(wire)), "retry", msg)
		return PushResult{}, fmt.Errorf("orgclient.PushOnce: server returned %d: %s", resp.StatusCode(), msg)
	}
}

// errIdle signals that a cycle did no work because the agent is not enrolled
// (or the enrolment row could not be read). The loop keeps its normal interval
// cadence rather than backing off — there is nothing failing, only nothing to
// do — so an enrol on a running daemon is picked up within one interval.
var errIdle = errors.New("orgclient: idle cycle")

// PushLoop runs the push cycle until ctx is cancelled (clean ctx-cancel
// returns ctx.Err) or an auth failure stops it (returns nil — a stopped loop
// is a surfaced condition, not a daemon-fatal error: P1, never break the host
// tool). Each cycle waits one interval, then re-reads the enrolment state, so
// an `observer enroll`/`unenroll` on a running daemon takes effect within one
// interval without a restart. Retryable failures shorten the next wait to the
// current backoff (exponential, jittered); a success resets the backoff.
func (c *Client) PushLoop(ctx context.Context) error {
	return c.runLoop(ctx, c.pushInterval(), func(ctx context.Context) error {
		enr, err := c.store.LoadEnrolment(ctx)
		if err != nil {
			c.logger.Warn("org push: enrolment read failed", "err", err)
			return errIdle
		}
		if enr == nil {
			return errIdle // not enrolled (yet, or unenrolled while running)
		}
		_, err = c.PushOnce(ctx)
		if errors.Is(err, ErrNotEnrolled) {
			return errIdle
		}
		// Best-effort §R19.1 policy sync rides the same cycle: a fetch
		// failure never affects push health (P1 — the policy cache
		// just stays at its last verified version).
		if _, perr := c.FetchRoutingPolicy(ctx); perr != nil && !errors.Is(perr, ErrNotEnrolled) {
			c.logger.Warn("org routing policy fetch failed", "err", perr)
		}
		return err
	})
}

// runLoop is the timing core of PushLoop, parameterised on the per-cycle
// action so the backoff/interval/stop behaviour can be tested with a pure
// scripted action under testing/synctest (no real I/O). The action's return
// drives the next wait: nil (success) → interval + backoff reset; errIdle →
// interval (no backoff); ErrAuthFailed → stop; any other error → jittered
// exponential backoff.
func (c *Client) runLoop(ctx context.Context, interval time.Duration, action func(context.Context) error) error {
	backoff := initialBackoff
	sleep := interval
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sleep):
		}

		err := action(ctx)
		switch {
		case errors.Is(err, context.Canceled):
			return ctx.Err()
		case errors.Is(err, errIdle):
			sleep = interval
		case errors.Is(err, ErrAuthFailed):
			c.logger.Error("org push: authentication failed, stopping push loop", "err", err)
			return nil
		case err != nil:
			c.logger.Warn("org push failed, backing off", "err", err, "backoff", backoff.String())
			sleep = jitter(backoff)
			backoff = nextBackoff(backoff)
		default:
			sleep = interval
			backoff = initialBackoff
		}
	}
}

// genClient builds a generated API client bound to server, reusing the
// configured HTTP doer.
func (c *Client) genClient(server string) (*gen.ClientWithResponses, error) {
	return gen.NewClientWithResponses(server, gen.WithHTTPClient(c.httpClient))
}

func (c *Client) pushInterval() time.Duration {
	secs := c.cfg.PushIntervalSeconds
	if secs <= 0 {
		secs = config.DefaultPushIntervalSeconds
	}
	return time.Duration(secs) * time.Second
}

// maxPushBytes returns the configured uncompressed batch ceiling, defaulted and
// clamped to the contract bounds.
func (c *Client) maxPushBytes() int64 {
	mb := c.cfg.MaxPushBytes
	if mb <= 0 {
		mb = config.DefaultMaxPushBytes
	}
	if mb > config.MaxPushBytesCeiling {
		mb = config.MaxPushBytesCeiling
	}
	return mb
}

// --- helpers ---------------------------------------------------------------

// bearerEditor sets the Authorization header on the outgoing push request.
func bearerEditor(bearer string) gen.RequestEditorFn {
	return func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer "+bearer)
		return nil
	}
}

// gzipEncodingEditor declares the body's content coding so the server reads it
// through a gzip reader. The body bytes (and thus the signature) are the gzip
// bytes — Content-Encoding describes them, it does not transform them.
func gzipEncodingEditor(_ context.Context, req *http.Request) error {
	req.Header.Set("Content-Encoding", "gzip")
	return nil
}

// gzipBytes gzip-compresses raw, returning the wire bytes.
func gzipBytes(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		_ = zw.Close()
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// maxCursor flattens the per-table push cursor to a single representative
// scalar (the highest id across the four tables) for the envelope's
// cursor_from/cursor_to. The authoritative cursor is per-table and persisted
// locally; this scalar is a server-side progress hint only.
func maxCursor(c store.PushCursor) int64 {
	m := c.Sessions
	for _, v := range []int64{c.Actions, c.APITurns, c.TokenUsage, c.GuardEvents, c.OTelContent} {
		if v > m {
			m = v
		}
	}
	return m
}

// nextBackoff doubles d, capped at maxBackoff.
func nextBackoff(d time.Duration) time.Duration {
	d *= backoffFactor
	if d > maxBackoff {
		d = maxBackoff
	}
	return d
}

// jitter applies ±jitterFraction uniform jitter to d.
func jitter(d time.Duration) time.Duration {
	delta := (mrand.Float64()*2 - 1) * jitterFraction //nolint:gosec // G404: backoff jitter is not security-sensitive; math/rand is the right tool.
	j := time.Duration(float64(d) * (1 + delta))
	if j < 0 {
		j = 0
	}
	return j
}

// serverError renders a concise error string from a JSON error body, falling
// back to the status code when the body is empty/unparseable.
func serverError(body []byte, status int) string {
	var e struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		if e.Message != "" {
			return e.Error + ": " + e.Message
		}
		return e.Error
	}
	if s := strings.TrimSpace(string(body)); s != "" {
		return s
	}
	return http.StatusText(status)
}

func strPtr(s string) *string { return &s }
