package store

// workspace.go — the ONE store seam for the Terminal Workspace dock-grid
// layout (workspace_layouts, migration 073; NODE-LOCAL, never pushed —
// pinned by tests/invariant/privacy_test.go).
//
// The layout blob is presentation state only (per-breakpoint grid cells keyed
// by terminal handle / session id + tray membership). The store validates it
// is JSON and bounds its size; it never interprets the contents — the grid is
// a dashboard concern, the store just persists the arrangement.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// MaxWorkspaceLayoutBytes bounds a saved layout blob. A real layout is a few
// KiB; 256 KiB leaves generous headroom while keeping a buggy/hostile caller
// from growing the node DB unboundedly through this seam.
const MaxWorkspaceLayoutBytes = 256 * 1024

// ErrWorkspaceLayoutInvalid rejects a save that is not valid JSON or exceeds
// MaxWorkspaceLayoutBytes.
var ErrWorkspaceLayoutInvalid = errors.New("store: workspace layout must be valid JSON under the size bound")

// GetWorkspaceLayout returns the stored layout JSON for the named workspace.
// ok=false (no error) when the workspace has never been saved.
func (s *Store) GetWorkspaceLayout(ctx context.Context, name string) (layoutJSON string, ok bool, err error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT layout_json FROM workspace_layouts WHERE name = ?`, name)
	switch err := row.Scan(&layoutJSON); {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("store.GetWorkspaceLayout: %w", err)
	}
	return layoutJSON, true, nil
}

// SaveWorkspaceLayout upserts the named workspace's layout blob. The blob must
// be valid JSON and under MaxWorkspaceLayoutBytes (ErrWorkspaceLayoutInvalid
// otherwise) — the store never interprets it beyond that.
func (s *Store) SaveWorkspaceLayout(ctx context.Context, name, layoutJSON string) error {
	if name == "" {
		return fmt.Errorf("store.SaveWorkspaceLayout: empty workspace name")
	}
	if len(layoutJSON) > MaxWorkspaceLayoutBytes || !json.Valid([]byte(layoutJSON)) {
		return ErrWorkspaceLayoutInvalid
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO workspace_layouts (name, layout_json, updated_at)
		 VALUES (?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET
		   layout_json = excluded.layout_json,
		   updated_at  = excluded.updated_at`,
		name, layoutJSON, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("store.SaveWorkspaceLayout: %w", err)
	}
	return nil
}
