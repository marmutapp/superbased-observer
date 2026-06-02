package messagesummary

import "testing"

// TestAuthCache_RoundTrip pins the basic Set+Get contract.
func TestAuthCache_RoundTrip(t *testing.T) {
	c := NewAuthCache(0)
	c.Set("sess-A", AuthCredentials{Authorization: "Bearer abc"})
	got, ok := c.Get("sess-A")
	if !ok {
		t.Fatal("Get(sess-A) miss after Set")
	}
	if got.Authorization != "Bearer abc" {
		t.Errorf("Authorization: got %q, want %q", got.Authorization, "Bearer abc")
	}
}

// TestAuthCache_SetEmptyNoOp pins that empty sessionID OR empty
// credentials are silently ignored — production callers may call Set
// before fully extracting both.
func TestAuthCache_SetEmptyNoOp(t *testing.T) {
	c := NewAuthCache(0)
	c.Set("", AuthCredentials{Authorization: "Bearer abc"})
	c.Set("sess-A", AuthCredentials{})
	if c.Len() != 0 {
		t.Errorf("expected empty cache, got %d entries", c.Len())
	}
}

// TestAuthCache_DropsAtCap pins the soft-cap drop-everything reset:
// once the cache exceeds capacity, the next Set zaps the map and
// inserts only the new entry.
func TestAuthCache_DropsAtCap(t *testing.T) {
	c := NewAuthCache(2)
	c.Set("a", AuthCredentials{APIKey: "1"})
	c.Set("b", AuthCredentials{APIKey: "2"})
	c.Set("c", AuthCredentials{APIKey: "3"})
	if _, ok := c.Get("a"); ok {
		t.Errorf("a should have been dropped at cap")
	}
	if got, _ := c.Get("c"); got.APIKey != "3" {
		t.Errorf("c should survive: got %v", got)
	}
}

// TestAuthCache_GetEmptySession pins that Get("") never returns a
// hit (defensive — the proxy may pass through an empty session_id).
func TestAuthCache_GetEmptySession(t *testing.T) {
	c := NewAuthCache(0)
	c.Set("sess-A", AuthCredentials{APIKey: "x"})
	if _, ok := c.Get(""); ok {
		t.Errorf("Get(\"\") should never hit")
	}
}

// TestAuthCredentials_Empty pins the zero-value detection — used by
// the Factory to decide whether to construct a Summarizer.
func TestAuthCredentials_Empty(t *testing.T) {
	if !(AuthCredentials{}).Empty() {
		t.Error("zero value not Empty")
	}
	if (AuthCredentials{Authorization: "x"}).Empty() {
		t.Error("Authorization-only should not be Empty")
	}
	if (AuthCredentials{APIKey: "x"}).Empty() {
		t.Error("APIKey-only should not be Empty")
	}
}
