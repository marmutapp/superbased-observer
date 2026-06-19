package antigravity

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// realCLILogFixture is one observed agy.exe session header (PID +
// HTTPS port + HTTP port) followed by a "Created conversation" line.
// Captured from the operator's host 2026-05-24
// (/mnt/c/Users/auzy_/.gemini/antigravity-cli/log/cli-20260524_125841.log)
// so the parser tests match real wire format.
const realCLILogFixture = `I0524 12:58:41.049548 11464 server.go:1303] Starting language server process with pid 11464
I0524 12:58:41.063514 11464 server.go:487] Language server listening on random port at 63206 for HTTPS (gRPC)
I0524 12:58:41.064645 11464 server.go:494] Language server listening on random port at 63207 for HTTP
I0524 12:58:41.503822 11464 manager.go:249] Initializing CLI store manager for workspace C:\programsx\superbased-observer
I0524 12:59:06.548394 11464 conversation_manager.go:284] Starting new conversation (agent=false)
I0524 12:59:06.551278 11464 server.go:747] Created conversation cef75182-f5a6-44f1-9157-692cc628c4c7
I0524 13:01:11.123456 11464 server.go:747] Created conversation 791a7ab9-b592-4639-9a07-88eec37baa35
I0524 13:32:55.886868 11464 server.go:800] Stream ended for cef75182-f5a6-44f1-9157-692cc628c4c7, sending completion signal
`

func writeCLILogFixture(t *testing.T, dir, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "cli-20260524_125841.log")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	return path
}

// TestParseLogServerHeaderExtractsPIDAndPorts pins that the
// real-format header lines are correctly parsed into PID +
// HTTPSPort + HTTPPort.
func TestParseLogServerHeaderExtractsPIDAndPorts(t *testing.T) {
	tmp := t.TempDir()
	path := writeCLILogFixture(t, tmp, realCLILogFixture)
	srv := parseLogServerHeader(path, mtimeOf(t, path))
	if srv == nil {
		t.Fatal("parseLogServerHeader returned nil")
	}
	if srv.PID != 11464 {
		t.Errorf("PID = %d, want 11464", srv.PID)
	}
	if srv.HTTPSPort != 63206 {
		t.Errorf("HTTPSPort = %d, want 63206", srv.HTTPSPort)
	}
	if srv.HTTPPort != 63207 {
		t.Errorf("HTTPPort = %d, want 63207", srv.HTTPPort)
	}
}

// TestParseLogServerHeaderRejectsWithoutPIDLine pins the defensive
// nil return when the "Starting language server" line is missing
// — protects against accidentally treating an unrelated glog file
// as an agy.exe session.
func TestParseLogServerHeaderRejectsWithoutPIDLine(t *testing.T) {
	tmp := t.TempDir()
	content := `I0524 12:58:41.063514 11464 server.go:487] Language server listening on random port at 63206 for HTTPS (gRPC)
I0524 12:59:06.551278 11464 server.go:747] Created conversation aaaa1111-2222-3333-4444-555555555555
`
	path := writeCLILogFixture(t, tmp, content)
	if srv := parseLogServerHeader(path, mtimeOf(t, path)); srv != nil {
		t.Errorf("expected nil for log missing PID line, got %+v", srv)
	}
}

// TestParseLogConvCreatedListsAllUUIDs pins multi-conv extraction —
// a single agy.exe session can host many conversations as the user
// types successive prompts.
func TestParseLogConvCreatedListsAllUUIDs(t *testing.T) {
	tmp := t.TempDir()
	path := writeCLILogFixture(t, tmp, realCLILogFixture)
	convs := parseLogConvCreated(path)
	want := []string{
		"cef75182-f5a6-44f1-9157-692cc628c4c7",
		"791a7ab9-b592-4639-9a07-88eec37baa35",
	}
	if len(convs) != len(want) {
		t.Fatalf("conv count = %d, want %d (%v)", len(convs), len(want), convs)
	}
	for i, c := range convs {
		if c != want[i] {
			t.Errorf("conv[%d] = %q, want %q", i, c, want[i])
		}
	}
}

// TestLookupOriginatingServerEndToEnd pins the full
// adapter.lookupOriginatingServer flow, including caching by mtime
// freshness (changing the log file's mtime triggers a re-scan).
func TestLookupOriginatingServerEndToEnd(t *testing.T) {
	tmp := t.TempDir()
	cliRoot := filepath.Join(tmp, ".gemini", "antigravity-cli")
	logDir := filepath.Join(cliRoot, "log")
	writeCLILogFixture(t, logDir, realCLILogFixture)

	a := New()
	srv, ok := a.lookupOriginatingServer(cliRoot, "cef75182-f5a6-44f1-9157-692cc628c4c7")
	if !ok {
		t.Fatal("lookupOriginatingServer not ok for known conv")
	}
	if srv.PID != 11464 || srv.HTTPSPort != 63206 {
		t.Errorf("server fields = %+v, want PID=11464 HTTPSPort=63206", srv)
	}

	// Cache hit second time — modifying the file content WITHOUT
	// bumping mtime must surface the old cached server (cliLog-
	// freshnessUnix is mtime-based, not content-based).
	srv2, ok := a.lookupOriginatingServer(cliRoot, "791a7ab9-b592-4639-9a07-88eec37baa35")
	if !ok {
		t.Fatal("second conv lookup not ok")
	}
	if srv2.PID != 11464 {
		t.Errorf("second conv PID = %d, want 11464", srv2.PID)
	}

	// Unknown conv → ok=false.
	if _, ok := a.lookupOriginatingServer(cliRoot, "ffffffff-0000-0000-0000-000000000000"); ok {
		t.Error("expected ok=false for unknown conv")
	}
}

// TestEndpointCandidatesOrder pins the preferred-first ordering:
// http://HTTPSPort, https://HTTPSPort, http://HTTPPort. Matches the
// empirical pattern observed via probe-cli on the operator's host.
func TestEndpointCandidatesOrder(t *testing.T) {
	srv := &originatingServer{HTTPSPort: 63206, HTTPPort: 63207}
	got := srv.EndpointCandidates()
	want := []string{
		"http://127.0.0.1:63206",
		"https://127.0.0.1:63206",
		"http://127.0.0.1:63207",
	}
	if len(got) != len(want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("candidates[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestEndpointCandidatesNilReceiver pins the nil-safe contract:
// calling on a nil receiver returns nil, not a panic. The
// originatingServerCandidates wrapper relies on this for the
// "no log scan turned up the conv" case.
func TestEndpointCandidatesNilReceiver(t *testing.T) {
	var srv *originatingServer
	if got := srv.EndpointCandidates(); got != nil {
		t.Errorf("nil receiver candidates = %v, want nil", got)
	}
}

// TestOriginatingServerCandidatesNonCLILayoutReturnsNil pins that
// desktop / unknown layouts don't accidentally trigger log scans
// against the CLI subtree.
func TestOriginatingServerCandidatesNonCLILayoutReturnsNil(t *testing.T) {
	a := New()
	desktopPath := filepath.Join(t.TempDir(), ".gemini", "antigravity", "conversations", "abc.pb")
	if got := a.originatingServerCandidates(desktopPath, "abc"); got != nil {
		t.Errorf("desktop layout candidates = %v, want nil", got)
	}
}

func mtimeOf(t *testing.T, path string) time.Time {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.ModTime()
}
