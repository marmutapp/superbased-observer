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

	// Codex fork/subagent lineage (migration 069): forked_from_id /
	// parent_thread_id / thread_source are NODE-LOCAL — never selected by
	// the push seam, in ANY share mode (unlike git_branch, they don't ship
	// even under full_content). Written only via Store.SetSessionLineage.
	secForkedFrom   = "SECRET_FORKEDFROM_grandiloquent_ff"
	secParentThread = "SECRET_PARENTTHREAD_perspicacious_pp"
	secThreadSource = "SECRET_THREADSOURCE_sesquipedalian_ss"

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

	// Obs input-admission (T6, obs migration 0005): the three content-bearing
	// verdict columns (tenant / user / reason_excerpt). They ride raw out of
	// the obs provider and are stripped in composeObsTiers unless the node
	// opted into full_content / admin_managed — while the content-free verdict
	// metadata (decision / message_hash / row_hash / hash-chain links) always
	// ships. Composed via the obs provider seam (orgpush.go names no obs_*
	// table); the raw request text is NEVER stored on the node at all.
	secAdmTenant = "SECRET_ADMTENANT_collywobbles_xx"
	secAdmUser   = "SECRET_ADMUSER_widdershins_vv"
	secAdmReason = "SECRET_ADMREASON_snollygoster_tt"

	// Obs per-item eval (T7, obs migration 0002 tables): the four
	// content-bearing item columns (input / expected / output excerpts +
	// scorer rationale). They ride raw out of the obs provider and are
	// stripped in composeObsTiers unless the node opted into
	// full_content / admin_managed — while the content-free score metadata
	// (run/dataset identity, scorer, score, pass, content_hash, ts) always
	// ships. Composed via the obs provider seam (orgpush.go names no obs_*
	// table); the raw bodies live on the node only under its ContentGate.
	secEvalInput    = "SECRET_EVALINPUT_borborygmus_aa"
	secEvalExpected = "SECRET_EVALEXPECTED_gongoozler_bb"
	secEvalOutput   = "SECRET_EVALOUTPUT_absquatulate_cc"
	secEvalRational = "SECRET_EVALRATIONALE_mumpsimus_dd"
)

// allSentinels is the union of the original 4 content-column sentinels,
// the 4 v1.8.0 target/path sentinels, and the 3 guard-layer verdict
// sentinels. Every push in the default (full_content=false) mode must
// ship NONE of these.
var allSentinels = []string{
	secRawInput, secRawOutput, secReasoning, secErrMsg,
	secTarget, secSourceFile, secRootPath, secGitRemote, secGitBranch,
	secForkedFrom, secParentThread, secThreadSource,
	secGuardReason, secGuardExcerpt, secGuardTaint,
	secOTelContent,
	secAdmTenant, secAdmUser, secAdmReason,
	secEvalInput, secEvalExpected, secEvalOutput, secEvalRational,
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
		`UPDATE sessions SET git_branch = ?, forked_from_id = ?, parent_thread_id = ?, thread_source = ?`,
		secGitBranch, secForkedFrom, secParentThread, secThreadSource); err != nil {
		t.Fatalf("stuff sessions git_branch + lineage: %v", err)
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

	// Obs input-admission (T6): wire a fake obs provider that yields one verdict
	// carrying the content-bearing sentinels (tenant/user/reason_excerpt) plus
	// content-free canaries (decision/message_hash/row_hash). With
	// full_content OFF the composeObsTiers strip must zero the sentinels while
	// the metadata rides — the raw obs_admission_events table doesn't exist in
	// the plain agent DB, so the provider seam (like fakeObsProviders) is the
	// legal source. The policy body is admin config → benign, always ships.
	st.SetObsOrgProviders(store.ObsOrgProviders{
		Admission: func(_ context.Context, _ orgcontract.ObsCursor, _ int) (orgcontract.ObsAdmissionBatch, error) {
			return orgcontract.ObsAdmissionBatch{
				Events: []orgcontract.ObsAdmissionRow{{
					TS: time.Now().UTC().Format(time.RFC3339), Mode: "enforce",
					Decision: "deny", Severity: "critical", PolicyHash: "adm-policy-canary",
					MessageHash: "sha256:adm-msg-canary", RowHash: "adm-rowhash-canary",
					Tenant: secAdmTenant, EndUser: secAdmUser, ReasonExcerpt: secAdmReason,
				}},
				Policies: []orgcontract.ObsAdmissionPolicyRow{{
					PolicyHash: "adm-policy-canary", CreatedAt: time.Now().UTC().Format(time.RFC3339),
					Mode: "enforce", Scope: "all", CriteriaCount: 1, Body: "criterion = benign-policy-body",
				}},
			}, nil
		},
		// T7 per-item eval: one item carrying the content-bearing sentinels
		// (input/expected/output excerpts + rationale) plus content-free canaries
		// (run identity, scorer, content_hash). With full_content OFF the
		// composeObsTiers strip must zero the sentinels while the metadata rides.
		EvalItems: func(_ context.Context, _ orgcontract.ObsCursor, _ int) (orgcontract.ObsEvalItemBatch, error) {
			return orgcontract.ObsEvalItemBatch{
				Items: []orgcontract.ObsEvalItemRow{{
					RunID: 7, RunName: "eval-canary-run", DatasetID: 3, DatasetName: "eval-canary-ds",
					ItemID: 11, SpanID: "eval-span-canary", TraceID: "eval-trace-canary",
					Scorer: "json_valid", Score: 0.5, Passed: false, Source: "run",
					TS: time.Now().UTC().Format(time.RFC3339), ContentHash: "sha256:eval-hash-canary",
					InputExcerpt: secEvalInput, ExpectedExcerpt: secEvalExpected,
					OutputExcerpt: secEvalOutput, Rationale: secEvalRational,
				}},
			}, nil
		},
	})

	// Default config: share.full_content is OFF — the seam should ship hashes
	// only. The [org_client.share.obs] admission opt-in is ON so the T6 arm
	// runs (and its content-bearing verdict columns get exercised by the
	// strip); every other content column stays metadata-only.
	c := orgclient.New(config.OrgClientConfig{
		Enabled: true, MaxPushBytes: config.DefaultMaxPushBytes, KeychainID: "k",
		Share: config.OrgClientShareConfig{
			Obs: config.OrgClientShareObsConfig{Admission: true, EvalItems: true},
		},
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

	// Obs admission canaries (T6): the verdict must have SHIPPED (metadata) for
	// the admission sentinel scan above to be non-vacuous, and its content-free
	// hash-chain link + message_hash must be on the wire — proving the seam
	// shipped the row but stripped the PII/prose (tenant/user/reason_excerpt)
	// rather than dropping the tier or shipping it whole. The decision must
	// ride VERBATIM (no wire translation to "would_block").
	if !bytes.Contains(raw, []byte(`"obs_admission_events"`)) {
		t.Errorf("expected obs_admission_events in the payload; the T6 admission push arm may be missing and the admission sentinel scan vacuous")
	}
	if !bytes.Contains(raw, []byte(`adm-rowhash-canary`)) {
		t.Errorf("expected the admission verdict's row_hash (dedup key) in the payload; the content-free hash-chain link must always ship")
	}
	if !bytes.Contains(raw, []byte(`sha256:adm-msg-canary`)) {
		t.Errorf("expected the admission verdict's message_hash in the payload; the content-free request provenance must always ship")
	}
	if !bytes.Contains(raw, []byte(`"decision":"deny"`)) {
		t.Errorf("expected the admission verdict decision to ship VERBATIM (allow|flag|ask|deny); the node ships the stored decision, the server does any would_block display mapping")
	}

	// Obs per-item eval canaries (T7): the score must have SHIPPED (metadata)
	// for the eval sentinel scan above to be non-vacuous, and its content-free
	// content_hash + run identity must be on the wire — proving the seam shipped
	// the row but stripped the item excerpts/rationale rather than dropping the
	// tier or shipping it whole.
	if !bytes.Contains(raw, []byte(`"obs_eval_items"`)) {
		t.Errorf("expected obs_eval_items in the payload; the T7 per-item eval push arm may be missing and the eval sentinel scan vacuous")
	}
	if !bytes.Contains(raw, []byte(`sha256:eval-hash-canary`)) {
		t.Errorf("expected the eval item's content_hash in the payload; the content-free signal must always ship")
	}
	if !bytes.Contains(raw, []byte(`eval-canary-run`)) {
		t.Errorf("expected the eval item's run_name in the payload; the content-free run identity must ship even in metadata-only mode")
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
	bodiesNeverShip := []string{
		secRawInput, secRawOutput, secReasoning, secErrMsg,
		// Codex fork/subagent lineage is node-local in EVERY share mode —
		// it must not ship even under full_content / admin_managed.
		secForkedFrom, secParentThread, secThreadSource,
	}

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
				`UPDATE sessions SET git_branch = ?, forked_from_id = ?, parent_thread_id = ?, thread_source = ?`,
				secGitBranch, secForkedFrom, secParentThread, secThreadSource); err != nil {
				t.Fatalf("stuff sessions git_branch + lineage: %v", err)
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
	"process_network_bodies",
	// Rate-limit snapshots (migration 049, the Next-Message Cost & Limit
	// Predictor's limit half) are NODE-LOCAL: per-account subscription-
	// window utilization + reset timestamps are personal usage telemetry
	// (same class as the cache tables). No paired orgserver migration; a
	// team-level "who's near their cap" view, if ever built, is a separate
	// opt-in AGGREGATE wire shape, never this table.
	"limit_snapshots",
	// Plane-A P0-5 unified policy resource, agent-side scoped persistence
	// (migration 081, docs/plans/plane-a-p0-5-unified-policy-resource-v1-plan.md
	// §6.2/§6.9/§6.10): org_enrolment_generation is the durable
	// cross-process enrolment fence (survives unenrol, carries a tombstone
	// bit) and org_policy_resource_state is the per-(org_key, family)
	// replay floor + last-verified-envelope identity. Both are RECEIVED,
	// generation-scoped control-plane state — like org_routing_policies
	// above, round-tripping either back onto the wire is pure noise even
	// setting aside that a generation counter and a replay floor reveal
	// nothing useful to the server it didn't already know, but the
	// underlying resource content (family bodies) could. internal/store/
	// policyresource.go is their one owner; orgpush.go must never read
	// them.
	"org_enrolment_generation",
	"org_policy_resource_state",
	// Admin-controlled Plane B, the ENROLMENT GRANT (migration 082,
	// docs/plans/admin-controlled-plane-b-spec-2026-08-15.md §2.4). It
	// records the bounded authority THIS machine handed to the
	// organization: consent mode, the consenting local actor, the signed
	// offer, the key pin. The org already knows what it offered; what it
	// must never receive back on the push wire is the node's own consent
	// record, which names a local user and is the developer's evidence of
	// what they agreed to — evidence whose whole value is that it lives on
	// their machine, under their control, deletable by `observer unenroll`.
	// internal/store/orggrant.go is its one owner.
	"org_enrolment_grant",
	// Session-handoff records (migration 055,
	// docs/plans/session-handoff-plan-2026-07-03.md §5) are NODE-LOCAL:
	// which tool a user moved a session to, when, at what fork point and
	// estimated cost is personal workflow telemetry. The rendered
	// HandoverDoc is never stored at all (delivered and forgotten); the
	// row holds counts/enums/hashes/paths only. Cross-machine handoff, if
	// ever built, is a separate opt-in wire shape behind
	// shipsRawContent(), never this table.
	"handoffs",
	// The Terminal Workspace dock-grid layout (migration 073,
	// docs/plans/terminal-dock-grid-design-2026-07-20.md) is NODE-LOCAL:
	// the blob maps terminal handles / session ids to grid cells — the
	// operator's personal screen arrangement, meaningless and unwanted
	// off-node. No paired orgserver migration, by design.
	"workspace_layouts",
	// The org-announcement CACHE (migration 076, rail R3 of
	// docs/plans/dashboard-announcements-banner-plan-2026-07-31.md §4)
	// is NODE-LOCAL and must stay that way for a reason specific to
	// this feature: the rail is ONE-WAY by design. Pushing the cached
	// document (or its version) back would tell the server which of its
	// own announcements this node holds — a read receipt, which plan §6
	// rules out as telemetry. The banner has no acknowledgment wire and
	// must never grow one by accident; the row is also RECEIVED state,
	// like org_routing_policies above, so round-tripping it is pure
	// noise even setting the receipt problem aside.
	"org_announcements",
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
	// §10, decision D3). These are the obs subsystem's OWN tables, read
	// ONLY by internal/obs/store. Their org-tier disclosure IS a real
	// data flow — the obs-org-tier subsystem (docs/plans/
	// obs-org-tier-plan-2026-06-29.md) pushes trace/span STRUCTURE (T2),
	// eval-run SUMMARIES (T4), per-end-user SPEND (T5), and — gated by
	// shipsRawContent() — raw span bodies (T3), each under its own
	// [org_client.share] obs_* opt-in (default OFF; on in the hosted-app
	// admin_managed deployment). What this sentinel enforces is the
	// MODULE BOUNDARY, not "never pushed": orgpush.go must NEVER name an
	// obs_* table directly — the disclosure is composed through the
	// injected obs func seam (store.ObsOrgProviders), exactly like
	// RoutingSummaries. So these names must not appear as literals in
	// orgpush.go even though the data they hold does (selectively) ship.
	"obs_traces",
	"obs_spans",
	"obs_span_events",
	"obs_span_links",
	"obs_span_content",
	// obs eval plane (migration 0002, plan §8): datasets, dataset items
	// (snapshot input/output bodies), eval runs and scores are obs-owned.
	// Eval HEALTH ships as the T4 aggregate (obs_eval_summaries server
	// side) via the EvalRuns func seam; these raw tables are never NAMED
	// in orgpush.go (module boundary — same rule as above).
	"obs_datasets",
	"obs_dataset_items",
	"obs_eval_runs",
	"obs_eval_scores",

	// Benchmarks Harness tables (migration 061,
	// docs/plans/benchmarks-harness-plan-2026-07-11.md §3.3) are NODE-LOCAL:
	// they hold repo paths, task prompts, final-answer excerpts, and judge
	// rationales — far higher privacy surface than the metadata that DOES
	// push, and personal benchmarking telemetry besides. They never enter the
	// wire path: no paired orgserver migration exists, and orgpush.go names an
	// explicit table allow-list these are not in. A future team-level
	// leaderboard, if ever built, would be a separate opt-in AGGREGATE wire
	// shape (per-config success-rate + cost, no prompts/paths), composed via a
	// function seam — never these tables (the routing_summary precedent).
	"benchmark_runs",
	"benchmark_attempts",
	"benchmark_session_members",
	"benchmark_scores",
	// obs input-admission audit (migration 0005, admission spec §7): the
	// verdict event log (raw request NEVER stored — only message_hash; the
	// reason_excerpt is gated by ContentGate) and the content-addressed policy
	// snapshots are NODE-LOCAL like the rest of obs_* — never on the org-push
	// wire, no server pair. (Org-level admission rollups, if ever wanted, are a
	// separate opt-in decision — obs plan §15 Q5.)
	"obs_admission_events",
	"obs_admission_policy_versions",

	// Opt-in aggregate rail state (migration 062,
	// docs/plans/g25-optin-aggregate-rail-design-2026-07-11.md §6.5). Both
	// tables are NODE-LOCAL by construction: aggregate_submissions is a
	// per-month submission ledger (month/hash/state/attempt bookkeeping + a
	// bounded snapshot of the CONTENT-FREE allow-listed payload), and
	// aggregate_consent is the single-row consent receipt. The aggregate rail
	// is an org-INDEPENDENT sibling of org-push, never an extension of it: it
	// has its OWN egress seam (internal/aggregateclient), its own consent gate,
	// and it does not round-trip through SelectUnpushedSince at all. These
	// literal sentinels keep both table names out of orgpush.go — the wire that
	// DOES leave under this rail is the aggregate.Submission (24-field
	// allow-list), pinned separately by tests/invariant/aggregate_test.go.
	"aggregate_submissions",
	"aggregate_consent",

	// Remote-access audit log (migration 063, remote-dashboard-access plan
	// §4.8) is NODE-LOCAL: it records remote-exposure events (paired session
	// ids, resolved capability, matched route, decision) for the operator's own
	// `observer remote status`. It never leaves the machine — no paired
	// orgserver migration, and orgpush.go names an explicit table allow-list
	// this is never in. Same posture as cachetrack / limit_snapshots. If a
	// team-level "who accessed my node remotely" view is ever wanted it is a
	// separate opt-in AGGREGATE wire shape, never this raw table.
	"remote_audit",

	// obs Plane-A egress-routing audit (migration 0007, G22 design §7): the
	// egress decision log is NODE-LOCAL like the rest of obs_* — v1 ships NO
	// egress org tier (design §8), so there is no push path at all. The raw
	// request is NEVER stored (only message_hash); user/tenant are node-local
	// PII. This literal sentinel keeps the table name out of orgpush.go; the
	// structural absence of any Egress provider on store.ObsOrgProviders
	// (TestObsEgressHasNoOrgProviderSeam) proves it can never be composed onto
	// the wire either.
	"obs_egress_decisions",

	// Terminal-run identity + correlation (migration 064, terminal-product-
	// exploitation plan §2.1a / §7 — Phase 0 item S0b). Both tables are
	// NODE-LOCAL: they record which tool a dashboard terminal launched, when, at
	// what (hashed) project root, and which agent sessions the launch was
	// confidently correlated to — personal workflow telemetry. They never enter
	// the wire path: no paired orgserver migration exists, and orgpush.go names
	// an explicit table allow-list neither is in. A future team-level "who ran
	// what" view, if ever built, is a separate opt-in AGGREGATE wire shape,
	// never these raw tables. Same posture as the remote_audit / limit_snapshots
	// node-local tables.
	"terminal_run",
	"terminal_run_session",
	// Terminal command/turn boundaries (migration 065, terminal-product-
	// exploitation plan §7 / F3). NODE-LOCAL: command/turn boundary
	// coordinates + exit codes + provenance for a dashboard terminal launch,
	// metadata/coordinates only (never command text or output). Never on the
	// wire — no server pair; same posture as terminal_run.
	"terminal_commands",
	// Persisted remote device sessions (migration 066, persist-remote-sessions
	// plan 2026-07-14). NODE-LOCAL: remote_sessions holds the sha256 hash of a
	// paired device's bearer cookie (never the raw token) + its generation and
	// idle clock; remote_session_state holds the single-row durable generation
	// fence. These let a paired phone survive a daemon restart without weakening
	// the revoke/rotate/disable invariant. They never leave the machine — no
	// paired orgserver migration, and orgpush.go names an explicit table
	// allow-list neither is in. Same posture as remote_audit / limit_snapshots.
	"remote_sessions",
	"remote_session_state",

	// Session classification (migration 075,
	// docs/plans/session-classification-tags-plan-2026-07-31.md §1). Tag names
	// and notes are the SAME PRIVACY CLASS as sessions.git_branch, which was
	// gated off the org wire on 2026-07-02 (security review M2) precisely
	// because free-text labels a developer authors encode client names,
	// codenames and ticket ids. A tag vocabulary is that failure mode by
	// construction ("acme-migration", "PROJ-1421", "junk"), and the note field
	// is unbounded prose about why a run mattered.
	//
	// Both tables are therefore NODE-LOCAL under EVERY share mode — including
	// admin_managed, which flips content-bearing columns raw for the other
	// surfaces. There is no share key for them and no paired orgserver
	// migration, by design; an org-shared taxonomy would need an explicit new
	// [org_client.share] key plus its own privacy review (plan §0, deferred).
	// session_tags additionally carries the end-to-end pin below
	// (TestSessionClassificationPinnedOutOfPush).
	"session_tags",
	"session_annotations",
}

// forbiddenGatewayTables names the SERVER-SIDE gateway tables that MUST NEVER be
// referenced as a string literal in internal/store/orgpush.go. Unlike the
// node-local forbiddenCacheTables above, these live in the ORG SERVER's DB — but
// the same source-level sentinel applies: the node→server push seam
// (SelectUnpushedSince) must never name them, because they are SERVER-ONLY ingest
// state the node never possesses. gateway_wal is the P0-9 durable-edge WAL — a
// server-only ingest queue whose payload holds mapped telemetry the node never
// possesses; naming it on the push wire would be nonsensical AND a boundary
// violation. Walked by TestSelectUnpushedSinceExcludesCacheTables alongside
// forbiddenCacheTables.
var forbiddenGatewayTables = []string{
	"gateway_wal",
	// The P1-2/P1-10 generic trace/span attribute-retention tier (server
	// migration 033) makes gateway_span carry retained OTLP attributes, and it
	// references the content-addressed gateway_resource/gateway_scope envelopes;
	// gateway_trace holds synthesized trace summaries. All four are SERVER-ONLY
	// gateway ingest tables (mig 026/027) that the NODE never possesses, so their
	// names must never appear on the org-push wire either.
	"gateway_resource",
	"gateway_scope",
	"gateway_span",
	"gateway_trace",
}

// forbiddenOrgControlPlaneTables are SERVER-ONLY P1-4/P1-7/P1-9 control-plane
// tables that must never appear in orgpush.go string literals.
var forbiddenOrgControlPlaneTables = []string{
	"org_intelligence_config",
	"insight_playbook",
	"insight_run",
	"insight_recommendation",
	"org_fleet_upgrade_cohorts",
	"org_fleet_cohort_members",
	"org_fleet_self_telemetry",
	"org_fleet_remote_config",
	"org_fleet_cert_events",
	"org_deployment_wizard",
	"org_deployment_connectors",
	// org_agent_policy_attributes (server migration 044) is the AUTHORITATIVE
	// per-subject policy-targeting binding: it decides which policy version a
	// node is served, and it exists precisely BECAUSE the node must not be the
	// one asserting those attributes. It is control-plane state the node never
	// possesses, so naming it on the node->server push wire would be both
	// nonsensical and a boundary violation. Pinned as a fixed expected member
	// by TestForbiddenOrgControlPlaneTablesExpectedSet below.
	"org_agent_policy_attributes",
	// The WS3 insight-harness trio (server migration 045). org_llm_provider is
	// the org's registered LLM connections and org_secret holds the SEALED
	// credential bodies those connections resolve — a credential store that the
	// node neither possesses nor may ever be shipped. insight_run_step is the
	// server-side audited step trace of an insight run (which query the agent
	// asked for, which model answered, what it cost). All three are org
	// control-plane state with no agent counterpart, so naming any of them on
	// the node->server push wire would be both nonsensical and a boundary
	// violation. Pinned as fixed expected members by
	// TestForbiddenOrgControlPlaneTablesExpectedSet below.
	"org_llm_provider",
	"org_secret",
	"insight_run_step",
	// The fleet remote-config mutation trail (server migration 046). It
	// records WHICH ADMIN changed a collector's remote config, in which org —
	// server-side attribution for a control-plane mutation the node never
	// makes and never sees. Naming it on the node->server push wire would be
	// both nonsensical and a boundary violation. Pinned as a fixed expected
	// member by TestForbiddenOrgControlPlaneTablesExpectedSet below.
	"org_fleet_config_events",
	// The WS2 guided-setup settings store (server migration 047). Its rows
	// hold the org's storage-connection settings and a secretref pointing at a
	// SEALED credential body in org_secret — org control-plane configuration
	// the node neither possesses nor may ever be shipped. Naming it on the
	// node->server push wire would be both nonsensical and a boundary
	// violation. Pinned as a fixed expected member by
	// TestForbiddenOrgControlPlaneTablesExpectedSet below.
	"org_setup_setting",
	// The WS1.4 project display-label store (server migration 048). Rows are
	// ADMIN-AUTHORED display strings for a project hash — org-side naming the
	// node never possesses and must never be asked for. The node ships the
	// hash and only the hash; a seam in orgpush.go that named this table would
	// mean the label had become node data (or, worse, that the wire had grown
	// a raw-path counterpart), which is exactly the inversion the hash posture
	// exists to prevent. Pinned as a fixed expected member by
	// TestForbiddenOrgControlPlaneTablesExpectedSet below.
	"org_project_labels",
	// The dashboard-registered external harness agent registry (server
	// migration 049). Rows are org control-plane configuration (an admin's
	// choice of which pre-placed binary answers an agent id) with no agent
	// counterpart — the node never registers, resolves, or executes these,
	// it only ever sees the resulting "external:<id>" agent id the same way
	// it would a TOML entry. Naming it on the node->server push wire would be
	// both nonsensical and a boundary violation. Pinned as a fixed expected
	// member by TestForbiddenOrgControlPlaneTablesExpectedSet below.
	"org_external_agent",
	// Datasets foundation (server migration 051, wave item B): reference-only
	// trace/span membership for evals and experiments. Org control-plane
	// state with no agent counterpart — the node neither builds nor reads
	// datasets. Pinned as fixed expected members below.
	"org_dataset",
	"org_dataset_item",
	// LLM job runner + annotations (server migration 052, wave items C/D):
	// deterministic single-shot job state and metadata-class trace labels,
	// org control-plane only — the node never runs or reads these.
	"org_trace_annotation",
	"org_llm_job",
	"org_llm_job_item",
	// Guardrailed chat (server migration 053, wave item F): chat transcripts
	// and proposal state are org control-plane content with no agent
	// counterpart — the node never sees the assistant conversation.
	"org_chat_session",
	"org_chat_message",
	// Plane-B admin intelligence (server migrations 057-059 + the P2 harness
	// trio reserved by the spec's §2.3 nine-table pin;
	// docs/plans/plane-b-admin-intelligence-and-enforcement-spec-2026-08-15.md).
	// All are org control-plane state with no agent counterpart: the coding
	// intelligence config document, session-referencing dataset membership,
	// the single-shot job runner's state, session annotations, and the
	// Plane-B playbook/run tables. The node never builds, runs, or reads any
	// of them; naming one on the node->server push wire would be a boundary
	// violation. The content-adjacent trio (job, job_item, annotation) is
	// additionally pinned as fixed expected members below.
	"org_coding_intel_config",
	"org_coding_dataset",
	"org_coding_dataset_item",
	"org_coding_job",
	"org_coding_job_item",
	"org_coding_annotation",
	"org_coding_playbook",
	"org_coding_run",
	"org_coding_run_step",
	// Self-Improving Policy Plane P0 (server migrations 061-065;
	// docs/plans/self-improving-policy-evolution-p0-build-plan-2026-08-16.md).
	// All five are org control-plane state with no agent counterpart: the
	// per-(org,family,target) evolution config, the review-run ledger, the
	// candidate-proposal store (proposed_body + LLM-authored rationale), the
	// hash-chained approve/reject audit, and the manual-Apply outcome record.
	// The node never builds, runs, or reads any of them; naming one on the
	// node->server push wire would be a boundary violation. The two
	// content-bearing tables (proposal, decision — where LLM-authored text
	// lands) are additionally pinned as fixed expected members below.
	"org_policy_evolution_config",
	"org_policy_evolution_run",
	"org_policy_evolution_proposal",
	"org_policy_evolution_decision",
	"org_policy_evolution_applied",
}

// TestForbiddenOrgControlPlaneTablesExpectedSet is the NON-GAMEABLE membership
// pin for the org control-plane sentinel, mirroring
// TestForbiddenGatewayTablesExpectedSet. Merely APPENDING a name to
// forbiddenOrgControlPlaneTables is trivially removable without any test
// failing, which would silently stop TestSelectUnpushedSinceExcludesCacheTables
// from guarding that name out of orgpush.go. A FIXED expected subset must
// therefore remain present: deleting any of these names fails HERE, by name.
func TestForbiddenOrgControlPlaneTablesExpectedSet(t *testing.T) {
	want := []string{
		"org_agent_policy_attributes",
		"org_intelligence_config",
		"org_fleet_upgrade_cohorts",
		"org_fleet_remote_config",
		"org_deployment_wizard",
		// WS3 insight harness (server migration 045). org_secret in particular
		// is a SEALED CREDENTIAL store: dropping it from the sentinel would
		// stop the orgpush.go guard from ever noticing a seam that named it.
		"org_llm_provider",
		"org_secret",
		"insight_run_step",
		// Fleet mutation attribution (server migration 046): the audit trail
		// of who reconfigured which collector. Server-only control-plane
		// state with no agent counterpart.
		"org_fleet_config_events",
		// WS2 guided setup (server migration 047): the settings store whose
		// secret_ref column points at a SEALED org_secret body. Dropping it
		// from the sentinel would stop the orgpush.go guard from ever noticing
		// a seam that named it.
		"org_setup_setting",
		// WS1.4 project labels (server migration 048): org-side display names
		// for hashed project identities. Dropping it from the sentinel would
		// stop the orgpush.go guard from noticing a seam that tried to carry
		// project naming — in either direction — on the agent wire.
		"org_project_labels",
		// Dashboard-registered external harness agents (server migration
		// 049): org control-plane configuration with no agent counterpart.
		// Dropping it from the sentinel would stop the orgpush.go guard from
		// noticing a seam that named it.
		"org_external_agent",
		// Datasets foundation (server migration 051): dataset membership is
		// org-side only; dropping these would stop the guard from noticing a
		// seam that tried to carry dataset state on the agent wire.
		"org_dataset",
		"org_dataset_item",
		// LLM jobs + annotations (server migration 052): job/annotation
		// state stays org-side; dropping these would stop the guard from
		// noticing a seam that named them.
		"org_trace_annotation",
		"org_llm_job",
		"org_llm_job_item",
		// Guardrailed chat (server migration 053): transcripts stay
		// org-side; dropping these would stop the guard from noticing a
		// seam that named them.
		"org_chat_session",
		"org_chat_message",
		// Plane-B admin intelligence (server migrations 057-059): the
		// content-adjacent trio — job state, per-item results, and session
		// annotations are the rows an LLM's output lands in. Dropping any of
		// them from the sentinel would stop the orgpush.go guard from
		// noticing a seam that named them.
		"org_coding_job",
		"org_coding_job_item",
		"org_coding_annotation",
		// Self-Improving Policy Plane P0 (server migrations 061-065): the
		// content-bearing pair. org_policy_evolution_proposal holds the
		// candidate proposed_body + LLM-authored rationale, and
		// org_policy_evolution_decision holds the approve/reject reason —
		// the rows an LLM's output lands in. Dropping either from the
		// sentinel would stop the orgpush.go guard from noticing a seam
		// that named them.
		"org_policy_evolution_proposal",
		"org_policy_evolution_decision",
	}
	have := make(map[string]struct{}, len(forbiddenOrgControlPlaneTables))
	for _, n := range forbiddenOrgControlPlaneTables {
		have[n] = struct{}{}
	}
	for _, w := range want {
		if _, ok := have[w]; !ok {
			t.Errorf("expected server-side control-plane table %q is missing from forbiddenOrgControlPlaneTables — "+
				"the privacy sentinel would no longer guard it out of orgpush.go", w)
		}
	}
}

// TestForbiddenGatewayTablesExpectedSet is the NON-GAMEABLE membership pin: a
// FIXED expected set (gateway_wal + the four gateway ingest tables the P1-2/P1-10
// retention tier touches) must be a SUBSET of forbiddenGatewayTables. Merely
// APPENDING a name is removable without a failure (the original TestGatewayWAL…
// only checked gateway_wal), so REMOVING any of these names from
// forbiddenGatewayTables fails HERE — which in turn would stop
// TestSelectUnpushedSinceExcludesCacheTables from guarding that name in
// orgpush.go. Mut 10 (drop gateway_span from forbiddenGatewayTables) fails this
// assertion by name.
func TestForbiddenGatewayTablesExpectedSet(t *testing.T) {
	want := []string{
		"gateway_wal",
		"gateway_resource",
		"gateway_scope",
		"gateway_span",
		"gateway_trace",
	}
	have := make(map[string]struct{}, len(forbiddenGatewayTables))
	for _, n := range forbiddenGatewayTables {
		have[n] = struct{}{}
	}
	for _, w := range want {
		if _, ok := have[w]; !ok {
			t.Errorf("expected server-side gateway table %q is missing from forbiddenGatewayTables — "+
				"the privacy sentinel would no longer guard it out of orgpush.go", w)
		}
	}
}

// TestGatewayWALPinnedOutOfPush pins gateway_wal (server migration 032, Plane-A
// P0-9 durable-edge WAL) at the source level: its name is in the
// forbiddenGatewayTables sentinel, so TestSelectUnpushedSinceExcludesCacheTables
// fails the build if it ever appears as a string literal in orgpush.go. There is
// no end-to-end seed arm because the table lives in the ORG SERVER's DB, not the
// agent store the push seam reads — the source-level pin is the guarantee.
func TestGatewayWALPinnedOutOfPush(t *testing.T) {
	found := false
	for _, n := range forbiddenGatewayTables {
		if n == "gateway_wal" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("gateway_wal is not in forbiddenGatewayTables — the source-level sentinel is missing")
	}
}

// TestRemoteAuditTablePinnedOutOfPush pins the remote-access audit log
// (migration 063, remote-dashboard-access plan §4.8) out of the org-push wire
// two ways: (1) the table name is in the forbidden-name sentinel set (so
// TestSelectUnpushedSinceExcludesCacheTables fails the build if it ever appears
// as a string literal in orgpush.go), and (2) an end-to-end assertion that a
// seeded remote_audit row never crosses the wire — even under full_content
// (maximum disclosure surface). The row carries no *_hash counterpart because
// it is never pushed at all.
func TestRemoteAuditTablePinnedOutOfPush(t *testing.T) {
	// (1) name pinned in the sentinel set.
	found := false
	for _, n := range forbiddenCacheTables {
		if n == "remote_audit" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("remote_audit is not in forbiddenCacheTables — the source-level sentinel is missing")
	}

	// (2) end-to-end: a seeded remote_audit row never rides the wire.
	const secRemoteAudit = "SECRET_REMOTEAUDIT_grandiloquent_zz"
	ctx := context.Background()
	database, err := db.Open(ctx, db.Options{Path: filepath.Join(t.TempDir(), "agent.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer func() { _ = database.Close() }()
	st := store.New(database)
	seed(ctx, t, st)

	if err := st.InsertRemoteAudit(ctx, store.RemoteAuditEvent{
		Kind: "session_paired", SessionID: secRemoteAudit, Principal: "view",
		RemoteAddr: secRemoteAudit, Route: "/api/remote/pair", Decision: "ok",
		Detail: secRemoteAudit,
	}); err != nil {
		t.Fatalf("InsertRemoteAudit: %v", err)
	}
	// Also seed the Phase-4 execute-tier audit kinds (writer-lease + terminal-
	// control lifecycle) — new kinds ride the SAME node-local table, so the
	// table-level exclusion must still hold with a canary in every column.
	for _, k := range []string{
		"terminal_writer_acquire", "terminal_writer_release", "terminal_writer_revoke",
		"terminal_local_takeover", "terminal_remote_takeover",
		"terminal_control_local_approval", "terminal_control_request",
		"terminal_control_capability_consume", "terminal_control_denied", "terminal_denied_frame",
	} {
		if err := st.InsertRemoteAudit(ctx, store.RemoteAuditEvent{
			Kind: k, SessionID: secRemoteAudit, Principal: "execute",
			RemoteAddr: secRemoteAudit, Route: secRemoteAudit, Decision: "ok",
			Detail: secRemoteAudit,
		}); err != nil {
			t.Fatalf("InsertRemoteAudit(%s): %v", k, err)
		}
	}

	// Full-content = maximum surface; the audit table must STILL be absent.
	batch, err := st.SelectUnpushedSince(ctx, store.PushCursor{}, 1<<20, "org-1", "dev@x",
		store.ShareOptions{FullContent: true}, store.ScopeOptions{})
	if err != nil {
		t.Fatalf("SelectUnpushedSince: %v", err)
	}
	raw, err := json.Marshal(batch)
	if err != nil {
		t.Fatalf("marshal batch: %v", err)
	}
	if bytes.Contains(raw, []byte(secRemoteAudit)) {
		t.Error("remote_audit content leaked into the push payload — the table must never be pushed")
	}
}

// TestTerminalRunTablesPinnedOutOfPush pins the terminal-run identity tables
// (migration 064, terminal-product-exploitation plan §2.1a / §7) out of the
// org-push wire two ways: (1) both table names are in the forbidden-name
// sentinel set (so TestSelectUnpushedSinceExcludesCacheTables fails the build if
// either appears as a string literal in orgpush.go), and (2) an end-to-end
// assertion that a seeded terminal_run + terminal_run_session row never crosses
// the wire — even under full_content (maximum disclosure surface). The rows
// carry no *_hash counterpart because they are never pushed at all.
func TestTerminalRunTablesPinnedOutOfPush(t *testing.T) {
	// (1) both names pinned in the sentinel set.
	for _, name := range []string{"terminal_run", "terminal_run_session", "terminal_commands"} {
		found := false
		for _, n := range forbiddenCacheTables {
			if n == name {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%s is not in forbiddenCacheTables — the source-level sentinel is missing", name)
		}
	}

	// (2) end-to-end: a seeded terminal-run row never rides the wire.
	const secTermRun = "SECRET_TERMRUN_pusillanimous_zz"
	ctx := context.Background()
	database, err := db.Open(ctx, db.Options{Path: filepath.Join(t.TempDir(), "agent.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer func() { _ = database.Close() }()
	st := store.New(database)
	seed(ctx, t, st)

	if err := st.InsertTerminalRun(ctx, store.TerminalRun{
		RunID: secTermRun, Tool: secTermRun, Kind: "handoff",
		SourceSessionID: secTermRun, ProjectRootHash: secTermRun,
		CorrelationTokenHash: secTermRun,
	}); err != nil {
		t.Fatalf("InsertTerminalRun: %v", err)
	}
	if err := st.UpsertCorrelation(ctx, store.TerminalCorrelation{
		RunID: secTermRun, SessionID: secTermRun, Confidence: 0.95, Source: "oob",
	}); err != nil {
		t.Fatalf("UpsertCorrelation: %v", err)
	}
	if err := st.InsertTerminalCommand(ctx, store.TerminalCommand{
		RunID: secTermRun, TurnSeq: 1, Trust: "hint", CmdHash: secTermRun,
	}); err != nil {
		t.Fatalf("InsertTerminalCommand: %v", err)
	}

	// Full-content = maximum surface; the identity tables must STILL be absent.
	batch, err := st.SelectUnpushedSince(ctx, store.PushCursor{}, 1<<20, "org-1", "dev@x",
		store.ShareOptions{FullContent: true}, store.ScopeOptions{})
	if err != nil {
		t.Fatalf("SelectUnpushedSince: %v", err)
	}
	raw, err := json.Marshal(batch)
	if err != nil {
		t.Fatalf("marshal batch: %v", err)
	}
	if bytes.Contains(raw, []byte(secTermRun)) {
		t.Error("terminal_run content leaked into the push payload — the tables must never be pushed")
	}
}

// TestSessionClassificationPinnedOutOfPush pins the session-classification
// tables (migration 075, plan §1) out of the org-push wire two ways: (1) both
// table names are in the forbidden-name sentinel set (so
// TestSelectUnpushedSinceExcludesCacheTables fails the build if either ever
// appears as a string literal in orgpush.go), and (2) an end-to-end assertion
// that a seeded tag AND a seeded note never cross the wire — even under
// full_content, the maximum disclosure surface. The rows carry no *_hash
// counterpart because they are never pushed at all.
//
// PROOF SCOPE — what this test does and does not establish. It proves absence
// from the CURRENT SelectUnpushedSince payload (one seeded canary tag + note on
// a session the push actually visits, marshalled and byte-searched) and it
// proves the source-level sentinel covers both table names inside orgpush.go.
// It does NOT prove the tables are unreachable by any future egress path: a new
// wire-building query added ELSEWHERE — a second push seam, an export handler,
// an org-side pull — would not be caught here. That case is held by CONVENTION,
// not by this test: the single-SQL-seam rule (CLAUDE.md "Teams / org-server
// invariants" — SelectUnpushedSince is the only place wire rows are built) plus
// the review requirement that any new content-bearing wire column be gated at
// that seam AND added to the sentinel set. Widening egress therefore requires
// touching the one file this test watches; a reviewer who accepts a second seam
// has stepped outside what any of these invariants can see.
func TestSessionClassificationPinnedOutOfPush(t *testing.T) {
	// (1) both names pinned in the sentinel set.
	for _, name := range []string{"session_tags", "session_annotations"} {
		found := false
		for _, n := range forbiddenCacheTables {
			if n == name {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%s is not in forbiddenCacheTables — the source-level sentinel is missing", name)
		}
	}

	// (2) end-to-end: a seeded tag + note never ride the wire. The tag must
	// survive NormalizeTag's charset, so the canary is lowercase/hyphenated —
	// exactly the shape a real client codename would take.
	const (
		secTag  = "secret-sessiontag-obstreperous-zz"
		secNote = "SECRET_SESSIONNOTE_obstreperous_zz"
	)
	ctx := context.Background()
	database, err := db.Open(ctx, db.Options{Path: filepath.Join(t.TempDir(), "agent.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer func() { _ = database.Close() }()
	st := store.New(database)
	seed(ctx, t, st)

	// Tag a session that DOES ride the wire, so the exclusion is proven for a
	// row the push actually visits — not merely for an orphan id.
	if err := st.MutateSessionTags(ctx, "sess-cc-1", []string{secTag}, nil); err != nil {
		t.Fatalf("MutateSessionTags: %v", err)
	}
	fav := true
	note := secNote
	rating := 9 // the overall rating (migration 080) rides the same node-local row.
	if err := st.SetSessionAnnotation(ctx, "sess-cc-1", &fav, &note, &rating); err != nil {
		t.Fatalf("SetSessionAnnotation: %v", err)
	}

	// Full-content = maximum surface; the classification tables must STILL be
	// absent.
	batch, err := st.SelectUnpushedSince(ctx, store.PushCursor{}, 1<<20, "org-1", "dev@x",
		store.ShareOptions{FullContent: true}, store.ScopeOptions{})
	if err != nil {
		t.Fatalf("SelectUnpushedSince: %v", err)
	}
	raw, err := json.Marshal(batch)
	if err != nil {
		t.Fatalf("marshal batch: %v", err)
	}
	if bytes.Contains(raw, []byte(secTag)) {
		t.Error("session_tags content leaked into the push payload — the table must never be pushed")
	}
	if bytes.Contains(raw, []byte(secNote)) {
		t.Error("session_annotations note leaked into the push payload — the table must never be pushed")
	}

	// The same must hold under admin_managed, which deliberately flips the
	// OTHER content-bearing columns raw (CLAUDE.md native-console carve-out).
	batch, err = st.SelectUnpushedSince(ctx, store.PushCursor{}, 1<<20, "org-1", "dev@x",
		store.ShareOptions{AdminManaged: true}, store.ScopeOptions{})
	if err != nil {
		t.Fatalf("SelectUnpushedSince(admin_managed): %v", err)
	}
	raw, err = json.Marshal(batch)
	if err != nil {
		t.Fatalf("marshal admin_managed batch: %v", err)
	}
	if bytes.Contains(raw, []byte(secTag)) || bytes.Contains(raw, []byte(secNote)) {
		t.Error("session classification leaked under admin_managed — the tables are node-local under EVERY share mode")
	}
}

// TestPolicyResourceTablesPinnedOutOfPush pins the Plane-A P0-5 unified
// policy resource tables (migration 081, plan §6.2/§6.9/§6.10) out of the
// org-push wire two ways: (1) both table names are in the forbidden-name
// sentinel set (so TestSelectUnpushedSinceExcludesCacheTables fails the
// build if either appears as a string literal in orgpush.go), and (2) an
// end-to-end assertion that seeded rows in both tables never cross the wire
// — even under full_content, the maximum disclosure surface. The rows carry
// no *_hash counterpart because they are never pushed at all.
func TestPolicyResourceTablesPinnedOutOfPush(t *testing.T) {
	// (1) both names pinned in the sentinel set.
	for _, name := range []string{"org_enrolment_generation", "org_policy_resource_state"} {
		found := false
		for _, n := range forbiddenCacheTables {
			if n == name {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%s is not in forbiddenCacheTables — the source-level sentinel is missing", name)
		}
	}

	// (2) end-to-end: seeded generation + state rows never ride the wire.
	const secOrgKey = "secret-orgkey-perspicacious-zz"
	ctx := context.Background()
	database, err := db.Open(ctx, db.Options{Path: filepath.Join(t.TempDir(), "agent.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer func() { _ = database.Close() }()
	st := store.New(database)
	seed(ctx, t, st)

	if _, err := st.BumpEnrolmentGeneration(ctx, secOrgKey, false); err != nil {
		t.Fatalf("BumpEnrolmentGeneration: %v", err)
	}
	err = st.WithPolicyResourceFence(ctx, secOrgKey, "admission.input", func(_ context.Context, fence store.PolicyResourceFence) (*store.PolicyResourceCommit, error) {
		return &store.PolicyResourceCommit{
			Generation: fence.Generation, FloorVersion: 1, LastVersion: 1,
			BodyHash: secOrgKey, MsgDigest: secOrgKey,
		}, nil
	})
	if err != nil {
		t.Fatalf("WithPolicyResourceFence: %v", err)
	}

	batch, err := st.SelectUnpushedSince(ctx, store.PushCursor{}, 1<<20, "org-1", "dev@x",
		store.ShareOptions{FullContent: true}, store.ScopeOptions{})
	if err != nil {
		t.Fatalf("SelectUnpushedSince: %v", err)
	}
	raw, err := json.Marshal(batch)
	if err != nil {
		t.Fatalf("marshal batch: %v", err)
	}
	if bytes.Contains(raw, []byte(secOrgKey)) {
		t.Error("policy-resource state leaked into the push payload — the tables must never be pushed")
	}
}

// TestObsEgressHasNoOrgProviderSeam pins the G22 privacy posture structurally
// (design §7/§8): v1 ships NO egress org tier, so store.ObsOrgProviders must
// carry no Egress provider — obs_egress_decisions can never be composed onto
// the push wire. If a future opt-in egress_summary aggregate is ever added
// (design §8 deferred), it lands as its own AGGREGATE wire shape + share key +
// sentinel update, never the raw decisions table — and this test is the
// forcing function that makes that a conscious change.
func TestObsEgressHasNoOrgProviderSeam(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeOf(store.ObsOrgProviders{})
	for i := 0; i < typ.NumField(); i++ {
		if strings.Contains(strings.ToLower(typ.Field(i).Name), "egress") {
			t.Errorf("store.ObsOrgProviders gained an egress provider field %q — v1 ships no egress org tier; obs_egress_decisions must have NO push seam (design §8)", typ.Field(i).Name)
		}
	}
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
		for _, name := range forbiddenGatewayTables {
			if strings.Contains(lit.Value, name) {
				pos := fset.Position(lit.Pos())
				t.Errorf("%s:%d: forbidden server-side gateway table name %q appears in string literal — "+
					"gateway_wal is a SERVER-ONLY ingest queue (P0-9 durable-edge WAL); it MUST NOT enter the push wire path",
					pos.Filename, pos.Line, name)
			}
		}
		for _, name := range forbiddenOrgControlPlaneTables {
			if strings.Contains(lit.Value, name) {
				pos := fset.Position(lit.Pos())
				t.Errorf("%s:%d: forbidden org control-plane table name %q appears in string literal — "+
					"P1-4/P1-7/P1-9 control-plane tables are SERVER-ONLY and MUST NOT enter the push wire path",
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

// TestPolicyStateRowWireShapeIsHashOnly pins the P0-6 effective-state row
// (docs/plans/plane-a-p0-6-effective-policy-state-plan.md §2.1/§5.1): the
// PolicyStateRow that rides POST /api/agent/policy-ack may carry ONLY the 12
// allow-listed hash/version/enum/timestamp fields — no Body/TOML/Path/Detail/
// Message/Excerpt/Tenant/EndUser/Prompt or any free-text field, forever. A new
// field fails loudly. Modeled on TestRoutingSummaryWireShapeIsAggregateOnly.
func TestPolicyStateRowWireShapeIsHashOnly(t *testing.T) {
	t.Parallel()
	allowed := map[string]bool{
		"OrgID": true, "UserEmail": true, // server-stamped attribution (empty-on-wire)
		"Family": true, "EnforcementPoint": true, // closed enums
		"DesiredVersion": true, "RunningVersion": true, // integer versions
		"EffectiveHash": true,                 // per-point hex digest
		"Status":        true, "Reason": true, // closed enums (Reason a typed code, never Detail)
		"RestartRequired": true, "Mode": true, // bool + closed enum
		"LastSeen": true, // RFC3339 liveness
	}
	typ := reflect.TypeOf(orgcontract.PolicyStateRow{})
	if typ.NumField() != len(allowed) {
		t.Errorf("PolicyStateRow has %d fields, want exactly %d (§2.1 12-field allow-list, R4-NIT)", typ.NumField(), len(allowed))
	}
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if !allowed[name] {
			t.Errorf("PolicyStateRow gained non-allow-listed field %q — the P0-6 §2.1 wire shape is hash/version/enum/timestamp ONLY", name)
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

// TestObsAdmissionWireShape pins the T6 admission wire contract structurally
// (Plane-A admission org tier, gap-audit §2.1 / #1a): every ObsAdmissionRow
// field must be classified as either content-free-always (ships in
// metadata-only mode) or gated (ships ONLY under shipsRawContent()), and no
// field may be both or unclassified — so a new content-bearing column can't
// silently ride the always-ships path. ObsAdmissionPolicyRow carries only
// admin-authored config fields (Body always ships, like RoutingPolicyDoc.Body).
// Mirrors TestRoutingSummaryWireShapeIsAggregateOnly.
func TestObsAdmissionWireShape(t *testing.T) {
	t.Parallel()
	// Content-free-always: verdict metadata + hashes + soft-join ids + the
	// node hash-chain links. These ride even in the default metadata-only mode.
	contentFree := map[string]bool{
		"OrgID": true, "UserEmail": true, // agent-stamped attribution
		"TS": true, "Mode": true, "Decision": true, "Severity": true, // verdict enums
		"CriterionID": true, "PolicyHash": true, // criterion + soft join
		"JudgeUsed": true, "JudgeHosting": true, "Degraded": true, "LatencyMS": true, // judge facts
		"MessageHash": true,                                       // content-free request provenance (raw text never stored)
		"TraceID":     true, "SessionID": true, "RequestID": true, // content-free soft join keys
		"PrevHash": true, "RowHash": true, // tamper-evidence hash-chain links
	}
	// Gated: PII / prose — ship ONLY under shipsRawContent() (stripped in
	// composeObsTiers otherwise).
	gated := map[string]bool{
		"Tenant": true, "EndUser": true, "ReasonExcerpt": true,
	}
	typ := reflect.TypeOf(orgcontract.ObsAdmissionRow{})
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		switch {
		case contentFree[name] && gated[name]:
			t.Errorf("ObsAdmissionRow field %q classified as BOTH content-free and gated — pick one", name)
		case !contentFree[name] && !gated[name]:
			t.Errorf("ObsAdmissionRow gained UNCLASSIFIED field %q — classify it content-free-always or gated (shipsRawContent) so the strip in composeObsTiers stays exhaustive", name)
		}
	}
	// The policy snapshot carries only admin-authored config — every field
	// always ships (Body included, like RoutingPolicyDoc.Body).
	policyAllowed := map[string]bool{
		"OrgID": true, "UserEmail": true,
		"PolicyHash": true, "CreatedAt": true, "Mode": true, "Scope": true,
		"CriteriaCount": true, "Body": true,
	}
	ptyp := reflect.TypeOf(orgcontract.ObsAdmissionPolicyRow{})
	for i := 0; i < ptyp.NumField(); i++ {
		name := ptyp.Field(i).Name
		if !policyAllowed[name] {
			t.Errorf("ObsAdmissionPolicyRow gained unexpected field %q — the policy snapshot is admin config only (hash/created_at/mode/scope/criteria_count/body)", name)
		}
	}
}

// TestObsEvalItemWireShape pins the T7 per-item eval wire contract structurally
// (Plane-A eval-run detail org tier, gap-audit §1 / §2.2 / §6): every
// ObsEvalItemRow field must be classified as either content-free-always (ships
// in metadata-only mode) or gated (ships ONLY under shipsRawContent()), and no
// field may be both or unclassified — so a new content-bearing column can't
// silently ride the always-ships path. Mirrors TestObsAdmissionWireShape.
func TestObsEvalItemWireShape(t *testing.T) {
	t.Parallel()
	// Content-free-always: attribution + run/dataset identity + span/trace soft
	// joins + the score verdict + duration/ts + the dataset item's content_hash.
	contentFree := map[string]bool{
		"OrgID": true, "UserEmail": true, // agent-stamped attribution
		"RunID": true, "RunName": true, "DatasetID": true, "DatasetName": true, // run/dataset identity
		"ItemID": true, "SpanID": true, "TraceID": true, // item + content-free soft joins
		"Scorer": true, "Score": true, "Passed": true, "Source": true, // verdict
		"DurationMs": true, "TS": true, // span duration + score instant
		"ContentHash": true, // content-free signal (raw bodies gated below)
	}
	// Gated: bounded content excerpts + scorer prose — ship ONLY under
	// shipsRawContent() (stripped in composeObsTiers otherwise).
	gated := map[string]bool{
		"InputExcerpt": true, "ExpectedExcerpt": true, "OutputExcerpt": true, "Rationale": true,
	}
	typ := reflect.TypeOf(orgcontract.ObsEvalItemRow{})
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		switch {
		case contentFree[name] && gated[name]:
			t.Errorf("ObsEvalItemRow field %q classified as BOTH content-free and gated — pick one", name)
		case !contentFree[name] && !gated[name]:
			t.Errorf("ObsEvalItemRow gained UNCLASSIFIED field %q — classify it content-free-always or gated (shipsRawContent) so the strip in composeObsTiers stays exhaustive", name)
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
