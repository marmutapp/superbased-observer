package dashboard

import (
	"net/http"
	"os"
	"sort"
	"time"
)

// suspectedMisroutedMinBytes is the file-size threshold below which a
// cursor-at-EOF-with-zero-actions row is NOT flagged as suspected
// misrouted. Stub session files (created but never written to)
// legitimately have zero events; only files with enough content that
// some adapter SHOULD have emitted rows are worth surfacing. 1 KB is
// well below any real Codex rollout (a single token_count line is
// ~400 bytes) and well above the empty-stub case.
const suspectedMisroutedMinBytes int64 = 1024

// handleWatcherHealth serves /api/health/watcher — surfaces every
// JSONL file the watcher knows about (one row in parse_cursors per
// file), the saved offset, the current file size on disk, and how far
// behind the watcher is. Lets the dashboard render a "data is being
// dropped" banner when the watcher silently falls behind a session
// file (typical failure mode: fsnotify event drops on a busy session,
// or a daemon restart that lost in-flight state).
//
// The threshold for "behind" is non-zero — even a few bytes mean a
// JSONL line was appended to disk that the watcher hasn't ingested
// yet. The UI ranks the worst offenders by `behind_bytes` so the
// recovery prompt fires once the gap looks concerning (>10 KB, say —
// thresholding lives in the JS).
//
// v1.4.51 added the `suspected_misrouted` signal: parse_cursors rows
// whose cursor reached EOF on a non-trivial file BUT the actions
// table has zero rows for that source_file. That's the fingerprint
// the pre-v1.4.51 adapter-misrouting bug class produced — claude-code
// silently "parsed" Codex rollout-*.jsonl files (every JSON line
// unmarshalled cleanly so the cursor advanced to EOF) but none of
// the Codex-schema fields matched claude-code handlers, so zero
// actions landed. Surface these so the operator can run
// `observer scan --force --adapter <name>` to recover.
func (s *Server) handleWatcherHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	// LEFT JOIN against actions so a single query returns both the
	// cursor state AND the per-file action count needed for the
	// suspected_misrouted heuristic. parse_cursors is small (one row
	// per known session file) so the JOIN is cheap.
	// s.opts.DB, not s.db(): watcher health describes the live
	// watcher's real cursors even while demo mode is active (P6.7).
	rows, err := s.opts.DB.QueryContext(r.Context(),
		`SELECT pc.source_file, pc.byte_offset, pc.last_parsed,
		        COALESCE(COUNT(a.id), 0) AS action_count
		   FROM parse_cursors pc
		   LEFT JOIN actions a ON a.source_file = pc.source_file
		  GROUP BY pc.source_file, pc.byte_offset, pc.last_parsed`)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()

	type fileHealth struct {
		Path               string `json:"path"`
		ByteOffset         int64  `json:"byte_offset"`
		FileSize           int64  `json:"file_size"`
		BehindBytes        int64  `json:"behind_bytes"`
		LastParsed         string `json:"last_parsed"`
		BehindSeconds      int64  `json:"behind_seconds,omitempty"`
		Missing            bool   `json:"missing,omitempty"`
		OrphanUnmatched    bool   `json:"orphan_unmatched,omitempty"`
		SuspectedMisrouted bool   `json:"suspected_misrouted,omitempty"`
		MisrouteReason     string `json:"misroute_reason,omitempty"`
		ActionCount        int64  `json:"action_count,omitempty"`
	}
	out := []fileHealth{}
	var totalBehind int64
	orphanCount := 0
	suspectedMisroutedCount := 0
	now := time.Now().UTC()
	for rows.Next() {
		var path, lastParsed string
		var off, actionCount int64
		if err := rows.Scan(&path, &off, &lastParsed, &actionCount); err != nil {
			writeErr(w, err)
			return
		}
		stat, statErr := os.Stat(path)
		f := fileHealth{
			Path:        path,
			ByteOffset:  off,
			LastParsed:  lastParsed,
			ActionCount: actionCount,
		}
		// orphan_unmatched: parse_cursors row exists but no currently
		// registered adapter's IsSessionFile claims this path. Almost
		// always means an older adapter version once tracked it and
		// has since tightened its filter (e.g. the v1.4.20 copilot
		// adapter narrowed from "any *.log under copilot-chat" to
		// "main.jsonl under debug-logs"). Surface the row but DON'T
		// count it as "behind" — the recovery flow can't process
		// these so the banner would never close.
		if s.opts.RecognizesSessionFile != nil && !s.opts.RecognizesSessionFile(path) {
			f.OrphanUnmatched = true
			orphanCount++
			if statErr == nil {
				f.FileSize = stat.Size()
			} else {
				f.Missing = true
			}
			out = append(out, f)
			continue
		}
		if statErr != nil {
			// File on disk gone (e.g. user deleted a session). Surface
			// it so the user can clean up parse_cursors, but don't
			// count it as "behind" — there's nothing to recover.
			f.Missing = true
			out = append(out, f)
			continue
		}
		f.FileSize = stat.Size()
		if f.FileSize > f.ByteOffset {
			f.BehindBytes = f.FileSize - f.ByteOffset
			totalBehind += f.BehindBytes
			if t, parseErr := time.Parse(time.RFC3339Nano, lastParsed); parseErr == nil {
				f.BehindSeconds = int64(now.Sub(t).Seconds())
			}
		}
		// suspected_misrouted: cursor at EOF on a non-trivial file but
		// zero actions emitted. The pre-v1.4.51 fingerprint — surface
		// for operator-driven recovery via
		// `observer scan --force --adapter <name>`.
		if f.BehindBytes == 0 && f.FileSize >= suspectedMisroutedMinBytes && actionCount == 0 {
			f.SuspectedMisrouted = true
			f.MisrouteReason = "cursor at EOF on non-trivial file but 0 actions emitted; likely a v1.4.51 adapter misroute — run `observer scan --force --adapter <name>` to recover"
			suspectedMisroutedCount++
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		writeErr(w, err)
		return
	}

	// Surface the worst offenders first so the UI's "click to recover"
	// banner can show the top-N that matter. Behind-bytes is the
	// primary sort key; ties broken by suspected-misrouted so the
	// pre-v1.4.51 fingerprint floats up among caught-up rows.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].BehindBytes != out[j].BehindBytes {
			return out[i].BehindBytes > out[j].BehindBytes
		}
		return out[i].SuspectedMisrouted && !out[j].SuspectedMisrouted
	})
	behindCount := 0
	for _, f := range out {
		if f.BehindBytes > 0 && !f.OrphanUnmatched {
			behindCount++
		}
	}

	writeJSON(w, map[string]any{
		"files":                     out,
		"behind_count":              behindCount,
		"behind_total_bytes":        totalBehind,
		"orphan_count":              orphanCount,
		"suspected_misrouted_count": suspectedMisroutedCount,
		"checked_at":                now.Format(time.RFC3339),
	})
}
