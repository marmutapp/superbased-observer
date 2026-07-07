package routing

import "testing"

// TestTierRank_Ordering pins the quality-class ordering the downshift
// logic depends on: opus > sonnet > haiku > free == local > unclassified.
// One row per ordered pair that must hold.
func TestTierRank_Ordering(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		higher Tier
		lower  Tier
	}{
		{"opus_above_sonnet", TierOpusClass, TierSonnetClass},
		{"sonnet_above_haiku", TierSonnetClass, TierHaikuClass},
		{"haiku_above_free", TierHaikuClass, TierFree},
		{"haiku_above_local", TierHaikuClass, TierLocal},
		{"free_above_unclassified", TierFree, TierUnclassified},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.higher.Rank() <= tc.lower.Rank() {
				t.Errorf("Rank(%s)=%d not above Rank(%s)=%d",
					tc.higher, tc.higher.Rank(), tc.lower, tc.lower.Rank())
			}
		})
	}
}

// TestTierRank_FreeLocalPeers pins free and local as rank peers — neither
// orders above the other (quality varies too much to claim otherwise).
func TestTierRank_FreeLocalPeers(t *testing.T) {
	t.Parallel()
	if TierFree.Rank() != TierLocal.Rank() {
		t.Errorf("free rank %d != local rank %d", TierFree.Rank(), TierLocal.Rank())
	}
}

// TestTier_DownshiftTargetable pins §R7.1: unclassified (and unknown tier
// strings) are never route-to destinations; every shipped real tier is.
func TestTier_DownshiftTargetable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		tier Tier
		want bool
	}{
		{TierOpusClass, true},
		{TierSonnetClass, true},
		{TierHaikuClass, true},
		{TierFree, true},
		{TierLocal, true},
		{TierUnclassified, false},
		{Tier("made-up-tier"), false},
	}
	for _, tc := range cases {
		t.Run(string(tc.tier), func(t *testing.T) {
			t.Parallel()
			if got := tc.tier.DownshiftTargetable(); got != tc.want {
				t.Errorf("DownshiftTargetable(%q) = %v, want %v", tc.tier, got, tc.want)
			}
		})
	}
}

// TestTier_Known covers the vocabulary membership check both ways.
func TestTier_Known(t *testing.T) {
	t.Parallel()
	for _, tier := range AllTiers() {
		if !tier.Known() {
			t.Errorf("AllTiers() member %q not Known()", tier)
		}
	}
	if Tier("nope").Known() {
		t.Error("Known(nope) = true, want false")
	}
}

// TestTurnKind_Soft pins which turn-kinds budget/downshift logic may treat
// as soft (§R14): read_only/housekeeping/subagent/test_run yes; plan, edit,
// long_context, unknown never. One row per taxonomy member.
func TestTurnKind_Soft(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind TurnKind
		want bool
	}{
		{TurnReadOnly, true},
		{TurnHousekeeping, true},
		{TurnSubagent, true},
		{TurnTestRun, true},
		{TurnPlan, false},
		{TurnEdit, false},
		{TurnLongContext, false},
		{TurnUnknown, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			t.Parallel()
			if got := tc.kind.Soft(); got != tc.want {
				t.Errorf("Soft(%s) = %v, want %v", tc.kind, got, tc.want)
			}
		})
	}
}

// TestAllTurnKinds_CoversTaxonomy pins the v1 taxonomy size so adding a
// kind forces a deliberate update here and in dependent rule tables.
func TestAllTurnKinds_CoversTaxonomy(t *testing.T) {
	t.Parallel()
	if got := len(AllTurnKinds()); got != 8 {
		t.Errorf("taxonomy size %d, want 8 (update rule tables + this pin together)", got)
	}
}

// TestKnownReasonCodes_ClosedEnum guards the closed enum: no duplicates,
// none empty. Dashboards aggregate on these strings.
func TestKnownReasonCodes_ClosedEnum(t *testing.T) {
	t.Parallel()
	seen := map[ReasonCode]bool{}
	for _, rc := range KnownReasonCodes() {
		if rc == "" {
			t.Error("empty reason code in KnownReasonCodes")
		}
		if seen[rc] {
			t.Errorf("duplicate reason code %q", rc)
		}
		seen[rc] = true
	}
}
