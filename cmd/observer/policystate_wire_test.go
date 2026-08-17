package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/orgclient"
	"github.com/marmutapp/superbased-observer/internal/orgcontract"
	"github.com/marmutapp/superbased-observer/internal/policyfam/providers"
	"github.com/marmutapp/superbased-observer/internal/policystate"
	"github.com/marmutapp/superbased-observer/internal/store"
)

const psHex64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// fakePoster is a policyStatePoster double.
type fakePoster struct {
	mu    sync.Mutex
	calls int
	last  orgcontract.PolicyStateReport
	block chan struct{} // when non-nil, PostPolicyState blocks until closed or ctx done
	err   error
}

func (f *fakePoster) PostPolicyState(ctx context.Context, rep orgcontract.PolicyStateReport) error {
	f.mu.Lock()
	f.calls++
	f.last = rep
	block, e := f.block, f.err
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return e
}

func (f *fakePoster) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fourReaders returns a valid four-point reader map (all none/no_policy).
func fourReaders() map[string]policystate.PointReader {
	ok := func(f policystate.PointFacts) policystate.PointReader {
		return func(context.Context) (policystate.PointFacts, error) { return f, nil }
	}
	now := time.Now
	return map[string]policystate.PointReader{
		policystate.PointGuard:         newGuardPointReader(func() (int64, error) { return 0, nil }, func() guardRunning { return guardRunning{} }, func() orgclient.GuardFetchOutcome { return orgclient.GuardFetchOutcome{} }, now),
		policystate.PointRouter:        newRouterPointReader(nil, func(context.Context) (int64, error) { return 0, nil }, func() orgclient.RoutingFetchOutcome { return orgclient.RoutingFetchOutcome{} }, now),
		policystate.PointProxyAdmitter: ok(policystate.PointFacts{LastSeen: now()}),
		policystate.PointProxyEgress:   ok(policystate.PointFacts{LastSeen: now()}),
	}
}

func seqCounter(t *testing.T) *orgclient.ReportSeqCounter {
	t.Helper()
	return orgclient.NewReportSeqCounter(filepath.Join(t.TempDir(), "seq"))
}

// TestReporter_PostsCompleteFourRowSnapshot — the happy path baseline.
func TestReporter_PostsCompleteFourRowSnapshot(t *testing.T) {
	fp := &fakePoster{}
	rep := newPolicyStateReporter(fp, fourReaders(), seqCounter(t), "v", true, nil)
	rep.report(context.Background())
	if fp.callCount() != 1 {
		t.Fatalf("calls = %d, want 1", fp.callCount())
	}
	if len(fp.last.Rows) != 4 {
		t.Fatalf("rows = %d, want 4", len(fp.last.Rows))
	}
	if fp.last.ReportSeq <= 0 {
		t.Fatalf("report_seq = %d, want > 0", fp.last.ReportSeq)
	}
}

// TestReporter_GatedOff — a disabled reporter never posts.
func TestReporter_GatedOff(t *testing.T) {
	fp := &fakePoster{}
	rep := newPolicyStateReporter(fp, fourReaders(), seqCounter(t), "v", false, nil)
	rep.report(context.Background())
	if fp.callCount() != 0 {
		t.Fatalf("calls = %d, want 0 when gated off", fp.callCount())
	}
}

// TestReporter_ShortSnapshotLoggedAndSkipped — a reader error yields 3 rows;
// the reporter MUST skip the POST, never send a short snapshot (R3-S2).
func TestReporter_ShortSnapshotLoggedAndSkipped(t *testing.T) {
	readers := fourReaders()
	readers[policystate.PointRouter] = func(context.Context) (policystate.PointFacts, error) {
		return policystate.PointFacts{}, errors.New("router reader boom")
	}
	fp := &fakePoster{}
	rep := newPolicyStateReporter(fp, readers, seqCounter(t), "v", true, nil)
	rep.report(context.Background())
	if fp.callCount() != 0 {
		t.Fatalf("calls = %d, want 0 (short snapshot must skip)", fp.callCount())
	}
}

// TestReporter_404LatchesOff — an ErrPolicyAckUnsupported from the poster
// latches the channel off; a second report makes no further POST (S8).
func TestReporter_404LatchesOff(t *testing.T) {
	fp := &fakePoster{err: orgclient.ErrPolicyAckUnsupported}
	rep := newPolicyStateReporter(fp, fourReaders(), seqCounter(t), "v", true, nil)
	rep.report(context.Background())
	rep.report(context.Background())
	if fp.callCount() != 1 {
		t.Fatalf("calls = %d, want exactly 1 (latched off after 404)", fp.callCount())
	}
}

// TestGuardReader_RunningVersionFromPolicyStates — version + hash come TOGETHER
// from the injected running source (Guard.PolicyStates()), never split.
func TestGuardReader_RunningVersionFromPolicyStates(t *testing.T) {
	reader := newGuardPointReader(
		func() (int64, error) { return 9, nil },
		func() guardRunning { return guardRunning{RunningVersion: 7, EffectiveHash: psHex64, Mode: "enforce"} },
		func() orgclient.GuardFetchOutcome { return orgclient.GuardFetchOutcome{} },
		time.Now,
	)
	f, _ := reader(context.Background())
	if f.RunningVersion != 7 || f.EffectiveHash != psHex64 {
		t.Fatalf("running/hash = %d/%q, want 7/%s together", f.RunningVersion, f.EffectiveHash, psHex64)
	}
	if f.CachedAcceptedVersion != 9 {
		t.Fatalf("cached = %d, want 9 (from the envelope)", f.CachedAcceptedVersion)
	}
	if !f.HasOrgRail {
		t.Fatal("guard must always report HasOrgRail")
	}
}

// TestGuardReader_AbsentOrgLayerIsZeroEmpty — an absent org layer (first
// install / not-yet-loaded) reads 0 + empty hash (R3-B2).
func TestGuardReader_AbsentOrgLayerIsZeroEmpty(t *testing.T) {
	reader := newGuardPointReader(
		func() (int64, error) { return 3, nil },
		func() guardRunning { return guardRunning{} }, // absent org layer
		func() orgclient.GuardFetchOutcome { return orgclient.GuardFetchOutcome{} },
		time.Now,
	)
	f, _ := reader(context.Background())
	if f.RunningVersion != 0 || f.EffectiveHash != "" {
		t.Fatalf("running/hash = %d/%q, want 0/empty when org layer absent", f.RunningVersion, f.EffectiveHash)
	}
	// guardRunningFromGuard(nil) must also be zero (nil-safe).
	if gr := guardRunningFromGuard(nil)(); gr.RunningVersion != 0 || gr.EffectiveHash != "" {
		t.Fatalf("guardRunningFromGuard(nil) = %+v, want zero", gr)
	}
}

// TestGuardReader_DeliveredRejectFromLastFetch — the reject facts come from the
// in-memory last-fetch slot, not the cache.
func TestGuardReader_DeliveredRejectFromLastFetch(t *testing.T) {
	reader := newGuardPointReader(
		func() (int64, error) { return 5, nil },
		func() guardRunning { return guardRunning{RunningVersion: 5, EffectiveHash: psHex64, Mode: "enforce"} },
		func() orgclient.GuardFetchOutcome {
			return orgclient.GuardFetchOutcome{OK: true, Reached: true, RejectCode: orgclient.RejectLintFailed, Version: 6}
		},
		time.Now,
	)
	f, _ := reader(context.Background())
	if !f.LatestFetchRejected || f.RejectCode != string(orgclient.RejectLintFailed) || f.RejectedVersion != 6 {
		t.Fatalf("reject facts = %v/%q/%d, want true/lint_failed/6", f.LatestFetchRejected, f.RejectCode, f.RejectedVersion)
	}
}

// TestRouterReader_NilHandleIsNoOrgRail — routing off (nil handle) → HasOrgRail
// false so the point resolves to none/no_policy.
func TestRouterReader_NilHandleIsNoOrgRail(t *testing.T) {
	reader := newRouterPointReader(nil, func(context.Context) (int64, error) { return 0, nil }, func() orgclient.RoutingFetchOutcome { return orgclient.RoutingFetchOutcome{} }, time.Now)
	f, _ := reader(context.Background())
	if f.HasOrgRail {
		t.Fatal("nil routing handle must report HasOrgRail=false")
	}
}

// TestLastFetchOutcome_AuthFailedClearsPriorUnreachable (R4-B4) — Unreachable
// then AuthFailed: the 401 proved reachability, so the slot must STOP reporting
// Unreachable (else the point keeps claiming stale_lkg).
func TestLastFetchOutcome_AuthFailedClearsPriorUnreachable(t *testing.T) {
	rep := newPolicyStateReporter(nil, nil, nil, "v", true, nil)
	rep.recordGuard(orgclient.GuardFetchOutcome{Unreachable: true, Reached: false})
	if !rep.guardSlot().Unreachable {
		t.Fatal("precondition: Unreachable must be set after the transport failure")
	}
	rep.recordGuard(orgclient.GuardFetchOutcome{AuthFailed: true, Reached: true})
	if rep.guardSlot().Unreachable {
		t.Fatal("a reached AuthFailed must CLEAR the prior Unreachable (R4-B4)")
	}
}

// TestLastFetchOutcome_IndeterminateLeavesPriorSlot (R4-B4) — a purely-local
// (Reached:false) Indeterminate after a Reject leaves the decisive slot.
func TestLastFetchOutcome_IndeterminateLeavesPriorSlot(t *testing.T) {
	rep := newPolicyStateReporter(nil, nil, nil, "v", true, nil)
	rep.recordGuard(orgclient.GuardFetchOutcome{OK: true, Reached: true, RejectCode: orgclient.RejectSigInvalid, Version: 5})
	rep.recordGuard(orgclient.GuardFetchOutcome{Indeterminate: true, Reached: false})
	if rep.guardSlot().RejectCode != orgclient.RejectSigInvalid {
		t.Fatalf("reject = %q, want sig_invalid retained after a local Indeterminate", rep.guardSlot().RejectCode)
	}
}

// TestLastFetchOutcome_RoutingAuthFailedClearsUnreachable — the routing twin.
func TestLastFetchOutcome_RoutingAuthFailedClearsUnreachable(t *testing.T) {
	rep := newPolicyStateReporter(nil, nil, nil, "v", true, nil)
	rep.recordRouting(orgclient.RoutingFetchOutcome{Unreachable: true, Reached: false})
	rep.recordRouting(orgclient.RoutingFetchOutcome{AuthFailed: true, Reached: true})
	if rep.routingSlot().Unreachable {
		t.Fatal("routing: a reached AuthFailed must clear the prior Unreachable")
	}
}

// TestReporter_SlowPostDoesNotDelayFetchLoops (R2-S4) — recordGuard only
// records + pokes; it must NOT call report, so a slow POST cannot delay the
// poll loop that calls the sink. A mutation calling report() synchronously from
// the sink would block on the never-releasing poster and time out here.
func TestReporter_SlowPostDoesNotDelayFetchLoops(t *testing.T) {
	fp := &fakePoster{block: make(chan struct{})} // never released
	rep := newPolicyStateReporter(fp, fourReaders(), seqCounter(t), "v", true, nil)
	done := make(chan struct{})
	go func() {
		rep.recordGuard(orgclient.GuardFetchOutcome{OK: true, Reached: true})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("recordGuard blocked — it must not call report synchronously")
	}
	if fp.callCount() != 0 {
		t.Fatalf("poster called %d times from a sink, want 0", fp.callCount())
	}
}

// TestReporter_MultiplePokesCoalesceToOneFollowup — the buffered-1 poke channel
// collapses many pokes to exactly one queued follow-up.
func TestReporter_MultiplePokesCoalesceToOneFollowup(t *testing.T) {
	n := newPolicyStateNotifier()
	for i := 0; i < 5; i++ {
		n.Poke()
	}
	if got := len(n.poke); got != 1 {
		t.Fatalf("queued pokes = %d, want exactly 1 (coalesced)", got)
	}
	<-n.poke
	if len(n.poke) != 0 {
		t.Fatal("channel must be empty after one drain")
	}
}

// TestBuildAndRunReporter exercises the full daemon-side assembly
// (buildPolicyStateReporter + the four daemon source helpers + run +
// policyStateHeartbeat) over a real store with nil guard/admission/routing
// handles. All four points resolve to none/no_policy, so a complete four-row
// snapshot posts. This is the reporter-level analog of the deferred
// TestStartupEmitsFourRows (which additionally needs the start.go g.Go launch).
func TestBuildAndRunReporter(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "agent.db")
	conn, err := db.Open(ctx, db.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer func() { _ = conn.Close() }()
	st := store.New(conn)

	cfg := config.Default()
	cfg.Observer.DBPath = dbPath
	cfg.OrgClient.Share.PolicyState = true

	if got := policyStateHeartbeat(cfg); got != time.Duration(config.DefaultPolicyStateHeartbeatSeconds)*time.Second {
		t.Fatalf("heartbeat = %v, want default", got)
	}

	fp := &fakePoster{}
	rep := buildPolicyStateReporter(fp, nil, st, nil, nil, nil, nil, cfg, "v", nil)
	// v3: six readers — the four core points plus the two OPTIONAL ones
	// (proxy-gateway, node-dashboard), both registered even with nil
	// handles. An omitted row is indistinguishable from an older agent,
	// whereas an explicit none/no_policy row is an honest fact
	// (gateway spec §2.2; admin-controlled Plane B §3.8).
	if len(rep.readers) != 4+len(policystate.OptionalPoints) {
		t.Fatalf("readers = %d, want %d", len(rep.readers), 4+len(policystate.OptionalPoints))
	}

	// One startup emit via the run loop, then cancel.
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- rep.run(rctx, 10*time.Millisecond) }()
	// Wait for at least one post.
	deadline := time.After(2 * time.Second)
	for fp.callCount() == 0 {
		select {
		case <-deadline:
			cancel()
			t.Fatal("reporter never posted a snapshot")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not stop on ctx cancel")
	}
	if want := 4 + len(policystate.OptionalPoints); len(fp.last.Rows) != want {
		t.Fatalf("posted rows = %d, want %d", len(fp.last.Rows), want)
	}
}

// TestReporter_CtxCancelStopsInflightSend (R2-S4) — report(ctx) hands ctx to
// the poster, so cancelling ctx abandons the in-flight POST. A poster built
// with context.Background() would ignore the cancel and hang.
func TestReporter_CtxCancelStopsInflightSend(t *testing.T) {
	fp := &fakePoster{block: make(chan struct{})} // released only via ctx
	rep := newPolicyStateReporter(fp, fourReaders(), seqCounter(t), "v", true, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		rep.report(ctx)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("report did not honor ctx cancellation on the in-flight POST")
	}
}

// TestMergeRoutingOutcome_KeepsGateReject (BLOCKER 1) — the routing classifier
// emits a gate rejection as {RejectCode, Reached:true} with OK=false. The merge
// MUST keep that RejectCode (so the router can report delivered_unaccepted) and
// MUST clear a prior Unreachable (reachability was proven). MUTATION intent:
// reverting the reject case guard to `o.OK && o.RejectCode != ""` drops the
// reject into the neutral default and fails this test.
func TestMergeRoutingOutcome_KeepsGateReject(t *testing.T) {
	prev := orgclient.RoutingFetchOutcome{Unreachable: true} // a prior transport failure
	o := orgclient.RoutingFetchOutcome{RejectCode: orgclient.RejectSigInvalid, Reached: true, Version: 7}
	got := mergeRoutingOutcome(prev, o)
	if got.RejectCode != orgclient.RejectSigInvalid {
		t.Fatalf("RejectCode = %q, want sig_invalid (a routing gate reject must not be dropped)", got.RejectCode)
	}
	if got.Version != 7 {
		t.Fatalf("Version = %d, want 7", got.Version)
	}
	if got.Unreachable {
		t.Fatal("a reached reject (Reached:true) must clear the prior Unreachable")
	}
}

// TestGuardCachedVersion_CorruptFileErrors (SF2) — a corrupt/unreadable cache
// file is a genuine error the reader must surface (so Assemble omits the point
// and the reporter skips), NOT a fabricated version-0 row. A MISSING file and
// an empty path stay the legitimate first-install (0, nil).
func TestGuardCachedVersion_CorruptFileErrors(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bundle-cache.json")
	if err := os.WriteFile(bad, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}
	if _, err := guardCachedVersionFromFile(bad)(); err == nil {
		t.Fatal("corrupt cache file must return a non-nil error, not version 0")
	}

	missing := filepath.Join(dir, "does-not-exist.json")
	if v, err := guardCachedVersionFromFile(missing)(); err != nil || v != 0 {
		t.Fatalf("missing file = (%d, %v), want (0, nil) — first-install path", v, err)
	}

	if v, err := guardCachedVersionFromFile("")(); err != nil || v != 0 {
		t.Fatalf("empty path = (%d, %v), want (0, nil)", v, err)
	}
}

// TestRouterCachedVersion_DBErrorPropagates (SF3) — a store/DB error must make
// the router reader error so Assemble OMITS the router point (short snapshot →
// reporter skips). A !ok (no policy cached) stays the legitimate version-0
// (0, nil) with no error. Tested at the reader level to avoid a heavy store
// stub; the helper's own !ok/err split is a thin wrapper over the same seam.
func TestRouterCachedVersion_DBErrorPropagates(t *testing.T) {
	dbBoom := errors.New("store boom")

	// A cached-version fn that surfaces a DB error → reader errors.
	errReader := newRouterPointReader(
		nil,
		func(context.Context) (int64, error) { return 0, dbBoom },
		func() orgclient.RoutingFetchOutcome { return orgclient.RoutingFetchOutcome{} },
		time.Now,
	)
	if _, err := errReader(context.Background()); err == nil {
		t.Fatal("a DB error from the cached-version fn must propagate out of the router reader")
	}

	// !ok (no policy cached) → (0, nil): the reader succeeds with version 0.
	okReader := newRouterPointReader(
		nil,
		func(context.Context) (int64, error) { return 0, nil },
		func() orgclient.RoutingFetchOutcome { return orgclient.RoutingFetchOutcome{} },
		time.Now,
	)
	f, err := okReader(context.Background())
	if err != nil {
		t.Fatalf("!ok path must not error, got %v", err)
	}
	if f.CachedAcceptedVersion != 0 {
		t.Fatalf("cached = %d, want 0 for the !ok path", f.CachedAcceptedVersion)
	}

	// Assemble OMITS the erroring router point → a short (<4-row) snapshot.
	readers := fourReaders()
	readers[policystate.PointRouter] = errReader
	rows, aerr := policystate.Assemble(context.Background(), readers)
	if aerr == nil {
		t.Fatal("Assemble must join the router reader error")
	}
	if len(rows) >= 4 {
		t.Fatalf("assembled rows = %d, want <4 (router point omitted)", len(rows))
	}
	for _, row := range rows {
		if row.EnforcementPoint == policystate.PointRouter {
			t.Fatal("the erroring router point must be omitted from the snapshot")
		}
	}
}

// TestAssemble_OmitsGuardPointOnCorruptCache (SF2, end-to-end) — a corrupt
// guard cache makes the guard reader error, so Assemble omits the guard point
// and the reporter posts a short snapshot → skip.
func TestAssemble_OmitsGuardPointOnCorruptCache(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bundle-cache.json")
	if err := os.WriteFile(bad, []byte("}{"), 0o600); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}
	readers := fourReaders()
	readers[policystate.PointGuard] = newGuardPointReader(
		guardCachedVersionFromFile(bad),
		func() guardRunning { return guardRunning{} },
		func() orgclient.GuardFetchOutcome { return orgclient.GuardFetchOutcome{} },
		time.Now,
	)
	rows, err := policystate.Assemble(context.Background(), readers)
	if err == nil {
		t.Fatal("Assemble must surface the guard reader error")
	}
	if len(rows) >= 4 {
		t.Fatalf("rows = %d, want <4 (guard omitted on corrupt cache)", len(rows))
	}

	// The reporter must SKIP the short snapshot (no POST).
	fp := &fakePoster{}
	rep := newPolicyStateReporter(fp, readers, seqCounter(t), "v", true, nil)
	rep.report(context.Background())
	if fp.callCount() != 0 {
		t.Fatalf("posts = %d, want 0 (short snapshot from corrupt guard cache must skip)", fp.callCount())
	}
}

// --- v2: the gateway.providers point reader + the pre-v2 server probe -----
//
// docs/plans/policy-state-v2-gateway-providers-spec-2026-08-15.md §2.2/§3.2.

// gatewayFactsReader builds a gateway PointReader over a fixed facts double,
// bypassing the handle so the reader's own mapping is what is under test.
func gatewayHandleWith(apply error, lanes map[string]string) *gatewayProvidersHandle {
	return newGatewayProvidersHandle(
		func(map[string]string, string) error { return apply },
		func() {},
		func() (map[string]string, string) { return lanes, "" },
	)
}

// TestGatewayReader_ResolvesEverySituation is the table-driven pin on §2.2:
// each install-seam situation must resolve, through the UNCHANGED §3.2
// decision table, to the documented (status, reason, mode) triple.
func TestGatewayReader_ResolvesEverySituation(t *testing.T) {
	spec, err := providers.Compile(providers.PolicyInput{Upstreams: map[string]string{"a": "https://a.example"}})
	if err != nil {
		t.Fatalf("providers.Compile: %v", err)
	}
	orgHash := strings.Repeat("a", 64)
	localLanes := map[string]string{"anthropic": "https://api.anthropic.com"}

	cases := []struct {
		name       string
		handle     func() *gatewayProvidersHandle
		lastFetch  orgclient.PolicyResourceFetchOutcome
		wantStatus string
		wantReason string
		wantMode   string
		wantHash   string
		wantRun    int64
	}{
		{
			name:       "no org rail, no lanes at all",
			handle:     func() *gatewayProvidersHandle { return gatewayHandleWith(nil, nil) },
			wantStatus: orgcontract.StatusNone, wantReason: orgcontract.ReasonNoPolicy, wantMode: "off",
		},
		{
			name:       "no org rail, node's own lanes routing",
			handle:     func() *gatewayProvidersHandle { return gatewayHandleWith(nil, localLanes) },
			wantStatus: orgcontract.StatusNone, wantReason: orgcontract.ReasonLocalEffective, wantMode: "enforce",
			wantHash: providers.HashLaneTable(localLanes, ""),
		},
		{
			name: "org lane table applied and routing",
			handle: func() *gatewayProvidersHandle {
				h := gatewayHandleWith(nil, localLanes)
				applyGatewayProviders(h, orgclient.PolicyResourceResult{
					Status: orgclient.PRApplied, Version: 5, BodyHash: orgHash, Spec: spec, EnforceAllowed: true,
				}, slog.Default())
				return h
			},
			wantStatus: orgcontract.StatusEffective, wantReason: orgcontract.ReasonOK, wantMode: "enforce",
			wantHash: orgHash, wantRun: 5,
		},
		{
			name: "org lane table accepted but not preauthorized",
			handle: func() *gatewayProvidersHandle {
				h := gatewayHandleWith(nil, localLanes)
				applyGatewayProviders(h, orgclient.PolicyResourceResult{
					Status: orgclient.PRAppliedInert, Version: 6, BodyHash: orgHash, Spec: spec,
					EnforceAllowed: false, InertReason: "not_preauthorized",
				}, slog.Default())
				return h
			},
			wantStatus: orgcontract.StatusAcceptedInert, wantReason: orgcontract.ReasonNotPreauthorized,
			wantMode: "observe", wantHash: orgHash, wantRun: 6,
		},
		{
			name: "the live proxy refused the delivered lane table",
			handle: func() *gatewayProvidersHandle {
				h := gatewayHandleWith(errors.New("bad lane"), localLanes)
				applyGatewayProviders(h, orgclient.PolicyResourceResult{
					Status: orgclient.PRApplied, Version: 8, BodyHash: orgHash, Spec: spec, EnforceAllowed: true,
				}, slog.Default())
				return h
			},
			wantStatus: orgcontract.StatusDeliveredUnaccepted, wantReason: orgcontract.ReasonCapabilityMismatch,
			// No org rail was ever established, so running is 0 and the
			// org-rail hash rule forces an empty hash. Mode stays enforce
			// because the node's OWN lanes are still routing, which is the
			// honest statement of what the proxy is doing.
			wantMode: "enforce",
		},
		{
			name: "delivery itself failed a gate",
			handle: func() *gatewayProvidersHandle {
				h := gatewayHandleWith(nil, localLanes)
				applyGatewayProviders(h, orgclient.PolicyResourceResult{
					Status: orgclient.PRApplied, Version: 5, BodyHash: orgHash, Spec: spec, EnforceAllowed: true,
				}, slog.Default())
				return h
			},
			lastFetch:  orgclient.PolicyResourceFetchOutcome{Reached: true, RejectCode: orgclient.PRRejectKeyPinMismatch, Version: 6},
			wantStatus: orgcontract.StatusDeliveredUnaccepted, wantReason: orgcontract.ReasonKeyPinMismatch,
			wantMode: "enforce", wantHash: orgHash, wantRun: 5,
		},
		{
			name: "control plane unreachable while a lane table is live",
			handle: func() *gatewayProvidersHandle {
				h := gatewayHandleWith(nil, localLanes)
				applyGatewayProviders(h, orgclient.PolicyResourceResult{
					Status: orgclient.PRApplied, Version: 5, BodyHash: orgHash, Spec: spec, EnforceAllowed: true,
				}, slog.Default())
				return h
			},
			lastFetch:  orgclient.PolicyResourceFetchOutcome{Unreachable: true},
			wantStatus: orgcontract.StatusStaleLKG, wantReason: orgcontract.ReasonControlPlaneUnreachable,
			wantMode: "enforce", wantHash: orgHash, wantRun: 5,
		},
	}

	now := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := newGatewayPointReader(tc.handle(), func() orgclient.PolicyResourceFetchOutcome { return tc.lastFetch }, now)
			facts, err := reader(context.Background())
			if err != nil {
				t.Fatalf("reader: %v", err)
			}
			row := policystate.Resolve(policystate.PointProxyGateway, policystate.FamilyGatewayProviders, facts)
			if row.Status != tc.wantStatus || row.Reason != tc.wantReason {
				t.Fatalf("status/reason = %s/%s, want %s/%s", row.Status, row.Reason, tc.wantStatus, tc.wantReason)
			}
			if row.Mode != tc.wantMode {
				t.Errorf("mode = %q, want %q", row.Mode, tc.wantMode)
			}
			if row.EffectiveHash != tc.wantHash {
				t.Errorf("effective_hash = %q, want %q", row.EffectiveHash, tc.wantHash)
			}
			if row.RunningVersion != tc.wantRun {
				t.Errorf("running_version = %d, want %d", row.RunningVersion, tc.wantRun)
			}
			if row.Family != policystate.FamilyGatewayProviders {
				t.Errorf("family = %q, want gateway.providers", row.Family)
			}
		})
	}
}

// TestGatewayReader_DeliveryRejectBeatsApplyFailure pins the §2.2 precedence:
// a DELIVERY failure is strictly upstream of a local install failure and is
// the more actionable one, so it wins the single reason slot.
func TestGatewayReader_DeliveryRejectBeatsApplyFailure(t *testing.T) {
	spec, _ := providers.Compile(providers.PolicyInput{Upstreams: map[string]string{"a": "https://a.example"}})
	h := gatewayHandleWith(errors.New("bad lane"), nil)
	applyGatewayProviders(h, orgclient.PolicyResourceResult{
		Status: orgclient.PRApplied, Version: 8, Spec: spec, EnforceAllowed: true,
	}, slog.Default())

	reader := newGatewayPointReader(h, func() orgclient.PolicyResourceFetchOutcome {
		return orgclient.PolicyResourceFetchOutcome{Reached: true, RejectCode: orgclient.PRRejectSigInvalid, Version: 9}
	}, time.Now)
	facts, err := reader(context.Background())
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	if facts.RejectCode != orgcontract.ReasonSigInvalid || facts.RejectedVersion != 9 {
		t.Fatalf("reject = %s@%d, want sig_invalid@9 (delivery reject wins)", facts.RejectCode, facts.RejectedVersion)
	}
}

// TestGatewayReader_NilHandleIsNoPolicy — a node with no lane wiring still
// emits an HONEST row rather than being omitted from the snapshot.
func TestGatewayReader_NilHandleIsNoPolicy(t *testing.T) {
	reader := newGatewayPointReader(nil, nil, time.Now)
	facts, err := reader(context.Background())
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	row := policystate.Resolve(policystate.PointProxyGateway, policystate.FamilyGatewayProviders, facts)
	if row.Status != orgcontract.StatusNone || row.Reason != orgcontract.ReasonNoPolicy || row.Mode != "off" {
		t.Fatalf("row = %+v, want none/no_policy/off", row)
	}
}

// fiveReaders is fourReaders plus the v2 gateway point.
func fiveReaders() map[string]policystate.PointReader {
	r := fourReaders()
	r[policystate.PointProxyGateway] = newGatewayPointReader(nil, nil, time.Now)
	return r
}

// TestReporter_PostsFiveRowSnapshot — the v2 happy path.
func TestReporter_PostsFiveRowSnapshot(t *testing.T) {
	fp := &fakePoster{}
	rep := newPolicyStateReporter(fp, fiveReaders(), seqCounter(t), "v", true, nil)
	rep.report(context.Background())
	if fp.callCount() != 1 {
		t.Fatalf("calls = %d, want 1", fp.callCount())
	}
	if len(fp.last.Rows) != 5 {
		t.Fatalf("rows = %d, want 5", len(fp.last.Rows))
	}
	var found bool
	for _, row := range fp.last.Rows {
		if row.EnforcementPoint == policystate.PointProxyGateway {
			found = true
			if row.Family != policystate.FamilyGatewayProviders {
				t.Fatalf("family = %q, want gateway.providers", row.Family)
			}
		}
	}
	if !found {
		t.Fatalf("no proxy-gateway row in %+v", fp.last.Rows)
	}
}

// rejectingPoster 400s a snapshot larger than acceptRows and accepts one at
// or below it — a v1 server for acceptRows=4.
type rejectingPoster struct {
	mu         sync.Mutex
	acceptRows int
	sizes      []int
	failAll    bool
	last       []orgcontract.PolicyStateRow
}

func (p *rejectingPoster) PostPolicyState(_ context.Context, rep orgcontract.PolicyStateReport) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sizes = append(p.sizes, len(rep.Rows))
	p.last = append([]orgcontract.PolicyStateRow(nil), rep.Rows...)
	if p.failAll || len(rep.Rows) > p.acceptRows {
		return orgclient.ErrPolicyAckRejected
	}
	return nil
}

// lastRows returns the rows of the most recent POST — accepted or not.
func (p *rejectingPoster) lastRows() []orgcontract.PolicyStateRow {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]orgcontract.PolicyStateRow(nil), p.last...)
}

func (p *rejectingPoster) snapshot() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.sizes...)
}

// TestReporter_PreV2ServerProbeLatchesCoreOnly is the §3.2 compat proof: a
// v1 server 400s the five-row snapshot, the reporter immediately re-posts the
// four CORE rows, that succeeds, and every later report is core-only. Without
// the probe the node would lose its ENTIRE snapshot on every heartbeat, which
// is strictly worse than v1 behaviour.
func TestReporter_PreV2ServerProbeLatchesCoreOnly(t *testing.T) {
	fp := &rejectingPoster{acceptRows: 4}
	rep := newPolicyStateReporter(fp, fiveReaders(), seqCounter(t), "v", true, nil)

	rep.report(context.Background())
	if got := fp.snapshot(); len(got) != 2 || got[0] != 5 || got[1] != 4 {
		t.Fatalf("first report posted %v, want [5 4] (full then core probe)", got)
	}
	if !rep.latched.Load() || rep.latchDepth.Load() != 0 {
		t.Fatalf("latched=%v depth=%d, want a core-only latch after a successful core-row probe",
			rep.latched.Load(), rep.latchDepth.Load())
	}

	rep.report(context.Background())
	if got := fp.snapshot(); len(got) != 3 || got[2] != 4 {
		t.Fatalf("second report posted %v, want a third entry of 4 rows", got)
	}
}

// TestReporter_ProbeDoesNotLatchWhenCoreAlsoRejected — a 400 caused by a
// genuinely malformed CORE row must NOT be laundered into a compat story.
func TestReporter_ProbeDoesNotLatchWhenCoreAlsoRejected(t *testing.T) {
	fp := &rejectingPoster{failAll: true}
	rep := newPolicyStateReporter(fp, fiveReaders(), seqCounter(t), "v", true, nil)

	rep.report(context.Background())
	if got := fp.snapshot(); len(got) != 2 {
		t.Fatalf("posted %v, want exactly two attempts", got)
	}
	if rep.latched.Load() {
		t.Fatal("the reporter latched even though the core rows were rejected too")
	}
	// And the probe is not retried: at most one extra request per daemon.
	rep.report(context.Background())
	if got := fp.snapshot(); len(got) != 3 {
		t.Fatalf("posted %v, want one further attempt only (no re-probe)", got)
	}
}

// TestRowsAtDepth_DropsNewestOptionalPointsFirst pins the filter itself: the
// ladder narrows from the NEWEST optional point down, never all-or-nothing.
func TestRowsAtDepth_DropsNewestOptionalPointsFirst(t *testing.T) {
	rows := []orgcontract.PolicyStateRow{
		{EnforcementPoint: policystate.PointGuard},
		{EnforcementPoint: policystate.PointRouter},
		{EnforcementPoint: policystate.PointProxyAdmitter},
		{EnforcementPoint: policystate.PointProxyEgress},
		{EnforcementPoint: policystate.PointProxyGateway},
		{EnforcementPoint: policystate.PointNodeDashboard},
	}
	cases := []struct {
		depth int
		want  []string
	}{
		{0, nil},
		{1, []string{policystate.PointProxyGateway}},
		{2, []string{policystate.PointProxyGateway, policystate.PointNodeDashboard}},
	}
	for _, tc := range cases {
		got := rowsAtDepth(rows, tc.depth)
		if len(got) != 4+len(tc.want) {
			t.Fatalf("depth %d: rows = %d, want %d", tc.depth, len(got), 4+len(tc.want))
		}
		present := map[string]bool{}
		for _, row := range got {
			present[row.EnforcementPoint] = true
		}
		for _, p := range policystate.CorePoints {
			if !present[p] {
				t.Fatalf("depth %d dropped the CORE point %s", tc.depth, p)
			}
		}
		for _, p := range tc.want {
			if !present[p] {
				t.Fatalf("depth %d dropped the optional point %s it should have kept", tc.depth, p)
			}
		}
	}
}

// sixReaders is fiveReaders plus the v3 node-dashboard (node.governance)
// point — the full snapshot a current agent sends.
func sixReaders() map[string]policystate.PointReader {
	r := fiveReaders()
	r[policystate.PointNodeDashboard] = newNodeGovernancePointReader(nil, nil, time.Now)
	return r
}

// TestReporter_PostsSixRowSnapshot — the v3 happy path against a server that
// accepts everything.
func TestReporter_PostsSixRowSnapshot(t *testing.T) {
	fp := &fakePoster{}
	rep := newPolicyStateReporter(fp, sixReaders(), seqCounter(t), "v", true, nil)
	rep.report(context.Background())
	if fp.callCount() != 1 {
		t.Fatalf("calls = %d, want 1", fp.callCount())
	}
	if len(fp.last.Rows) != 6 {
		t.Fatalf("rows = %d, want 6", len(fp.last.Rows))
	}
	var found bool
	for _, row := range fp.last.Rows {
		if row.EnforcementPoint == policystate.PointNodeDashboard {
			found = true
			if row.Family != policystate.FamilyNodeGovernance {
				t.Fatalf("family = %q, want node.governance", row.Family)
			}
			if row.Status != orgcontract.StatusNone || row.Reason != orgcontract.ReasonNoPolicy {
				t.Fatalf("ungoverned node reported %s/%s, want none/no_policy — an ungoverned node must say so, not be omitted",
					row.Status, row.Reason)
			}
		}
	}
	if !found {
		t.Fatalf("no node-dashboard row in %+v", fp.last.Rows)
	}
}

// TestPolicyAckLadderLatchPreservesGatewayRow is the adversarial review's
// A11 regression proof. A v3 agent against a W1.1-era server (which accepts
// the five-row snapshot but rejects the six-row one) must drop ONLY the
// governance row and keep reporting the gateway row the server accepts.
//
// The all-or-nothing core-only latch this replaced would have posted [6, 4]
// and latched core-only, silently switching OFF gateway effective-state
// reporting on every W1.1-generation server in the fleet.
func TestPolicyAckLadderLatchPreservesGatewayRow(t *testing.T) {
	fp := &rejectingPoster{acceptRows: 5}
	rep := newPolicyStateReporter(fp, sixReaders(), seqCounter(t), "v", true, nil)

	rep.report(context.Background())
	if got := fp.snapshot(); len(got) != 2 || got[0] != 6 || got[1] != 5 {
		t.Fatalf("first report posted %v, want [6 5] (full, then one optional point dropped)", got)
	}
	if !rep.latched.Load() || rep.latchDepth.Load() != 1 {
		t.Fatalf("latched=%v depth=%d, want depth 1 (gateway kept, governance dropped)",
			rep.latched.Load(), rep.latchDepth.Load())
	}
	rep.report(context.Background())
	got := fp.snapshot()
	if len(got) != 3 || got[2] != 5 {
		t.Fatalf("second report posted %v, want a third entry of 5 rows", got)
	}
	// And the row that survived is the GATEWAY one, not merely "some fifth row".
	var sawGateway, sawGovernance bool
	for _, row := range fp.lastRows() {
		switch row.EnforcementPoint {
		case policystate.PointProxyGateway:
			sawGateway = true
		case policystate.PointNodeDashboard:
			sawGovernance = true
		}
	}
	if !sawGateway {
		t.Fatal("the ladder dropped the proxy-gateway row a W1.1 server would have accepted")
	}
	if sawGovernance {
		t.Fatal("the ladder kept the node-dashboard row the server rejects")
	}
}

// TestReporter_LadderFallsAllTheWayToCoreOnAV1Server — a genuinely old
// server walks the whole ladder in one pass and latches at core-only.
func TestReporter_LadderFallsAllTheWayToCoreOnAV1Server(t *testing.T) {
	fp := &rejectingPoster{acceptRows: 4}
	rep := newPolicyStateReporter(fp, sixReaders(), seqCounter(t), "v", true, nil)

	rep.report(context.Background())
	if got := fp.snapshot(); len(got) != 3 || got[0] != 6 || got[1] != 5 || got[2] != 4 {
		t.Fatalf("posted %v, want [6 5 4] (one rung per optional point, bounded)", got)
	}
	if !rep.latched.Load() || rep.latchDepth.Load() != 0 {
		t.Fatalf("latched=%v depth=%d, want a core-only latch", rep.latched.Load(), rep.latchDepth.Load())
	}
}
