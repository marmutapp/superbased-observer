package main

import (
	"context"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/processobs"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// launchseed.go — the launcher half of direct process attribution for
// `observer <tool>` launches (migration 086).
//
// THE GAP this closes: the session_pid_bridge seed has historically been
// written by exactly one producer per tool — the Claude Code SessionStart
// hook's ancestor walk, plus later codex/cursor/hermes seeds. Every other
// launched tool fell through to the medium-confidence lazy CorrelateCrossOS
// pass, which is why the session-detail process/network panels are blank for
// most tools (cmd/observer/terminal_pidseed.go states this verbatim for
// daemon-launched terminals; docs/audits/
// process-attribution-coverage-audit-2026-07-15.md root cause #1).
//
// A bridge row needs a REAL session id, and sessions are created by adapter
// ingestion of the tool's own storage — unknowable at spawn. So the launcher
// records the child pid in launch_seeds AFTER a successful Start (the only
// moment the pid is knowable) and that is ALL it does: the daemon's
// correlation sweep consumes the seed once a matching session is ingested,
// and the 1h expiry pass cleans up seeds whose launches never produced one.
//
// The launcher deliberately does NOT retract the seed on child exit (live-
// verified 2026-08-21: a headless grok run exits seconds after spawn — long
// before the 90s sweep tick — so an exit-retract deleted the seed before it
// could ever be consumed). A pending seed is not identity: it carries no
// session id and every reader treats it as a hint, so leaving one behind is
// harmless and bounded by expiry.
//
// Best-effort by contract: every failure degrades to the pre-existing lazy
// correlation path and is reported on the launcher's stderr — it must never
// block or fail the launch itself.

// launchSeedWriteTimeout bounds the launcher's DB work so a wedged DB can
// never stall a tool launch.
const launchSeedWriteTimeout = 3 * time.Second

// recordLaunchSeed inserts the launch_seeds row for a successfully started
// child. Fire-and-forget: failures are reported on warn (the launcher's
// stderr) and degrade to lazy correlation.
func recordLaunchSeed(dbPath, tool, dir string, pid int, warn interface{ Write([]byte) (int, error) }) {
	if dbPath == "" || pid <= 0 || tool == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), launchSeedWriteTimeout)
	defer cancel()
	database, err := db.Open(ctx, db.Options{Path: dbPath})
	if err != nil {
		launchSeedWarn(warn, tool, "open db", err)
		return
	}
	defer database.Close()
	if err := store.New(database).InsertLaunchSeed(ctx, processobs.LaunchSeed{
		PID:  pid,
		Tool: tool,
		CWD:  dir,
	}); err != nil {
		launchSeedWarn(warn, tool, "record launch seed", err)
	}
}

// launchSeedWarn reports a best-effort failure on the launcher's stderr in
// the same one-line shape the launchers already use.
func launchSeedWarn(w interface{ Write([]byte) (int, error) }, tool, what string, err error) {
	if w == nil {
		return
	}
	_, _ = w.Write([]byte("observer " + tool + ": " + what +
		" (process attribution falls back to lazy correlation): " + err.Error() + "\n"))
}
