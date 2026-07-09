package handoffsvc

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/handoff"
	"github.com/marmutapp/superbased-observer/internal/models"
)

// DefaultLinkWindow bounds how far back the linker looks for unlinked
// handoffs. Linking a weeks-old handoff is meaningless — a target session
// that boots the handover always appears within minutes to hours — so the
// sweep only considers recent rows.
const DefaultLinkWindow = 7 * 24 * time.Hour

// LinkTargetSessions is the best-effort P3 target-session linker (plan §10
// post-injection accounting). For every delivered handoff in the window
// that still has no target_session_id, it lists candidate sessions of the
// target tool in the same project started after the handoff, re-reads each
// candidate's transcript via the SAME shared reader dispatch used for the
// source session, and stamps target_session_id on the FIRST candidate
// whose transcript carries the handoff's short-id marker.
//
// Everything is best-effort: a candidate whose transcript can't be read is
// skipped silently, an error on one handoff never aborts the sweep, and
// the guarded UPDATE (store.LinkTargetSession) makes a link idempotent.
// Returns the number of handoffs newly linked. A zero window uses
// DefaultLinkWindow.
//
// Note on lag: because the marker only becomes observable once the target
// session's first turn has been captured to disk, a freshly-armed handoff
// links on a LATER sweep, not the instant the target session starts.
func LinkTargetSessions(ctx context.Context, deps Deps, window time.Duration) (int, error) {
	if window <= 0 {
		window = DefaultLinkWindow
	}
	now := time.Now
	if deps.Now != nil {
		now = deps.Now
	}
	since := now().Add(-window)

	unlinked, err := deps.Store.ListUnlinkedHandoffs(ctx, since)
	if err != nil {
		return 0, err
	}

	linked := 0
	for _, h := range unlinked {
		if ctx.Err() != nil {
			return linked, ctx.Err()
		}
		// The marker's short-id comes from the stored short_id column
		// (migration 057) — so even a handoff written to a custom --out
		// path is linkable. Pre-057 rows have no stored id; they fall back
		// to recovering it from the delivered doc's file name (delivery_ref,
		// e.g. .../HANDOFF-abcd1234.md). A row with neither (a pre-057
		// custom --out, or a non-file lane with no doc) can't be linked;
		// skip it.
		shortID := h.ShortID
		if shortID == "" {
			shortID = shortIDFromRef(h.DeliveryRef)
		}
		if shortID == "" {
			continue
		}
		// Target tool must be a real tool for the candidate query to scope;
		// "unspecified" (a handoff created without --to) can't be linked.
		if h.TargetTool == "" || h.TargetTool == "unspecified" {
			continue
		}
		cands, err := deps.Store.CandidateTargetSessions(ctx, h.TargetTool, h.ProjectRoot, h.CreatedAt, 10)
		if err != nil {
			continue // best-effort: one handoff's failure never aborts the sweep
		}
		for _, c := range cands {
			if c.SessionID == h.SourceSessionID {
				continue // a fork never links back to its own source
			}
			// Pass the candidate's recorded source hints so the reader
			// opens the exact store it was captured from — nil hints can
			// fall through to the wrong store for foreign-mount sessions.
			msgs, _, _ := readSessionTranscript(ctx, deps.Adapters, models.Session{ID: c.SessionID, Tool: c.Tool}, c.Hints, false)
			if len(msgs) == 0 {
				continue
			}
			if handoff.ContainsMarker(msgs, shortID) {
				if err := deps.Store.LinkTargetSession(ctx, h.ID, c.SessionID); err == nil {
					linked++
				}
				break // first marker-carrying candidate wins
			}
		}
	}
	return linked, nil
}

// shortIDFromRef recovers a handoff's short-id from the delivered doc's
// file path. The default doc name is "HANDOFF-<shortid>.md" (plan §10 /
// [handoff] file_name); the short-id is the token between the last
// "HANDOFF-" and the ".md" extension. Returns "" when the path doesn't
// follow the convention (custom --out, non-file lanes) — the linker
// treats that handoff as unlinkable, by design.
func shortIDFromRef(ref string) string {
	if ref == "" {
		return ""
	}
	base := filepath.Base(ref)
	base = strings.TrimSuffix(base, ".md")
	const marker = "HANDOFF-"
	if i := strings.LastIndex(base, marker); i >= 0 {
		return base[i+len(marker):]
	}
	return ""
}
