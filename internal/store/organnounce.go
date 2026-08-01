package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Org announcement cache seam (rail R3 of
// docs/plans/dashboard-announcements-banner-plan-2026-07-31.md §4;
// migration 076). ONE OWNER: the table is written exclusively here.
// NODE-LOCAL: never pushed — pinned by the privacy sentinel, because
// pushing received announcements back would be a read receipt (plan §6
// rules acknowledgment wires out as telemetry).
//
// Deliberately a mirror of orgroutingpolicy.go: same single-row cache,
// same TOFU-pinned key column, same upsert. Two small files that each
// own one table beat one file that owns two.

// OrgAnnouncementRow is the cached document + the TOFU-pinned key.
// An EMPTY Body is the retraction — a real, verified, version-bumped
// document meaning "show nothing" — so a caller must distinguish
// "no row" (never enrolled / never published) from "row with empty
// body" (explicitly retracted).
type OrgAnnouncementRow struct {
	Version      int64
	Body         string
	BodyHash     string
	Signature    string
	ServerPubkey string
	ReceivedAt   time.Time
}

// UpsertOrgAnnouncement replaces the single-row cache.
func (s *Store) UpsertOrgAnnouncement(ctx context.Context, row OrgAnnouncementRow) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO org_announcements (id, version, body, body_hash, signature, server_pubkey, received_at)
		VALUES (1, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		  version = excluded.version, body = excluded.body,
		  body_hash = excluded.body_hash, signature = excluded.signature,
		  server_pubkey = excluded.server_pubkey, received_at = excluded.received_at`,
		row.Version, row.Body, row.BodyHash, row.Signature, row.ServerPubkey, timestamp(row.ReceivedAt))
	if err != nil {
		return fmt.Errorf("store.UpsertOrgAnnouncement: %w", err)
	}
	return nil
}

// GetOrgAnnouncement returns the cached announcement document.
// ok=false when absent — which is the state of every solo install, so
// callers degrade silently rather than treating it as a fault.
func (s *Store) GetOrgAnnouncement(ctx context.Context) (OrgAnnouncementRow, bool, error) {
	var (
		row OrgAnnouncementRow
		ts  string
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT version, body, body_hash, signature, server_pubkey, received_at
		FROM org_announcements WHERE id = 1`).
		Scan(&row.Version, &row.Body, &row.BodyHash, &row.Signature, &row.ServerPubkey, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return OrgAnnouncementRow{}, false, nil
	}
	if err != nil {
		return OrgAnnouncementRow{}, false, fmt.Errorf("store.GetOrgAnnouncement: %w", err)
	}
	row.ReceivedAt = parseStamp(ts)
	return row, true, nil
}
