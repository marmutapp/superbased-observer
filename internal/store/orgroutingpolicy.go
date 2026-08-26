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
	Version  int64
	Body     string
	BodyHash string
	// Signature is the v1 (body-only) signature, as served.
	Signature string
	// SignatureV2 is the domain-separated, VERSION-BOUND signature
	// (orgcontract.RoutingPolicySigningMessageV2), empty when the serving
	// org server predates it (migration 085 / server 078; docs/security.md
	// ROUTING-SIG-1). Cached for the same reason Signature is: so the node
	// can re-verify the document it is about to compose WITHOUT a live
	// server — and, since v2 exists, re-verify it against the bytes that
	// actually bind the version.
	SignatureV2  string
	ServerPubkey string
	ReceivedAt   time.Time
}

// UpsertOrgRoutingPolicy replaces the single-row cache.
func (s *Store) UpsertOrgRoutingPolicy(ctx context.Context, row OrgRoutingPolicyRow) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO org_routing_policies (id, version, body, body_hash, signature, signature_v2, server_pubkey, received_at)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		  version = excluded.version, body = excluded.body,
		  body_hash = excluded.body_hash, signature = excluded.signature,
		  signature_v2 = excluded.signature_v2,
		  server_pubkey = excluded.server_pubkey, received_at = excluded.received_at`,
		row.Version, row.Body, row.BodyHash, row.Signature, row.SignatureV2, row.ServerPubkey, timestamp(row.ReceivedAt))
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
		SELECT version, body, body_hash, signature, signature_v2, server_pubkey, received_at
		FROM org_routing_policies WHERE id = 1`).
		Scan(&row.Version, &row.Body, &row.BodyHash, &row.Signature, &row.SignatureV2, &row.ServerPubkey, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return OrgRoutingPolicyRow{}, false, nil
	}
	if err != nil {
		return OrgRoutingPolicyRow{}, false, fmt.Errorf("store.GetOrgRoutingPolicy: %w", err)
	}
	row.ReceivedAt = parseStamp(ts)
	return row, true, nil
}
