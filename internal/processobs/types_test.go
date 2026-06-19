package processobs

import "testing"

// TestProcessKeyStabilityAndPIDReuse pins §9.3: the key is a pure function
// of (boot_id, pid, start_time), so the same process always hashes the
// same, and a reused pid with a different start time NEVER collides with
// the earlier process — the foundation of PID-reuse refusal.
func TestProcessKeyStabilityAndPIDReuse(t *testing.T) {
	t.Parallel()
	base := ProcessKey("boot-1", 100, 5000)
	if base == "" {
		t.Fatal("empty key")
	}
	if got := ProcessKey("boot-1", 100, 5000); got != base {
		t.Errorf("key not stable: %q != %q", got, base)
	}

	cases := []struct {
		name              string
		boot              string
		pid               int
		start             int64
		wantDifferentFrom bool // must differ from base
	}{
		{"same", "boot-1", 100, 5000, false},
		{"reused_pid_new_start", "boot-1", 100, 6000, true},
		{"different_pid", "boot-1", 101, 5000, true},
		{"different_boot", "boot-2", 100, 5000, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ProcessKey(tc.boot, tc.pid, tc.start)
			if tc.wantDifferentFrom && got == base {
				t.Errorf("key collided with base for %s", tc.name)
			}
			if !tc.wantDifferentFrom && got != base {
				t.Errorf("key changed for %s", tc.name)
			}
		})
	}
}

func TestProcessRunAttributed(t *testing.T) {
	t.Parallel()
	var r ProcessRun
	if r.Attributed() {
		t.Error("zero run must be unattributed")
	}
	r.Attribution = Attribution{SessionID: "s1", Source: AttrBridge}
	if !r.Attributed() {
		t.Error("run with session+source must be attributed")
	}
	r.Attribution.Source = AttrNone
	if r.Attributed() {
		t.Error("AttrNone is unattributed even with a session id")
	}
}
