package obs

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/obs/admission"
	obsstore "github.com/marmutapp/superbased-observer/internal/obs/store"
)

// admissionGate is a test ContentGate whose AllowsRawContent is fixed.
type admissionGate struct{ allow bool }

func (g admissionGate) AllowsRawContent() bool { return g.allow }

// newAdmissionSvc opens an obs store on a temp DB and returns a service with
// the given compiled policy installed.
func newAdmissionSvc(t *testing.T, in admission.PolicyInput) (*AdmissionService, *obsstore.Store) {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Open(ctx, db.Options{Path: filepath.Join(t.TempDir(), "obs.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	st, err := obsstore.Open(ctx, conn)
	if err != nil {
		t.Fatalf("obsstore.Open: %v", err)
	}
	spec, err := admission.Compile(in)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	svc := NewAdmissionService(st, nil, admissionGate{allow: true}, nil, AdmissionOptions{Hosting: "off"})
	svc.SetPolicy(ctx, spec)
	return svc, st
}

// TestAdmissionEnforceMode covers the P2 enforcement flip (admission spec §6):
// enforce mode returns real decisions (Ask/Deny block, Flag admits) and records
// the actual mode; observe mode still admits everything while recording.
func TestAdmissionEnforceMode(t *testing.T) {
	ctx := context.Background()

	t.Run("enforce deny blocks and records enforce", func(t *testing.T) {
		svc, st := newAdmissionSvc(t, admission.PolicyInput{
			Mode:      "enforce",
			Prefilter: admission.PrefilterInput{Deny: []string{"forbidden-topic"}},
		})
		resp := svc.Check(ctx, AdmissionCheck{Text: "please help with forbidden-topic", RequestID: "req-1", Persist: true})
		if resp.Allowed {
			t.Errorf("enforce deny must block: %+v", resp)
		}
		if resp.Decision != "deny" || resp.Mode != "enforce" {
			t.Errorf("resp = %+v, want deny/enforce", resp)
		}
		if resp.EnforceDecision != "deny" {
			t.Errorf("EnforceDecision = %q, want deny", resp.EnforceDecision)
		}
		rows, err := st.ListAdmissionEvents(ctx, obsstore.AdmissionListOptions{})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(rows) != 1 || rows[0].Mode != "enforce" || rows[0].Decision != "deny" {
			t.Errorf("recorded rows = %+v, want one enforce/deny", rows)
		}
	})

	t.Run("enforce flag admits", func(t *testing.T) {
		svc, _ := newAdmissionSvc(t, admission.PolicyInput{
			Mode: "enforce",
			Criteria: []admission.CriterionInput{
				{ID: "AD-200", Type: "denied_topics", Topics: []string{"competitor:AcmeCorp"}, Decision: "flag", Severity: "warn"},
			},
		})
		resp := svc.Check(ctx, AdmissionCheck{Text: "tell me about AcmeCorp", Persist: false})
		if !resp.Allowed {
			t.Errorf("enforce flag must admit (flag is admit-but-record): %+v", resp)
		}
		if resp.Decision != "flag" || resp.Criterion != "AD-200" || resp.Mode != "enforce" {
			t.Errorf("resp = %+v, want flag/AD-200/enforce", resp)
		}
	})

	t.Run("observe deny admits", func(t *testing.T) {
		svc, st := newAdmissionSvc(t, admission.PolicyInput{
			Mode:      "observe",
			Prefilter: admission.PrefilterInput{Deny: []string{"forbidden-topic"}},
		})
		resp := svc.Check(ctx, AdmissionCheck{Text: "please help with forbidden-topic", Persist: true})
		if !resp.Allowed {
			t.Errorf("observe must admit even a deny verdict: %+v", resp)
		}
		if resp.Decision != "deny" || resp.Mode != "observe" {
			t.Errorf("resp = %+v, want deny/observe", resp)
		}
		rows, err := st.ListAdmissionEvents(ctx, obsstore.AdmissionListOptions{})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(rows) != 1 || rows[0].Mode != "observe" {
			t.Errorf("recorded rows = %+v, want one observe row", rows)
		}
	})
}
