package cursor

import (
	"crypto/sha256"
	"encoding/hex"
)

// shortHash returns the first 12 hex chars of SHA-256(s) — long enough to
// disambiguate hook payloads within a single turn, short enough to keep
// SourceEventID readable in the actions table.
func shortHash(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:6])
}
