package antigravity

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Phase 3 of docs/plans/antigravity-token-coverage-design-2026-05-24.md:
// parse cli-*.log files to map each conversation UUID to the
// language_server instance that originally hosted it (PID + listening
// ports). The bridge can then pin its first attempt to that specific
// endpoint, skipping the full PowerShell-WMI fan-out across every
// running agy.exe / language_server process.
//
// Why this matters: agy.exe holds the cascade trajectory in memory
// only. Once the originating instance terminates, no sibling
// instance can serve `GetCascadeTrajectory` for that conv —
// fan-out wastes 30–90s polling dead candidates only to fail. With
// the PID pin, we get either a fast success (originating server
// alive) or a fast failure (originating server dead → fall through
// immediately to the Phase 2 snapshot path).
//
// The log lines we parse appear exactly once per agy.exe session,
// in the first few hundred lines of the per-session log:
//
//	I0524 12:58:41 server.go:1303] Starting language server process with pid 11464
//	I0524 12:58:41 server.go:487] Language server listening on random port at 63206 for HTTPS (gRPC)
//	I0524 12:58:41 server.go:494] Language server listening on random port at 63207 for HTTP
//
// Followed by zero or more "Created conversation <uuid>" lines as
// the user starts new conversations within that session.

// originatingServer captures the language_server PID + listening
// ports for one agy.exe instance. Returned by lookupOriginatingServer
// when the requested conversation_uuid was created inside this
// instance.
type originatingServer struct {
	PID         int
	HTTPSPort   int // "Language server listening on random port at <N> for HTTPS (gRPC)"
	HTTPPort    int // "Language server listening on random port at <N> for HTTP"
	SourceLog   string
	SourceMtime time.Time
}

// originatingServerCacheEntry memoises one log-dir scan, keyed by
// log-dir freshness (cliLogFreshnessUnix). Re-scan triggers
// whenever a new log file is created OR an existing log file is
// appended to (matters for the live-agy-creates-new-conv case).
type originatingServerCacheEntry struct {
	mtime   int64
	servers map[string]*originatingServer
}

var (
	cliPIDStartRegex = regexp.MustCompile(`Starting language server process with pid\s+(\d+)`)
	cliPortRegex     = regexp.MustCompile(`Language server listening on random port at\s+(\d+)\s+for\s+(HTTPS|HTTP)`)
)

// lookupOriginatingServer returns the language_server PID + ports
// that hosted convID, derived from the cli-*.log file emitted at
// that agy.exe's startup. ok=false when no scanned log carries the
// corresponding "Created conversation" line — typical for:
//
//   - Desktop IDE convs (their logs live in antigravity/daemon/, not
//     antigravity-cli/log/; Phase 3 covers CLI only for v1).
//   - CLI convs whose origin log was already rotated out.
//   - Bursty observer-first-contact races where the .pb file was
//     written but the log line hadn't flushed yet.
//
// Cheap to call: per-logDir scan is memoised by freshness mtime, so
// steady-state polling pays a single map lookup. Cold scan walks
// every cli-*.log header (one Open + ~200-line scan per file).
func (a *Adapter) lookupOriginatingServer(cliRoot, convID string) (*originatingServer, bool) {
	if cliRoot == "" || convID == "" {
		return nil, false
	}
	logDir := filepath.Join(cliRoot, "log")
	if _, err := os.Stat(logDir); err != nil {
		return nil, false
	}
	a.origServerMu.Lock()
	defer a.origServerMu.Unlock()
	if a.origServerCache == nil {
		a.origServerCache = map[string]*originatingServerCacheEntry{}
	}
	fresh := cliLogFreshnessUnix(logDir)
	cached, ok := a.origServerCache[logDir]
	if !ok || cached.mtime != fresh {
		cached = &originatingServerCacheEntry{
			mtime:   fresh,
			servers: scanOriginatingServers(logDir),
		}
		a.origServerCache[logDir] = cached
	}
	srv, ok := cached.servers[convID]
	return srv, ok
}

// scanOriginatingServers walks every cli-*.log under logDir in
// mtime-desc order and builds a conversation_uuid → server map.
// Each log file represents exactly one agy.exe session; the PID +
// port lines appear once at the top, then any number of "Created
// conversation" lines as the user types prompts. Freshest log wins
// on the rare-to-impossible UUID collision (UUIDs are random).
func scanOriginatingServers(logDir string) map[string]*originatingServer {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return nil
	}
	type logFile struct {
		name  string
		mtime time.Time
	}
	var logs []logFile
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "cli-") || !strings.HasSuffix(name, ".log") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		logs = append(logs, logFile{name: name, mtime: fi.ModTime()})
	}
	sort.Slice(logs, func(i, j int) bool { return logs[i].mtime.After(logs[j].mtime) })
	out := map[string]*originatingServer{}
	for _, lg := range logs {
		path := filepath.Join(logDir, lg.name)
		srv := parseLogServerHeader(path, lg.mtime)
		if srv == nil {
			continue
		}
		for _, conv := range parseLogConvCreated(path) {
			if _, exists := out[conv]; exists {
				continue
			}
			out[conv] = srv
		}
	}
	return out
}

// parseLogServerHeader extracts the PID + ports from the first
// ~200 lines of a cli-*.log. Returns nil when neither the PID line
// nor any port line was found — guards against foreign log formats
// the parser shouldn't misinterpret.
func parseLogServerHeader(path string, mtime time.Time) *originatingServer {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	srv := &originatingServer{SourceLog: path, SourceMtime: mtime}
	scanned := 0
	haveStart := false
	for scanner.Scan() {
		scanned++
		if scanned > 200 {
			break
		}
		line := scanner.Text()
		if m := cliPIDStartRegex.FindStringSubmatch(line); m != nil {
			if p, err := strconv.Atoi(m[1]); err == nil {
				srv.PID = p
				haveStart = true
			}
			continue
		}
		if m := cliPortRegex.FindStringSubmatch(line); m != nil {
			port, _ := strconv.Atoi(m[1])
			switch m[2] {
			case "HTTPS":
				srv.HTTPSPort = port
			case "HTTP":
				srv.HTTPPort = port
			}
		}
	}
	if !haveStart || (srv.HTTPSPort == 0 && srv.HTTPPort == 0) {
		return nil
	}
	return srv
}

// parseLogConvCreated returns every UUID appearing in a "Created
// conversation <uuid>" line of the log. Reuses cliCreatedRegex from
// metadata_cli.go.
func parseLogConvCreated(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var out []string
	for scanner.Scan() {
		if m := cliCreatedRegex.FindStringSubmatch(scanner.Text()); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}

// EndpointCandidates returns the URLs to try for this server in
// preferred-first order. Verified empirically on the operator's
// host (2026-05-24): Antigravity CLI advertises one port as
// "HTTPS (gRPC)" and one as "HTTP", but the gRPC port serves
// plain http:// requests too — the linux-native path successfully
// reaches `http://<HTTPSPort>` consistently. We try that first,
// then fall back to https on the same port (for installs that
// genuinely require TLS), then the explicit HTTP port.
//
// Empty when receiver is nil (no log scan available).
func (s *originatingServer) EndpointCandidates() []string {
	if s == nil {
		return nil
	}
	var out []string
	if s.HTTPSPort > 0 {
		out = append(out, "http://127.0.0.1:"+strconv.Itoa(s.HTTPSPort))
		out = append(out, "https://127.0.0.1:"+strconv.Itoa(s.HTTPSPort))
	}
	if s.HTTPPort > 0 && s.HTTPPort != s.HTTPSPort {
		out = append(out, "http://127.0.0.1:"+strconv.Itoa(s.HTTPPort))
	}
	return out
}
