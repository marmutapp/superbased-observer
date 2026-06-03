package shell

import (
	"strings"
	"testing"
)

// TestParseCargoTestJSON_HappyPath pins the canonical libtest NDJSON
// shape: suite-started, per-test events, suite-ok summary. Produces
// "<P> passed, <F> failed" header and lists every failing test name.
func TestParseCargoTestJSON_HappyPath(t *testing.T) {
	lines := []string{
		`{"type":"suite","event":"started","test_count":4}`,
		`{"type":"test","event":"started","name":"foo::a"}`,
		`{"type":"test","name":"foo::a","event":"ok"}`,
		`{"type":"test","name":"foo::b","event":"failed","stdout":"thread 'foo::b' panicked at src/lib.rs:42:5\nassertion failed: x == 1\n  left: 0\n  right: 1\nnote: run with RUST_BACKTRACE=1 environment variable to display a backtrace"}`,
		`{"type":"test","name":"foo::c","event":"ok"}`,
		`{"type":"test","name":"foo::d","event":"failed","stdout":"thread 'foo::d' panicked at src/lib.rs:50:5\nexplicit panic"}`,
		`{"type":"suite","event":"failed","passed":2,"failed":2,"allowed_fail":0,"ignored":0,"measured":0,"filtered_out":1}`,
	}
	out, ok := parseCargoTestJSON(lines, false)
	if !ok {
		t.Fatalf("parseCargoTestJSON: ok=false on canonical input")
	}
	joined := strings.Join(out, "\n")
	for _, want := range []string{
		"cargo test summary: 2 passed, 2 failed",
		"1 filtered out",
		"failures (2):",
		"  - foo::b",
		"  - foo::d",
		"thread 'foo::b' panicked",
		"thread 'foo::d' panicked",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected %q in summary:\n%s", want, joined)
		}
	}
	// Boilerplate the parser is supposed to drop.
	if strings.Contains(joined, "RUST_BACKTRACE") {
		t.Errorf("backtrace boilerplate leaked: %s", joined)
	}
}

// TestParseCargoTestJSON_NotJSON pins the fallback contract: lines
// that aren't libtest JSON return ok=false so the caller can emit the
// original buffered content verbatim. We never silently lose data.
func TestParseCargoTestJSON_NotJSON(t *testing.T) {
	lines := []string{
		"   Compiling foo v1.0.0 (/repo/foo)",
		"   Compiling bar v2.0.0 (/repo/bar)",
		"warning: unused variable `x`",
	}
	if _, ok := parseCargoTestJSON(lines, false); ok {
		t.Errorf("non-JSON input should return ok=false")
	}
}

// TestParseCargoTestJSON_NoSuiteOnlyFailures pins that we can still
// succeed when the libtest stream was truncated mid-run (e.g. the
// command was killed) — failures alone are useful signal.
func TestParseCargoTestJSON_NoSuiteOnlyFailures(t *testing.T) {
	lines := []string{
		`{"type":"test","name":"x::y","event":"failed","stdout":"thread 'x::y' panicked at src/lib.rs:1\nassertion failed"}`,
	}
	out, ok := parseCargoTestJSON(lines, false)
	if ok {
		t.Errorf("ok=true without suite line — caller would lose Compiling/warning context")
		_ = out
	}
}

// TestParseCargoTestJSON_DeterministicOrder pins cross-turn
// invariance: same NDJSON in two turns produces byte-identical
// summary output. Without this, Anthropic's prefix cache would miss
// on cargo output that's compressed via this rule.
func TestParseCargoTestJSON_DeterministicOrder(t *testing.T) {
	lines := []string{
		`{"type":"test","name":"z::z","event":"failed","stdout":"thread 'z::z' panicked at z.rs:1\nfail"}`,
		`{"type":"test","name":"a::a","event":"failed","stdout":"thread 'a::a' panicked at a.rs:1\nfail"}`,
		`{"type":"test","name":"m::m","event":"failed","stdout":"thread 'm::m' panicked at m.rs:1\nfail"}`,
		`{"type":"suite","event":"failed","passed":0,"failed":3,"ignored":0,"measured":0,"filtered_out":0}`,
	}
	first, _ := parseCargoTestJSON(lines, false)
	for i := 0; i < 50; i++ {
		got, _ := parseCargoTestJSON(lines, false)
		if strings.Join(got, "\n") != strings.Join(first, "\n") {
			t.Fatalf("non-deterministic on iteration %d", i)
		}
	}
	// Failures must be sorted alphabetically.
	idxA := indexOf(first, "  - a::a")
	idxM := indexOf(first, "  - m::m")
	idxZ := indexOf(first, "  - z::z")
	if !(idxA >= 0 && idxA < idxM && idxM < idxZ) {
		t.Errorf("failures not sorted: a@%d m@%d z@%d in %v", idxA, idxM, idxZ, first)
	}
}

// TestFormatParse_FallbackWhenParseFails pins the framework contract:
// when the format parser returns ok=false (non-JSON / wrong shape),
// the buffered lines are emitted verbatim so the model never silently
// loses content.
func TestFormatParse_FallbackWhenParseFails(t *testing.T) {
	r, err := newFormatParse(RuleSpec{Format: "cargo_test_json"})
	if err != nil {
		t.Fatalf("newFormatParse: %v", err)
	}
	input := []string{"   Compiling foo", "warning: unused", "error[E0308]: mismatched types"}
	got := runRule(r, input)
	assertEqualLines(t, got, input)
}

// TestFormatParse_RegistersOnlyKnownFormats pins the schema-side
// guard: unknown format names error at construction so the user gets
// a clear message instead of silently no-op-ing.
func TestFormatParse_RegistersOnlyKnownFormats(t *testing.T) {
	if _, err := newFormatParse(RuleSpec{Format: "pytest_xml"}); err == nil {
		t.Error("expected unknown-format error")
	}
	if _, err := newFormatParse(RuleSpec{}); err == nil {
		t.Error("expected missing-format error")
	}
}

func indexOf(lines []string, want string) int {
	for i, l := range lines {
		if l == want {
			return i
		}
	}
	return -1
}
