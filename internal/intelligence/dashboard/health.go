package dashboard

import (
	"net/http"
	"os"
	"sort"
	"time"
)

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
func (s *Server) handleWatcherHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	rows, err := s.opts.DB.QueryContext(r.Context(),
		`SELECT source_file, byte_offset, last_parsed FROM parse_cursors`)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()

	type fileHealth struct {
		Path            string `json:"path"`
		ByteOffset      int64  `json:"byte_offset"`
		FileSize        int64  `json:"file_size"`
		BehindBytes     int64  `json:"behind_bytes"`
		LastParsed      string `json:"last_parsed"`
		BehindSeconds   int64  `json:"behind_seconds,omitempty"`
		Missing         bool   `json:"missing,omitempty"`
		OrphanUnmatched bool   `json:"orphan_unmatched,omitempty"`
	}
	var out []fileHealth
	var totalBehind int64
	orphanCount := 0
	now := time.Now().UTC()
	for rows.Next() {
		var path, lastParsed string
		var off int64
		if err := rows.Scan(&path, &off, &lastParsed); err != nil {
			writeErr(w, err)
			return
		}
		stat, statErr := os.Stat(path)
		f := fileHealth{
			Path:       path,
			ByteOffset: off,
			LastParsed: lastParsed,
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
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		writeErr(w, err)
		return
	}

	// Surface the worst offenders first so the UI's "click to recover"
	// banner can show the top-N that matter.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].BehindBytes > out[j].BehindBytes
	})
	behindCount := 0
	for _, f := range out {
		if f.BehindBytes > 0 && !f.OrphanUnmatched {
			behindCount++
		}
	}

	writeJSON(w, map[string]any{
		"files":              out,
		"behind_count":       behindCount,
		"behind_total_bytes": totalBehind,
		"orphan_count":       orphanCount,
		"checked_at":         now.Format(time.RFC3339),
	})
}
