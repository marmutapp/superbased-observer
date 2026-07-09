package handoffsvc

import (
	"context"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/adapter"
	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// The three linker methods are added to *fakeSubstrate (defined in
// svc_test.go) here so the existing Build tests keep compiling against the
// extended Substrate interface. They are no-ops — the linker itself is
// exercised through the purpose-built fakeLinkStore below.
func (f *fakeSubstrate) ListUnlinkedHandoffs(context.Context, time.Time) ([]store.HandoffRecord, error) {
	return nil, nil
}

func (f *fakeSubstrate) CandidateTargetSessions(context.Context, string, string, time.Time, int) ([]store.CandidateSession, error) {
	return nil, nil
}

func (f *fakeSubstrate) LinkTargetSession(context.Context, int64, string) error { return nil }

// fakeLinkStore is a full Substrate implementation with configurable
// linker inputs. Only the linker methods carry behaviour; the Build-side
// methods are unused here.
type fakeLinkStore struct {
	unlinked   []store.HandoffRecord
	candidates map[string][]store.CandidateSession // key: tool|projectRoot
	links      map[int64]string                    // recorded LinkTargetSession calls
}

func (f *fakeLinkStore) LoadHandoffSubstrate(context.Context, string) (store.HandoffSubstrate, error) {
	return store.HandoffSubstrate{}, nil
}

func (f *fakeLinkStore) LoadSessionShape(context.Context, string) (store.PredictShape, error) {
	return store.PredictShape{}, nil
}

func (f *fakeLinkStore) InsertHandoff(context.Context, store.HandoffRecord) (int64, error) {
	return 0, nil
}

func (f *fakeLinkStore) ListUnlinkedHandoffs(context.Context, time.Time) ([]store.HandoffRecord, error) {
	return f.unlinked, nil
}

func (f *fakeLinkStore) CandidateTargetSessions(_ context.Context, tool, projectRoot string, _ time.Time, _ int) ([]store.CandidateSession, error) {
	return f.candidates[tool+"|"+projectRoot], nil
}

func (f *fakeLinkStore) LinkTargetSession(_ context.Context, id int64, target string) error {
	if f.links == nil {
		f.links = map[int64]string{}
	}
	// Guard-emulation: honour the store's write-once contract.
	if _, ok := f.links[id]; ok {
		return nil
	}
	f.links[id] = target
	return nil
}

// fakeLinkAdapter returns a per-session transcript so the linker can match
// distinct candidate sessions. It records the source hints it was asked
// with per session so a test can prove the linker threads them through.
type fakeLinkAdapter struct {
	name      string
	bySession map[string][]models.TranscriptMessage
	seenHints map[string][]string // session id → hints passed to ReadTranscript
}

func (f *fakeLinkAdapter) Name() string              { return f.name }
func (f *fakeLinkAdapter) WatchPaths() []string      { return nil }
func (f *fakeLinkAdapter) IsSessionFile(string) bool { return false }
func (f *fakeLinkAdapter) ParseSessionFile(context.Context, string, int64) (adapter.ParseResult, error) {
	return adapter.ParseResult{}, nil
}

func (f *fakeLinkAdapter) ReadTranscript(_ context.Context, sess models.Session, hints []string) ([]models.TranscriptMessage, error) {
	if f.seenHints == nil {
		f.seenHints = map[string][]string{}
	}
	f.seenHints[sess.ID] = hints
	return f.bySession[sess.ID], nil
}

func markerMsg(shortID string) []models.TranscriptMessage {
	return []models.TranscriptMessage{
		{Index: 0, Role: models.TranscriptUser, Text: "<!-- superbased-handoff " + shortID + " -->\ncontinue"},
	}
}

func TestLinkTargetSessions_LinksFirstMarkerMatch(t *testing.T) {
	fs := &fakeLinkStore{
		unlinked: []store.HandoffRecord{{
			ID:              7,
			SourceSessionID: "src-1",
			TargetTool:      "codex",
			ProjectRoot:     "/proj",
			DeliveryRef:     "/proj/HANDOFF-abcd1234.md",
			CreatedAt:       time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC),
		}},
		candidates: map[string][]store.CandidateSession{
			"codex|/proj": {
				{SessionID: "cand-no-marker", Tool: "codex"},
				{SessionID: "cand-hit", Tool: "codex"},
				{SessionID: "cand-late", Tool: "codex"},
			},
		},
	}
	deps := Deps{
		Store: fs,
		Cfg:   config.Default().Handoff,
		Adapters: []adapter.Adapter{&fakeLinkAdapter{
			name: "codex",
			bySession: map[string][]models.TranscriptMessage{
				"cand-no-marker": {{Role: models.TranscriptUser, Text: "unrelated work"}},
				"cand-hit":       markerMsg("abcd1234"),
				"cand-late":      markerMsg("abcd1234"),
			},
		}},
	}
	linked, err := LinkTargetSessions(context.Background(), deps, time.Hour)
	if err != nil {
		t.Fatalf("LinkTargetSessions: %v", err)
	}
	if linked != 1 {
		t.Fatalf("linked = %d, want 1", linked)
	}
	if fs.links[7] != "cand-hit" {
		t.Errorf("handoff 7 linked to %q, want cand-hit (first match wins)", fs.links[7])
	}
}

func TestLinkTargetSessions_NoMatchLeavesUnlinked(t *testing.T) {
	fs := &fakeLinkStore{
		unlinked: []store.HandoffRecord{{
			ID:          9,
			TargetTool:  "codex",
			ProjectRoot: "/proj",
			DeliveryRef: "/proj/HANDOFF-ffff0000.md",
			CreatedAt:   time.Now(),
		}},
		candidates: map[string][]store.CandidateSession{
			"codex|/proj": {{SessionID: "cand", Tool: "codex"}},
		},
	}
	deps := Deps{
		Store: fs,
		Cfg:   config.Default().Handoff,
		Adapters: []adapter.Adapter{&fakeLinkAdapter{
			name:      "codex",
			bySession: map[string][]models.TranscriptMessage{"cand": markerMsg("deadbeef")}, // different id
		}},
	}
	linked, err := LinkTargetSessions(context.Background(), deps, time.Hour)
	if err != nil {
		t.Fatalf("LinkTargetSessions: %v", err)
	}
	if linked != 0 || len(fs.links) != 0 {
		t.Errorf("no marker match must link nothing: linked=%d links=%v", linked, fs.links)
	}
}

func TestLinkTargetSessions_SkipsUnrecoverableShortID(t *testing.T) {
	fs := &fakeLinkStore{
		unlinked: []store.HandoffRecord{
			{ID: 1, TargetTool: "codex", ProjectRoot: "/p", DeliveryRef: "/p/custom-out.md", CreatedAt: time.Now()},
			{ID: 2, TargetTool: "unspecified", ProjectRoot: "/p", DeliveryRef: "/p/HANDOFF-aaaa1111.md", CreatedAt: time.Now()},
		},
	}
	deps := Deps{Store: fs, Cfg: config.Default().Handoff}
	linked, err := LinkTargetSessions(context.Background(), deps, time.Hour)
	if err != nil {
		t.Fatalf("LinkTargetSessions: %v", err)
	}
	if linked != 0 {
		t.Errorf("unrecoverable short-id / unspecified target must link nothing, got %d", linked)
	}
}

func TestLinkTargetSessions_StoredShortIDLinksCustomOut(t *testing.T) {
	// A handoff written to a custom --out path (delivery_ref carries no
	// recoverable HANDOFF-<shortid>.md name) is now linkable because the
	// short-id is stored on the row (migration 057).
	fs := &fakeLinkStore{
		unlinked: []store.HandoffRecord{{
			ID:          11,
			TargetTool:  "codex",
			ProjectRoot: "/proj",
			DeliveryRef: "/proj/custom-notes.md", // no shortid recoverable from the name
			ShortID:     "abcd1234",              // ...but stored on the row
			CreatedAt:   time.Now(),
		}},
		candidates: map[string][]store.CandidateSession{
			"codex|/proj": {{SessionID: "cand-hit", Tool: "codex"}},
		},
	}
	deps := Deps{
		Store: fs,
		Cfg:   config.Default().Handoff,
		Adapters: []adapter.Adapter{&fakeLinkAdapter{
			name:      "codex",
			bySession: map[string][]models.TranscriptMessage{"cand-hit": markerMsg("abcd1234")},
		}},
	}
	linked, err := LinkTargetSessions(context.Background(), deps, time.Hour)
	if err != nil {
		t.Fatalf("LinkTargetSessions: %v", err)
	}
	if linked != 1 || fs.links[11] != "cand-hit" {
		t.Fatalf("stored short-id must link custom --out handoff: linked=%d links=%v", linked, fs.links)
	}
}

func TestLinkTargetSessions_ThreadsCandidateHints(t *testing.T) {
	// The candidate's recorded source hints must reach the reader (nil
	// hints can open the wrong store for foreign-mount sessions).
	wantHints := []string{"/mnt/c/Users/x/opencode.db"}
	fs := &fakeLinkStore{
		unlinked: []store.HandoffRecord{{
			ID:          21,
			TargetTool:  "codex",
			ProjectRoot: "/proj",
			ShortID:     "abcd1234",
			CreatedAt:   time.Now(),
		}},
		candidates: map[string][]store.CandidateSession{
			"codex|/proj": {{SessionID: "cand-hit", Tool: "codex", Hints: wantHints}},
		},
	}
	adp := &fakeLinkAdapter{
		name:      "codex",
		bySession: map[string][]models.TranscriptMessage{"cand-hit": markerMsg("abcd1234")},
	}
	deps := Deps{Store: fs, Cfg: config.Default().Handoff, Adapters: []adapter.Adapter{adp}}

	if _, err := LinkTargetSessions(context.Background(), deps, time.Hour); err != nil {
		t.Fatalf("LinkTargetSessions: %v", err)
	}
	got := adp.seenHints["cand-hit"]
	if len(got) != 1 || got[0] != wantHints[0] {
		t.Fatalf("reader saw hints %v, want %v", got, wantHints)
	}
}

func TestShortIDFromRef(t *testing.T) {
	tests := []struct {
		ref  string
		want string
	}{
		{"/proj/HANDOFF-abcd1234.md", "abcd1234"},
		{"HANDOFF-deadbeef.md", "deadbeef"},
		{"/a/b/HANDOFF-t123456.md", "t123456"},
		{"/proj/custom.md", ""},
		{"", ""},
		{"/proj/nested/HANDOFF-cafe0001.md", "cafe0001"},
	}
	for _, tt := range tests {
		if got := shortIDFromRef(tt.ref); got != tt.want {
			t.Errorf("shortIDFromRef(%q) = %q, want %q", tt.ref, got, tt.want)
		}
	}
}
