package mcp

import (
	"bufio"
	"fmt"
	"os"
)

// File-read helper shared between the V7-12 retrieval-surface MCP
// tools. get_file (v1.7.8) and get_symbols (v1.7.9) both need
// "open file, return inclusive line slice or full body, cap at N
// bytes, report total line count" semantics — keeping one
// implementation here ensures both tools truncate identically and
// surface the same line-bookkeeping shape.

// lineRange describes which lines [start, end] readSlice actually
// emitted vs the file's true total line count. Marshalled directly
// as part of get_file's response (the JSON tags pin the wire shape
// v1.7.8 shipped — do not rename without updating the get_file
// reference doc). get_symbols decomposes the fields into per-match
// metadata and doesn't surface lineRange as a whole.
type lineRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
	Total int `json:"total"`
}

// readSlice opens path and returns either the full file body (start ==
// 0 && end == 0), a 1-based inclusive line slice, capped at maxBytes.
// Returns lineRange.Total = the file's full line count; Start/End
// describe what was actually returned (may differ from request when
// capped or when end exceeds the file).
//
// Implementation note: single-pass bufio scan to compute both the
// slice and the total line count. For files that vastly exceed
// maxBytes, we still scan past the truncation point to count lines
// — fine at the 100 KB / few MB scale typical for source files. If
// multi-MB files become common, a two-pass (line-index then ranged-
// read) implementation would be faster.
func readSlice(path string, start, end, maxBytes int) ([]byte, lineRange, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, lineRange{}, false, fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	wantStart := start
	if wantStart <= 0 {
		wantStart = 1
	}

	var out []byte
	total := 0
	emittedStart := 0
	emittedEnd := 0
	truncated := false
	wantingEnd := end > 0

	for scanner.Scan() {
		total++
		if total < wantStart {
			continue
		}
		if wantingEnd && total > end {
			// Keep counting lines to populate Total accurately, but
			// stop appending bytes.
			continue
		}
		// In-window: append (with newline) unless we've already hit
		// the byte cap.
		if !truncated {
			line := scanner.Bytes()
			if len(out)+len(line)+1 > maxBytes {
				truncated = true
				continue
			}
			if emittedStart == 0 {
				emittedStart = total
			}
			out = append(out, line...)
			out = append(out, '\n')
			emittedEnd = total
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, lineRange{}, false, fmt.Errorf("scan: %w", err)
	}

	// Resolve the reported window when nothing was emitted (empty
	// file, empty slice, or start past EOF).
	if total == 0 {
		return nil, lineRange{Start: 0, End: 0, Total: 0}, false, nil
	}
	if emittedStart == 0 {
		return nil, lineRange{},
			false,
			fmt.Errorf("start_line=%d exceeds file's %d lines", wantStart, total)
	}

	// bufio.Scanner with ScanLines strips trailing \n, so we always
	// add one back. Matches `cat` behavior; keeps responses byte-
	// faithful to common editor expectations.

	return out, lineRange{
		Start: emittedStart,
		End:   emittedEnd,
		Total: total,
	}, truncated, nil
}
