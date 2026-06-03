package antigravity

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// bridgeBinaryName is the file name of the Windows-side helper
// observer ships in `bin/`. Cross-compiled at build time via
// `make build` (target adds `bin/antigravity-bridge.exe`).
const bridgeBinaryName = "antigravity-bridge.exe"

// bridgePin lets the adapter skip the bridge's full discover()+
// per-server fan-out by naming the language_server endpoint to talk
// to. Populated from convEndpointCache (keyed by conversation_id):
// once a server has successfully responded for conv X, subsequent
// invocations for X pin directly to that endpoint.
//
// Both Endpoint and CSRFToken must be considered together — empty
// CSRFToken is legitimate (agy.exe / CLI embedded servers don't
// advertise --csrf_token). Empty Endpoint means "no pin".
type bridgePin struct {
	Endpoint  string
	CSRFToken string
}

// bridgeResult is the structured return from a bridge invocation:
// raw stdout payload + the working endpoint reported by the bridge's
// stderr hint line. The caller uses HitEndpoint/HitCSRF to populate
// the per-conversation endpoint cache after a successful call.
type bridgeResult struct {
	Stdout      []byte
	HitEndpoint string
	HitCSRF     string
}

// invokeBridgeFromWSL runs `antigravity-bridge.exe convert <uuid>`
// via powershell.exe (the helper itself runs Windows-side and uses
// Win32 APIs to discover language_server processes + their listening
// ports). Returns the bridge's stdout (Markdown) on success.
//
// Resolution order for the helper binary:
//
//  1. $OBSERVER_ANTIGRAVITY_BRIDGE — explicit override.
//  2. <observer-binary-dir>/antigravity-bridge.exe — the canonical
//     location when observer is installed via npm or `make build`.
//  3. <CWD>/bin/antigravity-bridge.exe — when running from the
//     development tree.
//
// If the resolved path is WSL-native (under /home/... rather than
// /mnt/<drive>/...), the bridge is auto-copied to a Windows-
// reachable temp location since powershell.exe can't reliably
// invoke WSL-native binaries by path on all distros.
//
// Returns ErrBridgeNotInstalled when no candidate exists; the
// caller surfaces this as an actionable warning rather than a
// hard error.
func invokeBridgeFromWSL(ctx context.Context, conversationID string, timeout time.Duration) (string, error) {
	res, err := invokeBridgeFromWSLPinned(ctx, conversationID, timeout, bridgePin{})
	if err != nil {
		return "", err
	}
	return string(res.Stdout), nil
}

// invokeBridgeFromWSLPinned is the cache-aware variant of
// invokeBridgeFromWSL: when pin.Endpoint is set, the bridge skips
// discovery and POSTs directly to the pinned server (~200-500ms total
// vs the ~5-60s cost of a full discover+fan-out). Returns the working
// endpoint via bridgeResult so the caller can populate the per-
// conversation cache.
func invokeBridgeFromWSLPinned(ctx context.Context, conversationID string, timeout time.Duration, pin bridgePin) (bridgeResult, error) {
	res, err := invokeBridge(ctx, "convert", conversationID, timeout, pin)
	if err != nil {
		return bridgeResult{}, err
	}
	if len(bytes.TrimSpace(res.Stdout)) == 0 {
		return bridgeResult{}, errors.New("bridge returned empty stdout")
	}
	return res, nil
}

// invokeBridgeStructuredFromWSL runs `antigravity-bridge.exe
// structured <uuid>` and returns the raw response payload (gRPC frame
// header already stripped by the bridge). The payload is binary
// protobuf — never decode as UTF-8.
//
// Failure modes mirror invokeBridgeFromWSL: bridge missing, no
// running language_server, gRPC error, etc. All non-zero exits
// surface as a single wrapped error so the caller can log and move
// on (the structured fetch is best-effort; markdown recovery already
// succeeded).
func invokeBridgeStructuredFromWSL(ctx context.Context, conversationID string, timeout time.Duration) ([]byte, error) {
	res, err := invokeBridgeStructuredFromWSLPinned(ctx, conversationID, timeout, bridgePin{})
	if err != nil {
		return nil, err
	}
	return res.Stdout, nil
}

// invokeBridgeStructuredFromWSLPinned is the cache-aware variant of
// invokeBridgeStructuredFromWSL. See invokeBridgeFromWSLPinned for
// the cache contract.
func invokeBridgeStructuredFromWSLPinned(ctx context.Context, conversationID string, timeout time.Duration, pin bridgePin) (bridgeResult, error) {
	res, err := invokeBridge(ctx, "structured", conversationID, timeout, pin)
	if err != nil {
		return bridgeResult{}, err
	}
	if len(res.Stdout) == 0 {
		return bridgeResult{}, errors.New("bridge returned empty stdout")
	}
	return res, nil
}

// invokeBridge runs the bridge with the given subcommand and returns
// stdout plus the endpoint hint parsed from the bridge's stderr
// (`bridge-endpoint=<url>\tbridge-csrf=<token>`). Subcommand-specific
// empty-stdout / decode logic stays in the caller.
//
// When pin.Endpoint is set, `--endpoint`+`--csrf` flags are passed
// through and the bridge skips discovery — this is the steady-state
// fast path keyed by the adapter's per-conversation endpoint cache.
func invokeBridge(ctx context.Context, subcommand, conversationID string, timeout time.Duration, pin bridgePin) (bridgeResult, error) {
	bridge, err := locateBridgeBinary()
	if err != nil {
		return bridgeResult{}, err
	}
	if _, err := exec.LookPath("powershell.exe"); err != nil {
		// Environment problem, not per-file: PATH could be fixed and
		// the next attempt would work. Don't poison the unrecoverable
		// cache.
		return bridgeResult{}, fmt.Errorf("powershell.exe not on PATH: %w: %w", err, ErrBridgeTransient)
	}

	winPath, err := bridgePathForPowerShell(bridge)
	if err != nil {
		// /mnt/c cache copy errors (permission denied during AV scan,
		// file locked by a running bridge invocation, transient ENOSPC
		// on the Windows-side temp dir) are environmental — wrap as
		// transient so the caller holds the cursor and retries on the
		// next tick instead of marking the per-file marker. Otherwise
		// a one-off race poisons the file until its size or mtime
		// drifts.
		return bridgeResult{}, fmt.Errorf("%w: %w", err, ErrBridgeTransient)
	}

	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	psCommand := "& '" + winPath + "' " + subcommand + " '" + conversationID + "'"
	if pin.Endpoint != "" {
		// Append --endpoint and --csrf as additional positional args
		// to the bridge. Single-quote each value so PowerShell doesn't
		// strip the URL's // or interpret the empty CSRF as a flag.
		psCommand += " --endpoint '" + pin.Endpoint + "' --csrf '" + pin.CSRFToken + "'"
	}
	cmd := exec.CommandContext(rctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", psCommand)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return bridgeResult{}, fmt.Errorf("%s %s: exit %w; stderr: %s", filepath.Base(bridge), subcommand, err, strings.TrimSpace(stderr.String()))
	}
	hitEndpoint, hitCSRF := parseBridgeEndpointHint(stderr.String())
	return bridgeResult{Stdout: stdout.Bytes(), HitEndpoint: hitEndpoint, HitCSRF: hitCSRF}, nil
}

// parseBridgeEndpointHint extracts the `bridge-endpoint=<url>\t
// bridge-csrf=<token>` line emitted by the bridge on a successful
// call. Returns ("", "") when the hint is absent (older bridge
// binaries, or stderr emitted without the marker). Tolerates extra
// stderr lines around the hint.
func parseBridgeEndpointHint(stderr string) (endpoint, csrf string) {
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "bridge-endpoint=") {
			continue
		}
		fields := strings.Split(line, "\t")
		for _, f := range fields {
			switch {
			case strings.HasPrefix(f, "bridge-endpoint="):
				endpoint = strings.TrimPrefix(f, "bridge-endpoint=")
			case strings.HasPrefix(f, "bridge-csrf="):
				csrf = strings.TrimPrefix(f, "bridge-csrf=")
			}
		}
		return endpoint, csrf
	}
	return "", ""
}

// bridgePathForPowerShell returns a Windows-shaped path that
// powershell.exe can invoke. If the source bridge path is already
// under /mnt/<drive>/, we just translate it. If it's WSL-native
// (under /home/...), we copy it once to a Windows-reachable temp
// directory and cache the destination — subsequent calls reuse
// the cached copy as long as it's same-size.
//
// The cache filename embeds the source file size — concretely
// `antigravity-bridge-<size>.exe`. This sidesteps a permanent
// permission-denied loop that surfaced 2026-05-24: the running
// observer constantly invokes the cached .exe via powershell, so
// the file is essentially always opened-for-execute by SOME
// powershell.exe instance. Overwriting a Windows .exe while it's
// open fails. Without versioning, a fresh `make build` would change
// the source size, copyIfNewer would try to overwrite the locked
// cache, the write would fail, the copyIfNewer graceful fallback
// would keep using the OLD cached binary, and new bridge code
// (endpoint cache, --endpoint/--csrf flags, etc.) would NEVER reach
// production until the operator manually stopped the daemon and
// deleted the cache. Versioning by size means new bridge code lands
// in a brand-new filename — no collision, immediate uptake.
//
// Old size-versioned files are NOT actively cleaned up here; they
// linger until the OS clears the temp dir or `observer doctor`
// prunes them (a future enhancement). Each is ~9 MiB; the bridge
// source rarely changes in a way that bumps the size, so growth is
// bounded in practice.
//
// Cache location: /mnt/c/Users/<u>/AppData/Local/Temp/observer/
// antigravity-bridge-<size>.exe. Falls back to /mnt/c/Windows/Temp/
// observer/ if no Users dirs exist.
func bridgePathForPowerShell(bridge string) (string, error) {
	if winPath := wslPathToWindows(bridge); winPath != "" {
		return winPath, nil
	}
	// WSL-native source. Pick a Windows-reachable destination.
	srcInfo, statErr := os.Stat(bridge)
	if statErr != nil {
		return "", fmt.Errorf("bridge stat: %w", statErr)
	}
	dest, err := windowsCacheDestinationFor(srcInfo.Size())
	if err != nil {
		return "", fmt.Errorf("bridge cache dest: %w", err)
	}
	if err := copyIfNewer(bridge, dest); err != nil {
		return "", fmt.Errorf("copy bridge to %s: %w", dest, err)
	}
	winPath := wslPathToWindows(dest)
	if winPath == "" {
		return "", fmt.Errorf("cached bridge path %q didn't translate", dest)
	}
	return winPath, nil
}

// windowsCacheDir is the /mnt/c/... directory where the bridge
// cache .exes live. Process-wide cached via sync.Once so concurrent
// invocations don't race the writable-probe marker.
//
// Cached BY DIRECTORY (not full filename) since the per-size
// filename is composed by the caller — multiple sizes coexist in
// the same dir.
var (
	cachedDirOnce sync.Once
	cachedDir     string
	cachedDirErr  error
)

// windowsCacheDestinationFor returns the size-versioned full cache
// path for a bridge binary of the given source size. Each unique
// size lands in its own .exe filename so newly-built bridge code
// never collides with an in-use older cached file (the running
// observer constantly invokes the cached .exe via powershell, so
// the file is effectively always opened-for-execute on a busy host
// — overwriting it would fail forever).
func windowsCacheDestinationFor(srcSize int64) (string, error) {
	dir, err := windowsCacheDir()
	if err != nil {
		return "", err
	}
	// Filename includes size: antigravity-bridge-<bytes>.exe. Strips
	// the .exe suffix from the canonical bridgeBinaryName and re-
	// appends after the size token so the result is still recognised
	// as a Windows executable by both powershell + AV scanners.
	base := strings.TrimSuffix(bridgeBinaryName, ".exe")
	return filepath.Join(dir, base+"-"+strconv.FormatInt(srcSize, 10)+".exe"), nil
}

// windowsCacheDir resolves the directory under which size-versioned
// bridge cache .exes live. Picks the first Users/<u>/AppData/Local/
// Temp dir that's writable, falling back to Windows/Temp. Cached
// process-wide via sync.Once.
//
// Why sync.Once: the original implementation re-probed every call,
// racing concurrent backfill invocations on a shared
// `.observer-write-probe` marker (one invocation removed the marker
// before the other read it, the loser returned `no writable /mnt/c
// destination`). The marker filename is PID-suffixed so cross-
// process first-calls don't race either.
func windowsCacheDir() (string, error) {
	cachedDirOnce.Do(func() {
		cachedDir, cachedDirErr = resolveWindowsCacheDir()
	})
	return cachedDir, cachedDirErr
}

func resolveWindowsCacheDir() (string, error) {
	candidates := []string{}
	if entries, err := os.ReadDir("/mnt/c/Users"); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			if name == "All Users" || name == "Default" || name == "Default User" || name == "Public" || name == "WsiAccount" {
				continue
			}
			candidates = append(candidates,
				"/mnt/c/Users/"+name+"/AppData/Local/Temp/observer")
		}
	}
	candidates = append(candidates, "/mnt/c/Windows/Temp/observer")
	pidSuffix := strconv.Itoa(os.Getpid())
	for _, dir := range candidates {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			continue
		}
		// Probe write access by creating a PID-suffixed marker so
		// concurrent processes don't race on the same filename.
		marker := filepath.Join(dir, ".observer-write-probe."+pidSuffix)
		if err := os.WriteFile(marker, []byte("ok"), 0o600); err == nil {
			_ = os.Remove(marker)
			return dir, nil
		}
	}
	return "", errors.New("no writable /mnt/c destination for bridge cache")
}

// copyIfNewer copies src to dst when dst is missing or has a
// different size than src. Idempotent + safe to call repeatedly;
// a same-size cached copy is reused.
//
// Permission-denied gracefully degrades to the existing cached copy.
// On Windows-side /mnt/c destinations the daemon's powershell.exe
// invocations frequently overlap with Windows Defender's post-write
// AV scan or another in-flight bridge invocation holding the .exe
// open — both produce `open ... permission denied` on the write
// step. Previously this surfaced as a hard bridge failure, which the
// adapter logged + (worse) wrote into the unrecoverable cache,
// stranding the .pb until manually cleared. Falling back to the
// existing dst loses only whatever changed in the source since the
// last successful copy — the bridge's invocation contract
// (subcommand args, stdout payload, stderr endpoint hint) is stable
// across the recent flag additions, so an older cached binary still
// works for everything except the brand-new --endpoint/--csrf flags
// (those callers tolerate cache-miss gracefully). Strict improvement
// over the previous hard-fail behaviour.
func copyIfNewer(src, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}
	if dstInfo, err := os.Stat(dst); err == nil && dstInfo.Size() == srcInfo.Size() {
		return nil
	}
	body, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if writeErr := os.WriteFile(dst, body, 0o755); writeErr != nil { //nolint:gosec // executable; needs +x
		if _, statErr := os.Stat(dst); statErr == nil {
			// An older cached copy exists and the overwrite failed
			// (likely AV/lock race). Use what's there rather than
			// failing the whole invocation. The lock will release on
			// the next call; the next successful copyIfNewer brings
			// the cache forward.
			return nil
		}
		return writeErr
	}
	return nil
}

// ErrBridgeNotInstalled is returned by locateBridgeBinary when
// the Windows-side helper isn't found. Callers should surface it
// as an installation hint rather than an internal error.
var ErrBridgeNotInstalled = errors.New("antigravity-bridge.exe not found — run `make build` to compile it, or set OBSERVER_ANTIGRAVITY_BRIDGE")

// ErrBridgeTransient marks bridge failures that are environmental
// rather than per-file — e.g. PowerShell missing from PATH, the
// /mnt/c cache copy hitting a Windows-side file lock or anti-virus
// scan (`permission denied` on the destination .exe), the Windows-
// side language_server not running yet. Wrapping the underlying
// error with `fmt.Errorf("...: %w", ErrBridgeTransient)` lets the
// adapter's recovery path distinguish these from per-file permanent
// failures (e.g. an unsupported encryption cipher) so the
// unrecoverable-cache marker isn't poisoned by races that will
// self-heal on the next poll tick.
//
// Why: the operator's c15725c6-dbe1-4747-bab5-b2c8f4755e28 case
// (2026-05-24) hit a bridge-copy `permission denied` once and the
// resulting unrecoverable marker stuck the file in place — every
// subsequent poll short-circuited via `lookupUnrecoverable` with the
// matching (size, mtime), so even after the AV lock released the
// file never re-entered the recovery path until either the .pb grew
// or the operator manually cleared the marker. Marking the error
// transient causes onUnrecoverableFailure to hold the cursor and
// retry on the next tick instead.
var ErrBridgeTransient = errors.New("antigravity-bridge: transient environment failure")

// locateBridgeBinary resolves the path to the bridge helper using
// the documented search order. Returns the first existing file or
// ErrBridgeNotInstalled.
func locateBridgeBinary() (string, error) {
	if env := strings.TrimSpace(os.Getenv("OBSERVER_ANTIGRAVITY_BRIDGE")); env != "" {
		if _, err := os.Stat(env); err == nil {
			return env, nil
		}
	}
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), bridgeBinaryName)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		candidate := filepath.Join(cwd, "bin", bridgeBinaryName)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", ErrBridgeNotInstalled
}

// wslPathToWindows converts /mnt/<drive>/path/to/file.exe to
// <DRIVE>:\path\to\file.exe. Returns "" for paths not under /mnt.
func wslPathToWindows(p string) string {
	if !strings.HasPrefix(p, "/mnt/") || len(p) < 7 {
		return ""
	}
	drive := strings.ToUpper(string(p[5]))
	rest := strings.ReplaceAll(p[6:], "/", `\`)
	if !strings.HasPrefix(rest, `\`) {
		rest = `\` + rest
	}
	return drive + ":" + rest
}
