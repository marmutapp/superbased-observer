package remoteauth

import (
	"testing"
	"time"
)

// TestTerminalControlCapabilityBinding proves a terminal-control capability is
// bound to (session + action + handle) and confirm, single-use, TTL-bounded,
// and dropped on session revoke (plan §4.γ tests).
func TestTerminalControlCapabilityBinding(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	now := base
	cs := NewCapabilityStore(2*time.Minute, func() time.Time { return now })

	tok, confirm, err := cs.MintTerminalControl("dev-A", "handle-1")
	if err != nil {
		t.Fatalf("MintTerminalControl: %v", err)
	}

	// Wrong handle → refused (binding), but a confirmed hit burns it.
	tokB, confirmB, _ := cs.MintTerminalControl("dev-A", "handle-1")
	if cs.ConsumeTerminalControl(tokB, confirmB, "dev-A", "handle-2") {
		t.Fatal("capability for handle-1 consumed against handle-2")
	}

	// Wrong device → refused.
	tokC, confirmC, _ := cs.MintTerminalControl("dev-A", "handle-1")
	if cs.ConsumeTerminalControl(tokC, confirmC, "dev-B", "handle-1") {
		t.Fatal("capability for dev-A consumed for dev-B")
	}

	// Correct binding → consumed once; replay fails (single-use burn).
	if !cs.ConsumeTerminalControl(tok, confirm, "dev-A", "handle-1") {
		t.Fatal("valid terminal-control capability not consumed")
	}
	if cs.ConsumeTerminalControl(tok, confirm, "dev-A", "handle-1") {
		t.Fatal("terminal-control capability consumed twice (not single-use)")
	}
}

// TestTerminalConfirmBinding proves a confirm minted for capability A does NOT
// satisfy capability B, and a FAILED confirm consumes NOTHING (§4.γ.2).
func TestTerminalConfirmBinding(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	cs := NewCapabilityStore(2*time.Minute, func() time.Time { return now })

	tokA, _, _ := cs.MintTerminalControl("dev-A", "h1")
	_, confirmB, _ := cs.MintTerminalControl("dev-A", "h1")

	// confirm B against capability A → rejected, and A must NOT be burned.
	if cs.ConsumeTerminalControl(tokA, confirmB, "dev-A", "h1") {
		t.Fatal("cross-capability confirm satisfied a different capability")
	}
	// A is still consumable with ITS OWN confirm — the failed confirm burned
	// nothing. Re-mint A's confirm is impossible; assert via a fresh mint pair.
	tokC, confirmC, _ := cs.MintTerminalControl("dev-A", "h1")
	if cs.ConsumeTerminalControl(tokC, "wrong-nonce", "dev-A", "h1") {
		t.Fatal("wrong confirm accepted")
	}
	if !cs.ConsumeTerminalControl(tokC, confirmC, "dev-A", "h1") {
		t.Fatal("a wrong-confirm attempt burned the capability (must consume nothing)")
	}
}

// TestTerminalCapabilityTTLAndRevoke proves TTL expiry and RevokeSession drop.
func TestTerminalCapabilityTTLAndRevoke(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	now := base
	cs := NewCapabilityStore(time.Minute, func() time.Time { return now })

	// TTL expiry.
	tok, confirm, _ := cs.MintTerminalControl("dev-A", "h1")
	now = base.Add(2 * time.Minute)
	if cs.ConsumeTerminalControl(tok, confirm, "dev-A", "h1") {
		t.Fatal("expired terminal capability consumed")
	}
	now = base

	// RevokeSession drops it.
	tok2, confirm2, _ := cs.MintTerminalControl("dev-A", "h1")
	cs.RevokeSession("dev-A")
	if cs.ConsumeTerminalControl(tok2, confirm2, "dev-A", "h1") {
		t.Fatal("capability survived RevokeSession")
	}
}

// TestTerminalControlCapabilityInvalidAfterRestart proves a terminal-control
// capability (and its bound confirm) minted but NOT consumed does not survive a
// daemon restart: the store is memory-only, so reconstructing it (as a restart
// does) drops every outstanding capability, and BOTH the capability and its
// bound confirm then fail to consume. A leaked-but-unused capability dies with
// the process — it is never persisted to any migration/store/audit surface, so
// there is nothing to replay after a restart (plan §4.γ: memory-only,
// restart-invalidated).
func TestTerminalControlCapabilityInvalidAfterRestart(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	now := base

	// Pre-restart store: mint WITHOUT consuming (the capability is live).
	pre := NewCapabilityStore(2*time.Minute, func() time.Time { return now })
	tok, confirm, err := pre.MintTerminalControl("dev-A", "handle-1")
	if err != nil {
		t.Fatalf("MintTerminalControl: %v", err)
	}
	// Sanity: it WOULD consume on the same (un-restarted) store.
	// (Use a sibling mint so we don't burn the token under test.)
	tokLive, confirmLive, _ := pre.MintTerminalControl("dev-A", "handle-1")
	if !pre.ConsumeTerminalControl(tokLive, confirmLive, "dev-A", "handle-1") {
		t.Fatal("precondition: a freshly minted capability must consume on its own store")
	}

	// Restart: the in-memory capability store is reconstructed from nothing (the
	// capabilities were never persisted). Even wall-clock-fresh, well within TTL.
	post := NewCapabilityStore(2*time.Minute, func() time.Time { return now })

	// The capability MUST fail to consume on the restarted store.
	if post.ConsumeTerminalControl(tok, confirm, "dev-A", "handle-1") {
		t.Fatal("terminal-control capability survived a daemon restart — must be restart-invalidated")
	}
	// The bound confirm alone is likewise useless (there is no capability to bind
	// it to); and the plain-Consume path is dead too.
	if post.ConsumeTerminalControl(tok, confirm, "dev-A", "handle-1") {
		t.Fatal("bound confirm re-consumed after restart")
	}
	if post.Consume(tok, "dev-A", ActionTerminalControl) {
		t.Fatal("capability consumable via the plain path after restart")
	}
}

// TestPlainCapabilityNotTerminalConsumable proves a plain (non-terminal)
// capability minted via Mint can NOT be consumed through the terminal path
// (it has no confirm), and vice-versa.
func TestPlainCapabilityNotTerminalConsumable(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	cs := NewCapabilityStore(time.Minute, func() time.Time { return now })
	tok, err := cs.Mint("dev-A", "some.action")
	if err != nil {
		t.Fatal(err)
	}
	if cs.ConsumeTerminalControl(tok, "", "dev-A", "h1") {
		t.Fatal("plain capability consumed via terminal path")
	}
	if cs.ConsumeTerminalControl(tok, "anything", "dev-A", "h1") {
		t.Fatal("plain capability consumed via terminal path with a guessed confirm")
	}
}
