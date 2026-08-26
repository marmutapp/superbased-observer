package govern

import "testing"

// TestSourceForBool pins the attribution column of the Privacy page's sharing
// table, one row per reachable outcome.
//
// The load-bearing rows are the two `local=false, org=true` ones. Before
// ShareSourceOrgRaised existed they BOTH fell through to ShareSourceLocal —
// so on a managed node, where RaiseBool really does turn the tier on, the
// surface named the developer as the source of a decision their organisation
// made. Splitting them on Managed is the whole fix, and the unmanaged row is
// what proves the split did not widen anything: raising stays inert on the
// individual plane, and there the source is still "you".
func TestSourceForBool(t *testing.T) {
	const key = "cache_detail"
	cases := []struct {
		name    string
		managed bool
		// share is the org's delivered directive block; nil means the org
		// published nothing at all for this key.
		share map[string]any
		local bool
		want  ShareSource
		// wantEffective is MergeBool over the same inputs. It is asserted in
		// the same row on purpose: the "In force" and "Source" columns are
		// rendered side by side, so a case where they disagree is a bug the
		// table must be able to see.
		wantEffective bool
	}{
		{
			name:  "org published nothing",
			local: true,
			want:  ShareSourceLocal, wantEffective: true,
		},
		{
			name:  "org lowered below the node's setting",
			share: map[string]any{key: false},
			local: true,
			want:  ShareSourceOrg, wantEffective: false,
		},
		{
			name:  "org pinned the node at its own setting",
			share: map[string]any{key: true},
			local: true,
			want:  ShareSourceBoth, wantEffective: true,
		},
		{
			name:    "managed node: org raised above the node's setting",
			managed: true,
			share:   map[string]any{key: true},
			local:   false,
			want:    ShareSourceOrgRaised, wantEffective: true,
		},
		{
			name:  "individual node: the same raise is inert",
			share: map[string]any{key: true},
			local: false,
			want:  ShareSourceLocal, wantEffective: false,
		},
		{
			name:    "managed node: org agreed with a local false",
			managed: true,
			share:   map[string]any{key: false},
			local:   false,
			want:    ShareSourceLocal, wantEffective: false,
		},
		{
			name:  "org published nothing, node at the floor",
			local: false,
			want:  ShareSourceLocal, wantEffective: false,
		},
		{
			name:    "malformed directive is not authority to change anything",
			managed: true,
			share:   map[string]any{key: "yes"},
			local:   false,
			want:    ShareSourceLocal, wantEffective: false,
		},
		{
			name:  "a directive on a DIFFERENT key does not attribute this one",
			share: map[string]any{"full_content": false},
			local: true,
			want:  ShareSourceLocal, wantEffective: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := Effective{Managed: tc.managed, Share: tc.share}
			if got := e.SourceForBool(key, tc.local); got != tc.want {
				t.Errorf("SourceForBool(%q, %v) = %q, want %q", key, tc.local, got, tc.want)
			}
			if got := e.MergeBool(key, tc.local); got != tc.wantEffective {
				t.Errorf("MergeBool(%q, %v) = %v, want %v", key, tc.local, got, tc.wantEffective)
			}
		})
	}
}

// TestMergeBoolAppliesLowerThenRaise pins the ORDER of the two halves, which
// is the only thing that makes MergeBool a faithful mirror of the push seam
// (cmd/observer.lowerShareOptions): lowering runs first, and on a managed
// node the raise can then lift the tier back. Raise-then-lower would leave a
// managed node's `false` directive winning, which is a different posture.
func TestMergeBoolAppliesLowerThenRaise(t *testing.T) {
	e := Effective{Managed: true, Share: map[string]any{"full_content": true}}
	if !e.MergeBool("full_content", false) {
		t.Error("managed raise did not survive the lowering pass")
	}
	// A false directive lowers and there is nothing to raise back to.
	e = Effective{Managed: true, Share: map[string]any{"full_content": false}}
	if e.MergeBool("full_content", true) {
		t.Error("org `false` on a managed node failed to lower")
	}
}

// TestShareSourceWireValuesAreStable pins the on-the-wire strings. They are
// the discriminant of web/src/lib/governance.ts's GovernanceShareSource union
// and of SHARE_SOURCE_LABEL's key set; renaming one here without renaming it
// there renders an unlabelled row.
func TestShareSourceWireValuesAreStable(t *testing.T) {
	want := map[ShareSource]string{
		ShareSourceLocal:     "you",
		ShareSourceOrg:       "org",
		ShareSourceBoth:      "both",
		ShareSourceOrgRaised: "org_raised",
	}
	for got, w := range want {
		if string(got) != w {
			t.Errorf("share source wire value = %q, want %q", string(got), w)
		}
	}
}
