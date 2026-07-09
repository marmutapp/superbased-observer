package main

import (
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/store"
)

func tableRun(key, parent string, pid int, exe, src, conf string, exited bool, code int, durMs int64, actionID int64, turn int) store.ProcessRunRow {
	r := store.ProcessRunRow{
		ProcessKey:            key,
		ParentProcessKey:      parent,
		PID:                   pid,
		ExeBasename:           exe,
		AttributionSource:     src,
		AttributionConfidence: conf,
		Exited:                exited,
		ExitCode:              code,
		DurationMs:            durMs,
		StartedAt:             time.Unix(1_700_000_000+int64(pid), 0).UTC(),
	}
	if actionID > 0 {
		r.ActionID = &actionID
		r.TurnIndex = &turn
	}
	return r
}

func TestRenderProcessTree(t *testing.T) {
	t.Parallel()
	runs := []store.ProcessRunRow{
		tableRun("k1", "", 100, "claude", "bridge", "high", true, 0, 12300, 0, 0),
		tableRun("k2", "k1", 200, "bash", "inherited", "high", true, 0, 8100, 5, 4),
		tableRun("k3", "k2", 300, "npm", "action_correlation", "medium", false, 0, 0, 5, 4),
	}
	cmds := map[int64]string{5: "npm test"}

	var b strings.Builder
	renderProcessTree(&b, "sess-1", runs, cmds)
	out := b.String()

	for _, want := range []string{
		"session sess-1 — 3 process runs",
		"claude (pid 100) · bridge/high · exit 0 (12.3s)",
		"bash (pid 200) · inherited/high",
		"↳ npm test (turn 4)", // the spawning command surfaces on the tree
		"npm (pid 300) · action_correlation/medium · running",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("tree output missing %q.\n--- got ---\n%s", want, out)
		}
	}
	// Tree structure: the root is at depth 0, children indented under it.
	if !strings.Contains(out, "└─ claude") {
		t.Errorf("root not rendered as a tree node:\n%s", out)
	}
}

func TestRenderProcessTreeEmpty(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	renderProcessTree(&b, "sess-x", nil, nil)
	if !strings.Contains(b.String(), "no process runs captured") {
		t.Errorf("empty tree message missing: %q", b.String())
	}
}

func TestRuntimeAndAttribLabels(t *testing.T) {
	t.Parallel()
	running := store.ProcessRunRow{Exited: false}
	if runtimeLabel(running) != "running" {
		t.Errorf("running label = %q", runtimeLabel(running))
	}
	sig := store.ProcessRunRow{Exited: true, ExitSignal: 9, DurationMs: 1500}
	if got := runtimeLabel(sig); !strings.HasPrefix(got, "sig 9") {
		t.Errorf("signal label = %q", got)
	}
	if attribLabel(store.ProcessRunRow{AttributionSource: "none"}) != "unattributed" {
		t.Error("none source should read 'unattributed'")
	}
}
