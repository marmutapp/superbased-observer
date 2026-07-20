package orgclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
	"github.com/marmutapp/superbased-observer/internal/orgserver/routingpolicy"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// FetchRoutingPolicy pulls the org's latest routing policy (§R19.1)
// over the enrolment bearer and caches it node-side after verifying:
//
//   - body hash matches,
//   - the Ed25519 signature verifies against the PINNED server key —
//     TOFU: the first received key is pinned with the cache row; a
//     later key change is REFUSED loudly (re-enrol to rotate trust).
//
// Caching a policy never enables anything (§R23): the composer ignores
// enabled/mode keys; the node's own [routing] config is the only
// enforce switch. Returns (false, nil) when no policy is published.
func (c *Client) FetchRoutingPolicy(ctx context.Context) (bool, error) {
	enr, err := c.store.LoadEnrolment(ctx)
	if err != nil {
		return false, fmt.Errorf("orgclient.FetchRoutingPolicy: enrolment: %w", err)
	}
	if enr == nil {
		return false, ErrNotEnrolled
	}
	bearer, err := c.bearers.LoadBearer()
	if err != nil {
		return false, fmt.Errorf("orgclient.FetchRoutingPolicy: bearer: %w", err)
	}
	url := strings.TrimRight(enr.OrgServerURL, "/") + "/api/agent/routing-policy"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("orgclient.FetchRoutingPolicy: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil // no policy published — fine
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("orgclient.FetchRoutingPolicy: server returned %d", resp.StatusCode)
	}
	var doc orgcontract.RoutingPolicyDoc
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return false, fmt.Errorf("orgclient.FetchRoutingPolicy: decode: %w", err)
	}

	cached, hasCached, err := c.store.GetOrgRoutingPolicy(ctx)
	if err != nil {
		return false, err
	}
	pinned := doc.PublicKey // TOFU on first receipt
	if hasCached {
		if cached.ServerPubkey != doc.PublicKey {
			return false, fmt.Errorf("orgclient.FetchRoutingPolicy: server policy key CHANGED (pinned %s…, got %s…) — refusing; re-enrol to rotate trust",
				prefix8(cached.ServerPubkey), prefix8(doc.PublicKey))
		}
		pinned = cached.ServerPubkey
		if cached.Version >= doc.Version {
			return false, nil // already current
		}
	}
	if err := routingpolicy.Verify(doc, pinned); err != nil {
		return false, fmt.Errorf("orgclient.FetchRoutingPolicy: %w", err)
	}
	if err := c.store.UpsertOrgRoutingPolicy(ctx, store.OrgRoutingPolicyRow{
		Version: doc.Version, Body: doc.Body, BodyHash: doc.BodyHash,
		Signature: doc.Signature, ServerPubkey: pinned, ReceivedAt: time.Now().UTC(),
	}); err != nil {
		return false, err
	}
	c.logger.Info("org routing policy cached", "version", doc.Version, "hash", doc.BodyHash[:12])
	return true, nil
}

func prefix8(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
