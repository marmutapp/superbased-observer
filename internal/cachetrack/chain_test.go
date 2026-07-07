package cachetrack

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// TestChain_Deterministic pins R3 at the chain level: the same
// sequence of blocks must produce the same cumulative hash across
// fresh Chain instances. Pre-image stability across runs is what
// makes §10 reconciliation honest — a flaky chain would invalidate
// every comparison.
func TestChain_Deterministic(t *testing.T) {
	t.Parallel()
	blocks := []Block{
		{Level: LevelTools, Kind: "tool_use", CanonicalBytes: []byte(`{"name":"x"}`)},
		{Level: LevelSystem, Kind: "text", CanonicalBytes: []byte(`{"text":"sys"}`)},
		{Level: LevelMessage, Kind: "text", CanonicalBytes: []byte(`{"text":"hi"}`)},
	}
	var first []byte
	for i := 0; i < 5; i++ {
		c := NewChain()
		for _, b := range blocks {
			c.Push(b)
		}
		got := c.PrefixHash()
		if i == 0 {
			first = got
			continue
		}
		if !bytes.Equal(got, first) {
			t.Fatalf("run %d: chain drift %x vs %x", i, got, first)
		}
	}
}

// TestChain_OrderSensitive proves the chain is order-sensitive —
// reordering blocks must produce a different hash (cache prefixes
// are ordered sequences, not sets).
func TestChain_OrderSensitive(t *testing.T) {
	t.Parallel()
	a := Block{Level: LevelMessage, Kind: "text", CanonicalBytes: []byte(`{"text":"a"}`)}
	b := Block{Level: LevelMessage, Kind: "text", CanonicalBytes: []byte(`{"text":"b"}`)}
	c1 := NewChain()
	c1.Push(a)
	c1.Push(b)
	c2 := NewChain()
	c2.Push(b)
	c2.Push(a)
	if bytes.Equal(c1.PrefixHash(), c2.PrefixHash()) {
		t.Fatal("AB and BA produced the same hash; chain is not order-sensitive")
	}
}

// TestChain_LevelSensitive proves the level label is part of the
// preimage — identical canonical bytes at different levels MUST
// produce different chain hashes (defense against tools-vs-system
// content collisions).
func TestChain_LevelSensitive(t *testing.T) {
	t.Parallel()
	body := []byte(`{"text":"x"}`)
	cTools := NewChain()
	cTools.Push(Block{Level: LevelTools, Kind: "text", CanonicalBytes: body})
	cSys := NewChain()
	cSys.Push(Block{Level: LevelSystem, Kind: "text", CanonicalBytes: body})
	if bytes.Equal(cTools.PrefixHash(), cSys.PrefixHash()) {
		t.Fatal("same canonical bytes at different levels produced same hash")
	}
}

// TestChain_KindSensitive proves the kind label is part of the
// preimage — identical canonical bytes with different kinds MUST
// produce different chain hashes. Length-prefixing in the
// preimage shape protects against KindA+payload colliding with
// KindB+payload' splits.
func TestChain_KindSensitive(t *testing.T) {
	t.Parallel()
	body := []byte(`{"text":"x"}`)
	cText := NewChain()
	cText.Push(Block{Level: LevelMessage, Kind: "text", CanonicalBytes: body})
	cThink := NewChain()
	cThink.Push(Block{Level: LevelMessage, Kind: "thinking", CanonicalBytes: body})
	if bytes.Equal(cText.PrefixHash(), cThink.PrefixHash()) {
		t.Fatal("same canonical bytes with different kinds produced same hash")
	}
}

// TestChain_MarkerStrippingEndToEnd proves that two blocks
// differing ONLY in cache_control placement produce identical
// chain hashes — the load-bearing property for §7 row 7 (TTL
// expiry detection): the cache prefix IS the content, not the
// content-plus-marker. Composes CanonicalizeAnthropic with Chain.
func TestChain_MarkerStrippingEndToEnd(t *testing.T) {
	t.Parallel()
	withMarker := []byte(`{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}`)
	without := []byte(`{"type":"text","text":"hi"}`)
	canon1, err := CanonicalizeAnthropic(withMarker)
	if err != nil {
		t.Fatal(err)
	}
	canon2, err := CanonicalizeAnthropic(without)
	if err != nil {
		t.Fatal(err)
	}
	c1 := NewChain()
	c1.Push(Block{Level: LevelMessage, Kind: "text", CanonicalBytes: canon1})
	c2 := NewChain()
	c2.Push(Block{Level: LevelMessage, Kind: "text", CanonicalBytes: canon2})
	if !bytes.Equal(c1.PrefixHash(), c2.PrefixHash()) {
		t.Errorf("marker presence changed chain hash:\n with: %x\n without: %x", c1.PrefixHash(), c2.PrefixHash())
	}
}

// TestChain_Reset proves a Reset returns the Chain to fresh state.
func TestChain_Reset(t *testing.T) {
	t.Parallel()
	c := NewChain()
	c.Push(Block{Level: LevelMessage, Kind: "text", CanonicalBytes: []byte(`{"t":"a"}`)})
	c.Push(Block{Level: LevelMessage, Kind: "text", CanonicalBytes: []byte(`{"t":"b"}`)})
	if c.Count() != 2 {
		t.Fatalf("Count = %d, want 2", c.Count())
	}
	c.Reset()
	if c.Count() != 0 {
		t.Errorf("post-reset Count = %d, want 0", c.Count())
	}
	if c.PrefixHash() != nil {
		t.Errorf("post-reset PrefixHash = %x, want nil", c.PrefixHash())
	}
	if c.PrefixHashHex() != "" {
		t.Errorf("post-reset PrefixHashHex = %q, want \"\"", c.PrefixHashHex())
	}
}

// TestChain_HexFormat pins the hex encoding round-trip.
func TestChain_HexFormat(t *testing.T) {
	t.Parallel()
	c := NewChain()
	if c.PrefixHashHex() != "" {
		t.Fatalf("empty chain Hex = %q, want \"\"", c.PrefixHashHex())
	}
	c.Push(Block{Level: LevelMessage, Kind: "text", CanonicalBytes: []byte(`{"t":"a"}`)})
	got := c.PrefixHashHex()
	if len(got) != 64 {
		t.Errorf("Hex len = %d, want 64", len(got))
	}
	raw, err := hex.DecodeString(got)
	if err != nil {
		t.Fatalf("hex decode: %v", err)
	}
	if !bytes.Equal(raw, c.PrefixHash()) {
		t.Error("Hex does not match PrefixHash")
	}
}

// TestChain_NoStateAliasing proves the slice returned by Push /
// PrefixHash is a copy — mutating it does not corrupt subsequent
// hashes. Defends against an entire class of latent bugs (caller
// stashes a returned hash and later mutates the underlying array).
func TestChain_NoStateAliasing(t *testing.T) {
	t.Parallel()
	c := NewChain()
	got := c.Push(Block{Level: LevelMessage, Kind: "text", CanonicalBytes: []byte(`{"t":"a"}`)})
	saved := make([]byte, len(got))
	copy(saved, got)
	// Mutate the returned slice.
	for i := range got {
		got[i] ^= 0xff
	}
	c.Push(Block{Level: LevelMessage, Kind: "text", CanonicalBytes: []byte(`{"t":"b"}`)})
	// PrefixHash after the second Push should be equivalent to a
	// fresh chain that ran both blocks — proves the first block's
	// state was not aliased through `got`.
	fresh := NewChain()
	fresh.Push(Block{Level: LevelMessage, Kind: "text", CanonicalBytes: []byte(`{"t":"a"}`)})
	fresh.Push(Block{Level: LevelMessage, Kind: "text", CanonicalBytes: []byte(`{"t":"b"}`)})
	if !bytes.Equal(c.PrefixHash(), fresh.PrefixHash()) {
		t.Errorf("aliasing detected:\n got %x\n want %x", c.PrefixHash(), fresh.PrefixHash())
	}
	_ = saved
}
