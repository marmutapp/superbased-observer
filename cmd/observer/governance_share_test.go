package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/govern"
	"github.com/marmutapp/superbased-observer/internal/policyfam/nodegov"
	"github.com/marmutapp/superbased-observer/internal/store"
)

func shareEffective(t *testing.T, body string) govern.Effective {
	t.Helper()
	spec, _, err := nodegov.CompileBody([]byte(body), 1<<20)
	if err != nil {
		t.Fatalf("CompileBody(%s): %v", body, err)
	}
	now := time.Now().UTC()
	return govern.Resolve(
		govern.Delivered{Present: true, Version: 14, BodyHash: "bh", Spec: spec},
		sidecarTestGrant(now),
		govern.LiveIdentity{Enrolled: true, OrgKey: "ok", Generation: 2, KeyPinSHA256: "pin"},
		now,
	)
}

// TestLowerShareOptionsNeverRaises walks every boolean share key at the
// ACTUAL seam — the provider cmd/observer installs on the push client — and
// asserts the org can only ever reduce.
func TestLowerShareOptionsNeverRaises(t *testing.T) {
	allOn := store.ShareOptions{
		FullContent: true, RoutingSummary: true,
		ObsSummary: true, ObsTraces: true, ObsContent: true,
		ObsEvalSummary: true, ObsAdmission: true, ObsEvalItems: true,
	}
	allOff := store.ShareOptions{}

	for _, key := range []string{
		"full_content", "routing_summary",
		"obs.summary", "obs.traces", "obs.content",
		"obs.eval_summary", "obs.admission", "obs.eval_items",
	} {
		t.Run(key+"/org lowers", func(t *testing.T) {
			eff := shareEffective(t, `{"schema":2,"share":{"`+key+`":false}}`)
			got := lowerShareOptions(allOn, eff)
			if reflect.DeepEqual(got, allOn) {
				t.Fatalf("an org directive of %s=false changed nothing", key)
			}
		})
		t.Run(key+"/org cannot raise", func(t *testing.T) {
			eff := shareEffective(t, `{"schema":2,"share":{"`+key+`":true}}`)
			if got := lowerShareOptions(allOff, eff); !reflect.DeepEqual(got, allOff) {
				t.Fatalf("an org directive of %s=true RAISED a node that had opted out: %+v", key, got)
			}
		})
	}
}

// TestShipsRawContentCanOnlyGoTrueToFalse is the headline consequence,
// stated at the seam: there is no org body, grant, or authority token by
// which a node that has not locally opted in ships raw content.
func TestShipsRawContentCanOnlyGoTrueToFalse(t *testing.T) {
	optedOut := store.ShareOptions{}
	raise := shareEffective(t, `{"schema":2,"share":{"full_content":true}}`)
	if lowerShareOptions(optedOut, raise).FullContent {
		t.Fatal("full_content was raised remotely")
	}

	optedIn := store.ShareOptions{FullContent: true}
	lower := shareEffective(t, `{"schema":2,"share":{"full_content":false}}`)
	if lowerShareOptions(optedIn, lower).FullContent {
		t.Fatal("a lowering directive did not take")
	}
}

// TestAdminManagedIsNeverTouchedByGovernance: admin_managed is excluded from
// the org vocabulary entirely, so no org body can reach it in EITHER
// direction — and the lowering seam must not "helpfully" fold it in.
func TestAdminManagedIsNeverTouchedByGovernance(t *testing.T) {
	local := store.ShareOptions{AdminManaged: true}
	// The compiler refuses a body naming it at all, so the only thing to
	// prove here is that the seam leaves the field alone under a body that
	// lowers everything else.
	eff := shareEffective(t, `{"schema":2,"share":{"full_content":false,"routing_summary":false}}`)
	if !lowerShareOptions(local, eff).AdminManaged {
		t.Fatal("the lowering seam cleared admin_managed — that flag is a node-side provisioning decision with no remote path")
	}
}

// TestTargetAllowlistIntersects: a list directive can only shrink the list.
func TestTargetAllowlistIntersects(t *testing.T) {
	local := store.ShareOptions{TargetActionAllowlist: []string{"read_file", "run_command", "edit_file"}}
	eff := shareEffective(t, `{"schema":2,"share":{"target_action_allowlist":["read_file","write_file"]}}`)
	got := lowerShareOptions(local, eff).TargetActionAllowlist
	if len(got) != 1 || got[0] != "read_file" {
		t.Fatalf("allowlist = %v, want the intersection [read_file]", got)
	}
}

// TestUngovernedNodeShareIsUnchanged: a dormant posture returns the node's
// own config byte-for-byte, so an unenrolled node's push is unchanged.
func TestUngovernedNodeShareIsUnchanged(t *testing.T) {
	local := store.ShareOptions{FullContent: true, RoutingSummary: true, TargetActionAllowlist: []string{"read_file"}}
	dormant := govern.Resolve(govern.Delivered{}, nil, govern.LiveIdentity{}, time.Now())
	got := lowerShareOptions(local, dormant)
	if !got.FullContent || !got.RoutingSummary || len(got.TargetActionAllowlist) != 1 {
		t.Fatalf("an ungoverned node's share posture changed: %+v", got)
	}
}
