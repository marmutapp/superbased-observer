package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/marmutapp/superbased-observer/internal/processobs"
)

// Bridge respawn policy: a capturer that exits is relaunched (self-healing
// capture on the canonical topology, where this is the ONLY capture path), with
// exponential backoff. A run that produced events resets the streak; after
// maxConsecutiveFailures fruitless attempts the backend gives up and closes its
// channel (the Observer then reports degraded health — daemon continues).
const (
	defaultMinBackoff      = 1 * time.Second
	defaultMaxBackoff      = 30 * time.Second
	maxConsecutiveFailures = 5
	stderrTailBytes        = 4096
)

// Options configure the WSL bridge Backend.
type Options struct {
	// WindowsBinaryPath is the Windows observer.exe to exec over interop
	// (a /mnt/<drive>/... path, or a C:\… path that is translated). Empty =
	// auto-resolve via ResolveWindowsObserver.
	WindowsBinaryPath string
	// PollIntervalMS is passed to the capturer's `--interval-ms`. 0 = omit
	// (the capturer's own default applies).
	PollIntervalMS int
	// Logger receives degraded-state warnings (decode errors, respawns,
	// give-up). Optional; nil silences them.
	Logger *slog.Logger
	// NetworkAccounting optionally receives the per-process network-byte
	// accounting status the capturer reports in its hello frame, so a Windows
	// deployment's health reflects what the WINDOWS capturer measured rather
	// than the daemon host's own (always pessimistic) guess.
	//
	// nil is the no-op default and means "this backend does not own the
	// handle". Hand it over only where the bridge is the sole source that
	// could measure bytes: it is single-owner state (CLAUDE.md rule 4), and a
	// bridge reporting "off" alongside a live local eBPF backend would
	// otherwise clobber a true "tcp".
	NetworkAccounting *processobs.NetworkAccounting
}

// Backend is the WSL half of the cross-OS bridge (spec §5.5): a
// processobs.Backend that execs the Windows observer.exe `process-bridge`
// subcommand over WSL interop and decodes its NDJSON stdout into the Observer's
// RawEvent channel. It holds no DB; all scrub/attribute/store runs in the WSL
// daemon downstream. Fail-open: a missing binary or non-WSL host is a clean
// Start error; a capturer crash is respawned, then surfaced as a health warning
// if it persists. Not safe for concurrent Start; one Observer drives it.
type Backend struct {
	explicitPath   string
	pollIntervalMS int
	logger         *slog.Logger
	netStatus      *processobs.NetworkAccounting

	resolvedPath string

	// Backoff bounds for the respawn loop; defaulted from the package consts
	// in New, overridable by white-box tests to avoid second-scale sleeps.
	minBackoff time.Duration
	maxBackoff time.Duration

	out      chan processobs.RawEvent
	stop     chan struct{}
	stopOnce sync.Once

	mu         sync.Mutex
	events     int64
	decodeErrs int64
	respawns   int64
	lastErr    string
	// claim tracks whether the LAST hello frame made a positive claim that
	// bytes are being counted, so it can be withdrawn when the capturer dies
	// — see invalidateNetworkStatus. Shared with the accept-mode Listener so
	// the rule has one implementation (stream.go).
	claim netClaim
}

// New builds a bridge Backend from Options (path resolution is deferred to
// Start so a missing binary surfaces as a fail-open Start error).
func New(opts Options) *Backend {
	return &Backend{
		explicitPath:   opts.WindowsBinaryPath,
		pollIntervalMS: opts.PollIntervalMS,
		logger:         opts.Logger,
		netStatus:      opts.NetworkAccounting,
		minBackoff:     defaultMinBackoff,
		maxBackoff:     defaultMaxBackoff,
		stop:           make(chan struct{}),
	}
}

// Name implements processobs.Backend.
func (b *Backend) Name() string { return "bridge" }

// RequiresUnattributedCapture implements processobs.UnattributedCapturer: the
// bridge's Windows events cannot be attributed at capture time (no pidbridge
// hit across the OS boundary), so the daemon must persist them unattributed and
// let the deferred CorrelateCrossOS pass join them to a session (§5.5).
func (b *Backend) RequiresUnattributedCapture() bool { return true }

// Start resolves the Windows binary, then launches the spawn→stream→respawn
// loop feeding the returned channel. It errors (fail-open) when not running
// under WSL interop or when no Windows observer.exe can be found.
func (b *Backend) Start(ctx context.Context) (<-chan processobs.RawEvent, error) {
	if !isWSL() {
		return nil, errors.New("processobs/bridge: requires WSL interop (no /proc/sys/fs/binfmt_misc/WSLInterop)")
	}
	path, ok := ResolveWindowsObserver(b.explicitPath)
	if !ok {
		return nil, errors.New("processobs/bridge: Windows observer.exe not found — set [observer.process].windows_binary_path to a /mnt/... path")
	}
	b.resolvedPath = path
	b.out = make(chan processobs.RawEvent, 1024)
	go b.loop(ctx)
	return b.out, nil
}

// Close stops the respawn loop. Idempotent; safe after a Start error.
func (b *Backend) Close() error {
	b.stopOnce.Do(func() { close(b.stop) })
	return nil
}

// loop spawns the capturer, streams it, and respawns on exit with backoff
// until ctx/stop or the failure cap. It owns closing b.out.
func (b *Backend) loop(ctx context.Context) {
	defer close(b.out)
	// Whatever ends this loop — ctx cancel, Close, or the failure cap — no
	// capturer is running afterwards, so a positive network-accounting claim
	// must not outlive it.
	defer b.invalidateNetworkStatus("cross-OS process capturer stopped — per-process network bytes are no longer being measured")

	backoff := b.minBackoff
	failures := 0
	for {
		if b.done(ctx) {
			return
		}
		produced, err := b.runCapturer(ctx)
		if b.done(ctx) {
			return
		}
		// The capturer is gone. Anything it claimed about live byte counting
		// is now stale, and stale is a lie — withdraw it before the respawn.
		b.invalidateNetworkStatus("cross-OS process capturer exited — per-process network bytes are not being measured until it respawns")

		if produced > 0 {
			failures, backoff = 0, b.minBackoff
		} else {
			failures++
		}
		b.recordRespawn(err)

		if failures >= maxConsecutiveFailures {
			b.setLastErr(fmt.Sprintf("capturer exited %d times without producing events: %v", failures, err))
			// Terminal for this backend: say so explicitly rather than leaving
			// the daemon's generic default, which cannot name the cause.
			b.netStatus.Set(processobs.NetworkAccountingUnavailable,
				fmt.Sprintf("cross-OS process capturer exited %d times without producing events — capture disabled", failures))
			b.warn("process bridge: capturer failing repeatedly — capture disabled (daemon continues)",
				"failures", failures, "err", err)
			return
		}
		b.warn("process bridge: capturer exited — respawning", "err", err, "backoff", backoff.String(), "events", produced)
		if !sleepCtx(ctx, b.stop, backoff) {
			return
		}
		backoff = minDur(backoff*2, b.maxBackoff)
	}
}

// runCapturer spawns one capturer process and streams its NDJSON stdout into
// b.out until the process exits (clean or crash) or our consumer goes away. It
// returns the number of events forwarded and the process's exit error (with the
// tail of its stderr appended for diagnostics).
func (b *Backend) runCapturer(ctx context.Context) (events int64, err error) {
	args := []string{"process-bridge"}
	if b.pollIntervalMS > 0 {
		args = append(args, "--interval-ms", strconv.Itoa(b.pollIntervalMS))
	}
	//nolint:gosec // G204: resolvedPath is the observer's own resolved executable (operator-configured/self-located), args are a fixed subcommand + integer interval — no external input.
	cmd := exec.CommandContext(ctx, b.resolvedPath, args...)
	stdout, perr := cmd.StdoutPipe()
	if perr != nil {
		return 0, fmt.Errorf("stdout pipe: %w", perr)
	}
	var stderr tailWriter
	stderr.max = stderrTailBytes
	cmd.Stderr = &stderr

	if serr := cmd.Start(); serr != nil {
		return 0, fmt.Errorf("spawn %s: %w", b.resolvedPath, serr)
	}

	// The stream itself is transport-agnostic (stream.go); only the spawn,
	// the kill, and the stderr tail below are specific to this transport. A
	// terminal read error on the pipe is discarded here on purpose: the
	// child's exit status plus its stderr tail, spliced in below, is the
	// strictly better diagnostic for THIS transport.
	events, consumerGone, _ := consumeFrames(ctx, stdout, b)
	if consumerGone {
		_ = cmd.Process.Kill() // consumer gone; stop the capturer
	}

	werr := cmd.Wait()
	if tail := stderr.String(); tail != "" {
		if werr != nil {
			werr = fmt.Errorf("%w; stderr: %s", werr, tail)
		} else {
			werr = fmt.Errorf("capturer stderr: %s", tail)
		}
	}
	return events, werr
}

// --- frameSink (stream.go): the spawn transport's half of the shared decode
// loop. Each method is deliberately trivial; the RULES live in consumeFrames
// and netClaim so the accept transport cannot drift from them.

func (b *Backend) onHello(h Hello) { b.applyHelloNetworkStatus(h) }

func (b *Backend) onDecodeErr(err error) {
	b.incDecodeErr()
	b.warn("process bridge: decode error (line skipped)", "err", err)
}

func (b *Backend) onWireMismatch(got, want int) {
	b.warn("process bridge: capturer wire-version mismatch", "got", got, "want", want)
}

func (b *Backend) onErrorFrame(msg string) {
	b.warn("process bridge: capturer reported error", "err", msg)
}

func (b *Backend) onEvent(ctx context.Context, ev processobs.RawEvent) bool {
	b.incEvents()
	return b.send(ctx, ev)
}

// onCapturerStats logs the capturer's decoder report and carries it NO FURTHER
// — deliberately, and this is the whole of the reasoning.
//
// The spawn transport is not a dial-in transport: it implements no
// processobs.TransportStatsSource, so it has nowhere to publish the fact that
// would not be a value invented for it. And the fact is nearly always absent
// here anyway: a spawn-mode capturer is exec'd from WSL's MEDIUM-integrity
// interop token, ETW session control always requires elevation, so its network
// decoder never starts and it sends no stats frame at all. The elevated
// capturer — the one whose drop count is the validation signal — reaches the
// daemon over the ACCEPT transport, which does carry it.
//
// Dropping it silently would still be wrong, so it is logged. If this ever
// needs to be a reported state, the fix is to give this backend the transport
// capability, not to fabricate a TransportStats it does not have.
func (b *Backend) onCapturerStats(s CapturerStats) {
	decode := s.DecodeStats()
	switch {
	case decode.Any():
		b.warn("process bridge: the capturer's network-telemetry decoder is REFUSING events — "+
			"per-process byte totals from this host may be wrong, not merely missing",
			"dropped", decode.NetworkDropped,
			"unsupported_version", decode.NetworkUnsupportedVersion)
	case decode.NothingClassified():
		b.warn("process bridge: the capturer's network-telemetry decoder has classified NO data events — "+
			"it refused nothing, but it accepted nothing either, so it is measuring no bytes at all; "+
			"if this host was moving TCP traffic, the provider's event ids no longer match this build's layout table",
			"ignored", decode.NetworkIgnored,
			"decoded", decode.NetworkDecoded)
	}
}

// applyHelloNetworkStatus carries the capturer's per-process network-byte
// accounting status from its hello frame onto the shared status handle, so
// `observer doctor` / the dashboard report what the WINDOWS side measured.
// The rule it applies (silence is UNKNOWN, never a confident "off") lives in
// netClaim.apply.
func (b *Backend) applyHelloNetworkStatus(h Hello) {
	b.claim.apply(b.netStatus, h)
}

// invalidateNetworkStatus withdraws a POSITIVE network-accounting claim once
// the capturer that made it is gone, replacing it with "unavailable" plus the
// given reason. See netClaim.invalidate for why only a positive claim is
// withdrawn.
func (b *Backend) invalidateNetworkStatus(reason string) {
	b.claim.invalidate(b.netStatus, reason)
}

// send delivers an event unless ctx/stop fires first; false means stop.
func (b *Backend) send(ctx context.Context, ev processobs.RawEvent) bool {
	select {
	case <-ctx.Done():
		return false
	case <-b.stop:
		return false
	case b.out <- ev:
		return true
	}
}

// done reports whether the backend should stop (ctx cancelled or Close called).
func (b *Backend) done(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}
	select {
	case <-b.stop:
		return true
	default:
		return false
	}
}

// Stats is a snapshot of the bridge's lifetime counters (events forwarded,
// decode errors, capturer respawns, last error). Read by tests; a hook for
// surfacing bridge health in doctor/metrics later.
type Stats struct {
	Events     int64
	DecodeErrs int64
	Respawns   int64
	LastErr    string
}

// Stats returns a snapshot of the lifetime counters.
func (b *Backend) Stats() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()
	return Stats{Events: b.events, DecodeErrs: b.decodeErrs, Respawns: b.respawns, LastErr: b.lastErr}
}

func (b *Backend) incEvents() {
	b.mu.Lock()
	b.events++
	b.mu.Unlock()
}

func (b *Backend) incDecodeErr() {
	b.mu.Lock()
	b.decodeErrs++
	b.mu.Unlock()
}

func (b *Backend) recordRespawn(err error) {
	b.mu.Lock()
	b.respawns++
	if err != nil {
		b.lastErr = err.Error()
	}
	b.mu.Unlock()
}

func (b *Backend) setLastErr(s string) {
	b.mu.Lock()
	b.lastErr = s
	b.mu.Unlock()
}

func (b *Backend) warn(msg string, args ...any) {
	if b.logger != nil {
		b.logger.Warn(msg, args...)
	}
}

// tailWriter retains only the last max bytes written — the tail of a crashed
// capturer's stderr, surfaced in a respawn warning without unbounded buffering.
// The exec runtime writes from a separate goroutine, so it is mutex-guarded.
type tailWriter struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	if w.max > 0 && len(w.buf) > w.max {
		w.buf = w.buf[len(w.buf)-w.max:]
	}
	return len(p), nil
}

func (w *tailWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.TrimSpace(string(w.buf))
}

func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// sleepCtx waits d, returning false if ctx/stop fires first.
func sleepCtx(ctx context.Context, stop <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-stop:
		return false
	case <-t.C:
		return true
	}
}
