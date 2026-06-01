//go:build !linux

package pidbridge

import (
	"context"
	"time"
)

// ProcResolver on non-Linux is a stub. Resolve always reports a clean
// miss so the proxy falls back to a NULL session_id on api_turns.
type ProcResolver struct {
	store *Store
}

// NewProcResolver returns a stub resolver on non-Linux builds. procDir
// and cacheTTL are accepted for API parity but ignored.
func NewProcResolver(store *Store, _ string, _ time.Duration) *ProcResolver {
	return &ProcResolver{store: store}
}

// SetClock is a no-op on non-Linux.
func (r *ProcResolver) SetClock(func() time.Time) {}

// Resolve always reports a clean miss.
func (r *ProcResolver) Resolve(context.Context, string) (string, bool, error) {
	return "", false, nil
}
