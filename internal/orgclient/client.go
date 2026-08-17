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
	"os"
	"path/filepath"
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

	// P0-6 effective-policy-state fetch-outcome sinks (nil-defaulted seam,
	// R6-1). When non-nil, PolicyPollLoop/PushLoop forward the TOTAL typed
	// fetch outcome (§2.5b/§2.5c) here on every decisive cycle so the reporter
	// can resolve stale_lkg and delivered_unaccepted, which onResult/success
	// never surface. A nil sink is EXACTLY today's behaviour, so the existing
	// call sites compile and run unchanged. Set via SetGuardOutcomeSink /
	// SetRoutingOutcomeSink.
	guardOutcomeSink   func(GuardFetchOutcome)
	routingOutcomeSink func(RoutingFetchOutcome)

	// routingReloadSink is the P0-7 router hot-reload trigger (docs/plans/
	// plane-a-p0-7-guard-router-hotreload-plan.md §2.2/§4.5): a nil-defaulted
	// additive seam invoked AFTER a routing policy is accepted + cached
	// (rfStageAccepted, both newly-cached and already-current arms — SF7), so
	// the caller can apply it to the live router in-process. Independent of
	// routingOutcomeSink (one owner per concern: that sink updates the P0-6
	// reporter slot, this one reloads the live router). Nil = today's exact
	// no-op. Set via SetRoutingReloadSink.
	routingReloadSink func(ctx context.Context)

	// orgIdentityChangedSink is the Plane-A P0-5 Phase W enrolment-transition
	// hook (plan §6.9): fired synchronously from Enroll/Unenroll AFTER the
	// durable org_enrolment_generation bump/tombstone commits, so a caller
	// that holds a live obs.AdmissionService IN THE SAME PROCESS (e.g. a
	// future dashboard-driven enroll/unenroll path) can ClearOrg both
	// families immediately rather than waiting for
	// AdmissionService's own short-TTL identity recheck. orgclient must not
	// import internal/obs (the boundary), so this is a plain func — nil
	// (today's only real call path: the separate `observer enroll`/
	// `observer unenroll` CLI processes, which hold no live
	// AdmissionService) is an exact no-op. The cross-process case — a
	// running daemon observing an unenrol/re-enrol performed by a SEPARATE
	// `observer` invocation — is NOT this sink; it self-heals via
	// AdmissionService's own activeEnrolmentIdentity cache instead (bounded
	// by identityCheckTTL), which is what actually matters today. Set via
	// SetOrgIdentityChangedSink.
	orgIdentityChangedSink func()

	// policyResourceCacheDir is the base directory for the generation-scoped
	// on-disk policy-resource cache tree (plan §6.2). Set via
	// SetPolicyResourceCacheDir; when non-empty, Enroll/Unenroll remove the
	// org_key subtree so identity transitions do not leave stale envelopes
	// (Codex SF3). Empty = no filesystem cleanup (tests that never install).
	policyResourceCacheDir string

	// shareProvider resolves the share posture PushOnce ships under
	// (admin-controlled Plane B, Phase 1b §2.4). Nil means "use the config
	// this client was constructed with", which is byte-identical to Phase
	// 1a — so a build with no governance wiring behaves exactly as before.
	//
	// It exists because share directives are LOWERING-ONLY and must be HOT:
	// a node that keeps shipping content for hours after the org said stop
	// is precisely the failure the directive exists to prevent, and Client
	// holds cfg from construction. One owner (store.ShareOptions), two feed
	// paths (TOML, governance) — the pattern CLAUDE.md #4 blesses.
	shareProvider func() store.ShareOptions

	// governanceSidecarPath is the node-local governance sidecar, removed by
	// Unenroll (§1.4 step 2b). Empty = nothing to remove (the solo case and
	// every test that never wires governance).
	governanceSidecarPath string

	// renewalSink receives the classified outcome of every authenticated
	// agent request (§4.2). Nil = today's exact behaviour.
	renewalSink func(RenewalOutcome)
}

// SetShareProvider installs the hot share-posture resolver (§2.4). Passing
// nil restores the cfg-derived default.
func (c *Client) SetShareProvider(fn func() store.ShareOptions) {
	if c == nil {
		return
	}
	c.shareProvider = fn
}

// SetGovernanceSidecarPath tells Unenroll which sidecar to delete. Safe to
// leave unset.
func (c *Client) SetGovernanceSidecarPath(path string) {
	if c == nil {
		return
	}
	c.governanceSidecarPath = path
}

// GovernanceSidecarPath reports the sidecar Unenroll would delete. It
// exists so the cmd layer can PIN its wiring (the 2026-08-15 smoke found
// only start.go set the path, leaving CLI unenrolls orphaning the file).
func (c *Client) GovernanceSidecarPath() string {
	if c == nil {
		return ""
	}
	return c.governanceSidecarPath
}

// shareOptions resolves the share posture for one push. The DEFAULT is
// exactly the cfg-derived value Phase 1a built inline, so the seam is inert
// until something installs a provider.
func (c *Client) shareOptions() store.ShareOptions {
	if c.shareProvider != nil {
		return c.shareProvider()
	}
	return ShareOptionsFromConfig(c.cfg)
}

// ShareOptionsFromConfig maps an [org_client] config block onto the push
// share posture. It is the ONE definition of that mapping; the governance
// provider in cmd/observer LOWERS the result rather than rebuilding it, so
// the two can never disagree about which keys exist.
func ShareOptionsFromConfig(cfg config.OrgClientConfig) store.ShareOptions {
	return store.ShareOptions{
		FullContent:           cfg.Share.FullContent,
		TargetActionAllowlist: cfg.Share.TargetActionAllowlist,
		AdminManaged:          cfg.Share.AdminManaged,
		RoutingSummary:        cfg.Share.RoutingSummary,
		ObsSummary:            cfg.Share.Obs.Summary,
		ObsTraces:             cfg.Share.Obs.Traces,
		ObsContent:            cfg.Share.Obs.Content,
		ObsEvalSummary:        cfg.Share.Obs.EvalSummary,
		ObsAdmission:          cfg.Share.Obs.Admission,
		ObsEvalItems:          cfg.Share.Obs.EvalItems,
	}
}

// SetOrgIdentityChangedSink injects the Phase W enrolment-transition hook
// (see the field's doc comment). Safe to leave unset.
func (c *Client) SetOrgIdentityChangedSink(fn func()) { c.orgIdentityChangedSink = fn }

// SetPolicyResourceCacheDir sets the on-disk policy-resource cache base
// (Codex SF3). Safe to leave unset.
func (c *Client) SetPolicyResourceCacheDir(dir string) { c.policyResourceCacheDir = dir }

// removeGovernanceSidecar deletes the node-local governance sidecar (§1.4
// unenrol step 2b). Best-effort and logged; never an unenrol failure.
func (c *Client) removeGovernanceSidecar() {
	if c == nil || c.governanceSidecarPath == "" {
		return
	}
	if err := os.Remove(c.governanceSidecarPath); err != nil && !os.IsNotExist(err) {
		c.logger.Warn("unenroll: could not remove the governance sidecar — pinned settings will lift at the next hook once the grant's own expiry passes",
			"path", c.governanceSidecarPath, "err", err)
	}
}

// clearPolicyResourceCacheTree removes <cacheDir>/<orgKey>/ best-effort.
func (c *Client) clearPolicyResourceCacheTree(orgKey string) {
	if c == nil || c.policyResourceCacheDir == "" || orgKey == "" {
		return
	}
	_ = os.RemoveAll(filepath.Join(c.policyResourceCacheDir, orgKey))
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
func (c *Client) Enroll(ctx context.Context, orgURL, token string) (*store.Enrolment, *GrantOffer, error) {
	orgURL = strings.TrimRight(strings.TrimSpace(orgURL), "/")
	if orgURL == "" {
		return nil, nil, errors.New("orgclient.Enroll: org server URL is required")
	}
	if strings.TrimSpace(token) == "" {
		return nil, nil, errors.New("orgclient.Enroll: enrolment token is required")
	}

	// Snapshot the OLD enrolment (nil on a fresh install) before it is
	// overwritten below, so a re-enrolment can tombstone the old identity's
	// policy-resource generation (plan §6.9) after the new one activates.
	oldEnr, _ := c.store.LoadEnrolment(ctx) //nolint:errcheck // best-effort snapshot; a read failure just skips old-identity cleanup

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("orgclient.Enroll: keygen: %w", err)
	}

	gc, err := c.genClient(orgURL)
	if err != nil {
		return nil, nil, fmt.Errorf("orgclient.Enroll: %w", err)
	}
	resp, err := gc.EnrollAgentWithResponse(ctx, gen.EnrollRequest{
		OneTimeToken:   token,
		AgentPublicKey: base64.RawURLEncoding.EncodeToString(pub),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("orgclient.Enroll: post: %w", err)
	}
	switch resp.StatusCode() {
	case http.StatusOK:
		// fall through
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, nil, fmt.Errorf("orgclient.Enroll: %w: invalid or expired enrolment token", ErrAuthFailed)
	default:
		return nil, nil, fmt.Errorf("orgclient.Enroll: server returned %d: %s", resp.StatusCode(), strings.TrimSpace(string(resp.Body)))
	}
	er := resp.JSON200
	if er == nil || er.Bearer == "" {
		return nil, nil, errors.New("orgclient.Enroll: server returned no bearer")
	}

	// Seed the cursor + persist secrets BEFORE the enrolment row (see doc).
	maxIDs, err := c.store.CurrentMaxIDs(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("orgclient.Enroll: seed cursor: %w", err)
	}
	if err := c.store.SavePushCursor(ctx, maxIDs); err != nil {
		return nil, nil, fmt.Errorf("orgclient.Enroll: save cursor: %w", err)
	}
	// Wipe any prior-run last-push state so `observer org status` after
	// a re-enroll reports "(none yet)" instead of a stale timestamp.
	// N5 in docs/teams-test-regression-2026-06-03.md.
	if err := c.store.ClearLastPushState(ctx); err != nil {
		return nil, nil, fmt.Errorf("orgclient.Enroll: clear last-push state: %w", err)
	}
	if err := c.bearers.SaveAgentKey(priv); err != nil {
		return nil, nil, fmt.Errorf("orgclient.Enroll: %w", err)
	}
	if err := c.bearers.SaveBearer(er.Bearer); err != nil {
		return nil, nil, fmt.Errorf("orgclient.Enroll: %w", err)
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

	// Plane-A P0-5 (plan §6.9 / R6-B2 / R3-B2 / Codex B1): activate the new
	// identity's policy-resource generation BEFORE WriteEnrolment so a
	// concurrent FetchAndAccept never observes enrolled-without-generation
	// (missing gen ≡ ErrNotEnrolled). Fail-closed on bump/clear errors —
	// never leave enrolment visible without an active generation row. On
	// any re-enrolment (same or different org key), clear prior
	// policy-resource state so CAS cannot wedge on a stale generation
	// column (Codex B3).
	newOrgKey := OrgKey(orgURL, er.OrgID)
	if oldEnr != nil {
		oldOrgKey := OrgKey(oldEnr.OrgServerURL, oldEnr.OrgID)
		if oldOrgKey != newOrgKey {
			if _, err := c.store.BumpEnrolmentGeneration(ctx, oldOrgKey, true); err != nil {
				return nil, nil, fmt.Errorf("orgclient.Enroll: tombstone old enrolment generation: %w", err)
			}
		}
		if err := c.store.ClearPolicyResourceState(ctx, oldOrgKey); err != nil {
			return nil, nil, fmt.Errorf("orgclient.Enroll: clear old policy-resource state: %w", err)
		}
		c.clearPolicyResourceCacheTree(oldOrgKey)
	}
	generation, err := c.store.BumpEnrolmentGeneration(ctx, newOrgKey, false)
	if err != nil {
		return nil, nil, fmt.Errorf("orgclient.Enroll: activate enrolment generation: %w", err)
	}
	if err := c.store.WriteEnrolment(ctx, enr); err != nil {
		return nil, nil, fmt.Errorf("orgclient.Enroll: write enrolment: %w", err)
	}
	if c.orgIdentityChangedSink != nil {
		c.orgIdentityChangedSink()
	}

	// Pin the org POLICY signing key when the server delivered one
	// (guard spec §14.2 — the pin every fetched bundle's embedded key
	// is checked against; guard_policy_state is the pin home, append-
	// only so rotations stay auditable and a re-enrol records the new
	// key). A pre-G13 server omits the field: the first bundle fetch
	// then pins trust-on-first-fetch instead. A malformed key is a
	// server bug, WARN-only — enrolment's primary job (the push
	// channel) must not fail over it; the fetch path will TOFU-pin.
	pinnedKeyHash := ""
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
			pinnedKeyHash = keyHash
			c.logger.Info("org policy signing key pinned at enrolment", "key_sha256", keyHash)
		}
	}

	// Admin-controlled Plane B (spec §2.3, adversarial review A3/A4): the
	// grant is evaluated ONLY AFTER the org policy key was actually pinned,
	// and it is RETURNED, never written here. Two reasons this ordering is
	// load-bearing:
	//
	//   - The pin write above is best-effort by design (enrolment's primary
	//     job, the push channel, must not fail over it). A grant accepted
	//     without a pin would be authority bound to nothing: the NEXT policy
	//     fetch would TOFU-pin whatever key it saw, which need not be the key
	//     the grant was signed under. So a missing/failed pin REFUSES the
	//     grant, loudly, instead of storing it unverified.
	//   - orgclient has no TTY, and by this point the bearer is saved, the
	//     enrolment is written and the generation is bumped — there is no
	//     honest place here to ask a human anything. cmd/observer/org.go owns
	//     the confirmation and the store write.
	offer := c.evaluateGrantOffer(er, orgURL, newOrgKey, generation, pinnedKeyHash)

	c.logger.Info("enrolled in org", "org", enr.OrgName, "org_id", enr.OrgID,
		"user_email", enr.UserEmail, "server", orgURL, "store", c.bearers.Backend())
	return &enr, offer, nil
}

// GrantOffer is a VERIFIED enrolment grant plus the identity facts the node
// needs in order to store it. Enroll returns one only when every gate passed;
// a nil offer means "enrolled, ungoverned", which is exactly today's
// behaviour and what every pre-governance server produces.
type GrantOffer struct {
	Grant        orgcontract.EnrolmentGrant
	OrgKey       string
	Generation   int64
	KeyPinSHA256 string
	ReceiptHash  string
}

// evaluateGrantOffer runs the accept gates for an offered grant. EVERY
// refusal is logged with a named reason (adversarial review A3: a silently
// skipped grant leaves the admin staring at a permanently ungoverned node
// with no diagnosis) and returns nil, which enrols the node ungoverned.
func (c *Client) evaluateGrantOffer(er *orgcontract.EnrollResponse, orgURL, orgKey string, generation int64, pinnedKeyHash string) *GrantOffer {
	if er.Grant == nil {
		return nil
	}
	g := *er.Grant
	switch {
	case er.OrgPolicyPublicKey == "":
		c.logger.Warn("enrolment grant REFUSED: the server offered governance but sent no org policy signing key, so the grant cannot be bound to any key — enrolling ungoverned")
		return nil
	case pinnedKeyHash == "":
		c.logger.Warn("enrolment grant REFUSED: the org policy signing key could not be pinned, so a grant would be authority bound to nothing — enrolling ungoverned")
		return nil
	case g.KeyPinSHA256 == "" || g.KeyPinSHA256 != pinnedKeyHash:
		c.logger.Warn("enrolment grant REFUSED: the grant names a different org policy signing key than the one this enrolment pinned — enrolling ungoverned",
			"grant_key_sha256", g.KeyPinSHA256, "pinned_key_sha256", pinnedKeyHash)
		return nil
	case g.OrgID != er.OrgID:
		c.logger.Warn("enrolment grant REFUSED: the grant names a different org than the enrolment — enrolling ungoverned",
			"grant_org_id", g.OrgID, "enrolment_org_id", er.OrgID)
		return nil
	case strings.TrimRight(strings.TrimSpace(g.OrgServerURL), "/") != orgURL:
		c.logger.Warn("enrolment grant REFUSED: the grant names a different org server than the one being enrolled with — enrolling ungoverned",
			"grant_server", g.OrgServerURL, "enrolment_server", orgURL)
		return nil
	}
	pubRaw, derr := base64.RawURLEncoding.DecodeString(er.OrgPolicyPublicKey)
	if derr != nil {
		c.logger.Warn("enrolment grant REFUSED: org policy public key did not decode — enrolling ungoverned", "err", derr)
		return nil
	}
	if verr := orgcontract.VerifyEnrolmentGrant(g, ed25519.PublicKey(pubRaw)); verr != nil {
		c.logger.Warn("enrolment grant REFUSED: signature did not verify under the pinned org policy key — enrolling ungoverned", "err", verr)
		return nil
	}
	if g.ExpiresAt != "" {
		exp, perr := time.Parse(time.RFC3339, g.ExpiresAt)
		if perr != nil {
			c.logger.Warn("enrolment grant REFUSED: expires_at is not RFC3339 — enrolling ungoverned", "expires_at", g.ExpiresAt)
			return nil
		}
		if !time.Now().UTC().Before(exp) {
			c.logger.Warn("enrolment grant REFUSED: it is already expired — enrolling ungoverned", "expires_at", g.ExpiresAt)
			return nil
		}
	}
	return &GrantOffer{
		Grant:        g,
		OrgKey:       orgKey,
		Generation:   generation,
		KeyPinSHA256: pinnedKeyHash,
		ReceiptHash:  orgcontract.EnrolmentGrantReceiptHash(g),
	}
}

// Unenroll deletes the local enrolment row and clears the keychain secrets.
// The enrolment row is removed first so a concurrent push loop, which re-reads
// the row each cycle, stops pushing as soon as it observes the absence. Absent
// state is not an error (idempotent).
func (c *Client) Unenroll(ctx context.Context) error {
	// Snapshot first, then tombstone the generation BEFORE deleting the
	// enrolment row (plan §6.9 / Codex B1): a concurrent fetch must observe
	// tombstoned (or missing gen after clear) rather than a live enrolment
	// with a still-active generation.
	enr, _ := c.store.LoadEnrolment(ctx) //nolint:errcheck // best-effort snapshot; absent is fine

	if enr != nil {
		orgKey := OrgKey(enr.OrgServerURL, enr.OrgID)
		if _, err := c.store.BumpEnrolmentGeneration(ctx, orgKey, true); err != nil {
			return fmt.Errorf("orgclient.Unenroll: tombstone enrolment generation: %w", err)
		}
		// Admin-controlled Plane B (spec §5.1): delete the enrolment GRANT
		// in the same fail-closed prefix as the tombstone, BEFORE the
		// policy-resource state and the enrolment row. Revocation must
		// leave nothing behind that could govern this machine, and the
		// grant is the only artifact that could.
		if err := c.store.DeleteEnrolmentGrant(ctx, orgKey); err != nil {
			return fmt.Errorf("orgclient.Unenroll: delete enrolment grant: %w", err)
		}
		// Step 2b (Phase 1b §1.4): remove the governance sidecar, so a hook
		// or MCP process spawned a millisecond later reads no pins. It is
		// BEST-EFFORT because step 1 already tombstoned the generation and
		// because the reader's own expiry rule is the backstop that makes a
		// failed delete converge anyway — but leaving it would keep pinning
		// keys in short-lived processes until the grant lapsed, which is a
		// bad enough surprise to be worth the attempt and the log line.
		c.removeGovernanceSidecar()
		if err := c.store.ClearPolicyResourceState(ctx, orgKey); err != nil {
			return fmt.Errorf("orgclient.Unenroll: clear policy-resource state: %w", err)
		}
		c.clearPolicyResourceCacheTree(orgKey)
	}
	// Belt-and-braces: an unenrol running against a DB whose enrolment row
	// is already gone cannot derive an org_key, and an orphan grant is
	// authority with no owner. Clearing unconditionally is safe — a grant
	// only ever exists for the current enrolment.
	if err := c.store.DeleteAllEnrolmentGrants(ctx); err != nil {
		return fmt.Errorf("orgclient.Unenroll: delete enrolment grants: %w", err)
	}

	if err := c.store.DeleteEnrolment(ctx); err != nil {
		return fmt.Errorf("orgclient.Unenroll: %w", err)
	}
	if err := c.bearers.Clear(); err != nil {
		return fmt.Errorf("orgclient.Unenroll: %w", err)
	}
	if c.orgIdentityChangedSink != nil {
		c.orgIdentityChangedSink()
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
		c.shareOptions(),
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
		// Org-tier observability (obs-org-tier plan). Each slice is
		// composed by orgpush.go::composeObsTiers only under its own
		// [org_client.share] flag; nil/empty when the node hasn't opted
		// in, so this mapping is inert for non-obs-sharing nodes.
		ObsSummaries:  batch.ObsSummaries,
		ObsTraces:     batch.ObsTraces,
		ObsSpans:      batch.ObsSpans,
		ObsSpanEvents: batch.ObsSpanEvents,
		ObsContent:    batch.ObsContent,
		ObsEvalRuns:   batch.ObsEvalRuns,
		// T5 per-end-user spend — composed only under ObsSummary &&
		// shipsRawContent() (end-user PII); nil/empty otherwise.
		ObsEndUserSpend: batch.ObsEndUserSpend,
		// T6 input-admission verdicts + policy snapshots — composed only
		// under the [org_client.share.obs] admission opt-in; nil/empty
		// otherwise. Verdict PII/prose is gated by shipsRawContent() in
		// composeObsTiers; policy Body always ships.
		ObsAdmissionEvents:   batch.ObsAdmissionEvents,
		ObsAdmissionPolicies: batch.ObsAdmissionPolicies,
		// T7 per-item eval scores — composed only under the
		// [org_client.share.obs] eval_items opt-in; nil/empty otherwise. Item
		// content excerpts are gated by shipsRawContent() in composeObsTiers;
		// the score metadata + content_hash always ship.
		ObsEvalItems: batch.ObsEvalItems,
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
		c.noteRenewal(RenewalPathPush, 0, err)
		_ = c.store.RecordPush(ctx, int64(batch.RowCount()), int64(len(wire)), "retry", err.Error())
		return PushResult{}, fmt.Errorf("orgclient.PushOnce: post: %w", err)
	}
	// The renewal signal (§4.2) is keyed off the PUSH path's own
	// authorization, which is the requirement parent §11.3 states: an org
	// that revokes push permission but leaves the policy poll working must
	// not keep governing the machine indefinitely.
	c.noteRenewal(RenewalPathPush, resp.StatusCode(), nil)

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
		_, routingOutcome, perr := c.FetchRoutingPolicy(ctx)
		// Forward the TOTAL typed outcome (§2.5b) to the P0-6 reporter. A nil
		// outcome is a skip (context.Canceled — a shutdown, not a verdict); a
		// nil sink (the default) is today's exact no-op (R6-1).
		if routingOutcome != nil && c.routingOutcomeSink != nil {
			c.routingOutcomeSink(*routingOutcome)
		}
		if perr != nil && !errors.Is(perr, ErrNotEnrolled) {
			c.logger.Warn("org routing policy fetch failed", "err", perr)
		}
		// Rail R3 of the dashboard-announcements plan (§4) rides the
		// SAME cycle for the same reason: no new timer, no new host, no
		// new connection — and a fetch failure never affects push
		// health (P1), it just leaves the banner at its last verified
		// version.
		if _, aerr := c.FetchOrgAnnouncement(ctx); aerr != nil && !errors.Is(aerr, ErrNotEnrolled) {
			c.logger.Warn("org announcement fetch failed", "err", aerr)
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
