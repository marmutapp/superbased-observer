package cachetrack

import (
	"testing"
	"time"
)

// TestEntryState_String pins the schema-stable state labels.
func TestEntryState_String(t *testing.T) {
	t.Parallel()
	tests := []struct {
		s    EntryState
		want string
	}{
		{StateLive, "live"},
		{StateExpired, "expired"},
		{StateInvalidated, "invalidated"},
		{StateUnverified, "unverified"},
		{StateUnknown, "unknown"},
		{EntryState(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.s.String(); got != tt.want {
			t.Errorf("EntryState(%d).String() = %q, want %q", tt.s, got, tt.want)
		}
	}
}

// TestStateTransitions covers the §6 transition table — one
// case per row + a handful of no-op cases that demonstrate the
// table's "first match wins, no match = no-op" shape.
//
// Failure here is a state-machine regression: a future change to
// stateTransitions must add or rewrite a case here in lockstep
// (§24.5 + §24.6).
func TestStateTransitions(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		from        EntryState
		trigger     Trigger
		ttl         BlockTTL
		wantState   EntryState
		wantRefresh bool
	}{
		// Live + observed read/write → refresh, stay live.
		{"live + read → live (refresh)", StateLive, TriggerRead, TTL5m, StateLive, true},
		{"live + write 1h → live (refresh, 1h ttl)", StateLive, TriggerWrite, TTL1h, StateLive, true},
		// Live + clock expiry → expired (kept for diagnosis).
		{"live + clock-expiry → expired", StateLive, TriggerClockExpiry, TTL5m, StateExpired, false},
		// Live + chain divergence → invalidated.
		{"live + prefix-diverged → invalidated", StateLive, TriggerPrefixDiverged, TTL5m, StateInvalidated, false},
		// Live + reconciliation contradiction → unverified.
		{"live + mispredict → unverified", StateLive, TriggerMispredict, TTL5m, StateUnverified, false},
		// Live + compaction → invalidated.
		{"live + compaction → invalidated", StateLive, TriggerCompactionReset, TTL5m, StateInvalidated, false},
		// Unverified + confirming observation → back to live.
		{"unverified + read → live (refresh)", StateUnverified, TriggerRead, TTL5m, StateLive, true},
		{"unverified + write → live (refresh)", StateUnverified, TriggerWrite, TTL1h, StateLive, true},
		// Unverified + another mispredict → stays unverified.
		{"unverified + mispredict → unverified (no refresh)", StateUnverified, TriggerMispredict, TTL5m, StateUnverified, false},
		// Unverified + clock expiry → expired.
		{"unverified + clock-expiry → expired", StateUnverified, TriggerClockExpiry, TTL5m, StateExpired, false},
		// Terminal states: triggers do not transition back.
		{"expired + read → expired (no-op)", StateExpired, TriggerRead, TTL5m, StateExpired, false},
		{"expired + write → expired (no-op)", StateExpired, TriggerWrite, TTL5m, StateExpired, false},
		{"invalidated + read → invalidated (no-op)", StateInvalidated, TriggerRead, TTL5m, StateInvalidated, false},
		{"invalidated + write → invalidated (no-op)", StateInvalidated, TriggerWrite, TTL5m, StateInvalidated, false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := &Entry{
				State:         tt.from,
				TTL:           tt.ttl,
				LastRefreshAt: now.Add(-time.Hour),
				ExpiresAt:     now.Add(-time.Minute), // intentionally stale to confirm refresh updates it
			}
			refreshed := e.Apply(tt.trigger, now)
			if e.State != tt.wantState {
				t.Errorf("State = %v, want %v", e.State, tt.wantState)
			}
			if refreshed != tt.wantRefresh {
				t.Errorf("refreshed = %v, want %v", refreshed, tt.wantRefresh)
			}
			if tt.wantRefresh {
				if !e.LastRefreshAt.Equal(now) {
					t.Errorf("LastRefreshAt = %v, want %v (refresh case)", e.LastRefreshAt, now)
				}
				wantExpiry := now.Add(TTLDuration(tt.ttl))
				if !e.ExpiresAt.Equal(wantExpiry) {
					t.Errorf("ExpiresAt = %v, want %v", e.ExpiresAt, wantExpiry)
				}
			} else if e.LastRefreshAt.Equal(now) {
				t.Errorf("LastRefreshAt advanced to %v on a non-refresh trigger", e.LastRefreshAt)
			}
		})
	}
}

// TestObservedTTL covers the R1-emphasized §6 TTL keying:
// 1h-tier usage is the common case for Claude Code — detect on
// ephemeral_1h_input_tokens > 0 first; marker takes precedence
// when set.
func TestObservedTTL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		creation1hTokens int64
		markerTTL        BlockTTL
		want             BlockTTL
	}{
		// Usage-derived classification:
		{"1h tokens observed → 1h", 32393, TTLUnset, TTL1h},
		{"zero 1h tokens → 5m default", 0, TTLUnset, TTL5m},
		{"R1 captured shape: 1h=32393, 5m=0 → 1h", 32393, TTLUnset, TTL1h},

		// Marker takes precedence — the request shape is the
		// stronger signal (we believe what we asked for).
		{"marker 5m, no 1h usage → 5m", 0, TTL5m, TTL5m},
		{"marker 1h, no 1h usage → 1h (marker wins)", 0, TTL1h, TTL1h},
		{"marker 5m + 1h usage → marker wins (5m)", 32393, TTL5m, TTL5m},
		{"marker 1h + 1h usage → 1h (consistent)", 32393, TTL1h, TTL1h},
	}
	for _, tt := range tests {
		if got := ObservedTTL(tt.creation1hTokens, tt.markerTTL); got != tt.want {
			t.Errorf("%s: ObservedTTL(%d, %v) = %v, want %v", tt.name, tt.creation1hTokens, tt.markerTTL, got, tt.want)
		}
	}
}

// TestCacheModel_SweepExpired covers the bulk clock-based
// transition: at-or-past expires_at flips live + unverified
// entries to expired; not-yet-expired entries are untouched;
// already-terminal entries are untouched.
func TestCacheModel_SweepExpired(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	m := NewCacheModel()
	cases := []struct {
		key       string
		state     EntryState
		expiresAt time.Time
		wantState EntryState
		wantSweep bool
	}{
		{"live-past", StateLive, now.Add(-time.Minute), StateExpired, true},
		{"live-at", StateLive, now, StateExpired, true},
		{"live-future", StateLive, now.Add(time.Minute), StateLive, false},
		{"unverified-past", StateUnverified, now.Add(-time.Minute), StateExpired, true},
		{"unverified-future", StateUnverified, now.Add(time.Minute), StateUnverified, false},
		// Terminal states do not get re-swept.
		{"expired-past", StateExpired, now.Add(-time.Minute), StateExpired, false},
		{"invalidated-past", StateInvalidated, now.Add(-time.Minute), StateInvalidated, false},
	}
	for _, c := range cases {
		m.AddEntry(&Entry{
			Key:       EntryKey{Model: "m", Scope: "s", PrefixHash: c.key},
			State:     c.state,
			ExpiresAt: c.expiresAt,
		})
	}
	swept := m.SweepExpired(now)
	if len(swept) != countSweepable(cases) {
		t.Errorf("SweepExpired returned %d entries, want %d", len(swept), countSweepable(cases))
	}
	for _, c := range cases {
		got := m.FindByPrefix(c.key)
		if got.State != c.wantState {
			t.Errorf("entry %q: state = %v, want %v", c.key, got.State, c.wantState)
		}
	}
}

func countSweepable(cases []struct {
	key       string
	state     EntryState
	expiresAt time.Time
	wantState EntryState
	wantSweep bool
},
) int {
	n := 0
	for _, c := range cases {
		if c.wantSweep {
			n++
		}
	}
	return n
}

// TestCacheModel_CompactionReset covers the §6 compaction rule:
// every LIVE message-level entry becomes invalidated; tools- and
// system-level entries persist; the chain resets;
// BlocksSinceLastBreakpoint resets. Terminal entries are
// untouched.
func TestCacheModel_CompactionReset(t *testing.T) {
	t.Parallel()
	m := NewCacheModel()
	m.BlocksSinceLastBreakpoint = 17
	m.Chain.Push(Block{Level: LevelMessage, Kind: "text", CanonicalBytes: []byte(`{"t":"x"}`)})
	pre := m.Chain.PrefixHash()
	if pre == nil {
		t.Fatal("chain should have state after push")
	}

	m.AddEntry(&Entry{Key: EntryKey{PrefixHash: "msg-live"}, Level: LevelMessage, State: StateLive})
	m.AddEntry(&Entry{Key: EntryKey{PrefixHash: "sys-live"}, Level: LevelSystem, State: StateLive})
	m.AddEntry(&Entry{Key: EntryKey{PrefixHash: "tools-live"}, Level: LevelTools, State: StateLive})
	m.AddEntry(&Entry{Key: EntryKey{PrefixHash: "msg-already-expired"}, Level: LevelMessage, State: StateExpired})

	invalidated := m.CompactionReset()

	if len(invalidated) != 1 {
		t.Errorf("CompactionReset returned %d entries, want 1 (only msg-live qualifies)", len(invalidated))
	}
	if m.FindByPrefix("msg-live").State != StateInvalidated {
		t.Errorf("msg-live state = %v, want invalidated", m.FindByPrefix("msg-live").State)
	}
	if m.FindByPrefix("sys-live").State != StateLive {
		t.Errorf("sys-live state = %v, want live (system survives compaction)", m.FindByPrefix("sys-live").State)
	}
	if m.FindByPrefix("tools-live").State != StateLive {
		t.Errorf("tools-live state = %v, want live (tools survives compaction)", m.FindByPrefix("tools-live").State)
	}
	if m.FindByPrefix("msg-already-expired").State != StateExpired {
		t.Errorf("expired entry must not be touched, got %v", m.FindByPrefix("msg-already-expired").State)
	}
	if m.Chain.PrefixHash() != nil {
		t.Errorf("chain not reset: PrefixHash = %x, want nil", m.Chain.PrefixHash())
	}
	if m.BlocksSinceLastBreakpoint != 0 {
		t.Errorf("BlocksSinceLastBreakpoint = %d, want 0", m.BlocksSinceLastBreakpoint)
	}
}

// TestCacheableTokens covers the min-cacheable predicate.
// Smoke-test only — the table walk is owned by TestMinCacheableTokens.
func TestCacheableTokens(t *testing.T) {
	t.Parallel()
	tests := []struct {
		model  string
		tokens int
		want   bool
	}{
		{"claude-opus-4-8", 1023, false},
		{"claude-opus-4-8", 1024, true},
		{"claude-opus-4-7", 4095, false},
		{"claude-opus-4-7", 4096, true},
		{"claude-haiku-3-5", 2047, false},
		{"claude-haiku-3-5", 2048, true},
	}
	for _, tt := range tests {
		if got := CacheableTokens(tt.model, tt.tokens); got != tt.want {
			t.Errorf("CacheableTokens(%q, %d) = %v, want %v", tt.model, tt.tokens, got, tt.want)
		}
	}
}

// TestCacheModel_FindByPrefix verifies the O(1) lookup + nil on
// miss.
func TestCacheModel_FindByPrefix(t *testing.T) {
	t.Parallel()
	m := NewCacheModel()
	want := &Entry{Key: EntryKey{PrefixHash: "abc"}, State: StateLive}
	m.AddEntry(want)
	if got := m.FindByPrefix("abc"); got != want {
		t.Errorf("FindByPrefix(\"abc\") = %v, want %v", got, want)
	}
	if got := m.FindByPrefix("missing"); got != nil {
		t.Errorf("FindByPrefix(\"missing\") = %v, want nil", got)
	}
}

// TestEntry_Apply_ChainsRefreshes proves the rare-but-real
// pattern: a live entry that takes a read, then a write, then a
// mispredict, ends up StateUnverified with the LATEST TTL refresh.
func TestEntry_Apply_ChainsRefreshes(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	e := &Entry{State: StateLive, TTL: TTL1h}
	e.Apply(TriggerRead, now)
	if !e.LastRefreshAt.Equal(now) {
		t.Fatalf("first refresh: LastRefreshAt = %v, want %v", e.LastRefreshAt, now)
	}
	later := now.Add(time.Minute)
	e.Apply(TriggerWrite, later)
	if !e.LastRefreshAt.Equal(later) {
		t.Fatalf("second refresh: LastRefreshAt = %v, want %v", e.LastRefreshAt, later)
	}
	wantExpiry := later.Add(TTL1hDuration)
	if !e.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("ExpiresAt after second refresh = %v, want %v", e.ExpiresAt, wantExpiry)
	}
	even := later.Add(time.Second)
	e.Apply(TriggerMispredict, even)
	if e.State != StateUnverified {
		t.Errorf("post-mispredict state = %v, want unverified", e.State)
	}
	if !e.LastRefreshAt.Equal(later) {
		t.Errorf("mispredict refreshed TTL: LastRefreshAt = %v, want %v", e.LastRefreshAt, later)
	}
}
