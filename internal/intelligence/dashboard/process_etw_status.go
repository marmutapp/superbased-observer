package dashboard

import (
	"net/http"
	"time"

	"github.com/marmutapp/superbased-observer/internal/diag"
	"github.com/marmutapp/superbased-observer/internal/processbridge/setup"
	"github.com/marmutapp/superbased-observer/internal/processobs"
)

// ETW setup states, as the dashboard card sees them. They are the
// setup.Outcome vocabulary spelled as stable machine tokens, so the frontend
// switches on a string this file owns rather than on an integer whose meaning
// lives in another package.
//
// There are FIVE and only five; anything this build cannot map is reported as
// "the plan could not be produced" (see etwStatusResponse.PlanDetectable),
// never squeezed into the nearest-looking one.
const (
	etwStateSkip    = "skip"
	etwStatePresent = "present"
	etwStateManual  = "manual"
	etwStateUnknown = "unknown"
	etwStateBlocked = "blocked"
)

// Sub-reasons for etwStateSkip. The two are completely different situations
// and the card renders them differently — one offers to turn the feature on,
// the other renders nothing at all because this is not a Windows host — so the
// state alone is not enough.
const (
	etwSkipDisabled  = "etw_disabled"
	etwSkipNoWindows = "no_schtasks"
)

// Probe tri-state, as the wire spells it.
const (
	etwProbeUnknown = "unknown"
	etwProbeAbsent  = "absent"
	etwProbePresent = "present"
)

// etwStatusResponse is the GET /api/process/etw/status wire shape: the
// elevated-capturer setup plan as structured facts, plus whatever the running
// daemon has published about the capturer link.
//
// It reports STATES, never errors — the Tailscale-status precedent — with one
// exception that is itself a state: PlanDetectable false, which says plainly
// that no plan could be produced and why. Squeezing "we could not read the
// config" into `unknown` would be a claim about the schtasks probe that this
// endpoint never ran.
type etwStatusResponse struct {
	// TaskName is the frozen Scheduled Task name, so the card and the
	// operator's own `schtasks /Query` cannot drift.
	TaskName string `json:"task_name"`

	// SchtasksPresent reports whether this host has a Windows Task Scheduler
	// CLI at all — a native Windows box, or WSL with interop. It answers "does
	// this feature apply here?" INDEPENDENTLY of State, and it is the field a
	// surface uses to decide whether to render at all.
	//
	// It exists because State cannot answer that. PlanTask's gate order checks
	// the feature being OFF first (which is the actionable reason on Windows),
	// so the default install — ETW disabled — reports skip_reason
	// "etw_disabled" on a Windows host AND on a Linux laptop alike. Without
	// this field a card would have to either offer a Windows-only feed to every
	// Linux user or suppress it for every Windows user; neither is honest.
	//
	// FALSE is also what a build with no probe seam reports. "We did not look"
	// resolves to "this does not apply", which renders nothing — the quiet
	// default, never a fabricated Windows host.
	SchtasksPresent bool `json:"schtasks_present"`

	// PlanDetectable reports whether a plan was produced AT ALL. False means
	// the config could not be read (or the dashboard has no config path), so
	// State is empty and every field below it is unset. It is the
	// `serve_detectable` pattern from the Tailscale card: a thing probed
	// unreliably gets its own boolean rather than a guessed value.
	PlanDetectable bool `json:"plan_detectable"`
	// PlanUndetectableReason names what stopped the plan, verbatim.
	PlanUndetectableReason string `json:"plan_undetectable_reason,omitempty"`

	// State is one of the five etwState* tokens, or "" when PlanDetectable is
	// false.
	State string `json:"state,omitempty"`
	// SkipReason distinguishes the two skips (etwSkipDisabled /
	// etwSkipNoWindows) and is empty in every other state.
	SkipReason string `json:"skip_reason,omitempty"`

	// Enabled reports whether the elevated feed can actually run: BOTH
	// [observer.process].enabled (the master switch for the whole
	// process-capture subsystem, without which no accept listener is ever
	// constructed) AND [observer.process.etw].enabled. The card's toggle
	// writes both, which is why one boolean is enough here.
	Enabled bool `json:"enabled"`
	// ListenAddr is the daemon's configured accept-listener bind. The address
	// the capturer must DIAL is embedded in Command (a wildcard bind is not
	// something an operator can type into --connect).
	ListenAddr string `json:"listen_addr,omitempty"`

	// Probe is the tri-state result of the read-only `schtasks /Query`, and
	// ProbeError carries the reason for etwProbeUnknown.
	//
	// UNKNOWN IS THE DEFAULT, mirroring setup.ProbeUnknown being the zero
	// value of the tri-state. "Absent" is the state that makes the card offer
	// to register the task, so a value nobody set must never land there.
	Probe string `json:"probe"`
	// ProbeError is the verbatim reason the probe could not answer. Empty
	// unless Probe is etwProbeUnknown.
	ProbeError string `json:"probe_error,omitempty"`

	// Command is the fully-resolved elevated schtasks line (manual/unknown
	// only). Empty everywhere else — there is deliberately no placeholder
	// command for a blocked plan.
	Command string `json:"command,omitempty"`
	// CommandCmdShellOnly marks a Command that parses in cmd.exe but NOT in
	// PowerShell (a token path containing a space forces the escaped form).
	// The card must then name the shell instead of saying "either".
	CommandCmdShellOnly bool `json:"command_cmd_shell_only"`
	// Reason names the exact missing dependency (blocked) or the probe
	// failure (unknown). Never vague — it is what the operator must fix.
	Reason string `json:"reason,omitempty"`
	// Notes are the planner's per-plan caveats (which account must own the
	// task, how to check a \\wsl.localhost token file is reachable).
	Notes []string `json:"notes,omitempty"`

	// LocalDetailWithheld marks this response as the REMOTE-FACING PROJECTION:
	// it was served to a remotely-exposed caller, so every field describing
	// the operator's machine — the command, the notes, and each reason that
	// quotes a filesystem path — has been dropped, and the card's setup
	// actions cannot be driven from here either (they are capability-Local
	// routes; a UAC prompt is a local physical act).
	//
	// It is stated rather than left implicit because an absent Command is
	// otherwise indistinguishable from a state that has no command, and a card
	// that silently shows nothing — or offers a button that will 403 — would be
	// the honest-disabled-copy rule broken. It is TRUE for every remote
	// response, including one where nothing needed dropping, because what it
	// reports is which projection the caller is looking at.
	LocalDetailWithheld bool `json:"local_detail_withheld,omitempty"`

	// Health is what the RUNNING daemon last published about the capturer
	// link, or null when no live daemon has published anything — which is the
	// normal state when the daemon is not running or process capture is off,
	// not a failure. HealthReason says which.
	Health *etwHealth `json:"health"`
	// HealthReason explains a null Health.
	HealthReason string `json:"health_reason,omitempty"`
}

// etwHealth is the daemon-published half: process-observability runtime
// health, read back out of the per-PID record beside the DB (the daemon, the
// dashboard and `observer metrics` are separate processes and cannot read each
// other's memory).
//
// Every present-tense fact below is qualified by Stale + AgeSeconds, because
// the record is a REPORT: a daemon that stopped refreshing minutes ago cannot
// support "a capturer is connected", only "a capturer was connected as of its
// last report".
type etwHealth struct {
	// PID is the daemon that wrote the record; the reader has already dropped
	// records from dead PIDs.
	PID int `json:"pid"`
	// ReportedAt is when the daemon sampled its health (UTC).
	ReportedAt time.Time `json:"reported_at"`
	// AgeSeconds is how old that report is, and Stale is diag's own
	// classification of it (three missed refreshes). Everything else in this
	// struct is present-tense and must be rendered as history when Stale.
	AgeSeconds float64 `json:"age_seconds"`
	Stale      bool    `json:"stale"`

	// Backend / BackendUp identify what is actually capturing.
	Backend   string `json:"backend"`
	BackendUp bool   `json:"backend_up"`

	// NetworkAccountingMode is "off" / "unavailable" / "tcp"; the reason is
	// carried verbatim because "unavailable" without it is not actionable.
	NetworkAccountingMode   string `json:"network_accounting_mode"`
	NetworkAccountingReason string `json:"network_accounting_reason,omitempty"`

	// TransportState is the honesty gate on Transport below: "none" (no
	// dial-in transport was requested — the 99% install), "unavailable" (one
	// was requested and could not be created) or "configured" (the counters
	// are real, INCLUDING when they are all zero).
	TransportState string `json:"transport_state"`
	// TransportUnavailableReason explains "unavailable", verbatim.
	TransportUnavailableReason string `json:"transport_unavailable_reason,omitempty"`
	// Transport is non-null ONLY when TransportState is "configured". A
	// transport that does not exist must not render as one with zero
	// connections — absent, zero and broken are three different facts.
	Transport *etwTransport `json:"transport"`
	// TransportLine is diag's own single-owner prose for the transport state,
	// already staleness-qualified. The card may show it verbatim; it never
	// asserts a cause the daemon did not record.
	TransportLine string `json:"transport_line,omitempty"`
}

// etwTransport is the capturer link's counters, meaningful only under a
// "configured" transport state.
type etwTransport struct {
	// Addr is the endpoint the capturer must dial.
	Addr string `json:"addr,omitempty"`
	// Connections counts capturers that connected AND authenticated.
	Connections int64 `json:"connections"`
	// AuthFailures counts connections refused at the handshake FOR ANY
	// REASON: a wrong shared token, a protocol version this daemon does not
	// speak, a malformed opening line, or an unrelated Windows-host process
	// probing the port — WSL2's localhostForwarding exposes this loopback
	// bind to the whole Windows host.
	//
	// THE COUNT NAMES NO CAUSE. A surface that turns it into "the token is
	// wrong" is asserting a diagnosis nothing measured. LastAuthErrorClass
	// and the verbatim LastAuthError are the only cause-bearing fields, and
	// both come from what the daemon actually recorded.
	AuthFailures int64 `json:"auth_failures"`
	// LastAuthError is the daemon's VERBATIM record of the most recent
	// refusal. Absent when none was recorded — never a substituted guess.
	LastAuthError string `json:"last_auth_error,omitempty"`
	// LastAuthErrorClass is its bounded classification (token_mismatch /
	// protocol_version / malformed / transport / unknown). Absent until a
	// refusal actually happens; "unknown" is a real answer for a refusal
	// recorded without one, not a filler for "no refusal".
	LastAuthErrorClass string `json:"last_auth_error_class,omitempty"`
	// Connected reports whether a capturer is streaming as of the report.
	Connected bool `json:"connected"`
	// LastConnectAt / LastDisconnectAt are POINTERS so a never-happened
	// timestamp is ABSENT rather than epoch 0 — a capturer that never
	// connected must not read as "connected in 1970". Both present with
	// Connected false is a capturer that came and went.
	LastConnectAt    *time.Time `json:"last_connect_at,omitempty"`
	LastDisconnectAt *time.Time `json:"last_disconnect_at,omitempty"`
	// CapturerDecode is the connected capturer's own decoder health, or null
	// when it has never reported. Null is NOT "zero events were refused":
	// a capturer with no running network decoder (every non-elevated run)
	// reports nothing, and rendering that as a clean zero would claim the
	// payload-length assumptions were exercised and held.
	CapturerDecode *etwCapturerDecode `json:"capturer_decode"`
}

// etwCapturerDecode is the single most important validation signal the whole
// ETW feature has: a non-zero Dropped means the capturer's fixed-offset
// payload-length assumption does not hold on that host, so the per-process
// byte totals are WRONG rather than merely missing.
type etwCapturerDecode struct {
	// Dropped counts network data events the capturer's decoder refused as
	// short or unexpectedly shaped.
	Dropped int64 `json:"dropped"`
	// UnsupportedVersion counts events refused because the OS stamped an
	// event version the capturer's layout table does not describe — the "a
	// new template shipped" signal, broken out because its fix differs.
	UnsupportedVersion int64 `json:"unsupported_version"`
	// Decoded counts data events the decoder ACCEPTED and counted bytes
	// from; Ignored counts events it classified as not-a-data-event
	// (control-plane, connect/disconnect/retransmit, UDP).
	//
	// A large Ignored is NORMAL — it is not a fault counter. Decoded is the
	// one that says the decoder measured anything at all, because every
	// per-process byte total comes from an accepted event.
	Decoded int64 `json:"decoded"`
	Ignored int64 `json:"ignored"`
	// Healthy is true when both REFUSAL counters are zero — "the decoder
	// refused nothing". It is deliberately NOT widened to mean "the decode is
	// fine", because that would silently change what an existing consumer
	// reads out of this field; NothingClassified is the separate fact.
	//
	// HEALTHY TRUE IS NOT A PASS ON ITS OWN. Read it beside
	// NothingClassified and Decoded: healthy true with NothingClassified true
	// is a decoder that refused nothing because it accepted nothing.
	Healthy bool `json:"healthy"`
	// NothingClassified is the renumbered-provider signature: events arrived,
	// none was classified as data, and none was refused. It is a derived
	// field rather than a job left to the caller because the three counters
	// are individually unalarming and only their conjunction means anything
	// — shipping raw numbers a reader cannot interpret is the failure this
	// field exists to close.
	//
	// It is a SUSPICION. On a host that was moving TCP traffic it means the
	// provider's event ids no longer match this build's layout table; on an
	// idle host it means the feed has not yet demonstrated that it decodes
	// anything. Neither is a pass.
	NothingClassified bool `json:"nothing_classified"`
	// ReportedAt is when the DAEMON received the report (stamped locally,
	// never taken from the wire).
	ReportedAt time.Time `json:"reported_at"`
	// Line is diag's prose for the failure case, empty when healthy.
	Line string `json:"line,omitempty"`
}

// handleProcessETWStatus serves GET /api/process/etw/status — the detection
// half of the dashboard-driven elevated-ETW-capturer setup (plan §E2).
//
// It is capability VIEW: it reads config, runs the SAME read-only
// `schtasks /Query` probe `observer init` runs, and reads the daemon's own
// published health record. It registers nothing and writes nothing — the
// elevation broker is a separate, Local + confirm-token route.
//
// IT IS NOT SIDE-EFFECT-FREE, and the earlier claim that it "spawns nothing"
// was wrong. On an applicable host every GET execs `schtasks.exe /Query`, and
// resolving an ABSENT task additionally shells out to cmd.exe for the Windows
// user name. Since capability VIEW is reachable by a paired REMOTE device,
// that is a remote-drivable local process execution — so the probes run
// through a short TTL cache (etwProbeCache) which bounds the exec rate no
// matter how many devices poll or how fast. The elevation broker deliberately
// does NOT share that cache; see planETWRegistration.
//
// WHAT IT DISCLOSES IS CALLER-DEPENDENT. The command and the notes carry the
// observer.exe path, the shared-token file path and the Windows user name —
// owner-local facts a paired remote viewer has no business reading. A
// remote-exposed caller therefore gets the state ladder and the health block,
// with those fields withheld and SAID to be withheld (withholdLocalDetail).
//
// Before this existed, the only path by which any of these facts reached a
// browser was the free-text details[] of GET /api/health/doctor, which nothing
// parses.
func (s *Server) handleProcessETWStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	// Listener provenance, resolved at the boundary (never from a request
	// field) — the same signal the /ws/launch writer bridge uses.
	remote := remoteExposedFromContext(r.Context())
	// ONE exit point for the health attach and the disclosure policy, so a
	// future early return cannot skip either. Both were previously repeated at
	// four call sites, which is exactly how one gets forgotten.
	respond := func(resp etwStatusResponse) {
		s.attachETWHealth(&resp)
		if remote {
			resp.withholdLocalDetail()
		}
		writeJSON(w, resp)
	}
	env := s.etwSetupEnv()
	resp := etwStatusResponse{
		TaskName: setup.TaskName,
		// The zero value of the tri-state is UNKNOWN, here as in the planner.
		// A response assembled down a path that never probed must not claim
		// the task is absent — absent is the state that makes the card offer
		// to register it.
		Probe: etwProbeUnknown,
	}

	if s.opts.ConfigPath == "" {
		resp.SchtasksPresent = setup.HasSchtasks(env)
		resp.PlanUndetectableReason = "this dashboard was started without a config path, so the ETW " +
			"setup plan cannot be composed (it needs the listen address and token path from config.toml)"
		respond(resp)
		return
	}
	cfg, err := loadConfigForDashboard(s.opts.ConfigPath)
	if err != nil {
		resp.SchtasksPresent = setup.HasSchtasks(env)
		resp.PlanUndetectableReason = "config.toml at " + s.opts.ConfigPath + " could not be read: " + err.Error()
		respond(resp)
		return
	}

	// The probes (a schtasks exec, a cmd.exe exec, a PATH lookup) run behind
	// the rate-bounding cache; the config that keys it is re-read every time,
	// so a config write — the card's own "turn it on" button — is never served
	// stale.
	probed := s.etwProbe(r.Context(), cfg, env)
	in := probed.inputs
	// Resolved on EVERY path, including the ones that produce no plan: "does
	// this host have a Task Scheduler" is independent of whether we could read
	// config, and it is what decides whether a Windows-only surface should
	// render at all.
	resp.SchtasksPresent = probed.schtasksPresent
	plan := setup.PlanTask(in)

	state, ok := etwStateOf(plan.Outcome)
	if !ok {
		// An outcome this build does not map. Say so; do not round it to the
		// nearest-looking state, which is how a "we do not know" becomes a
		// confident instruction.
		resp.PlanUndetectableReason = "the setup planner returned an outcome this build does not recognise"
		respond(resp)
		return
	}

	resp.PlanDetectable = true
	resp.State = state
	resp.Enabled = in.Enabled
	resp.ListenAddr = in.ListenAddr
	resp.Probe = etwProbeOf(in.Probe)
	if resp.Probe == etwProbeUnknown {
		resp.ProbeError = in.ProbeErr
	}
	resp.Command = plan.Command
	resp.CommandCmdShellOnly = plan.CmdShellOnly
	resp.Reason = plan.Reason
	resp.Notes = plan.Notes
	if state == etwStateSkip {
		// Mirror PlanTask's own gate order: the feature being off is checked
		// first, so a host that is neither Windows nor enabled reports the
		// actionable reason rather than "not a Windows host".
		//
		// in.Enabled is the CONJUNCTION of the two switches, so the master
		// switch being off lands here too — correctly: the card's toggle
		// writes both, and a feed whose subsystem never starts is off no
		// matter which of the two keys says so.
		if !in.Enabled {
			resp.SkipReason = etwSkipDisabled
		} else {
			resp.SkipReason = etwSkipNoWindows
		}
	}

	respond(resp)
}

// withholdLocalDetail strips the fields that describe the OPERATOR'S MACHINE
// rather than the feature's state, for a response being served to a
// remote-exposed caller (review finding 6).
//
// WHAT GOES: the elevated command (it embeds the observer.exe path AND the
// shared-token file path), the planner notes (Windows user name, token path),
// and every free-text reason that quotes a path verbatim — the blocked reason
// names the path it tried, the plan-undetectable reason names config.toml, the
// probe error is raw schtasks output, and the transport-unavailable reason can
// carry the token-file path out of the daemon's own startup error. None of
// them is the token's CONTENTS; all of them are filesystem layout and identity.
//
// WHAT STAYS: the whole state ladder (state, skip reason, probe tri-state,
// schtasks presence, enabled) and the health block's counters and timestamps.
// A remote user reading "capturer connected, 0 dropped, 3 handshakes refused"
// is exactly the legitimate remote use of this endpoint, and none of it names
// a path or a person.
//
// It SAYS it applied (LocalDetailWithheld) rather than silently emitting less.
// The flag is set unconditionally, not only when something was actually
// dropped: it tells the card WHICH PROJECTION it is rendering, and the card
// needs that even in a state that carries no command — the "turn the feature
// on" toggle it would otherwise offer writes config through a
// capability-Local route that a remote caller cannot drive, and a button that
// can only 403 is worse than an honest sentence.
func (r *etwStatusResponse) withholdLocalDetail() {
	r.Command = ""
	r.CommandCmdShellOnly = false // meaningless without a command to run
	r.Notes = nil
	r.Reason = ""
	r.ProbeError = ""
	r.PlanUndetectableReason = ""
	if h := r.Health; h != nil {
		h.TransportUnavailableReason = ""
		h.NetworkAccountingReason = ""
		// TransportLine is diag's prose and is kept verbatim — EXCEPT in the
		// one state that embeds the reason we just withheld. Re-deriving a
		// shortened sentence here would make this a second owner of diag's
		// wording; dropping it lets the card fall back to its own
		// transport_state === "unavailable" branch, which already renders
		// without a reason.
		if h.TransportState == processobs.TransportStateUnavailable {
			h.TransportLine = ""
		}
	}
	r.LocalDetailWithheld = true
}

// etwSetupEnv returns the I/O seam the planner probes through. Injected as a
// field so handler tests never touch the real Task Scheduler (the
// browserhost-tests-hit-the-real-registry class of mistake); nil falls back to
// the production probes.
func (s *Server) etwSetupEnv() setup.Env {
	if s.etwSetupEnvFn != nil {
		return s.etwSetupEnvFn()
	}
	return setup.ProductionEnv()
}

// etwStateOf maps a planner outcome onto the wire vocabulary. ok=false for an
// outcome this build does not define — reported as "no plan", never rounded to
// a neighbour.
func etwStateOf(o setup.Outcome) (string, bool) {
	switch o {
	case setup.OutcomeSkip:
		return etwStateSkip, true
	case setup.OutcomePresent:
		return etwStatePresent, true
	case setup.OutcomeManual:
		return etwStateManual, true
	case setup.OutcomeUnknown:
		return etwStateUnknown, true
	case setup.OutcomeBlocked:
		return etwStateBlocked, true
	default:
		return "", false
	}
}

// etwProbeOf maps the probe tri-state onto the wire vocabulary.
//
// The DEFAULT ARM IS UNKNOWN, and that is the whole point: setup.ProbeUnknown
// is the tri-state's zero value precisely so a forgotten assignment cannot
// report "the task does not exist" (which is the state that makes the card
// offer to create one). A default of "absent" here would reintroduce that bug
// one layer up, where the planner's care would be invisible.
func etwProbeOf(p setup.Probe) string {
	switch p {
	case setup.ProbeAbsent:
		return etwProbeAbsent
	case setup.ProbePresent:
		return etwProbePresent
	case setup.ProbeUnknown:
		return etwProbeUnknown
	default:
		return etwProbeUnknown
	}
}

// attachETWHealth reads the running daemon's published process-observability
// record and projects it onto the response. It is called on EVERY path,
// including the ones that could not produce a plan: whether a capturer is
// connected is independent of whether we could compose a setup command, and an
// operator debugging a refused handshake needs it either way.
func (s *Server) attachETWHealth(resp *etwStatusResponse) {
	dir := diag.ProcessHealthDir(s.opts.DBPath)
	if dir == "" {
		resp.HealthReason = "this dashboard was started without a database path, so the daemon's " +
			"process-observability health record cannot be located"
		return
	}
	h, ok := diag.LatestProcessHealth(dir)
	if !ok {
		resp.HealthReason = "no running daemon has published a process-observability health record — " +
			"normal when the daemon is not running or process capture is disabled, and not a failure of the feature"
		return
	}
	now := s.now()
	out := &etwHealth{
		PID:                        h.PID,
		ReportedAt:                 h.WrittenAt,
		AgeSeconds:                 h.Age(now).Seconds(),
		Stale:                      h.Stale(now),
		Backend:                    h.Backend,
		BackendUp:                  h.BackendUp,
		NetworkAccountingMode:      h.NetworkAccountingMode,
		NetworkAccountingReason:    h.NetworkAccountingReason,
		TransportState:             h.TransportState,
		TransportUnavailableReason: h.TransportUnavailableReason,
		// diag owns the wording, and TransportLine already prefixes a stale
		// record with "as of the daemon's last report … (STALE)". Re-deriving
		// the sentence here would be a second owner of the same prose.
		TransportLine: h.TransportLine(now),
	}
	// A record written by an older daemon carries no transport key at all; it
	// decodes empty, and empty must read as "none" rather than as a broken
	// transport with zero connections.
	if out.TransportState == "" {
		out.TransportState = processobs.TransportStateNone
	}
	if h.TransportConfigured() {
		out.Transport = etwTransportOf(h)
	}
	resp.Health = out
}

// etwTransportOf projects the configured transport's counters. Called ONLY
// under a configured transport state — see etwHealth.Transport.
func etwTransportOf(h diag.ProcessHealth) *etwTransport {
	t := &etwTransport{
		Addr:         h.TransportAddr,
		Connections:  h.TransportConnections,
		AuthFailures: h.TransportAuthFailures,
		Connected:    h.TransportConnected,
	}
	// The cause fields exist only once a refusal has actually been recorded.
	// Emitting class="unknown" for a link that has never refused anything
	// would invent a refusal; "unknown" is reserved for a refusal whose class
	// this build (or an older daemon) did not record.
	if h.TransportAuthFailures > 0 || h.TransportLastAuthError != "" {
		t.LastAuthError = h.TransportLastAuthError
		t.LastAuthErrorClass = processobs.NormalizeTransportAuthClass(h.TransportLastAuthErrorClass)
	}
	// Never-happened is ABSENT, not epoch 0.
	if !h.TransportLastConnectAt.IsZero() {
		at := h.TransportLastConnectAt
		t.LastConnectAt = &at
	}
	if !h.TransportLastDisconnectAt.IsZero() {
		at := h.TransportLastDisconnectAt
		t.LastDisconnectAt = &at
	}
	if h.TransportCapturerDecodeReported {
		decode := h.CapturerDecodeStats()
		t.CapturerDecode = &etwCapturerDecode{
			Dropped:            h.TransportCapturerDropped,
			UnsupportedVersion: h.TransportCapturerUnsupportedVersion,
			Decoded:            h.TransportCapturerDecoded,
			Ignored:            h.TransportCapturerIgnored,
			// Both predicates come from the one owner in processobs rather
			// than being re-spelled here, so this endpoint and /metrics and
			// `observer doctor` cannot drift into disagreeing about the same
			// record.
			Healthy:           !decode.Any(),
			NothingClassified: decode.NothingClassified(),
			ReportedAt:        h.TransportCapturerDecodeAt,
			Line:              h.CapturerDecodeLine(),
		}
	}
	return t
}
