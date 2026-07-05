package dashboard

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"testing"

	orgapi "github.com/marmutapp/superbased-observer/internal/orgserver/api"
	gen "github.com/marmutapp/superbased-observer/internal/orgserver/dashboard/gen"
	"github.com/marmutapp/superbased-observer/internal/orgserver/rollup"
)

// escalatingOrgTOML lints clean as an org bundle (the api/policy_test
// fixture); relaxingOrgTOML violates the §4.6 escalate-only floor.
const (
	escalatingOrgTOML = "[[override]]\nrule = \"R-110\"\ndecision = \"deny\"\nenforce = true\n"
	relaxingOrgTOML   = "[[override]]\nrule = \"R-110\"\ndecision = \"allow\"\n"
)

// newGuardAPI builds the api_test.go org fixture plus the §14.5 guard roles
// (dave = policy_admin, eve = security_viewer) and a small guard_events set:
// alice (team-a lead) has 2 events incl. one R-001 deny; bob (team-b lead)
// has 1 event with a BROKEN chain (missing predecessor + a genesis head).
func newGuardAPI(t *testing.T, signer PolicySigner) *API {
	t.Helper()
	a := newAPIWithData(t)
	a.policyAdmins = emailSet([]string{"dave@acme.example"})
	a.secViewers = emailSet([]string{"eve@acme.example"})
	a.policySigner = signer

	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := a.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// dave/eve carry the §14.5 roles via the config lists above; frank is a
	// provisioned plain member (no role, leads nothing).
	for _, u := range [][2]string{{"u-dave", "dave@acme.example"}, {"u-eve", "eve@acme.example"}, {"u-frank", "frank@acme.example"}} {
		exec(`INSERT INTO org_members (user_id, user_name, email, active, created_at, updated_at) VALUES (?, ?, ?, 1, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, u[0], u[1], u[1])
	}
	ge := func(user, ts, rule, decision string, enforced int, prev, hash string) {
		exec(`INSERT INTO guard_events (chain_hash, user_id, timestamp, rule_id, category, severity, decision, enforced, chain_prev, pushed_at, pushed_by_user_id)
		      VALUES (?, ?, ?, ?, 'destructive', 'high', ?, ?, ?, '2026-06-01T00:00:00Z', ?)`,
			hash, user, ts, rule, decision, enforced, prev, user)
	}
	now := a.now() // handlers window against real now; keep events recent
	day := now.AddDate(0, 0, -1).Format("2006-01-02T15:04:05Z")
	ge("u-alice", day, "R-001", "deny", 1, "", "ga0")
	ge("u-alice", day, "R-110", "flag", 0, "ga0", "ga1")
	ge("u-bob", day, "R-001", "deny", 1, "", "gb0")
	ge("u-bob", day, "R-001", "flag", 0, "missing", "gb1")
	return a
}

func staticSigner(t *testing.T) (PolicySigner, ed25519.PrivateKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	return func() (ed25519.PrivateKey, error) { return priv, nil }, priv
}

func TestGuardRollups_RoleScoping(t *testing.T) {
	a := newGuardAPI(t, nil)

	cases := []struct {
		name       string
		userID     string
		wantEvents int64
	}{
		{"admin_sees_org_wide", "u-boss", 4},
		{"policy_admin_sees_org_wide", "u-dave", 4},
		{"security_viewer_sees_org_wide", "u-eve", 4},
		{"lead_sees_their_team_only", "u-alice", 2}, // alice leads team-a
		{"plain_member_sees_empty_scope", "u-frank", 0},
		{"deprovisioned_user_sees_empty_scope", "u-gone", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := do(tc.userID, "GET", "/api/org/guard/overview", nil, func(w http.ResponseWriter, r *http.Request) {
				a.OrgGuardOverview(w, r, gen.OrgGuardOverviewParams{})
			})
			if w.Code != http.StatusOK {
				t.Fatalf("overview = %d, want 200; body=%s", w.Code, w.Body.String())
			}
			var res rollup.GuardOverviewResult
			if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if res.TotalEvents != tc.wantEvents {
				t.Errorf("total events = %d, want %d", res.TotalEvents, tc.wantEvents)
			}
		})
	}

	// Unauthenticated → 401 (no user id in context).
	w := do("", "GET", "/api/org/guard/overview", nil, func(w http.ResponseWriter, r *http.Request) {
		a.OrgGuardOverview(w, r.WithContext(context.Background()), gen.OrgGuardOverviewParams{})
	})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated overview = %d, want 401", w.Code)
	}
}

func TestGuardAgents_AuditedDisclosure(t *testing.T) {
	a := newGuardAPI(t, nil)

	w := do("u-eve", "GET", "/api/org/guard/agents", nil, a.OrgGuardAgents)
	if w.Code != http.StatusOK {
		t.Fatalf("agents = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var res rollup.GuardAgentsResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// bob's chain is broken (1 head + 1 unlinked) and sorts first.
	if len(res.Agents) != 2 || res.Agents[0].UserID != "u-bob" || !res.Agents[0].Broken {
		t.Errorf("agents = %+v, want bob broken-first", res.Agents)
	}

	var n int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = ?`, rollup.ActionViewGuardAgents).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("audit rows = %d, want 1 (disclosure must be audited)", n)
	}
}

func TestGuardPolicyBundles_ReadGate(t *testing.T) {
	signer, priv := staticSigner(t)
	a := newGuardAPI(t, signer)
	if _, err := orgapi.PublishPolicyBundle(context.Background(), a.db, priv, escalatingOrgTOML, "cli", "v1"); err != nil {
		t.Fatalf("seed publish: %v", err)
	}

	// A team lead is NOT a guard policy reader → 403.
	if w := do("u-alice", "GET", "/api/org/guard/policy/bundles", nil, a.OrgGuardPolicyBundles); w.Code != http.StatusForbidden {
		t.Errorf("lead bundles = %d, want 403", w.Code)
	}

	// A security_viewer reads history + content.
	w := do("u-eve", "GET", "/api/org/guard/policy/bundles", nil, a.OrgGuardPolicyBundles)
	if w.Code != http.StatusOK {
		t.Fatalf("viewer bundles = %d; body=%s", w.Code, w.Body.String())
	}
	var list rollup.GuardPolicyBundlesResult
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.ActiveVersion != 1 || !list.SigningConfigured || len(list.Bundles) != 1 || list.Bundles[0].CreatedBy != "cli" {
		t.Errorf("bundles list = %+v", list)
	}

	w = do("u-eve", "GET", "/api/org/guard/policy/bundles/1", nil, func(w http.ResponseWriter, r *http.Request) {
		a.OrgGuardPolicyBundleDetail(w, r, 1)
	})
	if w.Code != http.StatusOK {
		t.Fatalf("detail = %d", w.Code)
	}
	var det rollup.GuardPolicyBundleDetail
	if err := json.Unmarshal(w.Body.Bytes(), &det); err != nil {
		t.Fatal(err)
	}
	if det.Version != 1 || det.BundleTOML != escalatingOrgTOML {
		t.Errorf("detail = %+v", det)
	}

	// Unknown version → 404.
	if w := do("u-eve", "GET", "/api/org/guard/policy/bundles/99", nil, func(w http.ResponseWriter, r *http.Request) {
		a.OrgGuardPolicyBundleDetail(w, r, 99)
	}); w.Code != http.StatusNotFound {
		t.Errorf("unknown version = %d, want 404", w.Code)
	}
}

func TestGuardPolicyLint(t *testing.T) {
	a := newGuardAPI(t, nil)

	// security_viewer cannot lint (authoring surface) → 403.
	if w := do("u-eve", "POST", "/api/org/guard/policy/lint",
		map[string]string{"bundle_toml": escalatingOrgTOML}, a.OrgGuardPolicyLint); w.Code != http.StatusForbidden {
		t.Errorf("viewer lint = %d, want 403", w.Code)
	}

	// policy_admin: clean bundle → ok, override dry-run computable with the
	// seeded R-110 hit; a declared rule id reports computable=false.
	draft := escalatingOrgTOML +
		"[[rule]]\nid = \"ORG-1\"\ncategory = \"boundary\"\ndecision = \"flag\"\nmatch.command_base = \"scp\"\n"
	w := do("u-dave", "POST", "/api/org/guard/policy/lint", map[string]string{"bundle_toml": draft}, a.OrgGuardPolicyLint)
	if w.Code != http.StatusOK {
		t.Fatalf("lint = %d; body=%s", w.Code, w.Body.String())
	}
	var res rollup.GuardPolicyLintResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.OK || len(res.Problems) != 0 {
		t.Fatalf("lint result = %+v, want ok", res)
	}
	if len(res.DryRun) != 2 {
		t.Fatalf("dry-run = %+v, want [R-110, ORG-1]", res.DryRun)
	}
	if d := res.DryRun[0]; d.RuleID != "R-110" || !d.Computable || d.Hits != 1 || d.Agents != 1 {
		t.Errorf("override dry-run = %+v, want R-110 computable hits=1", d)
	}
	if d := res.DryRun[1]; d.RuleID != "ORG-1" || d.Computable || d.Hits != 0 {
		t.Errorf("declared dry-run = %+v, want ORG-1 not-computable", d)
	}

	// A relaxing bundle fails the same refusal the publish gate applies.
	w = do("u-dave", "POST", "/api/org/guard/policy/lint", map[string]string{"bundle_toml": relaxingOrgTOML}, a.OrgGuardPolicyLint)
	if w.Code != http.StatusOK {
		t.Fatalf("relaxing lint = %d", w.Code)
	}
	res = rollup.GuardPolicyLintResult{}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.OK || len(res.Problems) == 0 {
		t.Errorf("relaxing lint = %+v, want problems", res)
	}
}

func TestGuardPolicyPublish(t *testing.T) {
	t.Run("channel_off_409", func(t *testing.T) {
		a := newGuardAPI(t, nil)
		w := do("u-dave", "POST", "/api/org/guard/policy/publish",
			map[string]string{"bundle_toml": escalatingOrgTOML}, a.OrgGuardPolicyPublish)
		if w.Code != http.StatusConflict {
			t.Errorf("publish without key = %d, want 409; body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("policy_admin_publishes_audited", func(t *testing.T) {
		signer, _ := staticSigner(t)
		a := newGuardAPI(t, signer)

		// security_viewer cannot publish.
		if w := do("u-eve", "POST", "/api/org/guard/policy/publish",
			map[string]string{"bundle_toml": escalatingOrgTOML}, a.OrgGuardPolicyPublish); w.Code != http.StatusForbidden {
			t.Errorf("viewer publish = %d, want 403", w.Code)
		}

		w := do("u-dave", "POST", "/api/org/guard/policy/publish",
			map[string]any{"bundle_toml": escalatingOrgTOML, "description": "floor v1"}, a.OrgGuardPolicyPublish)
		if w.Code != http.StatusCreated {
			t.Fatalf("publish = %d; body=%s", w.Code, w.Body.String())
		}
		var res rollup.GuardPolicyPublishResult
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if res.Version != 1 {
			t.Errorf("version = %d, want 1", res.Version)
		}

		// created_by is the SAML user's email; the publish is audited.
		var createdBy, detail string
		if err := a.db.QueryRow(`SELECT created_by FROM org_policy_bundles WHERE version = 1`).Scan(&createdBy); err != nil {
			t.Fatal(err)
		}
		if createdBy != "dave@acme.example" {
			t.Errorf("created_by = %q, want the publisher's email", createdBy)
		}
		if err := a.db.QueryRow(`SELECT target_detail FROM audit_log WHERE action = ?`, rollup.ActionPublishBundle).Scan(&detail); err != nil {
			t.Fatalf("audit row missing: %v", err)
		}
		if detail != "v1" {
			t.Errorf("audit detail = %q, want v1", detail)
		}
	})

	t.Run("lint_failing_bundle_400", func(t *testing.T) {
		signer, _ := staticSigner(t)
		a := newGuardAPI(t, signer)
		w := do("u-dave", "POST", "/api/org/guard/policy/publish",
			map[string]string{"bundle_toml": relaxingOrgTOML}, a.OrgGuardPolicyPublish)
		if w.Code != http.StatusBadRequest {
			t.Errorf("relaxing publish = %d, want 400; body=%s", w.Code, w.Body.String())
		}
		var n int
		if err := a.db.QueryRow(`SELECT COUNT(*) FROM org_policy_bundles`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("bundle rows = %d, want 0 (refused bundle must not land)", n)
		}
	})
}
