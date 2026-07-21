package m365copilotanalytics

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Rail B — Office 365 Management Activity API (the metadata governance rail).
//
// SCAFFOLDED, NOT COMPLETE. The full Management Activity flow is a THREE-STEP
// subscription protocol that a live tenant + an Entra app with
// ActivityFeed.Read is required to exercise end-to-end; it is documented here and
// left with a clear TODO. Rail A (surface_graph.go) ships complete and is the
// content rail; Rail B is complementary METADATA (no prompt/response text).
//
// The flow (manage.office.com; NOT graph.microsoft.com):
//
//  1. Start a subscription:
//       POST /api/v1.0/{tenantId}/activity/feed/subscriptions/start?contentType=Audit.General
//     (idempotent; Copilot events ride the Audit.General content type).
//  2. List available content blobs for a window:
//       GET  /api/v1.0/{tenantId}/activity/feed/subscriptions/content
//            ?contentType=Audit.General&startTime=...&endTime=...
//     returns {contentUri} entries; page via the NextPageUri response HEADER.
//  3. Fetch + filter each blob:
//       GET  {contentUri}  → an array of audit records; keep only
//            RecordType == 261 (CopilotInteraction), Workload == "Copilot".
//
// Each kept record carries METADATA ONLY (parsePurviewRecord below documents the
// shape): AppHost, Contexts, ThreadId, a Messages[] array of message ids +
// isPrompt bools, AccessedResources (with sensitivity labels), and a grounding
// flag (AISystemPlugin.Id == "BingWebSearch"). There is NO body content — reading
// the transcript from audit alone needs DSPM-for-AI / eDiscovery.
//
// KNOWN LIMITATION (proposal §4.1, R9): the 2025 "Pistachio" incident — a
// prompt-phrasing variant omitted AccessedResources; MS silently server-patched,
// pre-fix data unrecoverable. This is why Rail A (content) and Rail B (metadata)
// are NOT interchangeable and we build both.
//
// TODO(m365-rail-b): implement steps 1–3 against a live tenant (subscription
// start is idempotent-safe to call each poll; the content-list NextPageUri lives
// in a response header, unlike Graph's body @odata.nextLink; blob fetch needs the
// SAME app registration with ActivityFeed.Read). Until then pollPurview returns no
// rows so a mis-enabled purview surface is a no-op, never an error.

// pollPurview is the Rail B poll entry. SCAFFOLDED: returns no rows (see the
// package-level TODO). The record parser + shape are implemented and unit-tested
// so the wire format is locked; only the subscription/blob-listing transport is
// deferred to a live-tenant spike.
func pollPurview(_ context.Context, _ *Poller, _ window) ([]DailyMetric, []ContentRow, error) {
	// Intentionally a no-op until the subscription/content-blob flow is wired
	// against a live tenant. Returning (nil, nil, nil) keeps a mis-enabled
	// surface harmless.
	return nil, nil, nil
}

// copilotRecordType is the Management Activity RecordType for a Copilot
// interaction audit record.
const copilotRecordType = 261

// purviewAuditRecord is one Office 365 Management Activity audit record for a
// Copilot interaction (RecordType 261). Metadata ONLY — no body content. This
// struct + parsePurviewRecord are the ONLY place that knows this rail's schema.
type purviewAuditRecord struct {
	ID               string `json:"Id"`
	RecordType       int    `json:"RecordType"`
	Workload         string `json:"Workload"`
	CreationTime     string `json:"CreationTime"`
	UserID           string `json:"UserId"`
	CopilotEventData struct {
		AppHost  string `json:"AppHost"`
		ThreadID string `json:"ThreadId"`
		Contexts []struct {
			ID   string `json:"Id"`
			Type string `json:"Type"`
		} `json:"Contexts"`
		Messages []struct {
			ID       string `json:"Id"`
			IsPrompt bool   `json:"isPrompt"`
		} `json:"Messages"`
		AccessedResources []struct {
			ID                 string `json:"Id"`
			Name               string `json:"Name"`
			SensitivityLabelID string `json:"SensitivityLabelId"`
		} `json:"AccessedResources"`
		AISystemPlugin []struct {
			ID string `json:"Id"`
		} `json:"AISystemPlugin"`
	} `json:"CopilotEventData"`
}

// parsePurviewRecord maps one CopilotInteraction audit record into metadata
// metrics for (day, user, appHost). Records that are not RecordType 261 /
// Workload Copilot are skipped. Implemented + unit-tested even though the
// transport that FEEDS it is scaffolded, so the shape is locked.
func parsePurviewRecord(raw []byte) ([]DailyMetric, error) {
	var rec purviewAuditRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("m365copilotanalytics: parse purview record: %w", err)
	}
	if rec.RecordType != copilotRecordType || !strings.EqualFold(rec.Workload, "Copilot") {
		return nil, nil
	}
	day := utcDayFromTimestamp(rec.CreationTime)
	userKey := rec.UserID
	if day == "" || userKey == "" {
		return nil, nil
	}
	appClass := classifyAppClass(rec.CopilotEventData.AppHost)

	grounded := 0.0
	for _, pl := range rec.CopilotEventData.AISystemPlugin {
		if strings.EqualFold(pl.ID, "BingWebSearch") {
			grounded = 1
			break
		}
	}

	return []DailyMetric{
		emitMetric(day, userKey, ActorUser, SurfacePurview, appClass, UnitCount, MetricGovInteractions, 1),
		emitMetric(day, userKey, ActorUser, SurfacePurview, appClass, UnitCount, MetricAccessedResources, float64(len(rec.CopilotEventData.AccessedResources))),
		emitMetric(day, userKey, ActorUser, SurfacePurview, appClass, UnitCount, MetricGroundedInteractions, grounded),
	}, nil
}
