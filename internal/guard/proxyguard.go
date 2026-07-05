package guard

import (
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/guard/mcpsec"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/policy"
	"github.com/marmutapp/superbased-observer/internal/scrub"
)

// Proxy-path evaluation seams (guard spec §3.2 seam 2, §8). The proxy
// is daemon-resident, so — unlike the short-lived hook process — its
// Guard instance holds LIVE taint state. cmd composition shares ONE
// Guard between the proxy and the watcher store (the cachetrack
// single-engine precedent), which is what finally arms T-501: the
// §8.4 injection heuristics here set Imperative taint that the
// watcher/hook seams consume on the next shell event.
//
// Three features, each gated by its own [guard.proxy] key:
//
//   - ScanProxyRequest / egress (§8.2, egress_scan): typed secret
//     detectors over the FINAL outbound body (post-compression — the
//     bytes the provider actually sees), resolved through the real
//     R-172 api_request row so mode/overrides/approvals all apply.
//     The proxy-side ACTION (forward / mask / deny) maps from the
//     verdict via [guard.proxy].egress_action — see the decision
//     table on resolveEgressAction.
//   - ScanProxyRequest / injection (§8.4, injection_heuristics): the
//     R-180 heuristics over NEW inbound tool-result / web / pasted
//     content segments; hits mark session taint with Imperative=true
//     and flag — they NEVER deny (gap F7).
//   - InspectProxyResponse (§8.3, response_scan): the response's
//     tool_use blocks are the model's intended next actions —
//     evaluated through the same engine a hook would use, a full
//     round-trip earlier. Flag/alert only in v1 (CanBlock=false; the
//     §6.2 degradation records what the verdict wanted).

// proxyRequestCaps are the egress-seam channel capabilities: the
// proxy sees the request BEFORE the provider does and can block it
// (synthetic 403, §8.5). No human in the loop → CanAsk=false (ask
// degrades to deny per §6.2 F5, then egress_action decides).
var proxyRequestCaps = policy.Capabilities{
	PreExecution: true,
	CanBlock:     true,
	CanAsk:       false,
	ProxyRouted:  true,
}

// proxyResponseCaps are the response-inspection capabilities:
// pre-execution intent, but v1 never rewrites model output (§8.3) —
// the channel cannot block, so deny/ask-class verdicts record with
// the §6.2 degradation marker.
var proxyResponseCaps = policy.Capabilities{
	PreExecution: true,
	CanBlock:     false,
	CanAsk:       false,
	ProxyRouted:  true,
}

// Proxy-action strings recorded on guard_events.decision for egress
// enforcement (the §8.2 "mask" decision exists ONLY at this seam —
// policy.Decision stays the 4-value ordered enum so the layering
// algebra is untouched).
const proxyActionMask = "mask"

// maxProxySeenSessions bounds the per-session event-dedup map; the
// oldest-touched session evicts on overflow (same shape as the taint
// tracker bounds).
const maxProxySeenSessions = 256

// maxProxySeenSigs bounds signatures kept per session.
const maxProxySeenSigs = 128

// ProxyToolUse is one intended next action extracted from a provider
// response by the proxy (which owns wire-format parsing; this layer
// owns meaning). Input is the tool's JSON input object.
type ProxyToolUse struct {
	// Name is the tool name as the model emitted it.
	Name string
	// Input is the JSON-encoded input object ({"command": ...}).
	Input []byte
}

// ProxyRequestResult is what the egress seam hands back to the proxy
// adapter: record-worthy verdicts plus the action to take on the
// request.
type ProxyRequestResult struct {
	// Verdicts are the record-worthy results (egress + injection),
	// ready for store.PersistGuardVerdicts.
	Verdicts []ActionVerdict
	// MaskedBody, when non-nil, is the rewritten outbound body
	// ([REDACTED:type] markers in place of certain findings). The
	// proxy MUST forward it instead of the original.
	MaskedBody []byte
	// Deny reports the §8.5 synthetic-403 decision. DenyRuleID /
	// DenyReason feed the provider-shaped error body.
	Deny       bool
	DenyRuleID string
	DenyReason string
	// MCPDecls are the request's MCP tool declarations when this is
	// the first time this session has shown this declaration set
	// (per-session dedup — request bodies re-send the whole tools
	// array every turn). The cmd adapter forwards them to the mcpsec
	// observation flow OFF the request path; the proxy package never
	// sees them.
	MCPDecls []mcpsec.ToolDecl
}

// proxySeen is the per-session record-dedup state: a request body
// carries the whole conversation, so without dedup the same finding
// signature would re-record on every subsequent turn of the session.
// Deny decisions are NEVER deduped (each enforced deny is an audit
// fact); flag/mask records dedup by signature.
type proxySeen struct {
	sigs      map[string]bool
	lastTouch time.Time
}

// ScanProxyRequest runs the §8.2 egress scan and the §8.4 injection
// heuristics over the final outbound request body. provider is the
// wire-format hint ("anthropic" | "openai" — a body shape, not a tool
// identity); sessionID may be empty (dedup and taint degrade to
// no-ops). now is injected for determinism; zero means time.Now().
//
// The call is synchronous on the request hot path — the §17.9 budget
// (≤10ms p99 added per request) is pinned by BenchmarkScanProxyRequest.
func (g *Guard) ScanProxyRequest(provider string, body []byte, sessionID string, now time.Time) ProxyRequestResult {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var res ProxyRequestResult
	if len(body) == 0 {
		return res
	}
	wantTools := g.cfg.MCP.Pinning || g.cfg.MCP.PoisoningHeuristics
	parsed := parseProxyBody(provider, body, wantTools)
	target := provider + ":" + parsed.model

	// §12.1 budget check first (cheapest pass — a TTL-cached lookup +
	// one engine evaluation). A hard-mode deny short-circuits the
	// pipeline: the request never reaches the provider, so there is
	// nothing to egress-scan and no new content enters the session.
	if g.budgetLookup != nil {
		g.scanBudget(&res, sessionID, target, now)
		if res.Deny {
			return res
		}
	}
	if g.cfg.Proxy.EgressScan {
		g.scanEgress(&res, body, sessionID, target, now)
	}
	if g.cfg.Proxy.InjectionHeuristics {
		g.scanInjection(&res, parsed, sessionID, now)
	}
	if len(parsed.mcpDecls) > 0 &&
		!g.proxyAlreadySeen(sessionID, mcpDeclsSignature(parsed.mcpDecls), now) {
		// First time this session shows this declaration set — hand it
		// out for the §9.2 observation flow (the cmd adapter runs the
		// pin diff off the request path).
		res.MCPDecls = parsed.mcpDecls
	}
	return res
}

// mcpDeclsSignature builds the per-session dedup key for a request's
// MCP declaration set: one ToolsHash per server, joined in sorted
// server order (ToolsHash itself sorts tools within a server).
func mcpDeclsSignature(decls []mcpsec.ToolDecl) string {
	byServer := map[string][]mcpsec.ToolDecl{}
	servers := make([]string, 0, 4)
	for _, d := range decls {
		if _, ok := byServer[d.Server]; !ok {
			servers = append(servers, d.Server)
		}
		byServer[d.Server] = append(byServer[d.Server], d)
	}
	sort.Strings(servers)
	var b strings.Builder
	b.WriteString("mcp_decls")
	for _, s := range servers {
		b.WriteByte('|')
		b.WriteString(s)
		b.WriteByte('=')
		b.WriteString(mcpsec.ToolsHash(byServer[s]))
	}
	return b.String()
}

// scanEgress implements the §8.2 half of ScanProxyRequest.
func (g *Guard) scanEgress(res *ProxyRequestResult, body []byte, sessionID, target string, now time.Time) {
	// One detector pass produces both the findings and the candidate
	// masked body. Only certain, non-allowlisted findings mask (§8.2:
	// mask is for detector-certain types; entropy hits never mask).
	maskable := func(f scrub.TypedFinding) bool {
		return f.Certain && !g.egressAllowed(f.Value)
	}
	masked, findings := scrub.MaskSecrets(string(body), maskable)

	// Allowlist filter ([guard.proxy].egress_allow): a finding whose
	// VALUE matches an operator pattern (test fixtures, known-fake
	// keys) doesn't count at all.
	secrets := make([]policy.SecretFinding, 0, len(findings))
	anyCertain := false
	for _, f := range findings {
		if g.egressAllowed(f.Value) {
			continue
		}
		secrets = append(secrets, policy.SecretFinding{Type: f.Type, Certain: f.Certain})
		if f.Certain {
			anyCertain = true
		}
	}
	if len(secrets) == 0 {
		return
	}

	ev := policy.Event{
		Kind:      policy.KindAPIRequest,
		Target:    target,
		SessionID: sessionID,
		Caps:      proxyRequestCaps,
		Secrets:   secrets,
		Now:       now,
	}
	verdict, guardErr := g.Evaluate(ev)
	verdict, approved := g.applyApprovals(verdict, &ev)
	if verdict.Decision < policy.DecisionFlag && guardErr == nil {
		return // R-172 disabled or overridden to allow
	}

	av := ActionVerdict{
		Input: ActionInput{
			SessionID: sessionID,
			Target:    target,
			Timestamp: now,
		},
		Kind:       policy.KindAPIRequest,
		Category:   g.CategoryFor(verdict.RuleID),
		Verdict:    verdict,
		GuardError: guardErr != nil,
	}
	if approved {
		av.DegradedFrom = "approved"
	}

	em := ResolveEmission(verdict, proxyRequestCaps)
	action := g.resolveEgressAction(em, anyCertain)
	switch action {
	case "deny":
		av.Enforced = true
		res.Deny = true
		res.DenyRuleID = verdict.RuleID
		res.DenyReason = verdict.Reason
	case proxyActionMask:
		av.Enforced = true
		av.ProxyAction = proxyActionMask
		res.MaskedBody = []byte(masked)
	default: // forward, record as flag-class
		if em.Permission == "deny" && av.DegradedFrom == "" {
			// The verdict wanted to block but egress_action (or an
			// entropy-only finding set) capped the channel at
			// flag/mask — record the downgrade, never silently weaker
			// (§6.2 F5).
			av.DegradedFrom = verdict.Decision.String()
			av.Verdict.Decision = policy.DecisionFlag
		}
	}

	// Dedup flag/mask records per session (a deny always records).
	if !res.Deny && g.proxyAlreadySeen(sessionID, egressSignature(&av), now) {
		return
	}
	res.Verdicts = append(res.Verdicts, av)
}

// resolveEgressAction maps a resolved emission onto the proxy action
// per [guard.proxy].egress_action — the §8.2 decision table:
//
//	verdict (post-approval, post-§6.2) | egress_action | certain? | action
//	flag-class                         | *             | *        | forward (record flag)
//	deny                               | flag          | *        | forward (record flag, degraded_from=deny)
//	deny                               | mask          | yes      | mask    (record mask, enforced)
//	deny                               | mask          | no       | forward (record flag, degraded_from=deny — entropy-only never masks)
//	deny                               | deny          | *        | 403     (record deny, enforced)
func (g *Guard) resolveEgressAction(em Emission, anyCertain bool) string {
	if em.Permission != "deny" {
		return "flag"
	}
	switch g.cfg.Proxy.EgressAction {
	case "deny":
		return "deny"
	case proxyActionMask:
		if anyCertain {
			return proxyActionMask
		}
		return "flag"
	default:
		return "flag"
	}
}

// egressAllowed reports whether a matched value is covered by an
// [guard.proxy].egress_allow pattern.
func (g *Guard) egressAllowed(value string) bool {
	for _, re := range g.egressAllow {
		if re.MatchString(value) {
			return true
		}
	}
	return false
}

// scanInjection implements the §8.4 half: the R-180 heuristics over
// the request's NEW inbound content segments (the trailing
// tool-result / user-paste run — earlier turns were scanned when they
// were new). Hits mark session taint with Imperative=true and record
// flag verdicts; they never deny (F7) — R-180's table row is flag in
// both modes, and nothing here consults egress_action.
func (g *Guard) scanInjection(res *ProxyRequestResult, parsed proxyParsedBody, sessionID string, now time.Time) {
	for i := range parsed.segments {
		seg := &parsed.segments[i]
		ev := policy.Event{
			Kind:      policy.KindAPIRequest,
			Target:    seg.origin,
			SessionID: sessionID,
			Caps:      proxyRequestCaps,
			Raw:       []byte(seg.text),
			Now:       now,
		}
		verdict, guardErr := g.Evaluate(ev)
		if verdict.Decision < policy.DecisionFlag && guardErr == nil {
			continue
		}
		if verdict.RuleID == policy.InjectionRuleID() &&
			!(seg.taintSource == policy.TaintSourceMCPUnpinned && g.mcpApproved(seg.mcpServer)) {
			// The arming move: untrusted content carrying
			// instruction-shaped patterns taints the session with the
			// Imperative bit T-501 consumes. A pinned-and-approved MCP
			// server's results skip the mark (§9.2 — approval is an
			// explicit operator trust grant); the R-180 flag verdict
			// still records below, so the content concern stays
			// visible either way.
			g.taint.Mark(sessionID, policy.TaintMark{
				Source:     seg.taintSource,
				Origin:     boundOrigin(seg.origin),
				Imperative: true,
				At:         now,
			})
		}
		av := ActionVerdict{
			Input: ActionInput{
				SessionID: sessionID,
				Target:    seg.origin,
				Timestamp: now,
			},
			Kind:       policy.KindAPIRequest,
			Category:   g.CategoryFor(verdict.RuleID),
			Verdict:    verdict,
			GuardError: guardErr != nil,
		}
		if g.proxyAlreadySeen(sessionID, injectionSignature(&av), now) {
			continue
		}
		res.Verdicts = append(res.Verdicts, av)
	}
}

// InspectProxyResponse evaluates the response's tool_use blocks — the
// model's intended next actions (§8.3) — through the same engine the
// hook path uses, with the session's LIVE taint snapshot stamped, so
// an R-101-class command or a T-501 sequence flags a full round-trip
// before the client's own hook would see it. Flag/alert only in v1:
// the channel cannot block (proxyResponseCaps), so blocking verdicts
// record with the §6.2 degradation marker and Enforced=false.
//
// Returns record-worthy verdicts for the adapter to persist + alert.
// ProjectRoot is unknown at the proxy (the hook-path precedent) —
// boundary/cross-project rules stay watcher-path; Dialect is posix
// (Claude Code's Bash tool runs POSIX shells on every platform;
// nested powershell/cmd payloads switch dialect mid-parse anyway).
func (g *Guard) InspectProxyResponse(sessionID string, tools []ProxyToolUse, now time.Time) []ActionVerdict {
	if !g.cfg.Proxy.ResponseScan || len(tools) == 0 {
		return nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var out []ActionVerdict
	for i := range tools {
		ev, ok := buildToolUseEvent(&tools[i])
		if !ok {
			continue
		}
		ev.SessionID = sessionID
		ev.Caps = proxyResponseCaps
		ev.Now = now
		ev.Taint = g.taint.Snapshot(sessionID, 0, now)
		verdict, guardErr := g.Evaluate(ev)
		verdict, approved := g.applyApprovals(verdict, &ev)
		if verdict.Decision < policy.DecisionFlag && guardErr == nil {
			continue
		}
		av := ActionVerdict{
			Input: ActionInput{
				SessionID:  sessionID,
				ActionType: ev.ActionType,
				Target:     ev.Target,
				Timestamp:  now,
			},
			Kind:        ev.Kind,
			Category:    g.CategoryFor(verdict.RuleID),
			Verdict:     verdict,
			TaintOrigin: taintOriginFor(verdict, ev.Taint),
			GuardError:  guardErr != nil,
		}
		if approved {
			av.DegradedFrom = "approved"
		}
		// §6.2: the channel can't block — record what the verdict
		// wanted via the degradation marker (emission seams only
		// overwrite DegradedFrom when non-empty, preserving the
		// "approved" marker).
		if em := ResolveEmission(verdict, proxyResponseCaps); em.DegradedFrom != "" {
			av.DegradedFrom = em.DegradedFrom
		}
		out = append(out, av)
	}
	return out
}

// responseToolShape maps a lowercased tool name onto the policy event
// vocabulary — the response-side sibling of the hook layer's
// claudeToolShape table, covering the common tool vocabularies of the
// proxy-routed clients (a wire-format mapping, not tool-identity
// branching: unknown names simply don't evaluate). Table-driven per
// Module rule 5; extend here.
var responseToolShape = map[string]struct {
	kind       policy.EventKind
	actionType string
	fields     []string // input fields tried in order for the target
}{
	"bash":             {policy.KindShellExec, models.ActionRunCommand, []string{"command"}},
	"shell":            {policy.KindShellExec, models.ActionRunCommand, []string{"command", "cmd"}},
	"run_command":      {policy.KindShellExec, models.ActionRunCommand, []string{"command", "cmd"}},
	"run_terminal_cmd": {policy.KindShellExec, models.ActionRunCommand, []string{"command"}},
	"execute_command":  {policy.KindShellExec, models.ActionRunCommand, []string{"command"}},
	"write":            {policy.KindFileAccess, models.ActionWriteFile, []string{"file_path", "path", "target_file"}},
	"write_file":       {policy.KindFileAccess, models.ActionWriteFile, []string{"file_path", "path", "target_file"}},
	"write_to_file":    {policy.KindFileAccess, models.ActionWriteFile, []string{"path", "file_path"}},
	"create_file":      {policy.KindFileAccess, models.ActionWriteFile, []string{"file_path", "path"}},
	"edit":             {policy.KindFileAccess, models.ActionEditFile, []string{"file_path", "path", "target_file"}},
	"multiedit":        {policy.KindFileAccess, models.ActionEditFile, []string{"file_path", "path"}},
	"edit_file":        {policy.KindFileAccess, models.ActionEditFile, []string{"file_path", "path", "target_file"}},
	"apply_diff":       {policy.KindFileAccess, models.ActionEditFile, []string{"path", "file_path"}},
	"notebookedit":     {policy.KindFileAccess, models.ActionEditFile, []string{"notebook_path", "file_path"}},
	"str_replace_based_edit_tool": {
		policy.KindFileAccess, models.ActionEditFile, []string{"path", "file_path"},
	},
	"read":       {policy.KindFileAccess, models.ActionReadFile, []string{"file_path", "path", "target_file"}},
	"read_file":  {policy.KindFileAccess, models.ActionReadFile, []string{"file_path", "path", "target_file"}},
	"webfetch":   {policy.KindToolCall, models.ActionWebFetch, []string{"url"}},
	"web_fetch":  {policy.KindToolCall, models.ActionWebFetch, []string{"url"}},
	"websearch":  {policy.KindToolCall, models.ActionWebSearch, []string{"query"}},
	"web_search": {policy.KindToolCall, models.ActionWebSearch, []string{"query"}},
}

// buildToolUseEvent classifies one tool_use into a policy.Event.
// ok=false means the tool isn't an evaluable shape (unknown name,
// missing operand) — unknown is never a violation.
func buildToolUseEvent(tu *ProxyToolUse) (policy.Event, bool) {
	if strings.HasPrefix(tu.Name, "mcp__") {
		return policy.Event{
			Kind:       policy.KindMCPCall,
			ActionType: models.ActionMCPCall,
			Target:     tu.Name,
		}, true
	}
	shape, ok := responseToolShape[strings.ToLower(tu.Name)]
	if !ok {
		return policy.Event{}, false
	}
	target := jsonStringField(tu.Input, shape.fields)
	if target == "" {
		return policy.Event{}, false
	}
	return policy.Event{
		Kind:       shape.kind,
		ActionType: shape.actionType,
		Target:     target,
	}, true
}

// proxyAlreadySeen consults + updates the per-session signature set.
// Empty session IDs never dedup (no stable key to dedup on).
func (g *Guard) proxyAlreadySeen(sessionID, sig string, now time.Time) bool {
	if sessionID == "" {
		return false
	}
	g.proxyMu.Lock()
	defer g.proxyMu.Unlock()
	if g.proxySeen == nil {
		g.proxySeen = make(map[string]*proxySeen)
	}
	st := g.proxySeen[sessionID]
	if st == nil {
		if len(g.proxySeen) >= maxProxySeenSessions {
			g.evictOldestProxySeenLocked()
		}
		st = &proxySeen{sigs: make(map[string]bool)}
		g.proxySeen[sessionID] = st
	}
	st.lastTouch = now
	if st.sigs[sig] {
		return true
	}
	if len(st.sigs) >= maxProxySeenSigs {
		// Bounded: drop the whole set rather than tracking insert
		// order — worst case a signature re-records once per reset,
		// which errs toward recording.
		st.sigs = make(map[string]bool)
	}
	st.sigs[sig] = true
	return false
}

// evictOldestProxySeenLocked removes the least-recently-touched
// session. Caller holds proxyMu.
func (g *Guard) evictOldestProxySeenLocked() {
	var oldestID string
	var oldest time.Time
	first := true
	for id, st := range g.proxySeen {
		if first || st.lastTouch.Before(oldest) {
			oldestID, oldest, first = id, st.lastTouch, false
		}
	}
	if oldestID != "" {
		delete(g.proxySeen, oldestID)
	}
}

// egressSignature / injectionSignature build the dedup keys. Reason
// carries the stable type×count summary (egress) / heuristic names
// (injection), so a CHANGED finding set records again.
func egressSignature(av *ActionVerdict) string {
	return av.Verdict.RuleID + "|" + av.Verdict.Reason + "|" + av.Verdict.Decision.String() + "|" + av.ProxyAction
}

func injectionSignature(av *ActionVerdict) string {
	return av.Verdict.RuleID + "|" + av.Input.Target + "|" + av.Verdict.Reason
}

// compileEgressAllow compiles [guard.proxy].egress_allow patterns,
// recording invalid ones as load issues (degrade-don't-fail — the
// pattern is skipped, scanning continues).
func compileEgressAllow(patterns []string) ([]*regexp.Regexp, []string) {
	var out []*regexp.Regexp
	var issues []string
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			issues = append(issues, "egress_allow pattern "+p+": "+err.Error())
			continue
		}
		out = append(out, re)
	}
	return out, issues
}
