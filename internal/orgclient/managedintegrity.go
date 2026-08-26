package orgclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/machineid"
	"github.com/marmutapp/superbased-observer/internal/orgcontract"
)

// ReportIntegrity performs the Arc 4 P6b managed-integrity probe report (plan
// §9): it presents this host's org-salted machine fingerprint plus coarse
// tamper-EVIDENCE labels (sibling-observer origins + drifted AI-tool names) so
// the admin Control Center can surface circumvention on the node's health. The
// counts are len(siblingLabels) / len(driftedTools); only labels cross the wire
// (no paths/usernames/config values — the §9 content-floor).
//
// Called ONLY under managed tenancy (the caller gates on enr.IsManaged()); an
// individual/BYO node never runs the probe, so the individual plane sends no
// integrity signal at all — the same by-construction guarantee as BindMachine.
//
// Best-effort by design: a host with no stable machine source (empty
// fingerprint) skips the call (nothing to correlate); an older server without
// the route (404) is a no-op. Neither is an error the caller must act on.
func (c *Client) ReportIntegrity(ctx context.Context, siblingLabels, driftedTools []string) (orgcontract.ManagedIntegrityResponse, error) {
	enr, err := c.store.LoadEnrolment(ctx)
	if err != nil {
		return orgcontract.ManagedIntegrityResponse{}, fmt.Errorf("orgclient.ReportIntegrity: enrolment: %w", err)
	}
	if enr == nil {
		return orgcontract.ManagedIntegrityResponse{}, ErrNotEnrolled
	}

	mid, err := machineid.ForOrg(enr.OrgID)
	if err != nil {
		return orgcontract.ManagedIntegrityResponse{}, fmt.Errorf("orgclient.ReportIntegrity: machine id: %w", err)
	}
	if mid == "" {
		// No stable machine identity — nothing to correlate to a managed_node.
		return orgcontract.ManagedIntegrityResponse{}, nil
	}

	report := orgcontract.ManagedIntegrityReport{
		MachineIdentity:  mid,
		SiblingObservers: len(siblingLabels),
		SiblingDetail:    siblingLabels,
		RouteDrift:       len(driftedTools),
		DriftedTools:     driftedTools,
	}
	body, err := json.Marshal(report)
	if err != nil {
		return orgcontract.ManagedIntegrityResponse{}, fmt.Errorf("orgclient.ReportIntegrity: marshal: %w", err)
	}

	bearer, err := c.bearers.LoadBearer()
	if err != nil {
		return orgcontract.ManagedIntegrityResponse{}, fmt.Errorf("orgclient.ReportIntegrity: bearer: %w", err)
	}
	url := strings.TrimRight(enr.OrgServerURL, "/") + "/api/agent/managed-integrity"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return orgcontract.ManagedIntegrityResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	c.noteRenewalFromResponse(RenewalPathOther, resp, err)
	if err != nil {
		return orgcontract.ManagedIntegrityResponse{}, fmt.Errorf("orgclient.ReportIntegrity: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		var out orgcontract.ManagedIntegrityResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return orgcontract.ManagedIntegrityResponse{}, fmt.Errorf("orgclient.ReportIntegrity: decode: %w", err)
		}
		return out, nil
	case http.StatusNotFound:
		// Older server without the route — treat as a no-op.
		return orgcontract.ManagedIntegrityResponse{}, nil
	default:
		return orgcontract.ManagedIntegrityResponse{}, fmt.Errorf("orgclient.ReportIntegrity: server returned %d", resp.StatusCode)
	}
}
