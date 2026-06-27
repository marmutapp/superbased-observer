package cachetrack

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
)

// Chain is a rolling SHA-256 over a sequence of canonical block
// bytes prefixed by Level + Kind labels. The cumulative hash after
// block i represents the cache prefix identity through that block;
// the Anthropic provider's prefix-cache identity follows the same
// pure-function-of-prefix-bytes shape.
//
// A Chain is owned by a single goroutine; the engine (spec §3)
// holds per-session Chains behind its own mutex. Reset is used
// after a compaction reset (§6) or a daemon-restart re-anchor
// (§3).
type Chain struct {
	state []byte // last cumulative SHA-256 sum (nil before first Push)
	n     int    // count of blocks pushed
}

// NewChain returns a Chain with no prior state (nil cumulative
// hash). The first Push produces H_0 = SHA256(label || canonical).
func NewChain() *Chain {
	return &Chain{}
}

// Push appends a block to the chain and returns a COPY of the new
// cumulative prefix hash. The internal state is not exposed.
//
// Preimage shape (spec §5):
//
//	H_i = SHA256(H_{i-1} || u8(Level) || u32be(len(Kind)) || Kind || u32be(len(CanonicalBytes)) || CanonicalBytes)
//
// Length prefixes prevent boundary ambiguity between Kind and
// CanonicalBytes: a Kind that happens to share a prefix with the
// canonical body cannot collide with a different (Kind, body)
// split. H_{-1} is the empty byte slice (Write(nil) is a no-op on
// hash.Hash).
func (c *Chain) Push(b Block) []byte {
	h := sha256.New()
	h.Write(c.state)
	h.Write([]byte{byte(b.Level)})
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(b.Kind)))
	h.Write(lenBuf[:])
	h.Write([]byte(b.Kind))
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(b.CanonicalBytes)))
	h.Write(lenBuf[:])
	h.Write(b.CanonicalBytes)
	c.state = h.Sum(nil)
	c.n++
	out := make([]byte, len(c.state))
	copy(out, c.state)
	return out
}

// PrefixHash returns a COPY of the current cumulative hash without
// appending. Returns nil before the first Push.
func (c *Chain) PrefixHash() []byte {
	if c.state == nil {
		return nil
	}
	out := make([]byte, len(c.state))
	copy(out, c.state)
	return out
}

// PrefixHashHex returns the current cumulative hash as a lowercase
// hex string — the form persisted to cache_segments.prefix_hash
// and cache_entries.prefix_hash. Returns "" before the first Push.
func (c *Chain) PrefixHashHex() string {
	if c.state == nil {
		return ""
	}
	return hex.EncodeToString(c.state)
}

// Count returns the number of blocks pushed onto the chain.
func (c *Chain) Count() int {
	return c.n
}

// Reset zeroes the chain — used after a compaction reset (§6) or
// a daemon-restart re-anchor (§3). After Reset, Count is 0 and
// PrefixHash returns nil; the next Push behaves as the first.
func (c *Chain) Reset() {
	c.state = nil
	c.n = 0
}
