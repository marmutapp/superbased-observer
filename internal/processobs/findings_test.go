package processobs

import "testing"

func TestDeriveFindings(t *testing.T) {
	t.Parallel()

	attr := func(f RunFacts) RunFacts {
		f.Attributed = true
		if f.SessionID == "" {
			f.SessionID = "sess-1"
		}
		return f
	}

	cases := []struct {
		name string
		run  RunFacts
		want []FindingRuleID // rule ids expected, in catalog order
	}{
		{
			name: "plain non-root exec — no findings",
			run:  attr(RunFacts{ProcessKey: "k1", ExePath: "/usr/bin/node", ExeBasename: "node", UID: 1000, EUID: 1000}),
			want: nil,
		},
		{
			name: "setuid-root (euid 0, uid non-root) → privileged_exec",
			run:  attr(RunFacts{ProcessKey: "k2", ExePath: "/usr/bin/sudo", ExeBasename: "sudo", UID: 1000, EUID: 0}),
			want: []FindingRuleID{FindingPrivilegedExec},
		},
		{
			name: "genuine root session (uid 0 euid 0) → not flagged (container noise)",
			run:  attr(RunFacts{ProcessKey: "k3", ExePath: "/bin/bash", ExeBasename: "bash", UID: 0, EUID: 0}),
			want: nil,
		},
		{
			name: "exe under /tmp → executable_from_tmp",
			run:  attr(RunFacts{ProcessKey: "k4", ExePath: "/tmp/build-xyz/payload", ExeBasename: "payload", UID: 1000, EUID: 1000}),
			want: []FindingRuleID{FindingExecutableFromTmp},
		},
		{
			name: "exe under windows AppData Local Temp → executable_from_tmp",
			run:  attr(RunFacts{ProcessKey: "k5", ExePath: `C:\Users\x\AppData\Local\Temp\go-build\a.exe`, ExeBasename: "a.exe", UID: 1000, EUID: 1000}),
			want: []FindingRuleID{FindingExecutableFromTmp},
		},
		{
			name: "exe under ~/.cache → executable_from_tmp",
			run:  attr(RunFacts{ProcessKey: "k6", ExePath: "/home/u/.cache/uv/bin/tool", ExeBasename: "tool", UID: 1000, EUID: 1000}),
			want: []FindingRuleID{FindingExecutableFromTmp},
		},
		{
			name: "both rules fire on one run, catalog order",
			run:  attr(RunFacts{ProcessKey: "k7", ExePath: "/tmp/escalate", ExeBasename: "escalate", UID: 1000, EUID: 0}),
			want: []FindingRuleID{FindingPrivilegedExec, FindingExecutableFromTmp},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DeriveFindings([]RunFacts{tc.run})
			if len(got) != len(tc.want) {
				t.Fatalf("got %d findings %+v, want %d %v", len(got), got, len(tc.want), tc.want)
			}
			for i, f := range got {
				if f.RuleID != tc.want[i] {
					t.Errorf("finding[%d] = %s, want %s", i, f.RuleID, tc.want[i])
				}
				if f.ProcessKey != tc.run.ProcessKey || f.SessionID != tc.run.SessionID {
					t.Errorf("finding[%d] identity mismatch: %+v", i, f)
				}
				if f.Severity == "" || f.Detail == "" {
					t.Errorf("finding[%d] missing severity/detail: %+v", i, f)
				}
			}
		})
	}
}

func TestDeriveFindingsSkipsUnattributed(t *testing.T) {
	t.Parallel()
	// A setuid-root run that is NOT attributed must produce no finding — a
	// signal with no session owner is noise.
	runs := []RunFacts{
		{ProcessKey: "u1", Attributed: false, ExePath: "/usr/bin/sudo", ExeBasename: "sudo", UID: 1000, EUID: 0},
		{ProcessKey: "u2", Attributed: true, SessionID: "", ExePath: "/tmp/x", ExeBasename: "x", UID: 1000, EUID: 1000},
	}
	if got := DeriveFindings(runs); len(got) != 0 {
		t.Errorf("unattributed/owner-less runs produced findings: %+v", got)
	}
}
