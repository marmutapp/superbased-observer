package diag

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	adapterdefaults "github.com/marmutapp/superbased-observer/internal/adapter/defaults"
	"github.com/marmutapp/superbased-observer/internal/integration"
	"github.com/marmutapp/superbased-observer/internal/models"
)

// transcriptReader is the diag-local mirror of handoffsvc.TranscriptReader
// (svc.go): an adapter that can re-read a completed session's message
// stream from the tool's own on-disk files. Declared here by STRUCTURE so
// the doctor probe never imports internal/handoffsvc or internal/store —
// diag stays standalone (same discipline the other checks keep: raw SQL,
// no store import), and dispatch is on capability shape, never tool name.
type transcriptReader interface {
	ReadTranscript(ctx context.Context, sess models.Session, sourceHints []string) ([]models.TranscriptMessage, error)
}

// handoffProbeRecentDays bounds how old the latest session may be and
// still be worth a live readability probe. Older than this, the tool's
// own files may legitimately have rotated away — reporting that as a
// reader fault would be a false alarm — so we report the declared
// capability without a live read.
const handoffProbeRecentDays = 30

// handoffProbeTimeout time-boxes each per-tool transcript read. `observer
// doctor` is interactive; a wedged reader must not hang the run.
const handoffProbeTimeout = 2 * time.Second

// handoffProbeMaxSessions bounds how many recent sessions the probe tries
// before reporting. Probing only the single latest session is fragile: a
// tool's own store legitimately rotates or prunes old sessions, so the
// most-recent row in our DB can point at a session whose transcript is
// already gone (e.g. a foreign-mount install whose store was replaced) —
// a false "held no messages" WARN even though the reader delivers fine on
// the next session down. Trying a small window and reporting the first
// that reads makes the probe robust to a rotated-away or genuinely-empty
// latest session, while staying cheap: each read is time-boxed and the
// loop stops at the first success.
const handoffProbeMaxSessions = 3

// checkHandoffReaders reports the session-handoff readiness of every
// registered adapter (plan §15 P4): the declared transcript tier + the
// grounded delivery lanes, plus — where the tool has a recent session in
// the DB and its adapter implements the reader — a lightweight, read-only,
// time-boxed readability probe against that latest session. A tool with no
// transcript capability says so honestly ("metadata handover only"); a
// tool classified readable whose latest session can't be re-read is a WARN
// (the reader is declared but not delivering), never a fabricated OK.
//
// The probe stays inside diag's existing import graph (internal/adapter,
// adapter/defaults, integration are already imported by checkAdapters);
// the reader is dispatched through a diag-local structural interface, and
// the "recent session" lookup is raw SQL, so no store/handoffsvc import is
// pulled in.
func checkHandoffReaders(ctx context.Context, database *sql.DB) Check {
	worst := StatusOK
	bump := func(s Status) {
		if s > worst {
			worst = s
		}
	}

	type row struct {
		name string
		line string
	}
	var rows []row
	var readable, probed, gapped int

	for _, a := range adapterdefaults.Adapters() {
		name := a.Name()
		ic, _ := integration.For(name)
		h := ic.Handoff

		// Actions-only tools carry metadata only — the honest floor, not a
		// fault. Report and move on (no live probe possible).
		if h.Transcript == integration.TranscriptActionsOnly {
			rows = append(rows, row{name, name + ": metadata handover only (no transcript reader) — lanes " + lanesSummary(h)})
			continue
		}
		readable++

		reader, hasReader := a.(transcriptReader)
		base := fmt.Sprintf("%s: transcript=%s, lanes %s", name, string(h.Transcript), lanesSummary(h))
		if h.Note != "" {
			base += " (" + h.Note + ")"
		}

		if !hasReader {
			// Registry claims a readable transcript but the adapter exposes
			// no reader yet — an honest classified-but-pending gap.
			gapped++
			bump(StatusWarn)
			rows = append(rows, row{name, base + " — classified readable but adapter implements no reader yet"})
			continue
		}

		// Live readability probe: try a small window of the most recent
		// sessions and report the first that reads. Robust to a latest
		// session whose transcript the tool has since rotated away.
		cands, err := recentSessionsToProbe(ctx, database, name, handoffProbeMaxSessions)
		if err != nil {
			bump(StatusWarn)
			rows = append(rows, row{name, base + " — could not query recent sessions: " + err.Error()})
			continue
		}
		if len(cands) == 0 {
			rows = append(rows, row{name, base + " — reader present, no recent session to probe"})
			continue
		}

		probed++
		outcome, msgs, chosen, lastErr := probeCandidates(ctx, cands, handoffProbeTimeout,
			func(pctx context.Context, id string, hints []string) ([]models.TranscriptMessage, error) {
				return reader.ReadTranscript(pctx, models.Session{ID: id, Tool: name}, hints)
			})
		age := shortSessionAge(chosen.started)
		switch outcome {
		case probeReadOK:
			rows = append(rows, row{name, base + fmt.Sprintf(" — read OK (%d msgs, session %s)", msgs, age)})
		case probeAllUnreadable:
			gapped++
			bump(StatusWarn)
			rows = append(rows, row{name, base + fmt.Sprintf(" — %s unreadable (%v)", probedCountPhrase(len(cands), age), lastErr)})
		default: // probeAllEmpty
			gapped++
			bump(StatusWarn)
			rows = append(rows, row{name, base + fmt.Sprintf(" — %s read but held no messages", probedCountPhrase(len(cands), age))})
		}
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
	details := make([]string, 0, len(rows))
	for _, r := range rows {
		details = append(details, r.line)
	}

	msg := fmt.Sprintf("%d adapter(s) with readable transcripts, %d probed live, %d gap(s)", readable, probed, gapped)
	if readable == 0 {
		msg = "no adapters declare a readable transcript"
	}
	return Check{Name: "handoff readers", Status: worst, Message: msg, Details: details}
}

// lanesSummary renders a handoff capability's grounded delivery lanes
// (always including the universal file lane) as a compact string.
func lanesSummary(h integration.HandoffCapability) string {
	lanes := h.Lanes()
	parts := make([]string, 0, len(lanes))
	for _, l := range lanes {
		parts = append(parts, string(l))
	}
	return strings.Join(parts, "/")
}

// probeCandidate is one recent session the handoff probe may re-read: its
// id, when it started (for the age hint) and the source-file hints we
// recorded for it, so the reader can open the exact store the session was
// captured from.
type probeCandidate struct {
	id      string
	started time.Time
	hints   []string
}

// probeOutcome is the result of a multi-session readability probe.
type probeOutcome int

const (
	probeReadOK        probeOutcome = iota // a candidate yielded messages
	probeAllEmpty                          // every candidate read cleanly but empty
	probeAllUnreadable                     // every candidate returned an error
)

// probeCandidates reads recent sessions in order via read, stopping at the
// first that yields messages. It is the pure retry core of the handoff
// probe (no SQL, no adapter registry): callers inject the actual reader as
// a closure so the retry logic is unit-testable. On success it returns the
// message count and the session that read; otherwise the count is 0 and
// chosen is the latest (first) candidate for the age hint. lastErr carries
// the final read error seen, populated for the all-unreadable case.
func probeCandidates(ctx context.Context, cands []probeCandidate, timeout time.Duration,
	read func(ctx context.Context, id string, hints []string) ([]models.TranscriptMessage, error),
) (outcome probeOutcome, msgs int, chosen probeCandidate, lastErr error) {
	if len(cands) == 0 {
		return probeAllEmpty, 0, probeCandidate{}, nil
	}
	readClean := false
	for _, c := range cands {
		pctx, cancel := context.WithTimeout(ctx, timeout)
		got, err := read(pctx, c.id, c.hints)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		readClean = true
		if len(got) > 0 {
			return probeReadOK, len(got), c, nil
		}
	}
	if readClean {
		return probeAllEmpty, 0, cands[0], lastErr
	}
	return probeAllUnreadable, 0, cands[0], lastErr
}

// probedCountPhrase renders how many sessions the probe tried, anchored to
// the latest one's age, for the empty/unreadable WARN lines.
func probedCountPhrase(n int, age string) string {
	if n <= 1 {
		return "latest session " + age
	}
	return fmt.Sprintf("%d recent sessions (latest %s)", n, age)
}

// recentSessionsToProbe returns up to limit of the most recent sessions
// for tool that started within handoffProbeRecentDays, newest first, each
// annotated with its recorded source-file hints. Raw SQL keeps diag
// standalone; datetime() bridges the RFC3339 / RFC3339Nano stamp formats
// the corpus mixes.
func recentSessionsToProbe(ctx context.Context, database *sql.DB, tool string, limit int) ([]probeCandidate, error) {
	cutoff := time.Now().UTC().Add(-handoffProbeRecentDays * 24 * time.Hour)
	rows, err := database.QueryContext(ctx,
		`SELECT id, started_at FROM sessions
		  WHERE tool = ? AND datetime(started_at) >= datetime(?)
		  ORDER BY started_at DESC, id DESC LIMIT ?`,
		tool, cutoff.Format(time.RFC3339Nano), limit)
	if err != nil {
		return nil, err
	}
	var cands []probeCandidate
	for rows.Next() {
		var id, startedStr string
		if err := rows.Scan(&id, &startedStr); err != nil {
			rows.Close()
			return nil, err
		}
		cands = append(cands, probeCandidate{id: id, started: parseSessionStamp(startedStr)})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	// Attach recorded source_file hints per session so the reader opens the
	// exact store the session was captured from — load-bearing for foreign-
	// mount installs where several stores of the same tool coexist.
	for i := range cands {
		cands[i].hints = sessionSourceHints(ctx, database, cands[i].id)
	}
	return cands, nil
}

// sessionSourceHints returns the distinct non-empty source_file paths
// recorded for a session, used as ReadTranscript hints. A query error
// yields no hints (the reader then falls back to its default roots) — the
// probe must never fail on a missing hint.
func sessionSourceHints(ctx context.Context, database *sql.DB, sessionID string) []string {
	rows, err := database.QueryContext(ctx,
		`SELECT DISTINCT source_file FROM actions
		  WHERE session_id = ? AND source_file IS NOT NULL AND source_file <> ''`,
		sessionID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var hints []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return hints
		}
		hints = append(hints, s)
	}
	return hints
}

// parseSessionStamp best-effort parses a stored session timestamp across
// the formats the corpus mixes; a zero time is acceptable (only used for a
// human-readable age hint).
func parseSessionStamp(s string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// shortSessionAge renders a coarse "how long ago" hint for a probed
// session, or "" when the stamp didn't parse.
func shortSessionAge(t time.Time) string {
	if t.IsZero() {
		return "(recent)"
	}
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
