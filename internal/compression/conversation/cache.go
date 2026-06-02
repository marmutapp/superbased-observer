package conversation

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// PrefixHash returns a stable lowercase-hex SHA-256 digest of the first n
// messages, hashing on role + text with a `\x1f` separator so identical
// content in different roles doesn't collide. The result is used as the
// `message_prefix_hash` on [models.APITurn] so the cost engine can detect
// identical prefixes across turns and flag expected cache hits on
// Anthropic's prefix cache.
//
// When n <= 0 or exceeds len(msgs), the full slice is hashed.
func PrefixHash(msgs []Message, n int) string {
	if n <= 0 || n > len(msgs) {
		n = len(msgs)
	}
	if n == 0 {
		return ""
	}
	h := sha256.New()
	for i := 0; i < n; i++ {
		h.Write([]byte(msgs[i].Role))
		h.Write([]byte{0x1f})
		h.Write([]byte(msgs[i].Text))
		h.Write([]byte{0x1e})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// SplitIndex returns the largest i such that the cumulative [Message.ByteLen]
// of msgs[:i] does not exceed targetBytes. Used by the proxy in cache mode
// to decide where a cache breakpoint should sit — downstream requests can
// set `cache_control: {type: "ephemeral"}` on msgs[i-1] to ensure the
// prefix-through-i is cacheable without growing too large to fit the cache.
//
// When targetBytes <= 0, returns 0. When every message fits, returns
// len(msgs). Preserved messages at the tail don't affect the prefix
// calculation — the split rides on byte budget alone.
func SplitIndex(msgs []Message, targetBytes int) int {
	if targetBytes <= 0 {
		return 0
	}
	total := 0
	for i, m := range msgs {
		if total+m.ByteLen > targetBytes {
			return i
		}
		total += m.ByteLen
	}
	return len(msgs)
}

// Summarize renders a short human-readable view of the first n messages'
// roles + a preview of each, useful for observer_log telemetry describing
// which prefix was hashed. Not load-bearing for correctness.
func Summarize(msgs []Message, n int) string {
	if n <= 0 || n > len(msgs) {
		n = len(msgs)
	}
	if n == 0 {
		return ""
	}
	parts := make([]string, 0, n)
	for i := 0; i < n; i++ {
		preview := msgs[i].Text
		if len(preview) > 40 {
			preview = preview[:40] + "…"
		}
		parts = append(parts, msgs[i].Role+":"+preview)
	}
	return strings.Join(parts, " | ")
}
