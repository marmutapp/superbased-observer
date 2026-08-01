package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// watcherHealthPayload is the decoded /api/health/watcher response.
type watcherHealthPayload struct {
	Files []struct {
		Path                string `json:"path"`
		ByteOffset          int64  `json:"byte_offset"`
		FileSize            int64  `json:"file_size"`
		BehindBytes         int64  `json:"behind_bytes"`
		CursorKind          string `json:"cursor_kind"`
		SuspectedMisrouted  bool   `json:"suspected_misrouted"`
		LagNotApplicable    bool   `json:"lag_not_applicable"`
		ZeroActionsExpected bool   `json:"zero_actions_expected"`
		ExcludedReason      string `json:"excluded_reason"`
	} `json:"files"`
	TotalFiles               int   `json:"total_files"`
	FilesTruncated           bool  `json:"files_truncated"`
	FilesLimit               int   `json:"files_limit"`
	BehindCount              int   `json:"behind_count"`
	BehindTotalBytes         int64 `json:"behind_total_bytes"`
	OrphanCount              int   `json:"orphan_count"`
	SuspectedMisroutedCount  int   `json:"suspected_misrouted_count"`
	LagNotApplicableCount    int   `json:"lag_not_applicable_count"`
	ZeroActionsExpectedCount int   `json:"zero_actions_expected_count"`
}

// fetchWatcherHealth drives the endpoint and decodes the payload.
func fetchWatcherHealth(t *testing.T, s *Server) watcherHealthPayload {
	t.Helper()
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/health/watcher", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d", rr.Code)
	}
	var got watcherHealthPayload
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

// seedCursor writes a file of bodyBytes and a parse_cursors row at
// offset, returning the path.
func seedCursor(t *testing.T, s *Server, root, name string, bodyBytes int, offset int64) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte(strings.Repeat("x", bodyBytes)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.opts.DB.ExecContext(context.Background(),
		`INSERT INTO parse_cursors (source_file, byte_offset, last_parsed) VALUES (?, ?, ?)`,
		path, offset, time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestWatcherHealthCursorSemantics is the core table. Each row seeds
// ONE parse_cursors entry with a declared cursor semantics and pins
// which of the two health signals is allowed to fire.
//
// The shapes are drawn from live parse_cursors rows measured
// 2026-08-01 on this host:
//
//   - hermes/goose/devin `state.db` carry a small AUTOINCREMENT row-id
//     watermark (89, 27, 110) against a multi-megabyte file, so
//     byte-offset math reported them "behind" for 16.5 days.
//   - cline-cli `sessions.db` and openclaw `sessions.json` carry a
//     UnixMilli watermark (~1.79e12) that dwarfs the file size, so the
//     SAME category error reported them "cursor at EOF, zero actions"
//     — permanently misrouted.
//
// Both are the same bug seen from opposite ends, which is why the fix
// is one capability flag pair rather than two special cases.
func TestWatcherHealthCursorSemantics(t *testing.T) {
	const bigBody = 4096 // comfortably over suspectedMisroutedMinBytes

	tests := []struct {
		name string
		// semantics returned by the injected seam for this row.
		sem CursorSemantics
		// offset seeded into parse_cursors.
		offset int64
		// body size on disk.
		body int
		// expected rollup counts.
		wantBehind        int
		wantMisrouted     int
		wantLagNA         int
		wantZeroExpected  int
		wantBehindBytesGT int64
	}{
		{
			name:              "byte offset behind is real lag",
			sem:               CursorSemantics{Kind: "byte_offset", LagMeaningful: true, ActionsExpected: true},
			offset:            0,
			body:              bigBody,
			wantBehind:        1,
			wantBehindBytesGT: 0,
		},
		{
			name:          "byte offset at EOF with zero actions is a real misroute",
			sem:           CursorSemantics{Kind: "byte_offset", LagMeaningful: true, ActionsExpected: true},
			offset:        bigBody,
			body:          bigBody,
			wantMisrouted: 1,
		},
		{
			name:      "row-id watermark below file size is not lag",
			sem:       CursorSemantics{Kind: "watermark", LagMeaningful: false, ActionsExpected: false, Reason: "row-id watermark"},
			offset:    89,
			body:      bigBody,
			wantLagNA: 1,
		},
		{
			name:             "unix-millis watermark above file size is not a misroute",
			sem:              CursorSemantics{Kind: "watermark", LagMeaningful: false, ActionsExpected: false, Reason: "UnixMilli watermark"},
			offset:           1785498630161,
			body:             bigBody,
			wantZeroExpected: 1,
		},
		{
			name:             "encrypted store emitting zero actions is expected",
			sem:              CursorSemantics{Kind: "encrypted", LagMeaningful: true, ActionsExpected: false, Reason: "OSCrypt-encrypted"},
			offset:           bigBody,
			body:             bigBody,
			wantZeroExpected: 1,
		},
		{
			name:             "token-only log emitting zero actions is expected",
			sem:              CursorSemantics{Kind: "no_actions", LagMeaningful: true, ActionsExpected: false, Reason: "token records only"},
			offset:           bigBody,
			body:             bigBody,
			wantZeroExpected: 1,
		},
		{
			name:              "token-only log still reports real byte lag",
			sem:               CursorSemantics{Kind: "no_actions", LagMeaningful: true, ActionsExpected: false, Reason: "token records only"},
			offset:            0,
			body:              bigBody,
			wantBehind:        1,
			wantBehindBytesGT: 0,
		},

		// --- decoys: shapes that must NOT trip any new signal ---
		{
			name:   "decoy: encrypted stub below the 1KB floor stays silent",
			sem:    CursorSemantics{Kind: "encrypted", LagMeaningful: true, ActionsExpected: false},
			offset: 16,
			body:   16,
		},
		{
			name:   "decoy: byte-offset stub below the 1KB floor stays silent",
			sem:    CursorSemantics{Kind: "byte_offset", LagMeaningful: true, ActionsExpected: true},
			offset: 16,
			body:   16,
		},
		{
			name:   "decoy: watermark exactly equal to file size stays silent",
			sem:    CursorSemantics{Kind: "watermark", LagMeaningful: false, ActionsExpected: false},
			offset: bigBody,
			body:   bigBody,
			// grown == false, so no lag row; actions not expected, so
			// no misroute row either.
			wantZeroExpected: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, root := newTestServer(t)
			seedCursor(t, s, root, "seeded.jsonl", tc.body, tc.offset)
			s.opts.RecognizesSessionFile = func(string) bool { return true }
			s.opts.CursorSemanticsFor = func(string) CursorSemantics { return tc.sem }

			got := fetchWatcherHealth(t, s)
			if got.BehindCount != tc.wantBehind {
				t.Errorf("behind_count = %d, want %d", got.BehindCount, tc.wantBehind)
			}
			if got.SuspectedMisroutedCount != tc.wantMisrouted {
				t.Errorf("suspected_misrouted_count = %d, want %d", got.SuspectedMisroutedCount, tc.wantMisrouted)
			}
			if got.LagNotApplicableCount != tc.wantLagNA {
				t.Errorf("lag_not_applicable_count = %d, want %d", got.LagNotApplicableCount, tc.wantLagNA)
			}
			if got.ZeroActionsExpectedCount != tc.wantZeroExpected {
				t.Errorf("zero_actions_expected_count = %d, want %d", got.ZeroActionsExpectedCount, tc.wantZeroExpected)
			}
			if got.BehindTotalBytes < tc.wantBehindBytesGT {
				t.Errorf("behind_total_bytes = %d, want > %d", got.BehindTotalBytes, tc.wantBehindBytesGT)
			}
			if tc.wantBehind == 0 && got.BehindTotalBytes != 0 {
				t.Errorf("behind_total_bytes = %d, want 0 when nothing is behind", got.BehindTotalBytes)
			}
			// The excluded row must stay visible and self-explanatory.
			if len(got.Files) != 1 {
				t.Fatalf("files = %d, want 1 (excluded rows stay listed)", len(got.Files))
			}
			f := got.Files[0]
			if f.CursorKind != tc.sem.Kind {
				t.Errorf("cursor_kind = %q, want %q", f.CursorKind, tc.sem.Kind)
			}
			if (f.LagNotApplicable || f.ZeroActionsExpected) && f.ExcludedReason == "" {
				t.Errorf("excluded row carries no excluded_reason: %+v", f)
			}
		})
	}
}

// TestWatcherHealthNilSeamKeepsByteOffsetBehaviour pins the
// additive-not-invasive contract: with no CursorSemanticsFor wired
// (tests, embedders, an older assembly), every row is treated as a
// byte-offset tail exactly as before the capability existed.
func TestWatcherHealthNilSeamKeepsByteOffsetBehaviour(t *testing.T) {
	s, root := newTestServer(t)
	seedCursor(t, s, root, "lagging.jsonl", 4096, 0)
	seedCursor(t, s, root, "at-eof.jsonl", 4096, 4096)
	s.opts.RecognizesSessionFile = func(string) bool { return true }
	s.opts.CursorSemanticsFor = nil

	got := fetchWatcherHealth(t, s)
	if got.BehindCount != 1 {
		t.Errorf("behind_count = %d, want 1", got.BehindCount)
	}
	if got.SuspectedMisroutedCount != 1 {
		t.Errorf("suspected_misrouted_count = %d, want 1", got.SuspectedMisroutedCount)
	}
	if got.LagNotApplicableCount != 0 || got.ZeroActionsExpectedCount != 0 {
		t.Errorf("nil seam must never suppress: lagNA=%d zeroExpected=%d",
			got.LagNotApplicableCount, got.ZeroActionsExpectedCount)
	}
	for _, f := range got.Files {
		if f.CursorKind != "byte_offset" {
			t.Errorf("cursor_kind = %q, want byte_offset", f.CursorKind)
		}
	}
}

// TestWatcherHealthEmptyKindFallsBackToByteOffset pins the guard
// against a seam that returns a zero-value CursorSemantics (an adapter
// declining to declare for a path it doesn't own). Silently treating
// that as "lag meaningless, actions not expected" would suppress every
// real signal.
func TestWatcherHealthEmptyKindFallsBackToByteOffset(t *testing.T) {
	s, root := newTestServer(t)
	seedCursor(t, s, root, "lagging.jsonl", 4096, 0)
	s.opts.RecognizesSessionFile = func(string) bool { return true }
	s.opts.CursorSemanticsFor = func(string) CursorSemantics { return CursorSemantics{} }

	got := fetchWatcherHealth(t, s)
	if got.BehindCount != 1 {
		t.Errorf("behind_count = %d, want 1 (zero-value semantics must not suppress)", got.BehindCount)
	}
	if got.Files[0].CursorKind != "byte_offset" {
		t.Errorf("cursor_kind = %q, want byte_offset", got.Files[0].CursorKind)
	}
}

// TestWatcherHealthTruncatesFilesButNotCounts pins the payload cap:
// the per-file array stops at watcherHealthMaxFiles, the rollup counts
// still cover every row, and the response says so explicitly rather
// than implying it returned everything.
func TestWatcherHealthTruncatesFilesButNotCounts(t *testing.T) {
	s, root := newTestServer(t)
	const n = watcherHealthMaxFiles + 17
	for i := 0; i < n; i++ {
		// Descending body sizes ⇒ descending behind_bytes ⇒ the sort
		// order is known, so the truncation boundary is deterministic.
		seedCursor(t, s, root, fmt.Sprintf("lag-%03d.jsonl", i), 8192-i, 0)
	}
	s.opts.RecognizesSessionFile = func(string) bool { return true }
	s.opts.CursorSemanticsFor = func(string) CursorSemantics {
		return CursorSemantics{Kind: "byte_offset", LagMeaningful: true, ActionsExpected: true}
	}

	got := fetchWatcherHealth(t, s)
	if len(got.Files) != watcherHealthMaxFiles {
		t.Errorf("files = %d, want %d", len(got.Files), watcherHealthMaxFiles)
	}
	if got.TotalFiles != n {
		t.Errorf("total_files = %d, want %d", got.TotalFiles, n)
	}
	if !got.FilesTruncated {
		t.Error("files_truncated = false, want true")
	}
	if got.FilesLimit != watcherHealthMaxFiles {
		t.Errorf("files_limit = %d, want %d", got.FilesLimit, watcherHealthMaxFiles)
	}
	if got.BehindCount != n {
		t.Errorf("behind_count = %d, want %d (counts cover ALL rows, not the cap)", got.BehindCount, n)
	}
	// The VS Code watcher-lag notifier picks the max-behind row out of
	// `files`; truncation must not move it.
	if got.Files[0].BehindBytes != 8192 {
		t.Errorf("worst row behind_bytes = %d, want 8192 (worst-first ordering survives truncation)",
			got.Files[0].BehindBytes)
	}
}

// TestWatcherHealthNotTruncatedBelowCap is the decoy for the cap: a
// small payload must not claim truncation.
func TestWatcherHealthNotTruncatedBelowCap(t *testing.T) {
	s, root := newTestServer(t)
	seedCursor(t, s, root, "one.jsonl", 4096, 0)
	s.opts.RecognizesSessionFile = func(string) bool { return true }

	got := fetchWatcherHealth(t, s)
	if got.FilesTruncated {
		t.Error("files_truncated = true, want false")
	}
	if got.TotalFiles != 1 || len(got.Files) != 1 {
		t.Errorf("total_files=%d len(files)=%d, want 1/1", got.TotalFiles, len(got.Files))
	}
}

// TestStatAllPreservesIndexOrder pins that the parallel stat fan-out
// writes results by index, so the output order is byte-identical to a
// serial pass regardless of goroutine scheduling.
func TestStatAllPreservesIndexOrder(t *testing.T) {
	root := t.TempDir()
	const n = 200
	paths := make([]string, n)
	for i := 0; i < n; i++ {
		p := filepath.Join(root, fmt.Sprintf("f-%03d", i))
		if err := os.WriteFile(p, []byte(strings.Repeat("x", i)), 0o600); err != nil {
			t.Fatal(err)
		}
		paths[i] = p
	}
	// One deliberately missing path to pin the error channel.
	paths[7] = filepath.Join(root, "does-not-exist")

	sizes := make([]int64, n)
	errs := make([]bool, n)
	statAll(n, func(i int) string { return paths[i] }, sizes, errs)

	for i := 0; i < n; i++ {
		if i == 7 {
			if !errs[i] {
				t.Errorf("index %d: expected stat error", i)
			}
			continue
		}
		if errs[i] {
			t.Errorf("index %d: unexpected stat error", i)
		}
		if sizes[i] != int64(i) {
			t.Errorf("index %d: size = %d, want %d (results must land at their own index)", i, sizes[i], i)
		}
	}
}
