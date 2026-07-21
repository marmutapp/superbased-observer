//go:build !unix

package attachsock

// listenLock is a no-op HELD lock on platforms without flock. Session-attach is
// Linux/WSL-only in v1 (design §6 decision 3), so the AF_UNIX listen path is
// not exercised here; the ListenSocket stale-socket-steal guard still applies.
type listenLock struct{}

// release does nothing on platforms without flock.
func (l *listenLock) release() {}

// acquireListenLock is a no-op on platforms without flock: it always succeeds
// with a no-op held lock and never reports errLockHeld.
func acquireListenLock(string) (*listenLock, error) {
	return &listenLock{}, nil
}
