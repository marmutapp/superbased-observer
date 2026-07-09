//go:build linux

package poll

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/processobs"
)

// deepEnricher implements [processobs.DeepEnricher] on Linux. It is invoked
// once per persisted NEW process (post-attribution), so the sensitive
// (environ) and expensive (executable hash) reads stay targeted — never run
// across the whole process table on every poll. Co-located with the poll
// backend because both are the Linux /proc readers; the enrichment itself is
// backend-agnostic (a future eBPF backend reuses it).
type deepEnricher struct {
	scrub        *processobs.FieldScrubber
	hashExe      bool
	maxHashBytes int64
}

// NewDeepEnricher builds the Linux /proc deep-enricher.
//
//   - scrub supplies the env-posture policy (allowlist, PATH-hash, secret-key
//     suppression — see FieldScrubber.EnvPosture). A nil scrub, or one with
//     EnvEnabled=false, disables environ capture.
//   - hashExe gates executable content hashing (spec §19 Q6, default off);
//     maxHashFileSizeMB caps the file size eligible for hashing (≤0 = no cap).
//
// Returns a non-nil [processobs.DeepEnricher]; the per-feature gating lives
// inside DeepEnrich so callers wire it unconditionally.
func NewDeepEnricher(scrub *processobs.FieldScrubber, hashExe bool, maxHashFileSizeMB int) processobs.DeepEnricher {
	var maxBytes int64
	if maxHashFileSizeMB > 0 {
		maxBytes = int64(maxHashFileSizeMB) * 1024 * 1024
	}
	return &deepEnricher{scrub: scrub, hashExe: hashExe, maxHashBytes: maxBytes}
}

// DeepEnrich reads the process environment for posture and (optionally) hashes
// the executable, mutating run in place. Best-effort and fail-open at every
// step: a process that has already exited, or a file/permission we lack,
// simply leaves the field empty — never an error.
func (d *deepEnricher) DeepEnrich(run *processobs.ProcessRun) {
	if run == nil || run.PID <= 0 {
		return
	}
	base := "/proc/" + strconv.Itoa(run.PID)

	// Environment posture: read the raw environ here and reduce it to the
	// allowlisted presence/hash map immediately, so a full environment value
	// never escapes this function (spec §8.1, §12.1).
	if d.scrub != nil && d.scrub.EnvEnabled {
		if raw, err := os.ReadFile(base + "/environ"); err == nil {
			if env := parseEnviron(raw); len(env) > 0 {
				if posture := d.scrub.EnvPosture(env); len(posture) > 0 {
					run.EnvPosture = posture
				}
			}
		}
	}

	// Executable content hash (off by default): /proc/<pid>/exe resolves to
	// the running binary even if it was moved/renamed since exec.
	if d.hashExe {
		if h := d.hashExecutable(base + "/exe"); h != "" {
			run.ExeHash = h
		}
	}
}

// hashExecutable returns the "sha256:"-prefixed hex digest of the file at
// path, or "" on any read error (gone process, deleted exe, permission) or
// when the file exceeds maxHashBytes. The size guard runs before the read so
// an oversized binary is skipped, not streamed.
func (d *deepEnricher) hashExecutable(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	if d.maxHashBytes > 0 {
		if fi, err := f.Stat(); err == nil && fi.Size() > d.maxHashBytes {
			return ""
		}
	}

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// parseEnviron splits a raw /proc/<pid>/environ blob (NUL-separated
// KEY=VALUE pairs) into a map. Entries without an '=', or with an empty key,
// are skipped; a later duplicate key wins, matching the kernel layout (last
// assignment effective).
func parseEnviron(raw []byte) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	out := make(map[string]string)
	for _, kv := range strings.Split(string(raw), "\x00") {
		if kv == "" {
			continue
		}
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 { // no '=', or '=' is the first byte (empty key)
			continue
		}
		out[kv[:eq]] = kv[eq+1:]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
