package orgcontract

// SessionNetworkEventRow is the W2.2b session-scoped network-observability
// wire row (org-parity plan §4 W2.2b,
// docs/plans/org-parity-full-depth-plan-2026-08-24.md). It is the network
// sibling of SessionProcessRow: that row ships process_runs (one row per OS
// process, RAW); this row ships process_events rows of event_type
// "network_connect" (one row per observed egress call) PLUS the optional
// process_network_bodies excerpt when a plaintext capture source produced
// one, so the org session detail can render the same network-egress panel
// the node dashboard shows (grep web/src for the Network-egress panel /
// `observer process network`), scoped to one session.
//
// Per §0 of the org-parity plan (enterprise-first — the admin sees all raw),
// bodies ARE shipped: this is a DELIBERATE broadening past the W2.2 process
// row's stance (which explicitly excluded process_network_bodies as "a later
// content wave" — that later wave is this file). It ships under
// ShareOptions.shipsRawContent() (full_content or the enterprise
// admin_managed default), exactly like SessionProcessRow and OTelContentRow.
//
// HONESTY RULE (unchanged from the node-side process-network arc, see
// internal/proxy/network_capture.go and migration 067's header comment):
// body excerpts exist ONLY for traffic a plaintext capture source could see
// (currently: the Observer proxy path). Every network event ships — proxied
// or not — but a non-proxied TLS connection is metadata-only BY CAPTURE, not
// by policy, and its body fields are simply absent (HasBody=false). Never
// treat an absent body as evidence the traffic didn't happen; the metadata
// fields (Target/RemoteAddr/BytesIn/BytesOut/...) still describe it.
//
// Deferred (not carried by this row): derived §14 findings (severity/
// finding_rule_id ride as raw event fields, but the rules-engine narrative
// that produced them stays node-only), and the OS-observed
// thread/handle/security-posture columns process_runs itself doesn't have
// either (see SessionProcessRow's own deferred list).
type SessionNetworkEventRow struct {
	// OrgID / UserEmail are the agent-stamped attribution (same stamping rule
	// as every other wire row).
	OrgID     string `json:"org_id,omitempty"`
	UserEmail string `json:"user_email,omitempty"`

	// SessionID is the owning session. EventKey is the stable per-node
	// idempotency key this row upserts on: "<process_key>:<process_events.id>"
	// (internal/store/networkorgrows.go builds it). process_events has no
	// natural content key of its own (unlike process_runs' sha256 process_key
	// identity) — its local autoincrement id is stable for the life of the
	// row (rows are write-once; PersistProcessEvents never updates one), so
	// pairing it with the emitting run's process_key gives a key that is
	// stable across re-pushes of the same trailing window and collision-safe
	// across nodes/orgs (process_key itself is content-derived).
	SessionID string `json:"session_id"`
	EventKey  string `json:"event_key"`

	// RunKey mirrors SessionProcessRow.RunKey's semantics (the emitting
	// process_events.process_key) so the org UI can join a network event back
	// to its process tree row. It is BEST-EFFORT: proxy-captured events are
	// emitted with a synthetic process_key ("proxy:"+hash(request_id)) that
	// was never written to process_runs (see
	// internal/proxy/network_capture.go::captureProcessNetwork), so it will
	// often NOT resolve to any SessionProcessRow.RunKey — the join is
	// opportunistic, not guaranteed.
	RunKey string `json:"run_key,omitempty"`

	// Timestamp is RFC3339 UTC, the event's own timestamp column.
	Timestamp string `json:"timestamp"`

	// Tool / ActionID / TurnIndex are the same §9.2.4 spawning-message link
	// carried on process_events, mirroring SessionProcessRow's fields.
	Tool      string `json:"tool,omitempty"`
	ActionID  int64  `json:"action_id,omitempty"`
	TurnIndex int64  `json:"turn_index,omitempty"`

	// TargetKind / Target / TargetHash / Severity / FindingRuleID are the
	// node's own process_events columns, RAW, carried as-is. For the proxy
	// capture path Target is the request URL; for the OS-observed path it is
	// the resolved remote host (or address when no hostname was available) —
	// see internal/processobs/observer.go::networkTarget.
	TargetKind    string `json:"target_kind,omitempty"`
	Target        string `json:"target,omitempty"`
	TargetHash    string `json:"target_hash,omitempty"`
	Severity      string `json:"severity,omitempty"`
	FindingRuleID string `json:"finding_rule_id,omitempty"`

	// CaptureSource discriminates the two node-side backends that write
	// network_connect events, mirroring the honesty discriminator
	// store.NetworkSummaryForSession already uses server-side... except here
	// it is node-side: "proxy" (internal/proxy/network_capture.go — can see
	// plaintext, HTTP-shaped fields below are populated) or
	// "process_backend" (internal/processobs/observer.go's eBPF/poll
	// backend — socket-shaped fields below are populated, body is never
	// available). Provider is the proxy's upstream provider name
	// (anthropic/openai/gemini/...), proxy-only.
	CaptureSource string `json:"capture_source,omitempty"`
	Provider      string `json:"provider,omitempty"`

	// HTTP-shaped fields — populated for CaptureSource=="proxy" ONLY,
	// sourced from the event's own details (present even when body capture
	// is disabled — these are metadata, not body). ErrorMessage is the
	// (truncated) upstream transport error when the call failed outright.
	Method       string `json:"method,omitempty"`
	URL          string `json:"url,omitempty"`
	Host         string `json:"host,omitempty"`
	StatusCode   int64  `json:"status_code,omitempty"`
	DurationMs   int64  `json:"duration_ms,omitempty"`
	Stream       bool   `json:"stream,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`

	// Socket-shaped fields — populated for CaptureSource=="process_backend"
	// ONLY (the OS-observed egress backend has no HTTP visibility, only
	// connection-level metadata). NetworkStatus is the backend's own
	// connection-state string (e.g. "established"/"closed"), distinct from
	// StatusCode (an HTTP status, proxy-only).
	Protocol      string `json:"protocol,omitempty"`
	Family        string `json:"family,omitempty"`
	RemoteAddr    string `json:"remote_addr,omitempty"`
	RemotePort    int64  `json:"remote_port,omitempty"`
	LocalAddr     string `json:"local_addr,omitempty"`
	LocalPort     int64  `json:"local_port,omitempty"`
	NetworkStatus string `json:"network_status,omitempty"`
	BytesIn       int64  `json:"bytes_in,omitempty"`
	BytesOut      int64  `json:"bytes_out,omitempty"`

	// BodyUnavailableReason explains an absent body honestly — sourced from
	// the body row's own column when HasBody, else from the event's details
	// (e.g. "metadata_only_non_plaintext" for OS-observed egress,
	// "body_capture_disabled" for a proxied call with capture off).
	BodyUnavailableReason string `json:"body_unavailable_reason,omitempty"`

	// HasBody reports whether a process_network_bodies row exists for this
	// event. Per the honesty rule above: false is a genuine, expected outcome
	// for the vast majority of events (anything not observer-proxied), not a
	// data-loss signal.
	HasBody bool `json:"has_body"`

	// The remaining fields are populated ONLY when HasBody — the node's
	// process_network_bodies columns, RAW (request/response excerpts are
	// already capped + scrubbed at node capture time, see
	// internal/proxy/network_capture.go::capBody; this row ships that
	// excerpt as-is, not a further reduction).
	APITurnID             int64  `json:"api_turn_id,omitempty"`
	RequestID             string `json:"request_id,omitempty"`
	RequestHeadersJSON    string `json:"request_headers_json,omitempty"`
	ResponseHeadersJSON   string `json:"response_headers_json,omitempty"`
	RequestBody           string `json:"request_body,omitempty"`
	RequestBodySHA256     string `json:"request_body_sha256,omitempty"`
	RequestBodyBytes      int64  `json:"request_body_bytes,omitempty"`
	RequestBodyTruncated  bool   `json:"request_body_truncated,omitempty"`
	ResponseBody          string `json:"response_body,omitempty"`
	ResponseBodySHA256    string `json:"response_body_sha256,omitempty"`
	ResponseBodyBytes     int64  `json:"response_body_bytes,omitempty"`
	ResponseBodyTruncated bool   `json:"response_body_truncated,omitempty"`
	ResponseContentType   string `json:"response_content_type,omitempty"`
}
