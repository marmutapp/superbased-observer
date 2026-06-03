package invariant

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/orgclient"
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
)

// allSentinels is the union of the original 4 content-column sentinels and
// the 4 new target/path sentinels. Every push in the default
// (full_content=false) mode must ship NONE of these.
var allSentinels = []string{
	secRawInput, secRawOutput, secReasoning, secErrMsg,
	secTarget, secSourceFile, secRootPath, secGitRemote,
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
	t.Skip("opt-in path covered by M1.2 (OrgClientShareConfig); test will activate once the config field exists")
	// Intentional placeholder: once M1.2 lands, this body mirrors
	// TestPushPayloadCarriesNoContent but constructs the client with
	// config.OrgClientShareConfig{FullContent: true} and ASSERTS each
	// sentinel DOES appear (proving the opt-in flips the seam correctly).
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
