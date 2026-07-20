//go:build unix

package attachsock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// ResumeClaim is a HELD advisory (flock) lock on a per-session resume lock file
// inside the attach dir. It is the DURABLE, cross-process half of the
// double-spawn guard (review finding H3): a bare `observer <tool> --resume <id>`
// run during daemon downtime (the deliberate default-on fallback) is invisible
// to the returned daemon's in-memory live-session view, so without this claim
// the original client's auto-resume could duplicate the transcript. BOTH the
// bare launcher and the daemon's attach-resume spawn take the claim, so a resume
// in flight in one process is seen by the other.
//
// The OS releases an flock automatically when the last fd of the open file
// description is closed — including on process death — so a crashed holder's
// claim self-clears with no stale-lock cleanup (pinned by the _unix test).
type ResumeClaim struct {
	f    *os.File
	once sync.Once
}

// Release unlocks and closes the claim's lock file. Idempotent — safe to call
// from both the run-lifecycle release and a defensive failure-path defer.
func (c *ResumeClaim) Release() {
	if c == nil {
		return
	}
	c.once.Do(func() {
		if c.f != nil {
			_ = syscall.Flock(int(c.f.Fd()), syscall.LOCK_UN)
			_ = c.f.Close()
			c.f = nil
		}
	})
}

// AcquireResumeClaim takes a NON-BLOCKING exclusive advisory (flock) lock on
// <dir>/resume-<sanitized(sessionID)>.lock, creating the file 0600 inside the
// owner-only (0700) attach dir. It mirrors the listen-lock discipline
// (listenlock_unix.go): a held flock is a probe-free positive signal that
// another process owns the resume.
//
// Returns (claim, true, nil) on success — the caller MUST Release() when the
// resumed run ends. Returns (nil, false, nil) when another live holder owns the
// claim (the conflict signal — not an error). Returns (nil, false, err) only on
// an unexpected filesystem error.
func AcquireResumeClaim(dir, sessionID string) (*ResumeClaim, bool, error) {
	if dir == "" || sessionID == "" {
		return nil, false, fmt.Errorf("attachsock: resume claim requires a dir and session id")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, false, fmt.Errorf("attachsock: resume claim dir %q: %w", dir, err)
	}
	path := filepath.Join(dir, "resume-"+sanitizeSessionID(sessionID)+".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("attachsock: open resume claim %q: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil // another process holds the resume — conflict
		}
		return nil, false, fmt.Errorf("attachsock: flock resume claim %q: %w", path, err)
	}
	return &ResumeClaim{f: f}, true, nil
}

// sanitizeSessionID reduces a session id to a filesystem-safe lock-filename
// token: every rune outside [A-Za-z0-9._-] becomes '_'. Session ids are
// UUID-shaped in practice; this is defence against an unexpected value smuggling
// a path separator into the lock file name.
func sanitizeSessionID(id string) string {
	var b strings.Builder
	b.Grow(len(id))
	for _, r := range id {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}
