package orgcontract

import "testing"

// TestManagedIntegrityReportState pins the counts→state derivation the admin
// Control Center badge renders (Arc 4 P6b, plan §9).
func TestManagedIntegrityReportState(t *testing.T) {
	cases := []struct {
		name     string
		siblings int
		drift    int
		want     string
	}{
		{"clean", 0, 0, ManagedIntegrityOK},
		{"sibling only", 2, 0, ManagedIntegritySibling},
		{"drift only", 0, 3, ManagedIntegrityRouteDrift},
		{"both", 1, 1, ManagedIntegrityBoth},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := ManagedIntegrityReport{SiblingObservers: tc.siblings, RouteDrift: tc.drift}
			if got := r.State(); got != tc.want {
				t.Fatalf("State() = %q, want %q", got, tc.want)
			}
		})
	}
}
