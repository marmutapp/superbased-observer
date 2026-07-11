package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/intelligence/advisor"
)

// advisorSessionDigest returns the ≤400-token advisory digest for
// session-start injection, or "" when disabled / unavailable. One config
// load + one DB point-read; every failure path degrades to "" (P6) so the
// hook's approve reply is never blocked.
func advisorSessionDigest(ctx context.Context) string {
	cfg, err := config.Load(config.LoadOptions{})
	if err != nil || !cfg.Advisor.Enabled || !cfg.Advisor.SessionDigest {
		return ""
	}
	database, err := db.Open(ctx, db.Options{Path: cfg.Observer.DBPath, SkipIntegrityCheck: true})
	if err != nil {
		return ""
	}
	defer database.Close()
	rep, ok, err := advisor.LoadDigest(ctx, database)
	if err != nil || !ok || len(rep.Suggestions) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Observer advisor — top cost-saving notes for this machine (")
	fmt.Fprintf(&b, "last %dd, ~$%.0f avoidable):\n", rep.WindowDays, rep.TotalSavingsUSD)
	for i, s := range rep.Suggestions {
		if i >= 3 {
			break
		}
		fmt.Fprintf(&b, "- %s — %s\n", s.Title, s.Nudge)
	}
	out := b.String()
	// Hard cap ≈400 tokens (~1,600 chars) so the injection can never
	// meaningfully tax the session it's trying to save money for.
	if len(out) > 1600 {
		out = out[:1600] + "…"
	}
	return out
}
