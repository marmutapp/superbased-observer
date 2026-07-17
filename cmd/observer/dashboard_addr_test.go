package main

import "testing"

// TestResolveDashboardAddr pins the durable dashboard listen-address precedence
// ladder (issue #8): explicit flag override > OBSERVER_DASHBOARD_ADDR env >
// [dashboard].addr config > the caller's built-in default. flagVal is passed
// non-empty ONLY when the caller detected the flag was set (cobra Changed).
func TestResolveDashboardAddr(t *testing.T) {
	cases := []struct {
		name       string
		flagVal    string
		env        string
		configAddr string
		def        string
		want       string
	}{
		{"default when all empty", "", "", "", "127.0.0.1:8081", "127.0.0.1:8081"},
		{"config over default", "", "", "127.0.0.1:8082", "127.0.0.1:8081", "127.0.0.1:8082"},
		{"env over config", "", "127.0.0.1:9000", "127.0.0.1:8082", "127.0.0.1:8081", "127.0.0.1:9000"},
		{"flag over env and config", "127.0.0.1:7000", "127.0.0.1:9000", "127.0.0.1:8082", "127.0.0.1:8081", "127.0.0.1:7000"},
		{"whitespace flag falls through to config", "   ", "", "127.0.0.1:8082", "127.0.0.1:8081", "127.0.0.1:8082"},
		{"whitespace config falls through to default", "", "", "   ", "127.0.0.1:8081", "127.0.0.1:8081"},
		// #3: a MALFORMED env value is ignored — a valid flag still wins, and
		// with no flag it falls through to config/default rather than binding
		// a garbage address.
		{"valid flag wins over garbage env", "127.0.0.1:7000", "garbage", "127.0.0.1:8082", "127.0.0.1:8081", "127.0.0.1:7000"},
		{"garbage env falls through to config", "", "not-an-addr", "127.0.0.1:8082", "127.0.0.1:8081", "127.0.0.1:8082"},
		{"port-out-of-range env falls through to default", "", "127.0.0.1:70000", "", "127.0.0.1:8081", "127.0.0.1:8081"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.env == "" {
				t.Setenv("OBSERVER_DASHBOARD_ADDR", "")
			} else {
				t.Setenv("OBSERVER_DASHBOARD_ADDR", tc.env)
			}
			if got := resolveDashboardAddr(tc.flagVal, tc.configAddr, tc.def); got != tc.want {
				t.Errorf("resolveDashboardAddr(%q,%q,%q) with env=%q = %q, want %q",
					tc.flagVal, tc.configAddr, tc.def, tc.env, got, tc.want)
			}
		})
	}
}
