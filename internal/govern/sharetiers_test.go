package govern

import (
	"reflect"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/policyfam/nodegov"
)

// TestShareTierTable_TableDriven exercises ExtractionAuthorized against a
// range of resolved postures: unmanaged, managed-but-bare, umbrella-only,
// and per-tier grants, mirroring TestGovernedFamilies_TableDriven's shape in
// authorityfamilies_test.go.
func TestShareTierTable_TableDriven(t *testing.T) {
	managedUmbrella := Effective{Managed: true, Authority: []string{AuthorityExtractManaged}}
	managedCacheOnly := Effective{Managed: true, Authority: []string{AuthorityExtractCache}}
	managedCodeintelOnly := Effective{Managed: true, Authority: []string{AuthorityExtractCodeintel}}
	unmanagedUmbrella := Effective{Managed: false, Authority: []string{AuthorityExtractManaged}}

	cases := []struct {
		name string
		eff  Effective
		key  string
		want bool
	}{
		{name: "zero value, headline key", eff: Effective{}, key: "cache_detail", want: false},
		{name: "unmanaged umbrella grants nothing", eff: unmanagedUmbrella, key: "cache_detail", want: false},
		{name: "managed umbrella authorizes a headline key", eff: managedUmbrella, key: "cache_detail", want: true},
		{name: "managed umbrella authorizes routing_detail too", eff: managedUmbrella, key: "routing_detail", want: true},
		{name: "managed umbrella authorizes every obs.* key", eff: managedUmbrella, key: "obs.eval_items", want: true},
		{name: "managed umbrella does NOT authorize codeintel_detail", eff: managedUmbrella, key: "codeintel_detail", want: false},
		{name: "managed umbrella does NOT authorize process_detail", eff: managedUmbrella, key: "process_detail", want: false},
		{name: "managed umbrella does NOT authorize terminal_detail", eff: managedUmbrella, key: "terminal_detail", want: false},
		{name: "per-tier cache grant authorizes cache_detail only", eff: managedCacheOnly, key: "cache_detail", want: true},
		{name: "per-tier cache grant does not authorize routing_detail", eff: managedCacheOnly, key: "routing_detail", want: false},
		{name: "per-tier codeintel grant authorizes codeintel_detail", eff: managedCodeintelOnly, key: "codeintel_detail", want: true},
		{name: "policy_state is never authorized, even under the umbrella", eff: managedUmbrella, key: "policy_state", want: false},
		{name: "target_action_allowlist is never authorized, even under the umbrella", eff: managedUmbrella, key: "target_action_allowlist", want: false},
		{name: "unknown key is never authorized", eff: managedUmbrella, key: "some.future.key", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractionAuthorized(tc.eff, tc.key); got != tc.want {
				t.Errorf("ExtractionAuthorized(%+v, %q) = %v, want %v", tc.eff, tc.key, got, tc.want)
			}
		})
	}
}

// TestShareTierTable_EveryShareKeyMapsOrIsExempt is the bidirectional drift
// guard: every key in nodegov.ShareKeys (the live 17-row source of truth)
// must appear in EXACTLY ONE of shareTierTable or shareTierExempt — never
// both, never neither.
func TestShareTierTable_EveryShareKeyMapsOrIsExempt(t *testing.T) {
	for _, sk := range nodegov.ShareKeys {
		if !KnownShareTierKey(sk.Key) {
			t.Errorf("nodegov.ShareKeys key %q is not mapped in shareTierTable and not in shareTierExempt - "+
				"add a row to shareTierTable, or a conscious exemption with a reason", sk.Key)
		}
		_, mapped := shareTierByKey[sk.Key]
		exempt := shareTierExempt[sk.Key]
		if mapped && exempt {
			t.Errorf("share key %q is BOTH mapped in shareTierTable AND listed in shareTierExempt - "+
				"remove it from one", sk.Key)
		}
	}
}

// TestShareTierTable_EveryRowIsALiveShareKey is the reverse direction of the
// drift guard above: a row in shareTierTable or shareTierExempt naming a key
// nodegov.ShareKeys no longer carries is stale and must be removed.
func TestShareTierTable_EveryRowIsALiveShareKey(t *testing.T) {
	live := make(map[string]bool, len(nodegov.ShareKeys))
	for _, sk := range nodegov.ShareKeys {
		live[sk.Key] = true
	}
	for _, row := range shareTierTable {
		if !live[row.Key] {
			t.Errorf("shareTierTable row %q does not name a live nodegov.ShareKeys key - stale row, remove it", row.Key)
		}
	}
	for key := range shareTierExempt {
		if !live[key] {
			t.Errorf("shareTierExempt key %q does not name a live nodegov.ShareKeys key - stale exemption, remove it", key)
		}
	}
}

// TestShareTierTable_EveryAuthorityIsAnExtractionAuthority pins every
// mapped Authority value against govern's own ExtractionAuthority
// classifier, so a row can never point at a non-extraction (or retired, or
// misspelled) token.
func TestShareTierTable_EveryAuthorityIsAnExtractionAuthority(t *testing.T) {
	for _, row := range shareTierTable {
		if !ExtractionAuthority(row.Authority) {
			t.Errorf("shareTierTable row %q names Authority %q, which ExtractionAuthority rejects - "+
				"every mapped authority must be a real extraction token", row.Key, row.Authority)
		}
	}
}

// TestShareTierTable_UnknownKeyNeverAuthorized is the "never fabricates"
// twin of TestGovernedFamilies_UnknownTokensNeverFabricated.
func TestShareTierTable_UnknownKeyNeverAuthorized(t *testing.T) {
	eff := Effective{Managed: true, Authority: []string{AuthorityExtractManaged}}
	if got := ExtractionAuthorized(eff, "definitely-not-a-real-share-key"); got {
		t.Errorf("ExtractionAuthorized must never authorize an unmapped key, got true")
	}
	if KnownShareTierKey("definitely-not-a-real-share-key") {
		t.Errorf("KnownShareTierKey must never claim an unmapped key, got true")
	}
}

// TestExtractionTokensInForce_TableDriven pins the P4-2 consumer surface:
// only tokens that are BOTH present in acceptedAuthority AND currently
// authorize a live raise are reported, sorted and deduplicated.
func TestExtractionTokensInForce_TableDriven(t *testing.T) {
	managedUmbrella := Effective{Managed: true, Authority: []string{AuthorityExtractManaged}}
	managedCacheOnly := Effective{Managed: true, Authority: []string{AuthorityExtractCache}}
	unmanagedUmbrella := Effective{Managed: false, Authority: []string{AuthorityExtractManaged}}

	cases := []struct {
		name              string
		eff               Effective
		acceptedAuthority []string
		want              []string
	}{
		{name: "nil accepted authority", eff: managedUmbrella, acceptedAuthority: nil, want: nil},
		{
			name:              "managed umbrella, umbrella accepted",
			eff:               managedUmbrella,
			acceptedAuthority: []string{AuthorityExtractManaged},
			want:              []string{AuthorityExtractManaged},
		},
		{
			name:              "unmanaged umbrella authorizes nothing even if accepted",
			eff:               unmanagedUmbrella,
			acceptedAuthority: []string{AuthorityExtractManaged},
			want:              nil,
		},
		{
			name:              "per-tier cache grant, cache token accepted",
			eff:               managedCacheOnly,
			acceptedAuthority: []string{AuthorityExtractCache},
			want:              []string{AuthorityExtractCache},
		},
		{
			name:              "accepted authority not among the authorized tiers",
			eff:               managedCacheOnly,
			acceptedAuthority: []string{AuthorityExtractRouting},
			want:              nil,
		},
		{
			name:              "high-sensitivity token never satisfied by the umbrella",
			eff:               managedUmbrella,
			acceptedAuthority: []string{AuthorityExtractCodeintel},
			want:              nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractionTokensInForce(tc.eff, tc.acceptedAuthority)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ExtractionTokensInForce(%+v, %v) = %v, want %v", tc.eff, tc.acceptedAuthority, got, tc.want)
			}
		})
	}
}
