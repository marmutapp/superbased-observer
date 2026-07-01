package invariant

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/orgclient"
	"github.com/marmutapp/superbased-observer/internal/orgcontract"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// memBearer is a deterministic in-test orgclient.BearerStore (the production
// OpenBearerStore would probe — and on a developer machine, write to — the OS
// keychain, which a test must not do).
type memBearer struct {
	bearer string
	key    ed25519.PrivateKey
}

func (m *memBearer) SaveBearer(b string) error { m.bearer = b; return nil }
func (m *memBearer) LoadBearer() (string, error) {
	if m.bearer == "" {
		return "", orgclient.ErrNoSecret
	}
	return m.bearer, nil
}
func (m *memBearer) SaveAgentKey(k ed25519.PrivateKey) error { m.key = k; return nil }
func (m *memBearer) LoadAgentKey() (ed25519.PrivateKey, error) {
	if m.key == nil {
		return nil, orgclient.ErrNoSecret
	}
	return m.key, nil
}
func (m *memBearer) Clear() error    { m.bearer, m.key = "", nil; return nil }
func (m *memBearer) Backend() string { return "mem" }

func itoa(i int64) string { return strconv.FormatInt(i, 10) }

// privacy sentinels — distinct, unmistakable markers stuffed into every
// agent-side string column. None may appear in the pushed bytes when the
// node has NOT opted in to full-content sharing.
//
// First four (secRawInput ... secErrMsg): the original v1.5+ posture —
// content columns that have ALWAYS been forbidden on the wire.
// Next four (secTarget ... secGitRemote): the v1.8.0 additions covering
// the four columns the 2026-06-02 teams test found leaking
// (actions.target, actions.source_file, projects.root_path,
// projects.git_remote). Pre-M1.x these ship raw; post-M1.x the seam ships
// only the hashed counterparts (target_hash / source_file_hash /
// project_root_hash / git_remote_hash).
const (
	secRawInput  = "SECRET_RAW_INPUT_xyzzy_42"
	secRawOutput = "SECRET_RAW_OUTPUT_plugh_99"
	secReasoning = "SECRET_REASONING_hunter2_zz"
	secErrMsg    = "SECRET_ERRMSG_swordfish_qq"

	secTarget     = "SECRET_TARGET_correcthorsebatterystaple"
	secSourceFile = "SECRET_SOURCEFILE_tetragrammaton_ww"
	secRootPath   = "SECRET_ROOTPATH_voluptuous_qq"
	secGitRemote  = "SECRET_GITREMOTE_omphaloskepsis_uu"
	// sessions.git_branch: no hash counterpart and no server feature keys on
	// it, but branch names routinely encode client/codename/ticket ids, so it
	// ships raw ONLY under full_content / admin_managed (stripped by default).
	secGitBranch = "SECRET_GITBRANCH_antidisestablishment_bb"

	// Guard-layer additions (migration 040, guard spec §10.2): the
	// three content-bearing guard_events columns. guard_events rows DO
	// push (unlike the node-local cache_*/advisor_* tables) — these
	// columns must be stripped in Go at SelectUnpushedSince unless the
	// node opted into [org_client.share].full_content, while
	// target_hash always ships.
	secGuardReason  = "SECRET_GUARDREASON_balderdash_kk"
	secGuardExcerpt = "SECRET_GUARDEXCERPT_rigmarole_jj"
	secGuardTaint   = "SECRET_GUARDTAINT_skulduggery_mm"

	// Native-console (otel_content) addition: the captured content body is
	// content-bearing — it ships ONLY under full_content / admin_managed, with
	// content_hash always shipping. Migration 048 (agent) / 007 (server).
	secOTelContent = "SECRET_OTELCONTENT_flibbertigibbet_pp"
)

// allSentinels is the union of the original 4 content-column sentinels,
// the 4 v1.8.0 target/path sentinels, and the 3 guard-layer verdict
// sentinels. Every push in the default (full_content=false) mode must
// ship NONE of these.
var allSentinels = []string{
	secRawInput, secRawOutput, secReasoning, secErrMsg,
	secTarget, secSourceFile, secRootPath, secGitRemote, secGitBranch,
	secGuardReason, secGuardExcerpt, secGuardTaint,
	secOTelContent,
}

// TestPushPayloadCarriesNoContent is the privacy invariant: a push must ship
// only the content-free rollup shapes. It seeds the corpus, then stuffs a
// distinctive secret string into EVERY agent-side string column that the
// push seam can in principle read (raw_tool_input, raw_tool_output,
// preceding_reasoning, error_message, target, source_file, projects.root_path,
// projects.git_remote, token_usage.source_file), runs one real push cycle
// against an in-process server, and asserts that not one of those secrets
// appears anywhere in the bytes that crossed the wire.
//
// This guards the structural privacy posture end to end: the orgcontract row
// types carry no content fields, store.SelectUnpushedSince selects no content
// columns when share.full_content=false (the default), and this test fails
// loudly if either ever regresses.
//
// Pre-v1.8.0: this test FAILS — the seam at internal/store/orgpush.go ships
// `target`, `source_file`, `root_path`, `git_remote` raw. That's the leak the
// 2026-06-02 teams test caught. The fix (M1.1–M1.3 + M1.5 of the remediation
// plan at docs/teams-test-findings-remediation-plan-2026-06-03.md) ships only
// the corresponding *_hash columns by default and gates the raw fields behind
// an opt-in node config.
func TestPushPayloadCarriesNoContent(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, db.Options{Path: filepath.Join(t.TempDir(), "agent.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer func() { _ = database.Close() }()
	st := store.New(database)
	seed(ctx, t, st)

	// Stuff the secret-bearing columns. Order matters: we update the agent
	// DB AFTER the benign seed has gone in via Ingest, so the secrets land
	// in the same rows the push seam will read.
	// actions has no UNIQUE on target/source_file, so blanket-update is fine.
	if _, err := database.ExecContext(ctx,
		`UPDATE actions SET raw_tool_input = ?, raw_tool_output = ?, preceding_reasoning = ?, error_message = ?, target = ?, source_file = ?`,
		secRawInput, secRawOutput, secReasoning, secErrMsg, secTarget, secSourceFile); err != nil {
		t.Fatalf("stuff actions: %v", err)
	}
	// projects has UNIQUE(root_path) — per-id update with the same prefix so
	// `bytes.Contains(raw, secRootPath)` still triggers regardless of which
	// project's row leaks.
	rows, err := database.QueryContext(ctx, `SELECT id FROM projects ORDER BY id`)
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	var projectIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			t.Fatalf("scan project id: %v", err)
		}
		projectIDs = append(projectIDs, id)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close projects rows: %v", err)
	}
	for _, id := range projectIDs {
		if _, err := database.ExecContext(ctx,
			`UPDATE projects SET root_path = ?, git_remote = ? WHERE id = ?`,
			secRootPath+"-p"+itoa(id), secGitRemote, id); err != nil {
			t.Fatalf("stuff project %d: %v", id, err)
		}
	}
	if _, err := database.ExecContext(ctx,
		`UPDATE token_usage SET source_file = ?`, secSourceFile); err != nil {
		t.Fatalf("stuff token_usage: %v", err)
	}
	if _, err := database.ExecContext(ctx,
		`UPDATE sessions SET git_branch = ?`, secGitBranch); err != nil {
		t.Fatalf("stuff sessions git_branch: %v", err)
	}
	// Guard events go through the one-owner store helper (the only
	// legal write path — a direct INSERT would also have to fake the
	// §10.4 hash chain). The three content-bearing columns carry their
	// sentinels; target_hash is the content-free counterpart that must
	// still ship.
	if _, err := st.InsertGuardEvents(ctx, []store.GuardEventRow{{
		TS: time.Now().UTC(), SessionID: "sess-cc-1",
		Tool: "claude-code", EventKind: "shell_exec",
		RuleID: "R-101", Category: "destructive", Severity: "critical",
		Decision: "flag", Source: "builtin",
		Reason:        secGuardReason,
		TargetHash:    "sha256:guard-target-canary",
		TargetExcerpt: secGuardExcerpt,
		TaintOrigin:   secGuardTaint,
	}}); err != nil {
		t.Fatalf("seed guard event: %v", err)
	}
	// Native-OTel content (migration 048): the body carries the sentinel; its
	// content_hash is the content-free counterpart that must still ship.
	if _, err := st.InsertOTelContent(ctx, []models.OTelContent{{
		RequestID: "req-cc-1", SessionID: "sess-cc-1", Kind: "prompt",
		Content: secOTelContent, Timestamp: time.Now().UTC(), Source: "cc_otel",
	}}); err != nil {
		t.Fatalf("seed otel_content: %v", err)
	}

	// Enrol directly (leave the push cursor at zero so the whole corpus is a
	// push candidate — we want maximum surface for a leak to show).
	if err := st.WriteEnrolment(ctx, store.Enrolment{
		OrgID: "org-1", OrgName: "Acme", OrgServerURL: "PLACEHOLDER",
		UserID: "scim-1", UserEmail: "dev@acme.example", BearerKeyID: "k",
	}); err != nil {
		t.Fatalf("WriteEnrolment: %v", err)
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	bs := &memBearer{bearer: "bearer-xyz", key: priv}

	// In-process server: capture the exact wire bytes, ACK 200.
	var wire []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wire, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int64{"accepted_rows": 1, "deduped_rows": 0, "next_cursor": 1})
	}))
	defer srv.Close()
	// Point the enrolment at the test server.
	if err := st.WriteEnrolment(ctx, store.Enrolment{
		OrgID: "org-1", OrgName: "Acme", OrgServerURL: srv.URL,
		UserID: "scim-1", UserEmail: "dev@acme.example", BearerKeyID: "k",
	}); err != nil {
		t.Fatalf("WriteEnrolment (url): %v", err)
	}

	// Default config: share.full_content is OFF — the seam should ship hashes
	// only. (Pre-M1.2, OrgClientConfig has no Share substruct; this still
	// compiles because the default zero value of the (yet-to-be-added) flag
	// means "metadata only", which is the same behavior we expect once the
	// flag exists.)
	c := orgclient.New(config.OrgClientConfig{
		Enabled: true, MaxPushBytes: config.DefaultMaxPushBytes, KeychainID: "k",
	}, st, bs, "inv-test", http.DefaultClient, nil)

	res, err := c.PushOnce(ctx)
	if err != nil {
		t.Fatalf("PushOnce: %v", err)
	}
	if res.Empty || res.RowCount == 0 {
		t.Fatalf("expected a non-empty push, got %+v", res)
	}
	if len(wire) == 0 {
		t.Fatal("server received no body")
	}

	// Decompress and scan the actual transmitted bytes.
	zr, err := gzip.NewReader(bytes.NewReader(wire))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	for _, s := range allSentinels {
		if bytes.Contains(raw, []byte(s)) {
			t.Errorf("content secret %q leaked into the push payload", s)
		}
	}

	// Sanity: the rollup must carry the hashed counterpart of the columns
	// it stripped — otherwise the wire could be empty and the leak-check
	// would trivially pass. target_hash is the canary: it's present on
	// every action and is the M1.1 contract addition that proves the new
	// (hash-only) shape is in effect.
	if !bytes.Contains(raw, []byte(`"target_hash"`)) {
		t.Errorf("expected hashed counterpart (target_hash) in the payload; scan may be vacuous or the M1.1 wire-shape change hasn't landed")
	}

	// Guard-layer canaries (G2, guard spec §10.2): the guard event must
	// have SHIPPED (metadata) for the guard sentinel scan above to be
	// non-vacuous, and its content-free hash counterpart must be on the
	// wire — proving the seam shipped the row but stripped the verdict
	// prose rather than dropping the table or shipping it whole.
	if !bytes.Contains(raw, []byte(`"guard_events"`)) {
		t.Errorf("expected guard_events in the payload; the guard push arm may be missing and the guard sentinel scan vacuous")
	}
	if !bytes.Contains(raw, []byte(`"R-101"`)) {
		t.Errorf("expected the guard event's rule_id (R-101) in the payload; guard metadata should ship even in metadata-only mode")
	}
	if !bytes.Contains(raw, []byte(`sha256:guard-target-canary`)) {
		t.Errorf("expected the guard event's target_hash in the payload; the content-free counterpart must always ship")
	}
}

// TestPushPayloadCarriesContentWhenOptedIn is the inverse guard: when the
// node operator has explicitly enabled share.full_content, the seam must
// ship the raw target/source_file/project_root/git_remote columns. Without
// this test the opt-in path could silently regress to metadata-only without
// any signal.
//
// Pre-M1.2 the OrgClientConfig.Share field doesn't exist; the test is
// marked skipped until the config plumbing lands, so it documents the
// expected behavior without blocking the M1.4 RED-to-GREEN flow.
func TestPushPayloadCarriesContentWhenOptedIn(t *testing.T) {
	// Both postures route through ShareOptions.shipsRawContent(): the node's
	// own full_content opt-in, and the native-console admin-managed default-flip
	// (admin authors it via the node's provisioning; still node-side, no remote
	// force). Each must ship the GATED path/excerpt columns raw — while the
	// never-read body columns (raw_tool_input/output, reasoning, error_message)
	// stay off the wire even then (they are not selected by the seam at all).
	gatedShouldShip := []string{
		secTarget, secSourceFile, secRootPath, secGitRemote, secGitBranch,
		secGuardReason, secGuardExcerpt, secGuardTaint,
		secOTelContent,
	}
	bodiesNeverShip := []string{secRawInput, secRawOutput, secReasoning, secErrMsg}

	for _, tc := range []struct {
		name  string
		share store.ShareOptions
	}{
		{"full_content", store.ShareOptions{FullContent: true}},
		{"admin_managed", store.ShareOptions{AdminManaged: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			database, err := db.Open(ctx, db.Options{Path: filepath.Join(t.TempDir(), "agent.db")})
			if err != nil {
				t.Fatalf("db.Open: %v", err)
			}
			defer func() { _ = database.Close() }()
			st := store.New(database)
			seed(ctx, t, st)

			if _, err := database.ExecContext(ctx,
				`UPDATE actions SET raw_tool_input = ?, raw_tool_output = ?, preceding_reasoning = ?, error_message = ?, target = ?, source_file = ?`,
				secRawInput, secRawOutput, secReasoning, secErrMsg, secTarget, secSourceFile); err != nil {
				t.Fatalf("stuff actions: %v", err)
			}
			// projects has UNIQUE(root_path): per-id update with the sentinel as a
			// prefix so bytes.Contains still triggers on whichever row ships.
			prows, err := database.QueryContext(ctx, `SELECT id FROM projects ORDER BY id`)
			if err != nil {
				t.Fatalf("list projects: %v", err)
			}
			var projectIDs []int64
			for prows.Next() {
				var id int64
				if err := prows.Scan(&id); err != nil {
					_ = prows.Close()
					t.Fatalf("scan project id: %v", err)
				}
				projectIDs = append(projectIDs, id)
			}
			if err := prows.Close(); err != nil {
				t.Fatalf("close projects rows: %v", err)
			}
			for _, id := range projectIDs {
				if _, err := database.ExecContext(ctx,
					`UPDATE projects SET root_path = ?, git_remote = ? WHERE id = ?`,
					secRootPath+"-p"+itoa(id), secGitRemote, id); err != nil {
					t.Fatalf("stuff project %d: %v", id, err)
				}
			}
			if _, err := database.ExecContext(ctx,
				`UPDATE token_usage SET source_file = ?`, secSourceFile); err != nil {
				t.Fatalf("stuff token_usage: %v", err)
			}
			if _, err := database.ExecContext(ctx,
				`UPDATE sessions SET git_branch = ?`, secGitBranch); err != nil {
				t.Fatalf("stuff sessions git_branch: %v", err)
			}
			if _, err := st.InsertGuardEvents(ctx, []store.GuardEventRow{{
				TS: time.Now().UTC(), SessionID: "sess-cc-1",
				Tool: "claude-code", EventKind: "shell_exec",
				RuleID: "R-101", Category: "destructive", Severity: "critical",
				Decision: "flag", Source: "builtin",
				Reason:        secGuardReason,
				TargetHash:    "sha256:guard-target-canary",
				TargetExcerpt: secGuardExcerpt,
				TaintOrigin:   secGuardTaint,
			}}); err != nil {
				t.Fatalf("seed guard event: %v", err)
			}
			if _, err := st.InsertOTelContent(ctx, []models.OTelContent{{
				RequestID: "req-cc-1", SessionID: "sess-cc-1", Kind: "prompt",
				Content: secOTelContent, Timestamp: time.Now().UTC(), Source: "cc_otel",
			}}); err != nil {
				t.Fatalf("seed otel_content: %v", err)
			}

			batch, err := st.SelectUnpushedSince(ctx, store.PushCursor{}, 1<<20, "org-1", "dev@acme.example", tc.share, store.ScopeOptions{})
			if err != nil {
				t.Fatalf("SelectUnpushedSince: %v", err)
			}
			raw, err := json.Marshal(batch)
			if err != nil {
				t.Fatalf("marshal batch: %v", err)
			}

			for _, s := range gatedShouldShip {
				if !bytes.Contains(raw, []byte(s)) {
					t.Errorf("[%s] gated content %q did NOT ship — the opt-in/flip failed to flip the seam", tc.name, s)
				}
			}
			for _, s := range bodiesNeverShip {
				if bytes.Contains(raw, []byte(s)) {
					t.Errorf("[%s] never-read body column %q leaked — these are not selected by the seam even under full content", tc.name, s)
				}
			}
		})
	}
}

// forbiddenCacheTables names the three node-local cachetrack tables
// that MUST NEVER appear in the org-push wire path. Spec §11:
// cachetrack data (segments, entries, events) is local, passive,
// node-side telemetry — it never leaves the agent. Adding any
// of them to internal/store/orgpush.go::SelectUnpushedSince (or
// any helper in that file) is a privacy regression — even though
// the rows themselves carry no raw text content, they reveal
// per-turn cache-hit patterns that the operator may treat as
// private even when they've opted into full-content sharing on
// the existing wire surfaces.
//
// Pinned by [TestSelectUnpushedSinceExcludesCacheTables] at the
// SOURCE level so the regression fires before any row is even
// constructed.
var forbiddenCacheTables = []string{
	"cache_segments",
	"cache_entries",
	"cache_events",
	// Advisor (suggestions engine, spec §15.7) tables are node-local
	// for the same reason and more: suggestion state + the digest
	// snapshot embed file paths and command summaries. Migration 039.
	"advisor_state",
	"advisor_digest",
	// Model-routing tables (model-routing spec §R9.1 + §R20) are
	// node-local: decision rows reveal per-turn model-selection
	// patterns and policy state; calibration rows reveal per-project
	// outcome aggregates. The §R19.4 org rollup pushes a separate
	// AGGREGATE wire shape when it ships — never these rows.
	// Migrations 041 + 042.
	"router_decisions",
	"model_calibration",
	// The agent-side org routing-policy CACHE (migration 043) is
	// received state — its body may describe org-internal projects —
	// and must never round-trip back onto the wire. The §R19.4
	// rollup that IS allowed on the wire is the routing_summaries
	// AGGREGATE, computed in store/routingsummary.go and composed
	// into the push via a function call precisely so this sentinel
	// can keep forbidding the underlying table names here.
	"org_routing_policies",
	// Guard-layer tables (migration 040, guard spec §10.2): pins,
	// policy state and approvals are NODE-LOCAL until the G13/G14
	// teams arc deliberately adds their wire surfaces (pins ship
	// hash-only, approvals ship granted_by — per guard spec §14.3)
	// and consciously updates this sentinel. guard_events is
	// deliberately ABSENT from this list — it DOES push, with its
	// content-bearing columns (reason / target_excerpt / taint_origin)
	// stripped in Go unless [org_client.share].full_content; that
	// posture is pinned data-side by TestPushPayloadCarriesNoContent's
	// guard sentinels + canaries above.
	"guard_pins",
	"guard_policy_state",
	"guard_approvals",
	// Process-observability tables (migration 044,
	// docs/process-observability.md §10.1) are NODE-LOCAL: they record
	// OS-level process trees, argv previews, executable identity, and
	// side-effect events for AI sessions — far higher privacy surface than
	// the metadata that DOES push. They never enter the wire path until a
	// separate privacy review designs hash-only rollups. No paired
	// orgserver migration exists, by the same design.
	"process_runs",
	"process_events",
	// Rate-limit snapshots (migration 049, the Next-Message Cost & Limit
	// Predictor's limit half) are NODE-LOCAL: per-account subscription-
	// window utilization + reset timestamps are personal usage telemetry
	// (same class as the cache tables). No paired orgserver migration; a
	// team-level "who's near their cap" view, if ever built, is a separate
	// opt-in AGGREGATE wire shape, never this table.
	"limit_snapshots",
	// Code-intelligence tables (internal/codeintel, migrations 050+,
	// docs/codeintel/) are NODE-LOCAL: they hold a project's source
	// paths, symbol names, signatures, import specifiers, and the
	// call/import graph — a structural map of private code that must
	// never leave the agent. [codeintel] config is local-only (like
	// [routing]/[cachewarm]); these tables are pre-registered here
	// before the schema lands so any attempt to add one to the push
	// seam fails loudly. If a team-level code-graph rollup is ever
	// built it would be a separate opt-in AGGREGATE wire shape, never
	// these tables.
	"codeintel_files",
	"codeintel_nodes",
	"codeintel_edges",
	"codeintel_sites",
	"codeintel_fts",
	"codeintel_embeddings",
	"codeintel_minhash",
	// Generalized-observability subsystem tables (internal/obs, plan
	// docs/plans/generalized-observability-custom-app-plan-2026-06-27.md
	// §10, decision D3). NODE-LOCAL by design: they hold an arbitrary
	// custom app's full trace/span topology, model/token/cost, and raw
	// prompt/response/tool bodies (obs_span_content, gated like
	// otel_content but never pushed). Owned + migrated by the obs
	// subsystem itself, off when [observability] disabled. No paired
	// orgserver migration — an org-level span rollup, if ever wanted, is
	// a separate opt-in AGGREGATE decision (plan §15 Q5), never these
	// rows. Mirrors the cache_* posture exactly.
	"obs_traces",
	"obs_spans",
	"obs_span_events",
	"obs_span_links",
	"obs_span_content",
	// obs eval plane (migration 0002, plan §8): datasets, dataset items
	// (snapshot input/output bodies, gated like obs_span_content), eval runs
	// and scores are NODE-LOCAL like the rest of the obs_* subsystem — never
	// on the org-push wire.
	"obs_datasets",
	"obs_dataset_items",
	"obs_eval_runs",
	"obs_eval_scores",
}

// TestSelectUnpushedSinceExcludesCacheTables is the structural
// privacy sentinel for the cachetrack arc. Walks every string
// literal in internal/store/orgpush.go (the single SQL seam
// for the push path per CLAUDE.md "Single SQL seam" Teams
// invariant) and fails if any contains a cachetrack table name.
//
// Strict-source check rather than data-side: catches the
// regression at the moment someone writes the JOIN/SELECT,
// before any row even exists. Pairs with
// TestPushPayloadCarriesNoContent which catches the
// alternative-path case (someone exports cache_* via a separate
// seam).
//
// orgpush.go also contains a long list of `*_hash` allowed
// columns — the regex deliberately matches the table NAMES, not
// "cache" substrings — so future "cache_read_hash" / "cache_*"
// column names on api_turns don't trigger a false positive.
func TestSelectUnpushedSinceExcludesCacheTables(t *testing.T) {
	t.Parallel()
	path := filepath.Join("..", "..", "internal", "store", "orgpush.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		for _, name := range forbiddenCacheTables {
			if strings.Contains(lit.Value, name) {
				pos := fset.Position(lit.Pos())
				t.Errorf("%s:%d: forbidden cachetrack table name %q appears in string literal — "+
					"cache_* tables are NODE-LOCAL per spec §11; they MUST NOT enter the push wire path",
					pos.Filename, pos.Line, name)
			}
		}
		return true
	})
}

// TestPushPayloadHasOnlyAllowlistedKeys is the structural-allowlist guard:
// in metadata-only mode (the default), the actions / sessions / api_turns /
// token_usage objects must contain ONLY keys from a fixed allowlist. Adding
// a new content-bearing field anywhere would silently break this invariant
// and the test fails loudly rather than relying on a denylist of known
// sentinels.
//
// Pre-M1.1 the allowlist would have to include `target`, `source_file`,
// `project_root`, `git_remote` (since those currently ship raw); the
// post-M1.1 allowlist drops those names in favor of their *_hash equivalents.
// Marked skipped until M1.1 lands so we don't have to bake a stale shape
// into the assertion.
func TestPushPayloadHasOnlyAllowlistedKeys(t *testing.T) {
	t.Skip("structural allowlist covered by M1.1 (orgcontract hash columns); test will activate once the wire shape stabilises")
	// Intentional placeholder: once M1.1 lands, the body decodes the
	// payload, walks each row in sessions/actions/api_turns/token_usage,
	// and t.Errorf on any unexpected key. The allowlist set is the final
	// list of JSON tag names declared on each orgcontract row type AFTER
	// the M1.1 change.
}

// TestRoutingSummaryWireShapeIsAggregateOnly pins the §R19.4 rollup
// contract structurally: the RoutingSummaryRow wire type may carry
// ONLY the allow-listed aggregate keys (attribution, date, enums,
// counts, dollars) — no model ids, no session ids, no paths. A new
// field on the type fails here before it can ship.
func TestRoutingSummaryWireShapeIsAggregateOnly(t *testing.T) {
	t.Parallel()
	allowed := map[string]bool{
		"OrgID": true, "UserEmail": true, // agent-stamped attribution
		"Day": true, "Tier": true, "Reason": true, "Mode": true, // date + closed enums
		"Decisions": true, "Applied": true, // counts
		"EstSavingsUSD": true, "CacheForfeitUSD": true, // dollars
	}
	typ := reflect.TypeOf(orgcontract.RoutingSummaryRow{})
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if !allowed[name] {
			t.Errorf("RoutingSummaryRow gained non-aggregate field %q — the §R19.4 wire shape is counts + dollars by tier/reason ONLY", name)
		}
	}
}

// newInvariantStore opens a fresh migrated agent DB for the routing
// wire-shape tests.
func newInvariantStore(t *testing.T) (*store.Store, func()) {
	t.Helper()
	database, err := db.Open(context.Background(), db.Options{Path: filepath.Join(t.TempDir(), "agent.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return store.New(database), func() { _ = database.Close() }
}

// TestRoutingSummaryGatedOffByDefault pins the consent posture: the
// zero-valued ShareOptions (the default) attaches NO routing summaries
// to a push batch, even when decision rows exist.
func TestRoutingSummaryGatedOffByDefault(t *testing.T) {
	t.Parallel()
	s, _ := newInvariantStore(t)
	ctx := context.Background()
	if err := s.InsertRouterDecisions(ctx, []store.RouterDecisionRow{{
		SessionID: "s1", Timestamp: time.Now().UTC(), Mode: "advise", Channel: "B",
		OriginalModel: "claude-opus-4-8", SelectedModel: "claude-haiku-4-5",
		TurnKind: "read_only", PolicyName: "value", PolicyHash: "h",
		ReasonCodes: []string{"overpowered_read"}, EstSavingsUSD: 1, EstimateVersion: "p1-v1",
	}}); err != nil {
		t.Fatalf("seed decision: %v", err)
	}

	batch, err := s.SelectUnpushedSince(ctx, store.PushCursor{}, 1<<20, "org-1", "dev@x", store.ShareOptions{}, store.ScopeOptions{})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(batch.RoutingSummaries) != 0 {
		t.Fatalf("default share shipped %d routing summaries — the §R26.4 consent toggle must gate them", len(batch.RoutingSummaries))
	}

	opted, err := s.SelectUnpushedSince(ctx, store.PushCursor{}, 1<<20, "org-1", "dev@x", store.ShareOptions{RoutingSummary: true}, store.ScopeOptions{})
	if err != nil {
		t.Fatalf("select opted: %v", err)
	}
	if len(opted.RoutingSummaries) == 0 {
		t.Fatal("opt-in shipped no summaries despite decision rows")
	}
	row := opted.RoutingSummaries[0]
	if row.Tier != "opus-class" || row.Decisions != 1 || row.OrgID != "org-1" {
		t.Errorf("summary row = %+v", row)
	}
}

// TestObsSummaryWireShapeIsAggregateOnly pins the T1 obs rollup wire shape
// (obs-org-tier plan §1): ObsSummaryRow may carry ONLY attribution + the
// content-free dimensions (day/model/provider/project_hash/source) + numeric
// counts/sums. A new free-text/topology field would fail loudly — the T1 floor
// is content-free by construction.
func TestObsSummaryWireShapeIsAggregateOnly(t *testing.T) {
	t.Parallel()
	allowed := map[string]bool{
		"OrgID": true, "UserEmail": true,
		"Day": true, "Model": true, "Provider": true, "ProjectHash": true, "Source": true,
		"Traces": true, "Spans": true, "InputTokens": true, "OutputTokens": true,
		"CacheReadTokens": true, "CacheWriteTokens": true, "ReasoningTokens": true,
		"TotalTokens": true, "CostUSD": true, "ErrorTraces": true,
		"DurationMsSum": true, "DurationMsCount": true,
	}
	typ := reflect.TypeOf(orgcontract.ObsSummaryRow{})
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if !allowed[name] {
			t.Errorf("ObsSummaryRow gained non-aggregate field %q — the T1 obs rollup is content-free counts/sums by closed dimensions ONLY", name)
		}
	}
}

// TestObsTraceWireShapeCarriesNoBody pins the T2 structure tier: ObsSpanRow /
// ObsTraceRow / ObsContentRow must not grow a raw prompt/response/tool body
// field on the STRUCTURE rows. Bodies live ONLY on ObsContentRow.Content
// (gated). A field literally named for content on a structure row fails.
func TestObsTraceWireShapeCarriesNoBody(t *testing.T) {
	t.Parallel()
	forbiddenOnStructure := []string{"Content", "Prompt", "Response", "Input", "Output", "Body", "Messages", "Attributes"}
	for _, typ := range []reflect.Type{reflect.TypeOf(orgcontract.ObsTraceRow{}), reflect.TypeOf(orgcontract.ObsSpanRow{}), reflect.TypeOf(orgcontract.ObsSpanEventRow{})} {
		for i := 0; i < typ.NumField(); i++ {
			name := typ.Field(i).Name
			for _, bad := range forbiddenOnStructure {
				if name == bad {
					t.Errorf("%s gained body-bearing field %q — T2 structure ships hashes/labels only; bodies belong on ObsContentRow (T3, gated)", typ.Name(), name)
				}
			}
		}
	}
}

// fakeObsProviders returns one row per tier so the gating logic in
// composeObsTiers can be exercised. The content row carries a raw body so the
// content-strip path is observable.
func fakeObsProviders() store.ObsOrgProviders {
	return store.ObsOrgProviders{
		Summaries: func(_ context.Context, _ int) ([]orgcontract.ObsSummaryRow, error) {
			return []orgcontract.ObsSummaryRow{{Day: "2026-06-29", Model: "gpt-4o", Traces: 1, CostUSD: 0.01}}, nil
		},
		Spans: func(_ context.Context, _ orgcontract.ObsCursor, _ int) (orgcontract.ObsSpanBatch, error) {
			return orgcontract.ObsSpanBatch{
				Traces: []orgcontract.ObsTraceRow{{TraceID: "t1", StartedAt: "2026-06-29T00:00:00Z", ProjectHash: "ph", ProjectRoot: "/secret/path"}},
				Spans:  []orgcontract.ObsSpanRow{{TraceID: "t1", SpanID: "s1", Kind: "llm", Name: "chat"}},
			}, nil
		},
		Content: func(_ context.Context, _ orgcontract.ObsCursor, _ int) ([]orgcontract.ObsContentRow, error) {
			return []orgcontract.ObsContentRow{{SpanID: "s1", Kind: "prompt", ContentHash: "h1", Content: "SECRET BODY"}}, nil
		},
		EvalRuns: func(_ context.Context, _ int) ([]orgcontract.ObsEvalRow, error) {
			return []orgcontract.ObsEvalRow{{Day: "2026-06-29", RunName: "r1", ScorerName: "json_valid", Total: 2, Passed: 1}}, nil
		},
	}
}

// TestObsTiersGatedOffByDefault pins the consent posture for all four obs
// tiers (obs-org-tier plan §1): the zero ShareOptions attaches NO obs rows even
// when the providers would yield them; each tier ships ONLY under its own flag;
// and T2 project_root + T3 content are stripped unless the node shares full
// content (content_hash / project_hash always survive).
func TestObsTiersGatedOffByDefault(t *testing.T) {
	t.Parallel()
	s, _ := newInvariantStore(t)
	s.SetObsOrgProviders(fakeObsProviders())
	ctx := context.Background()

	// Default share → nothing obs.
	def, err := s.SelectUnpushedSince(ctx, store.PushCursor{}, 1<<20, "org-1", "dev@x", store.ShareOptions{}, store.ScopeOptions{})
	if err != nil {
		t.Fatalf("select default: %v", err)
	}
	if len(def.ObsSummaries)+len(def.ObsTraces)+len(def.ObsSpans)+len(def.ObsContent)+len(def.ObsEvalRuns) != 0 {
		t.Fatalf("default share shipped obs rows: summaries=%d traces=%d spans=%d content=%d evals=%d",
			len(def.ObsSummaries), len(def.ObsTraces), len(def.ObsSpans), len(def.ObsContent), len(def.ObsEvalRuns))
	}

	// Each flag independently gates its tier; content/path stripped (metadata-only).
	opt, err := s.SelectUnpushedSince(ctx, store.PushCursor{}, 1<<20, "org-1", "dev@x",
		store.ShareOptions{ObsSummary: true, ObsTraces: true, ObsContent: true, ObsEvalSummary: true}, store.ScopeOptions{})
	if err != nil {
		t.Fatalf("select opted: %v", err)
	}
	if len(opt.ObsSummaries) == 0 || len(opt.ObsTraces) == 0 || len(opt.ObsSpans) == 0 || len(opt.ObsContent) == 0 || len(opt.ObsEvalRuns) == 0 {
		t.Fatalf("opt-in dropped a tier: summaries=%d traces=%d spans=%d content=%d evals=%d",
			len(opt.ObsSummaries), len(opt.ObsTraces), len(opt.ObsSpans), len(opt.ObsContent), len(opt.ObsEvalRuns))
	}
	if opt.ObsSummaries[0].OrgID != "org-1" {
		t.Errorf("attribution not stamped: %+v", opt.ObsSummaries[0])
	}
	// Metadata-only (no full_content): raw project_root + content MUST be stripped; hashes survive.
	if opt.ObsTraces[0].ProjectRoot != "" {
		t.Errorf("project_root leaked without full_content: %q", opt.ObsTraces[0].ProjectRoot)
	}
	if opt.ObsTraces[0].ProjectHash == "" {
		t.Error("project_hash was stripped — it must always ride")
	}
	if opt.ObsContent[0].Content != "" {
		t.Errorf("raw content leaked without full_content: %q", opt.ObsContent[0].Content)
	}
	if opt.ObsContent[0].ContentHash == "" {
		t.Error("content_hash was stripped — it must always ride")
	}

	// With full_content, the raw body + path DO ride.
	full, err := s.SelectUnpushedSince(ctx, store.PushCursor{}, 1<<20, "org-1", "dev@x",
		store.ShareOptions{FullContent: true, ObsTraces: true, ObsContent: true}, store.ScopeOptions{})
	if err != nil {
		t.Fatalf("select full: %v", err)
	}
	if full.ObsContent[0].Content != "SECRET BODY" || full.ObsTraces[0].ProjectRoot != "/secret/path" {
		t.Errorf("full_content did not ship raw body/path: content=%q root=%q", full.ObsContent[0].Content, full.ObsTraces[0].ProjectRoot)
	}
}
