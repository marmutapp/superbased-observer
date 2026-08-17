package run

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// correlationIDBytes is the number of SHA-256 prefix bytes kept for a derived
// correlation id. 16 bytes render as 32 lowercase hex chars — the same shape as
// an OTel trace id, which is what sbo.run.trace_id looks like to a consumer.
const correlationIDBytes = 16

// CorrelationID derives a stable, opaque correlation id from a value that must
// NOT leave the node verbatim (a filesystem path, a project root, any
// user-identifying string).
//
// Why hash rather than drop: sbo.run.trace_id is classified
// ClassOperationalMetadata by the gateway classify tier
// (internal/orgserver/gateway/classify), so it is retained VERBATIM at every
// capture level — L0 included. A raw value there is an unconditional
// disclosure. Hashing keeps the field's only real job (correlating the runs of
// one logical subject across time) while disclosing nothing about the subject
// itself; it mirrors the node→org wire's established `*_hash` posture
// (internal/store/orgpush.go).
//
// The empty string maps to the empty string: an absent subject has no
// correlation id, and hashing "" would fabricate a shared id for every run that
// simply had no subject.
//
// Honesty note: SHA-256 over a low-entropy domain (filesystem paths) is
// enumerable by a party that can already guess candidate paths. It defeats
// bulk disclosure, not a targeted confirmation oracle — the same property, and
// the same accepted limit, as the wire's other `*_hash` columns.
func CorrelationID(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:correlationIDBytes])
}

// PathShaped reports whether v looks like a filesystem path and therefore must
// never be emitted verbatim as an operational scalar.
//
// The test is deliberately cheap and conservative: a path separator (POSIX or
// Windows) or a leading "~" (home-relative). A bare basename ("myproject") is
// indistinguishable from a legitimate opaque id and is NOT reported — a
// producer that knows its value is a path must hash it deliberately with
// CorrelationID rather than rely on this detector.
func PathShaped(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	if strings.HasPrefix(v, "~") {
		return true
	}
	return strings.ContainsAny(v, `/\`)
}

// SanitizeCorrelationID is the defence-in-depth backstop applied by the shaper
// to free-form correlation scalars: it returns v unchanged unless v is
// PathShaped, in which case it returns CorrelationID(v).
//
// It exists because the producers of these ids live outside this module (every
// cmd-side emit site), so the ONE shaping seam is the only place that can hold
// the class invariant for producers added later. Producers whose value is known
// to be sensitive should still call CorrelationID at the source — this is the
// net, not the fix.
func SanitizeCorrelationID(v string) string {
	if PathShaped(v) {
		return CorrelationID(v)
	}
	return v
}
