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

// TestPushPayloadCarriesNoContent is the privacy invariant: a push must ship
// only the content-free rollup shapes. It seeds the corpus, then stuffs a
// distinctive secret string into EVERY content column the agent stores
// (raw_tool_input, raw_tool_output, preceding_reasoning, error_message), runs
// one real push cycle against an in-process server, and asserts that not one
// of those secrets appears anywhere in the bytes that crossed the wire.
//
// This guards the structural privacy posture end to end: the orgcontract row
// types carry no content fields, store.SelectUnpushedSince selects no content
// columns, and this test fails loudly if either ever regresses.
func TestPushPayloadCarriesNoContent(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, db.Options{Path: filepath.Join(t.TempDir(), "agent.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer func() { _ = database.Close() }()
	st := store.New(database)
	seed(ctx, t, st)

	// Distinctive secrets, one per content column. None of these may appear in
	// the pushed bytes.
	const (
		secRawInput  = "SECRET_RAW_INPUT_xyzzy_42"
		secRawOutput = "SECRET_RAW_OUTPUT_plugh_99"
		secReasoning = "SECRET_REASONING_hunter2_zz"
		secErrMsg    = "SECRET_ERRMSG_swordfish_qq"
	)
	if _, err := database.ExecContext(ctx,
		`UPDATE actions SET raw_tool_input = ?, raw_tool_output = ?, preceding_reasoning = ?, error_message = ?`,
		secRawInput, secRawOutput, secReasoning, secErrMsg); err != nil {
		t.Fatalf("stuff content columns: %v", err)
	}
	secrets := []string{secRawInput, secRawOutput, secReasoning, secErrMsg}

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
	for _, s := range secrets {
		if bytes.Contains(raw, []byte(s)) {
			t.Errorf("content secret %q leaked into the push payload", s)
		}
	}

	// Sanity: the rollup actually carried the row's allowed columns (target
	// path), proving we scanned a real, populated payload — not an empty one
	// that would trivially pass the leak check.
	if !bytes.Contains(raw, []byte("main.go")) {
		t.Errorf("expected an allowed field (target=main.go) in the payload; scan may be vacuous")
	}
}
