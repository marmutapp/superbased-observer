-- 073_workspace_layouts.sql — server-side persistence for the Terminal
-- Workspace dock-grid arrangement (docs/plans/terminal-dock-grid-design
-- -2026-07-20.md, operator decision 2026-07-21: server-side, not localStorage,
-- so the layout is shared across the operator's devices).
--
-- One row per named workspace ("default" until named workspaces ship). The
-- layout_json blob carries ONLY presentation state: per-breakpoint grid cells
-- ({i,x,y,w,h} keyed by terminal handle / session id) and tray membership —
-- never PTY content, never credentials. Handles are opaque short-lived tokens;
-- session ids already live in this database.
--
-- NODE-LOCAL: never pushed. workspace_layouts must NOT appear in
-- internal/store/orgpush.go::SelectUnpushedSince (privacy sentinel:
-- tests/invariant/privacy_test.go) — it is operator UI state, meaningless and
-- unwanted off-node. No paired orgserver migration, by design.

CREATE TABLE IF NOT EXISTS workspace_layouts (
    name        TEXT PRIMARY KEY,           -- workspace name ("default")
    layout_json TEXT NOT NULL,              -- presentation-state JSON blob
    updated_at  TEXT NOT NULL               -- RFC3339 UTC of the last save
);
