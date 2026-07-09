package main

import (
	"context"
	"os"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/git"
	"github.com/marmutapp/superbased-observer/internal/handoff"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// handoffHookPayload returns the SessionStart additionalContext for a
// hook-armed session handoff (plan §10 inject_hook), or "" when nothing is
// armed / the feature is off / anything fails. It mirrors
// advisorSessionDigest: one config load + one best-effort point-read
// against the daemon's DB, and EVERY failure path degrades to "" so the
// hook's approve reply is never blocked (fail-open — a hook must never
// break the session; watchdog exits 0, not 2).
//
// The claim is one-shot and race-safe (store.ClaimArmedHandoffHook), so a
// handover fires for exactly the next matching session and never again. The
// doc content itself lives only on disk (delivery_ref); we read it here (a
// cmd-layer os.ReadFile is allowed) and budget it to [handoff].hook_max_bytes
// via handoff.HookPayload before returning it — Phase 0 D-P0.2's 8KB cap.
func handoffHookPayload(ctx context.Context, cwd string) string {
	cfg, err := config.Load(config.LoadOptions{})
	if err != nil || !cfg.Handoff.Enabled {
		return ""
	}
	database, err := db.Open(ctx, db.Options{Path: cfg.Observer.DBPath, SkipIntegrityCheck: true})
	if err != nil {
		return ""
	}
	defer database.Close()

	// Best-effort project match: an armed handoff is claimed only for its
	// own project root. git.Resolve maps the session cwd to the working-tree
	// root (which is what the store recorded); a non-git cwd falls back to
	// the cwd itself.
	projectRoot := cwd
	if info, gerr := git.Resolve(cwd); gerr == nil {
		projectRoot = info.Root
	}

	docPath, ok, err := store.New(database).ClaimArmedHandoffHook(ctx, models.ToolClaudeCode, projectRoot, time.Now())
	if err != nil || !ok || docPath == "" {
		return ""
	}
	body, err := os.ReadFile(docPath)
	if err != nil {
		return ""
	}
	return handoff.HookPayload(string(body), docPath, hookMaxBytesOrDefault(cfg.Handoff.HookMaxBytes))
}

// composeSessionStartContext builds the additionalContext for a claude-code
// SessionStart. A hook-armed handoff (an explicit user action via
// `observer handoff --deliver hook`) takes precedence and consumes the whole
// hook budget — it is already budgeted to hook_max_bytes — so the advisor
// digest is skipped for that one session. Otherwise the advisor digest is
// returned as before. Both halves fail open to "".
func composeSessionStartContext(ctx context.Context, cwd string) string {
	if h := handoffHookPayload(ctx, cwd); h != "" {
		return h
	}
	return advisorSessionDigest(ctx)
}

func hookMaxBytesOrDefault(v int) int {
	if v <= 0 {
		return 8192
	}
	return v
}
