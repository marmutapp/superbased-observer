package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Org routing-policy cache seam (§R19.1; migration 043). ONE OWNER:
// the table is written exclusively here. NODE-LOCAL: never pushed —
// pinned by the privacy sentinel.

// OrgRoutingPolicyRow is the cached document + the TOFU-pinned key.
type OrgRoutingPolicyRow struct {
	Version      int64
	Body         string
	BodyHash     string
	Signature    string
	ServerPubkey string
	ReceivedAt   time.Time
}

// UpsertOrgRoutingPolicy replaces the single-row cache.
func (s *Store) UpsertOrgRoutingPolicy(ctx context.Context, row OrgRoutingPolicyRow) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO org_routing_policies (id, version, body, body_hash, signature, server_pubkey, received_at)
		VALUES (1, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		  version = excluded.version, body = excluded.body,
		  body_hash = excluded.body_hash, signature = excluded.signature,
		  server_pubkey = excluded.server_pubkey, received_at = excluded.received_at`,
		row.Version, row.Body, row.BodyHash, row.Signature, row.ServerPubkey, timestamp(row.ReceivedAt))
	if err != nil {
		return fmt.Errorf("store.UpsertOrgRoutingPolicy: %w", err)
	}
	return nil
}

// GetOrgRoutingPolicy returns the cached policy. ok=false when absent.
func (s *Store) GetOrgRoutingPolicy(ctx context.Context) (OrgRoutingPolicyRow, bool, error) {
	var (
		row OrgRoutingPolicyRow
		ts  string
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT version, body, body_hash, signature, server_pubkey, received_at
		FROM org_routing_policies WHERE id = 1`).
		Scan(&row.Version, &row.Body, &row.BodyHash, &row.Signature, &row.ServerPubkey, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return OrgRoutingPolicyRow{}, false, nil
	}
	if err != nil {
		return OrgRoutingPolicyRow{}, false, fmt.Errorf("store.GetOrgRoutingPolicy: %w", err)
	}
	row.ReceivedAt = parseStamp(ts)
	return row, true, nil
}
