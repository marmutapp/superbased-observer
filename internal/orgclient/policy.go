package orgclient

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/guard"
	"github.com/marmutapp/superbased-observer/internal/orgclient/gen"
	"github.com/marmutapp/superbased-observer/internal/orgcontract"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// Org policy-bundle channel, agent side (guard spec §14.2, G13). The
// agent polls GET /api/v1/policy-bundle on `observer start` and every
// [org_client].policy_poll_interval_seconds, verifies the envelope
// against the §14.2 acceptance gate, and on success atomically
// replaces the local bundle cache ([guard.rules].org_bundle) that the
// guard's org layer loads from. The channel DISTRIBUTES policy
// (server → agent) — nothing flows back, and nothing here touches the
// push pipeline's privacy seam.
//
// Acceptance gate, in order (a failed step REJECTS the fetch and the
// previous cache stays in place):
//
//  1. Ed25519 signature over the canonical message verifies against
//     the envelope's embedded key (orgcontract.VerifyPolicyBundle).
//  2. The embedded key's hash matches the pin recorded at enrolment
//     in guard_policy_state ("#policy-key" row). Missing pin =
//     trust-on-first-fetch: the key is pinned NOW (pre-G13 enrolments
//     have no enrol-time pin; the TLS+bearer channel anchoring this
//     first fetch is the same trust that anchored enrolment itself).
//  3. The bundle version is not lower than the last verified version
//     (downgrade protection — rollback is publishing old content as a
//     NEW version).
//  4. The TOML lints as an org-layer policy file (guard.Lint with the
//     escalate-only floor checks), so a malformed or floor-violating
//     bundle never evicts a good cache.
//
// A rejection is a RESULT, not an error: the caller (cmd layer) turns
// PolicyRejected into an R-205 guard event; transport failures return
// errors and ride the poll loop's backoff.

// PolicyKeyPinSuffix tags the guard_policy_state row that pins the org
// policy public key: Path = <org-server-url> + PolicyKeyPinSuffix,
// ContentHash = orgcontract.PublicKeyPinHash of the raw key bytes.
// guard_policy_state is the pin home per §14.2 — append-only, so key
// rotation history stays auditable; re-enrolment appends the new pin.
const PolicyKeyPinSuffix = "#policy-key"

// PolicyKeyPinPath returns the guard_policy_state path identity of the
// key-pin row for an org server.
func PolicyKeyPinPath(orgURL string) string { return orgURL + PolicyKeyPinSuffix }

// policyBundleStatePath returns the guard_policy_state path identity
// of the fetched-bundle row (one row per verified version, distinct
// from the load-time row guard records against the cache file path).
func policyBundleStatePath(orgURL string) string { return orgURL + "/api/v1/policy-bundle" }

// PolicyStatus classifies one policy-bundle poll outcome.
type PolicyStatus string

// PolicyStatus values.
const (
	// PolicyApplied — a new version passed the acceptance gate and the
	// local cache was replaced. Effective at the next guard
	// construction (hook processes: their next invocation; the
	// daemon's engines: next start).
	PolicyApplied PolicyStatus = "applied"
	// PolicyUnchanged — 304: the cache already holds the current version.
	PolicyUnchanged PolicyStatus = "unchanged"
	// PolicyNone — 404: no bundle published, or a pre-G13 server
	// without the endpoint. The agent runs local-only policy; an
	// existing cache is deliberately kept (withdrawing a bundle is
	// publishing an empty rule set as a new version, never an
	// ambiguous 404).
	PolicyNone PolicyStatus = "none"
	// PolicyRejected — the envelope failed the acceptance gate. The
	// previous cache stays; the caller records an R-205 guard event.
	PolicyRejected PolicyStatus = "rejected"
)

// PolicyResult summarises one policy-bundle poll for the CLI / loop /
// R-205 emission.
type PolicyResult struct {
	Status  PolicyStatus
	Version int64  // served version (0 when none/unchanged-by-etag)
	Detail  string // human-readable specifics for rejected/none
}

// FetchPolicyBundle performs one poll of GET /api/v1/policy-bundle and
// runs the §14.2 acceptance gate. cachePath is the resolved
// [guard.rules].org_bundle location the verified envelope is written
// to (atomic replace; 0600). Returns ErrNotEnrolled when the agent has
// no enrolment, ErrAuthFailed on 401/403, and a retryable error on
// transport/5xx failures.
func (c *Client) FetchPolicyBundle(ctx context.Context, cachePath string) (PolicyResult, error) {
	enr, err := c.store.LoadEnrolment(ctx)
	if err != nil {
		return PolicyResult{}, fmt.Errorf("orgclient.FetchPolicyBundle: %w", err)
	}
	if enr == nil {
		return PolicyResult{}, ErrNotEnrolled
	}
	bearer, err := c.bearers.LoadBearer()
	if errors.Is(err, ErrNoSecret) {
		return PolicyResult{}, ErrNotEnrolled
	}
	if err != nil {
		return PolicyResult{}, fmt.Errorf("orgclient.FetchPolicyBundle: load bearer: %w", err)
	}

	// If-None-Match only when the cache file actually exists — a stored
	// ETag with a deleted cache must re-download, not 304 into nothing.
	params := &gen.GetPolicyBundleParams{}
	if etag, eerr := c.store.LoadOrgPolicyETag(ctx); eerr == nil && etag != "" {
		if _, serr := os.Stat(cachePath); serr == nil {
			params.IfNoneMatch = &etag
		}
	}

	gc, err := c.genClient(enr.OrgServerURL)
	if err != nil {
		return PolicyResult{}, fmt.Errorf("orgclient.FetchPolicyBundle: %w", err)
	}
	resp, err := gc.GetPolicyBundleWithResponse(ctx, params, bearerEditor(bearer))
	if err != nil {
		return PolicyResult{}, fmt.Errorf("orgclient.FetchPolicyBundle: get: %w", err)
	}
	switch resp.StatusCode() {
	case http.StatusOK:
		// fall through to verification
	case http.StatusNotModified:
		return PolicyResult{Status: PolicyUnchanged}, nil
	case http.StatusNotFound:
		return PolicyResult{Status: PolicyNone, Detail: "no bundle published (or pre-guard server)"}, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return PolicyResult{}, fmt.Errorf("orgclient.FetchPolicyBundle: %w", ErrAuthFailed)
	default:
		return PolicyResult{}, fmt.Errorf("orgclient.FetchPolicyBundle: server returned %d", resp.StatusCode())
	}
	if resp.JSON200 == nil {
		return PolicyResult{}, errors.New("orgclient.FetchPolicyBundle: 200 with no bundle body")
	}
	b := *resp.JSON200

	// Gate 1: self-contained signature check.
	pub, err := orgcontract.VerifyPolicyBundle(b)
	if err != nil {
		return PolicyResult{
			Status: PolicyRejected, Version: b.Version,
			Detail: fmt.Sprintf("signature verification failed: %v", err),
		}, nil
	}

	// Gate 2: key pin (TOFU when no pin exists yet).
	keyHash := orgcontract.PublicKeyPinHash(pub)
	pinned, err := c.loadKeyPin(ctx, enr.OrgServerURL)
	if err != nil {
		return PolicyResult{}, fmt.Errorf("orgclient.FetchPolicyBundle: read key pin: %w", err)
	}
	switch {
	case pinned == "":
		if _, err := c.store.RecordGuardPolicyState(ctx, store.GuardPolicyStateRow{
			Layer:       "org",
			Path:        PolicyKeyPinPath(enr.OrgServerURL),
			ContentHash: keyHash,
			LoadedAt:    time.Now().UTC(),
		}); err != nil {
			return PolicyResult{}, fmt.Errorf("orgclient.FetchPolicyBundle: pin key: %w", err)
		}
		c.logger.Info("org policy: signing key pinned on first fetch", "key_sha256", keyHash)
	case pinned != keyHash:
		return PolicyResult{
			Status: PolicyRejected, Version: b.Version,
			Detail: "signing key does not match the enrolment pin (re-enrol if the org key legitimately rotated)",
		}, nil
	}

	// Gate 3: monotonic version.
	lastVersion, err := c.lastBundleVersion(ctx, enr.OrgServerURL)
	if err != nil {
		return PolicyResult{}, fmt.Errorf("orgclient.FetchPolicyBundle: read last version: %w", err)
	}
	if b.Version < lastVersion {
		return PolicyResult{
			Status: PolicyRejected, Version: b.Version,
			Detail: fmt.Sprintf("version regression: served %d after %d (rollback = publish old content as a new version)", b.Version, lastVersion),
		}, nil
	}

	// Gate 4: the TOML must lint as an org-layer policy file so a
	// malformed or floor-violating bundle never evicts a good cache.
	if problems := guard.Lint([]byte(b.BundleTOML), "org"); len(problems) > 0 {
		return PolicyResult{
			Status: PolicyRejected, Version: b.Version,
			Detail: fmt.Sprintf("bundle does not lint as an org policy file: %s", problems[0]),
		}, nil
	}

	// Accepted: atomically replace the cache, record the version row,
	// remember the ETag.
	raw, err := json.Marshal(b)
	if err != nil {
		return PolicyResult{}, fmt.Errorf("orgclient.FetchPolicyBundle: marshal cache: %w", err)
	}
	if err := writeFileAtomic(cachePath, raw); err != nil {
		return PolicyResult{}, fmt.Errorf("orgclient.FetchPolicyBundle: write cache: %w", err)
	}
	sum := sha256.Sum256([]byte(b.BundleTOML))
	if _, err := c.store.RecordGuardPolicyState(ctx, store.GuardPolicyStateRow{
		Layer:       "org",
		Path:        policyBundleStatePath(enr.OrgServerURL),
		Version:     strconv.FormatInt(b.Version, 10),
		ContentHash: hex.EncodeToString(sum[:]),
		Signature:   b.Signature,
		LoadedAt:    time.Now().UTC(),
	}); err != nil {
		return PolicyResult{}, fmt.Errorf("orgclient.FetchPolicyBundle: record state: %w", err)
	}
	if etag := resp.HTTPResponse.Header.Get("ETag"); etag != "" {
		if err := c.store.SaveOrgPolicyETag(ctx, etag); err != nil {
			c.logger.Warn("org policy: etag save failed (next poll re-downloads)", "err", err)
		}
	}
	c.logger.Info("org policy: bundle applied", "version", b.Version,
		"effective", "next guard construction (hooks: next tool call; daemon: next start)")
	return PolicyResult{Status: PolicyApplied, Version: b.Version}, nil
}

// PolicyPollLoop fetches the policy bundle once immediately (§14.2:
// the poll fires on `observer start`), then on every poll interval,
// until ctx is cancelled or an auth failure stops it (same loop
// contract as PushLoop). onResult, when non-nil, receives every
// successful poll outcome — the cmd layer uses it to emit R-205 on
// PolicyRejected. Transport failures ride the shared jittered backoff.
func (c *Client) PolicyPollLoop(ctx context.Context, cachePath string, onResult func(PolicyResult)) error {
	cycle := func(ctx context.Context) error {
		res, err := c.FetchPolicyBundle(ctx, cachePath)
		if errors.Is(err, ErrNotEnrolled) {
			return errIdle // not enrolled (yet, or unenrolled while running)
		}
		if err != nil {
			return err
		}
		if onResult != nil {
			onResult(res)
		}
		return nil
	}
	// Immediate first fetch; its failure classes anyway repeat through
	// the loop, so a failure here only logs (auth failures still stop).
	if err := cycle(ctx); errors.Is(err, ErrAuthFailed) {
		c.logger.Error("org policy: authentication failed, stopping policy poll", "err", err)
		return nil
	} else if err != nil && !errors.Is(err, errIdle) && !errors.Is(err, context.Canceled) {
		c.logger.Warn("org policy: initial fetch failed", "err", err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return c.runLoop(ctx, c.policyPollInterval(), cycle)
}

// policyPollInterval returns the configured poll cadence, defaulted.
func (c *Client) policyPollInterval() time.Duration {
	secs := c.cfg.PolicyPollIntervalSeconds
	if secs <= 0 {
		secs = config.DefaultPolicyPollIntervalSeconds
	}
	return time.Duration(secs) * time.Second
}

// loadKeyPin returns the pinned policy-key hash for orgURL, or ""
// when no pin row exists (pre-G13 enrolment).
func (c *Client) loadKeyPin(ctx context.Context, orgURL string) (string, error) {
	states, err := c.store.LatestGuardPolicyStates(ctx)
	if err != nil {
		return "", err
	}
	pinPath := PolicyKeyPinPath(orgURL)
	for _, st := range states {
		if st.Layer == "org" && st.Path == pinPath {
			return st.ContentHash, nil
		}
	}
	return "", nil
}

// lastBundleVersion returns the version of the last verified bundle
// for orgURL, or 0 when none was ever applied (or the recorded
// version string predates versioning and does not parse).
func (c *Client) lastBundleVersion(ctx context.Context, orgURL string) (int64, error) {
	states, err := c.store.LatestGuardPolicyStates(ctx)
	if err != nil {
		return 0, err
	}
	bundlePath := policyBundleStatePath(orgURL)
	for _, st := range states {
		if st.Layer == "org" && st.Path == bundlePath {
			v, perr := strconv.ParseInt(st.Version, 10, 64)
			if perr != nil {
				return 0, nil //nolint:nilerr // unparseable = no version baseline; the check degrades open by design
			}
			return v, nil
		}
	}
	return 0, nil
}

// writeFileAtomic writes data to path via a same-directory temp file +
// rename so the guard never reads a half-written envelope. 0600: the
// bundle is policy, not a secret, but it gates enforcement decisions —
// least privilege costs nothing here.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".org-policy-bundle-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// pinBase64Key decodes a base64url Ed25519 public key and returns its
// pin hash, validating the length. Shared by Enroll (enrol-time pin)
// and tests.
func pinBase64Key(b64 string) (string, error) {
	pub, err := base64.RawURLEncoding.DecodeString(b64)
	if err != nil {
		return "", fmt.Errorf("decode org policy public key: %w", err)
	}
	if len(pub) != 32 {
		return "", fmt.Errorf("org policy public key is %d bytes, want 32", len(pub))
	}
	return orgcontract.PublicKeyPinHash(pub), nil
}
