package policy

import (
	"testing"
	"time"
)

// Benchmarks pin the §6.4/§17.9 latency budget: hook-path evaluation
// must stay well under the 50ms p99 budget — these run in the
// microsecond range, leaving the budget to process spawn and JSON,
// not rule walking. CI keeps them compiling and runnable
// (`go test -bench . ./internal/policy/`).

// benchEngine builds the enforce-mode engine once per benchmark.
func benchEngine(b *testing.B) *Engine {
	b.Helper()
	eng, err := New(Config{Mode: ModeEnforce, Home: "/home/u"})
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	return eng
}

// benchShellEvent mirrors the test fixture event shape.
func benchShellEvent(cmd string) Event {
	return Event{
		Kind:        KindShellExec,
		ActionType:  "run_command",
		Target:      cmd,
		Cwd:         "/home/u/proj",
		ProjectRoot: "/home/u/proj",
		SessionID:   "bench",
		Now:         time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC),
	}
}

// BenchmarkEvaluate_SimpleShell is the common case: a short benign
// command.
func BenchmarkEvaluate_SimpleShell(b *testing.B) {
	eng := benchEngine(b)
	ev := benchShellEvent("go test ./...")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = eng.Evaluate(ev)
	}
}

// BenchmarkEvaluate_DestructiveHit exercises a full rule hit with
// target resolution.
func BenchmarkEvaluate_DestructiveHit(b *testing.B) {
	eng := benchEngine(b)
	ev := benchShellEvent("sudo rm -rf /etc/nginx")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = eng.Evaluate(ev)
	}
}

// BenchmarkEvaluate_NestedUnwrap5Deep is the parser worst case the
// spec bounds: five recursive bash -c levels.
func BenchmarkEvaluate_NestedUnwrap5Deep(b *testing.B) {
	eng := benchEngine(b)
	cmd := "rm -rf /"
	for i := 0; i < 5; i++ {
		cmd = `bash -c "` + cmd + `"`
	}
	ev := benchShellEvent(cmd)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = eng.Evaluate(ev)
	}
}

// BenchmarkEvaluate_FileAccessBoundary exercises the path-rule walk
// (resolution, home-relative rewrite, pattern tables).
func BenchmarkEvaluate_FileAccessBoundary(b *testing.B) {
	eng := benchEngine(b)
	ev := Event{
		Kind:        KindFileAccess,
		ActionType:  "read_file",
		Target:      "~/.ssh/id_rsa",
		Cwd:         "/home/u/proj",
		ProjectRoot: "/home/u/proj",
		SessionID:   "bench",
		Now:         time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = eng.Evaluate(ev)
	}
}

// BenchmarkEvaluate_NoMatchFastPath is the ingest-path case: an event
// kind with no applicable rules must cost near-zero (§7 ingest
// overhead budget ≤5%).
func BenchmarkEvaluate_NoMatchFastPath(b *testing.B) {
	eng := benchEngine(b)
	ev := Event{Kind: KindMCPCall, Target: "tools/call", SessionID: "bench"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = eng.Evaluate(ev)
	}
}
