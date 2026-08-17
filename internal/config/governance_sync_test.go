package config

import (
	"reflect"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/policyfam/nodegov"
)

// TestPinnableKeysMirrorInSyncWithPolicyfam pins internal/config's hand copy
// of the pinnable-key table to internal/policyfam/nodegov.PinnableKeys, the
// one owner.
//
// The copy exists because internal/config must not import the policy layer
// at runtime (see governancePinnableKeys' doc comment), and it is the exact
// shape that drifted before: TestPolicyResourceSupportedFamiliesMatchesPolicyfam
// in this package was written after gateway.providers shipped in policyfam
// and was missing here, which made accept_families reject the family so
// every gateway resource landed delivered_unaccepted. A test-only import
// closes the drift class without coupling the packages.
//
// A drift here is worse than that one: a key present in nodegov but missing
// here compiles cleanly at publish, reaches the sidecar, and is then SKIPPED
// by every reader — a node that reports `effective` while ignoring the pin,
// which is precisely the false compliance claim Phase 1b exists to remove.
func TestPinnableKeysMirrorInSyncWithPolicyfam(t *testing.T) {
	if len(governancePinnableKeys) != len(nodegov.PinnableKeys) {
		t.Fatalf("the config mirror has %d pinnable keys, policyfam/nodegov has %d — a key present only in nodegov would be published, delivered, and then silently skipped by every reader",
			len(governancePinnableKeys), len(nodegov.PinnableKeys))
	}
	for _, want := range nodegov.PinnableKeys {
		got, ok := governancePinnableByKey[want.Key]
		if !ok {
			t.Errorf("policyfam/nodegov pins %q but the config mirror does not", want.Key)
			continue
		}
		if got.Kind != want.Kind {
			t.Errorf("%q: mirror Kind %q, nodegov Kind %q", want.Key, got.Kind, want.Kind)
		}
		if got.Direction != string(want.Direction) {
			t.Errorf("%q: mirror Direction %q, nodegov Direction %q", want.Key, got.Direction, want.Direction)
		}
		if !reflect.DeepEqual(got.Enum, want.Enum) {
			t.Errorf("%q: mirror Enum %v, nodegov Enum %v", want.Key, got.Enum, want.Enum)
		}
		if !governanceValuesEqual(got.Safe, want.Safe) && !(got.Safe == nil && want.Safe == nil) {
			t.Errorf("%q: mirror Safe %v, nodegov Safe %v", want.Key, got.Safe, want.Safe)
		}
	}
}
