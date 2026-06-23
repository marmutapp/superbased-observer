package hermes

import (
	"crypto/sha256"
	"encoding/hex"
)

// hashHex12 returns the first 12 hex characters of sha256(s) — enough
// to disambiguate same-shape SourceEventIDs within a session without
// inflating the row width. Symmetric with the cursor adapter's
// shortHash helper (internal/adapter/cursor/hash.go) so the two
// hook paths produce visually-comparable IDs.
func hashHex12(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}
