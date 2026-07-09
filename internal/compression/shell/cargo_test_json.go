package shell

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// parseCargoTestJSON consumes the NDJSON output of
// `cargo test -- --format json` (or the libtest-side
// `--format json --report-time` shape) and emits a compact summary.
// Returns (summaryLines, true) on a successful parse, (nil, false)
// when the buffered content doesn't look like cargo test JSON at all
// — caller falls back to verbatim emit on false.
//
// libtest's NDJSON shape (one JSON object per line):
//
//	{"type":"suite","event":"started","test_count":N}
//	{"type":"test","event":"started","name":"<test::path>"}
//	{"type":"test","name":"<test::path>","event":"ok"}
//	{"type":"test","name":"<test::path>","event":"failed","stdout":"<panic msg + stack>"}
//	{"type":"suite","event":"ok","passed":P,"failed":F,"allowed_fail":A,"ignored":I,"measured":M,"filtered_out":X}
//
// Cargo also emits its own (non-libtest) lines like
// `{"reason":"compiler-message",...}` ahead of the suite block; we
// silently skip anything we don't recognise so a mixed `cargo test`
// stream parses cleanly.
//
// failureOnly suppresses the passing-test detail (still no detail is
// emitted today since we never list passing tests by name); kept as a
// signature parameter so the format-parse caller has a stable contract
// for when richer per-test detail lands.
func parseCargoTestJSON(lines []string, failureOnly bool) ([]string, bool) {
	type testFailure struct {
		name   string
		stdout string
	}
	var (
		suiteSeen   bool
		passed      int
		failed      int
		ignored     int
		filteredOut int
		failures    []testFailure
		seenJSON    bool
	)

	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		seenJSON = true
		t, _ := ev["type"].(string)
		event, _ := ev["event"].(string)

		switch t {
		case "suite":
			if event == "ok" || event == "failed" {
				suiteSeen = true
				passed = int(asFloat(ev["passed"]))
				failed = int(asFloat(ev["failed"]))
				ignored = int(asFloat(ev["ignored"]))
				filteredOut = int(asFloat(ev["filtered_out"]))
			}
		case "test":
			if event == "failed" {
				name, _ := ev["name"].(string)
				stdout, _ := ev["stdout"].(string)
				failures = append(failures, testFailure{name: name, stdout: stdout})
			}
		}
	}

	if !seenJSON {
		return nil, false
	}
	if !suiteSeen {
		// Missing the libtest summary block — the stream is either
		// non-libtest JSON (cargo's own compiler-message events that
		// happen to be JSON) or a truncated/killed test run. Falling
		// back to verbatim emit preserves whatever non-libtest context
		// preceded (Compiling lines, warnings) instead of summarising
		// against a partial stream.
		return nil, false
	}

	// Stable failure ordering for cross-turn determinism — libtest
	// already emits in test-discovery order but defending against
	// reordering doesn't cost anything.
	sort.SliceStable(failures, func(i, j int) bool {
		return failures[i].name < failures[j].name
	})

	out := make([]string, 0, 4+3*len(failures))
	header := fmt.Sprintf("cargo test summary: %d passed, %d failed", passed, failed)
	if ignored > 0 {
		header += fmt.Sprintf(", %d ignored", ignored)
	}
	if filteredOut > 0 {
		header += fmt.Sprintf(", %d filtered out", filteredOut)
	}
	out = append(out, header)

	if len(failures) > 0 {
		out = append(out, "")
		out = append(out, fmt.Sprintf("failures (%d):", len(failures)))
		for _, f := range failures {
			out = append(out, fmt.Sprintf("  - %s", f.name))
			detail := summariseStdout(f.stdout)
			for _, dl := range detail {
				out = append(out, "      "+dl)
			}
		}
	}
	_ = failureOnly // reserved for richer per-test detail later

	return out, true
}

// summariseStdout extracts the most informative lines from a libtest
// stdout blob (the panic/assert message and the location). Drops the
// rust-internal stack frames the model doesn't typically need to see
// the failure cause.
func summariseStdout(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	var out []string
	for _, line := range lines {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "thread '") && strings.Contains(t, "panicked at"):
			out = append(out, t)
		case strings.HasPrefix(t, "assertion ") || strings.HasPrefix(t, "assertion failed"):
			out = append(out, t)
		case strings.HasPrefix(t, "left:") || strings.HasPrefix(t, "right:"):
			out = append(out, t)
			// "note: run with ..." and other boilerplate lines match no case
			// and are dropped — they're noise the model can't act on.
		}
	}
	if len(out) == 0 && len(lines) > 0 {
		// Fallback: keep first non-empty line as a hint.
		for _, line := range lines {
			if strings.TrimSpace(line) != "" {
				out = append(out, strings.TrimSpace(line))
				break
			}
		}
	}
	return out
}

// asFloat coerces a JSON-decoded value to float64 so the cargo summary
// fields parse uniformly whether libtest emits integers or floats.
func asFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	}
	return 0
}
