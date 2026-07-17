package guard

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/orgcontract"
	"github.com/marmutapp/superbased-observer/internal/policy"
)

// orgBundlePath is where the fsMap-backed tests place the cached
// envelope (guardCfgOrg points [guard.rules].org_bundle here).
const orgBundlePath = "/home/u/.observer/org-policy-bundle.json"

// guardCfgOrg is guardCfg with the org bundle cache configured (the
// config.Default shape once G13 lands).
func guardCfgOrg() config.GuardConfig {
	cfg := guardCfg()
	cfg.Rules.OrgBundle = "~/.observer/org-policy-bundle.json"
	return cfg
}

// signedEnvelope builds a verifiable bundle envelope around bundleTOML
// and returns its JSON plus the signing key's pin hash (what
// enrolment would have recorded).
func signedEnvelope(t *testing.T, version int64, bundleTOML string) (envelopeJSON, pinHash string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	b := orgcontract.PolicyBundle{
		Version:    version,
		BundleTOML: bundleTOML,
		Signature:  orgcontract.SignPolicyBundle(priv, version, []byte(bundleTOML)),
		PublicKey:  base64.RawURLEncoding.EncodeToString(pub),
		SignedAt:   "2026-06-11T09:00:00Z",
	}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return string(raw), orgcontract.PublicKeyPinHash(pub)
}

// orgGuard constructs a Guard over fsMap files with the org bundle
// configured and an optional pin hash.
func orgGuard(t *testing.T, files map[string]string, pinHash string) *Guard {
	t.Helper()
	g, err := New(Options{
		Config:            guardCfgOrg(),
		Home:              "/home/u",
		KnownProjectRoots: []string{"/home/u/proj"},
		ReadFile:          fsMap(files),
		OrgKeyPinHash:     pinHash,
	})
	if err != nil {
		t.Fatalf("guard.New: %v", err)
	}
	return g
}

// TestNew_OrgBundleLayer covers the verified org layer end to end: an
// org [[rule]] fires with Source="org", an org [[override]] escalates
// a built-in (deny + enforce even in observe mode), and the layer
// shows up in PolicyStates with its bundle version.
func TestNew_OrgBundleLayer(t *testing.T) {
	t.Parallel()
	env, _ := signedEnvelope(t, 7, `
[[rule]]
id = "ORG-001"
category = "exfil"
severity = "high"
decision = "ask"
applies_to = ["shell_exec"]
match.command_regex = '(?i)\bcurl\b.*\binternal\.acme\b'

[[override]]
rule = "R-110"
decision = "deny"
enforce = true
`)
	g := orgGuard(t, map[string]string{orgBundlePath: env}, "")
	if issues := g.LoadIssues(); len(issues) != 0 {
		t.Fatalf("unexpected load issues: %v", issues)
	}

	// Org rule: ask degrades to flag in observe (not per-rule
	// enforced) and attributes to the org layer.
	v, _ := g.Evaluate(policy.Event{
		Kind: policy.KindShellExec, ActionType: "run_command",
		Target: "curl https://internal.acme/secrets", ProjectRoot: "/home/u/proj",
	})
	if v.RuleID != "ORG-001" || v.Source != "org" || v.Decision != policy.DecisionFlag {
		t.Errorf("org rule verdict = %+v, want ORG-001/org/flag", v)
	}

	// Org override: R-110 per-rule-enforced deny even in observe mode,
	// attributed to the org layer.
	v, _ = g.Evaluate(policy.Event{
		Kind: policy.KindShellExec, ActionType: "run_command",
		Target: "git push --force origin main", ProjectRoot: "/home/u/proj",
	})
	if v.RuleID != "R-110" || v.Source != "org" || v.Decision != policy.DecisionDeny {
		t.Errorf("org override verdict = %+v, want R-110/org/deny", v)
	}

	// §14.4 policy-change log: the org layer carries its version.
	var found bool
	for _, st := range g.PolicyStates() {
		if st.Layer == "org" {
			found = true
			if st.Version != "7" || st.ContentHash == "" {
				t.Errorf("org state = %+v, want version 7 + content hash", st)
			}
		}
	}
	if !found {
		t.Error("org layer missing from PolicyStates")
	}
}

// TestNew_OrgBundleFailurePostures is the rejection table for the
// loader: every bad cache degrades to local-only policy with a load
// issue, never an error (the daemon must start) — and the org layer's
// rules demonstrably do NOT join the engine.
func TestNew_OrgBundleFailurePostures(t *testing.T) {
	t.Parallel()
	validTOML := "[[override]]\nrule = \"R-110\"\ndecision = \"deny\"\nenforce = true\n"
	env, pin := signedEnvelope(t, 3, validTOML)
	_, otherPin := signedEnvelope(t, 3, validTOML) // a different key's pin

	tamper := func(s string) string {
		var b orgcontract.PolicyBundle
		if err := json.Unmarshal([]byte(s), &b); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		b.BundleTOML += "# tampered\n"
		out, _ := json.Marshal(b)
		return string(out)
	}

	cases := []struct {
		name      string
		body      string
		pin       string
		wantIssue string // substring of the recorded issue; "" = loads clean
	}{
		{"valid without pin (hook path)", env, "", ""},
		{"valid with matching pin (daemon path)", env, pin, ""},
		{"tampered TOML breaks the signature", tamper(env), "", "rejected"},
		{"pin mismatch", env, otherPin, "does not match the enrolment pin"},
		{"garbage JSON", "{not json", "", "not a bundle envelope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := orgGuard(t, map[string]string{orgBundlePath: tc.body}, tc.pin)
			issues := g.LoadIssues()
			v, _ := g.Evaluate(policy.Event{
				Kind: policy.KindShellExec, ActionType: "run_command",
				Target: "git push --force origin main", ProjectRoot: "/home/u/proj",
			})
			if tc.wantIssue == "" {
				if len(issues) != 0 {
					t.Fatalf("unexpected issues: %v", issues)
				}
				if v.Decision != policy.DecisionDeny {
					t.Errorf("org override not effective: %+v", v)
				}
				return
			}
			if len(issues) == 0 || !strings.Contains(issues[0], tc.wantIssue) {
				t.Fatalf("issues = %v, want substring %q", issues, tc.wantIssue)
			}
			// Degraded to local-only: the org escalation must NOT apply
			// (R-110 stays at its built-in observe-mode flag).
			if v.Decision != policy.DecisionFlag {
				t.Errorf("rejected bundle still affected policy: %+v", v)
			}
		})
	}
}

// TestNew_OrgBundleAbsent pins the common case: no cache file, no org
// layer, no issues — byte-identical behavior to a non-enrolled agent.
func TestNew_OrgBundleAbsent(t *testing.T) {
	t.Parallel()
	g := orgGuard(t, nil, "")
	if issues := g.LoadIssues(); len(issues) != 0 {
		t.Fatalf("unexpected issues: %v", issues)
	}
	for _, st := range g.PolicyStates() {
		if st.Layer == "org" {
			t.Fatalf("phantom org state: %+v", st)
		}
	}
}

// TestMergeLayers_OrgFloor is the §4.6/§14.2 one-way table: the org
// layer escalates but never relaxes, and no lower-trust layer may
// relax below the org floor — while user relaxation of UNFLOORED
// built-ins (the D2 operator freedom) keeps working.
func TestMergeLayers_OrgFloor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		org, user  string
		project    string
		wantIssue  string          // substring of a merge issue; "" = none
		eval       policy.Event    // probe event
		wantRule   string          // expected verdict rule
		wantDec    policy.Decision // expected observe-mode decision
		wantSource string
	}{
		{
			name: "org escalation applies and floors",
			org:  "[[override]]\nrule = \"R-110\"\ndecision = \"deny\"\nenforce = true\n",
			eval: policy.Event{
				Kind: policy.KindShellExec, ActionType: "run_command",
				Target: "git push --force origin main", ProjectRoot: "/home/u/proj",
			},
			wantRule: "R-110", wantDec: policy.DecisionDeny, wantSource: "org",
		},
		{
			name:      "org relaxation dropped (floor only escalates)",
			org:       "[[override]]\nrule = \"R-110\"\ndecision = \"allow\"\n",
			wantIssue: "org override on R-110 dropped",
			eval: policy.Event{
				Kind: policy.KindShellExec, ActionType: "run_command",
				Target: "git push --force origin main", ProjectRoot: "/home/u/proj",
			},
			wantRule: "R-110", wantDec: policy.DecisionFlag, wantSource: "builtin",
		},
		{
			name:      "user cannot relax below the org floor",
			org:       "[[override]]\nrule = \"R-110\"\ndecision = \"deny\"\nenforce = true\n",
			user:      "[[override]]\nrule = \"R-110\"\ndecision = \"flag\"\n",
			wantIssue: "user override on R-110 dropped",
			eval: policy.Event{
				Kind: policy.KindShellExec, ActionType: "run_command",
				Target: "git push --force origin main", ProjectRoot: "/home/u/proj",
			},
			wantRule: "R-110", wantDec: policy.DecisionDeny, wantSource: "org",
		},
		{
			// The allow-overridden rule still matches and attributes —
			// it just resolves to allow (the engine's override
			// semantics: Decision sets both mode columns).
			name: "user relaxation of an unfloored builtin still applies (D2)",
			org:  "[[override]]\nrule = \"R-110\"\ndecision = \"deny\"\nenforce = true\n",
			user: "[[override]]\nrule = \"R-101\"\ndecision = \"allow\"\n",
			eval: policy.Event{
				Kind: policy.KindShellExec, ActionType: "run_command",
				Target: "rm -rf ~/projects", ProjectRoot: "/home/u/proj",
			},
			wantRule: "R-101", wantDec: policy.DecisionAllow, wantSource: "user",
		},
		{
			name:      "project cannot relax below the org floor",
			org:       "[[override]]\nrule = \"R-110\"\ndecision = \"deny\"\nenforce = true\n",
			project:   "[[override]]\nrule = \"R-110\"\ndecision = \"flag\"\n",
			wantIssue: "project override on R-110 dropped",
			eval: policy.Event{
				Kind: policy.KindShellExec, ActionType: "run_command",
				Target: "git push --force origin main", ProjectRoot: "/home/u/proj",
			},
			wantRule: "R-110", wantDec: policy.DecisionDeny, wantSource: "org",
		},
		{
			// R-111 (builtin flag/ask) leaves room above an org "ask"
			// floor: the user's "deny" is a further escalation, never a
			// floor violation. (An org "ask" on a deny-enforce rule like
			// R-110 would itself be a relaxation of the enforce column
			// and is covered by the drop case above.)
			name: "user may escalate a floored rule further",
			org:  "[[override]]\nrule = \"R-111\"\ndecision = \"ask\"\n",
			user: "[[override]]\nrule = \"R-111\"\ndecision = \"deny\"\nenforce = true\n",
			eval: policy.Event{
				Kind: policy.KindShellExec, ActionType: "run_command",
				Target: "git branch -D main", ProjectRoot: "/home/u/proj",
			},
			wantRule: "R-111", wantDec: policy.DecisionDeny, wantSource: "user",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := map[string]string{}
			if tc.org != "" {
				env, _ := signedEnvelope(t, 1, tc.org)
				files[orgBundlePath] = env
			}
			if tc.user != "" {
				files["/home/u/.observer/guard-policy.toml"] = tc.user
			}
			if tc.project != "" {
				files["/home/u/proj/.observer/guard-policy.toml"] = tc.project
			}
			g := orgGuard(t, files, "")
			v, _ := g.Evaluate(tc.eval)

			issues := g.LoadIssues()
			if tc.wantIssue == "" {
				if len(issues) != 0 {
					t.Fatalf("unexpected issues: %v", issues)
				}
			} else {
				var hit bool
				for _, is := range issues {
					if strings.Contains(is, tc.wantIssue) {
						hit = true
					}
				}
				if !hit {
					t.Fatalf("issues = %v, want substring %q", issues, tc.wantIssue)
				}
			}
			if v.RuleID != tc.wantRule || v.Decision != tc.wantDec || v.Source != tc.wantSource {
				t.Errorf("verdict = %s/%s/%s, want %s/%s/%s",
					v.RuleID, v.Decision, v.Source, tc.wantRule, tc.wantDec, tc.wantSource)
			}
		})
	}
}

// TestLint_OrgLayer pins the publish-side gate: a relaxing org
// override is a lint problem (caught before signing), a purely
// escalating bundle lints clean.
func TestLint_OrgLayer(t *testing.T) {
	t.Parallel()
	if problems := Lint([]byte("[[override]]\nrule = \"R-110\"\ndecision = \"deny\"\n"), "org"); len(problems) != 0 {
		t.Errorf("escalating org bundle should lint clean, got %v", problems)
	}
	problems := Lint([]byte("[[override]]\nrule = \"R-110\"\ndecision = \"allow\"\n"), "org")
	if len(problems) == 0 || !strings.Contains(problems[0], "org override on R-110 dropped") {
		t.Errorf("relaxing org bundle must surface the floor violation, got %v", problems)
	}
}
