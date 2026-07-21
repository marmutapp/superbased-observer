package aggregatesvc

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/aggregate"
	"github.com/marmutapp/superbased-observer/internal/aggregateclient"
	"github.com/marmutapp/superbased-observer/internal/aggregatesource"
	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/integration"
	"github.com/marmutapp/superbased-observer/internal/intelligence/cost"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// --- fakes ---------------------------------------------------------------

type fakeStore struct {
	receipt *aggregate.Receipt
	rows    map[string]*store.AggregateSubmissionRow

	startID       string // if non-empty, StartAggregateAttempt returns this id
	starts        int
	submitted     int
	failed        int
	lastFailErr   string
	forceStartErr error
}

func newFakeStore(receipt *aggregate.Receipt) *fakeStore {
	return &fakeStore{receipt: receipt, rows: map[string]*store.AggregateSubmissionRow{}}
}

func (f *fakeStore) LoadConsentReceipt(context.Context) (*aggregate.Receipt, error) {
	return f.receipt, nil
}

func (f *fakeStore) LoadAggregateState(_ context.Context, month string) (*store.AggregateSubmissionRow, error) {
	return f.rows[month], nil
}

func (f *fakeStore) StartAggregateAttempt(_ context.Context, month, submissionID, payloadHash, payloadJSON string, schemaVersion int, now time.Time) (string, error) {
	if f.forceStartErr != nil {
		return "", f.forceStartErr
	}
	f.starts++
	id := submissionID
	if f.startID != "" {
		id = f.startID
	}
	r := f.rows[month]
	if r == nil {
		r = &store.AggregateSubmissionRow{Month: month}
		f.rows[month] = r
	}
	r.SubmissionID = id
	r.PayloadHash = payloadHash
	r.PayloadJSON = payloadJSON
	r.SchemaVersion = schemaVersion
	r.Attempts++
	r.State = store.AggregateStatePending
	return id, nil
}

func (f *fakeStore) MarkAggregateSubmitted(_ context.Context, month string, _ time.Time) error {
	f.submitted++
	if r := f.rows[month]; r != nil {
		r.State = store.AggregateStateSubmitted
	}
	return nil
}

func (f *fakeStore) MarkAggregateFailed(_ context.Context, month, errMsg string, _ time.Time) error {
	f.failed++
	f.lastFailErr = errMsg
	if r := f.rows[month]; r != nil {
		r.State = store.AggregateStateFailed
	}
	return nil
}

func (f *fakeStore) ListAggregateStates(context.Context) ([]store.AggregateSubmissionRow, error) {
	out := make([]store.AggregateSubmissionRow, 0, len(f.rows))
	for _, r := range f.rows {
		out = append(out, *r)
	}
	return out, nil
}

type fakeSubmitter struct {
	calls   int
	gotGate aggregateclient.Gate
	err     error
}

func (f *fakeSubmitter) Submit(_ context.Context, gate aggregateclient.Gate, _ aggregate.Submission) error {
	f.calls++
	f.gotGate = gate
	return f.err
}
func (f *fakeSubmitter) Endpoint() string { return "https://aggregate.superbased.app/v1/submit" }

// --- fixtures ------------------------------------------------------------

const fixedEndpoint = "https://aggregate.superbased.app/v1/submit"

func validLive() aggregate.LiveState {
	return aggregate.LiveState{
		Enabled:             true,
		SchemaVersion:       aggregate.SchemaVersion,
		Endpoint:            fixedEndpoint,
		ToolRegistryVersion: integration.RegistryVersion,
	}
}

func validReceipt() *aggregate.Receipt {
	return &aggregate.Receipt{
		SchemaVersion:       aggregate.SchemaVersion,
		Endpoint:            aggregate.NormalizeEndpoint(fixedEndpoint),
		ToolRegistryVersion: integration.RegistryVersion,
	}
}

// fixedNow sits inside 2026-07 so FinalizedMonth == "2026-06".
func fixedNow() time.Time { return time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC) }

const wantMonth = "2026-06"

// oneCellStats returns a single above-floor accurate stat so Build yields a
// non-vacuous cell that never coarsens to "other".
func oneCellStats() []aggregate.ModelToolStat {
	return []aggregate.ModelToolStat{{
		Model: "claude-opus-4-8", Tool: "claude-code", Accurate: true,
		Turns: 100, InputTokens: 1000, OutputTokens: 500, CostUSD: 5.0,
		CacheObservable: true, FastObservable: true,
	}}
}

// newSpyCollector wires a collector over fakes with a spying builder.
func newSpyCollector(t *testing.T, st Store, sub *fakeSubmitter, live func() aggregate.LiveState) (*Collector, *[]string) {
	t.Helper()
	var buildIDs []string
	c, err := New(Config{
		Store: st,
		Build: func(_ context.Context, month, id string) (aggregate.Submission, error) {
			buildIDs = append(buildIDs, id)
			return aggregate.Build(aggregate.Meta{ObserverVersion: "1.20", SubmissionID: id, Month: month}, oneCellStats()), nil
		},
		Live:      live,
		NewClient: func(string) (Submitter, error) { return sub, nil },
		Now:       fixedNow,
		NewID:     func() string { return "fresh-id" },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, &buildIDs
}

// --- tests ---------------------------------------------------------------

func TestSubmitMonth_NotConsented_ProducesNothing(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		live    aggregate.LiveState
		receipt *aggregate.Receipt
	}{
		{"disabled", func() aggregate.LiveState { l := validLive(); l.Enabled = false; return l }(), validReceipt()},
		{"missing receipt", validLive(), nil},
		{"schema changed", validLive(), func() *aggregate.Receipt {
			r := validReceipt()
			r.SchemaVersion = aggregate.SchemaVersion + 1
			return r
		}()},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st := newFakeStore(tc.receipt)
			sub := &fakeSubmitter{}
			c, buildIDs := newSpyCollector(t, st, sub, func() aggregate.LiveState { return tc.live })
			res, err := c.SubmitMonth(context.Background(), "", false)
			if err == nil {
				t.Fatalf("expected an error for %s", tc.name)
			}
			if !errors.Is(err, aggregateclient.ErrNotConsented) {
				t.Errorf("want ErrNotConsented, got %v", err)
			}
			if res.Status == aggregate.ConsentValid {
				t.Errorf("status should not be valid")
			}
			// Produced nothing: no build, no send, no ledger write.
			if len(*buildIDs) != 0 {
				t.Errorf("payload was built for a non-consented rail: %v", *buildIDs)
			}
			if sub.calls != 0 {
				t.Errorf("submitter called %d times for a non-consented rail", sub.calls)
			}
			if st.starts != 0 {
				t.Errorf("ledger written %d times for a non-consented rail", st.starts)
			}
		})
	}
}

func TestSubmitMonth_DryRun_BuildsButSendsNothing(t *testing.T) {
	t.Parallel()
	st := newFakeStore(validReceipt())
	sub := &fakeSubmitter{}
	c, buildIDs := newSpyCollector(t, st, sub, validLive)

	res, err := c.SubmitMonth(context.Background(), "", true)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !res.DryRun || res.Sent {
		t.Errorf("dry-run result flags wrong: %+v", res)
	}
	if res.Month != wantMonth {
		t.Errorf("month = %q, want %q", res.Month, wantMonth)
	}
	if res.Cells != 1 || res.Bytes == 0 {
		t.Errorf("dry-run should report the built payload: %+v", res)
	}
	if len(*buildIDs) != 1 {
		t.Errorf("dry-run should build exactly once, built %d", len(*buildIDs))
	}
	if sub.calls != 0 {
		t.Errorf("dry-run must not send, sent %d", sub.calls)
	}
	if st.starts != 0 {
		t.Errorf("dry-run must not touch the ledger, starts=%d", st.starts)
	}
}

func TestSubmitMonth_HappyPath_SendsAndMarks(t *testing.T) {
	t.Parallel()
	st := newFakeStore(validReceipt())
	sub := &fakeSubmitter{}
	c, buildIDs := newSpyCollector(t, st, sub, validLive)

	res, err := c.SubmitMonth(context.Background(), "", false)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !res.Sent {
		t.Errorf("expected Sent=true: %+v", res)
	}
	if res.SubmissionID != "fresh-id" {
		t.Errorf("submission_id = %q, want fresh-id", res.SubmissionID)
	}
	if st.starts != 1 || st.submitted != 1 || st.failed != 0 {
		t.Errorf("ledger transitions wrong: starts=%d submitted=%d failed=%d", st.starts, st.submitted, st.failed)
	}
	if sub.calls != 1 {
		t.Errorf("submitter called %d times, want 1", sub.calls)
	}
	if len(*buildIDs) != 1 {
		t.Errorf("built %d times, want 1", len(*buildIDs))
	}
}

func TestSubmitMonth_AlreadySubmitted_Skips(t *testing.T) {
	t.Parallel()
	st := newFakeStore(validReceipt())
	st.rows[wantMonth] = &store.AggregateSubmissionRow{Month: wantMonth, SubmissionID: "old-id", State: store.AggregateStateSubmitted, Attempts: 2}
	sub := &fakeSubmitter{}
	c, buildIDs := newSpyCollector(t, st, sub, validLive)

	res, err := c.SubmitMonth(context.Background(), "", false)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if res.Skipped != "already submitted" || res.Sent {
		t.Errorf("expected skip, got %+v", res)
	}
	if res.SubmissionID != "old-id" || res.Attempts != 2 {
		t.Errorf("skip result should carry prior ledger state: %+v", res)
	}
	if len(*buildIDs) != 0 || sub.calls != 0 {
		t.Errorf("already-submitted must not build or send")
	}
}

func TestSubmitMonth_ReusesSubmissionIDOnRetry(t *testing.T) {
	t.Parallel()
	st := newFakeStore(validReceipt())
	// A prior FAILED attempt persisted a submission_id — it must be reused.
	st.rows[wantMonth] = &store.AggregateSubmissionRow{Month: wantMonth, SubmissionID: "persisted-id", State: store.AggregateStateFailed, Attempts: 1}
	sub := &fakeSubmitter{}
	c, buildIDs := newSpyCollector(t, st, sub, validLive)

	res, err := c.SubmitMonth(context.Background(), "", false)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if res.SubmissionID != "persisted-id" {
		t.Errorf("retry must reuse persisted submission_id, got %q", res.SubmissionID)
	}
	// The build carried the persisted id (not the fresh one).
	if len(*buildIDs) == 0 || (*buildIDs)[0] != "persisted-id" {
		t.Errorf("build should have used persisted id, ids=%v", *buildIDs)
	}
}

func TestSubmitMonth_RebuildsWhenLedgerReturnsDifferentID(t *testing.T) {
	t.Parallel()
	st := newFakeStore(validReceipt())
	st.startID = "ledger-id" // StartAggregateAttempt hands back a different id
	sub := &fakeSubmitter{}
	c, buildIDs := newSpyCollector(t, st, sub, validLive)

	res, err := c.SubmitMonth(context.Background(), "", false)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if res.SubmissionID != "ledger-id" {
		t.Errorf("result should carry the ledger-returned id, got %q", res.SubmissionID)
	}
	// Built twice: once with the fresh id, once rebuilt with the ledger id.
	if len(*buildIDs) != 2 || (*buildIDs)[1] != "ledger-id" {
		t.Errorf("expected a rebuild with the ledger id, ids=%v", *buildIDs)
	}
}

func TestSubmitMonth_SubmitFailure_MarksFailed(t *testing.T) {
	t.Parallel()
	st := newFakeStore(validReceipt())
	sub := &fakeSubmitter{err: errors.New("boom")}
	c, _ := newSpyCollector(t, st, sub, validLive)

	_, err := c.SubmitMonth(context.Background(), "", false)
	if err == nil {
		t.Fatal("expected a submit error")
	}
	if st.failed != 1 || st.submitted != 0 {
		t.Errorf("failure must mark the ledger failed (not submitted): failed=%d submitted=%d", st.failed, st.submitted)
	}
}

func TestSubmitMonth_PartialMonthRefused(t *testing.T) {
	t.Parallel()
	st := newFakeStore(validReceipt())
	c, _ := newSpyCollector(t, st, &fakeSubmitter{}, validLive)
	// The current month (relative to fixedNow) is never finalized.
	if _, err := c.SubmitMonth(context.Background(), "2026-07", false); err == nil {
		t.Error("a partial/current month must be refused")
	}
}

func TestSubmitDue_InertWhenDisabled(t *testing.T) {
	t.Parallel()
	st := newFakeStore(validReceipt())
	sub := &fakeSubmitter{}
	c, buildIDs := newSpyCollector(t, st, sub, func() aggregate.LiveState { l := validLive(); l.Enabled = false; return l })

	res, err := c.SubmitDue(context.Background())
	if err != nil {
		t.Fatalf("SubmitDue must be quiet (nil error) when inert, got %v", err)
	}
	if res.Sent || res.Skipped == "" {
		t.Errorf("disabled SubmitDue should skip: %+v", res)
	}
	if len(*buildIDs) != 0 || sub.calls != 0 || st.starts != 0 {
		t.Error("inert SubmitDue must build/send/persist nothing")
	}
}

func TestSubmitDue_ValidSends(t *testing.T) {
	t.Parallel()
	st := newFakeStore(validReceipt())
	sub := &fakeSubmitter{}
	c, _ := newSpyCollector(t, st, sub, validLive)

	res, err := c.SubmitDue(context.Background())
	if err != nil {
		t.Fatalf("SubmitDue: %v", err)
	}
	if !res.Sent || res.Month != wantMonth {
		t.Errorf("valid SubmitDue should send the finalized month: %+v", res)
	}
	if sub.calls != 1 || st.submitted != 1 {
		t.Errorf("SubmitDue did not drive the send: submit=%d marked=%d", sub.calls, st.submitted)
	}
}

func TestPreview_NoLedgerNoSend_EvenWhenDisabled(t *testing.T) {
	t.Parallel()
	st := newFakeStore(nil) // no receipt, rail off
	sub := &fakeSubmitter{}
	c, buildIDs := newSpyCollector(t, st, sub, func() aggregate.LiveState { l := validLive(); l.Enabled = false; return l })

	got, err := c.Preview(context.Background(), "")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if got.Month != wantMonth || len(got.Cells) != 1 {
		t.Errorf("preview payload unexpected: %+v", got)
	}
	if len(*buildIDs) != 1 {
		t.Errorf("preview should build once")
	}
	if sub.calls != 0 || st.starts != 0 || st.submitted != 0 {
		t.Error("preview must never send or touch the ledger")
	}
}

// TestSubmitMonth_FixtureDB_ExpectedPayload wires the REAL aggregatesource
// builder over a seeded fixture DB and asserts the collector's dry-run path
// yields the expected (family,tool) payload — the full "fixture DB -> expected
// payload" path THROUGH the collector — and that a disabled rail produces
// nothing (no ledger row written).
func TestSubmitMonth_FixtureDB_ExpectedPayload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, err := db.Open(ctx, db.Options{Path: filepath.Join(t.TempDir(), "agg.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer database.Close()
	st := store.New(database)

	ts := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC) // inside wantMonth
	projID, err := st.UpsertProject(ctx, "/fixture/proj", "")
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if err := st.UpsertSession(ctx, models.Session{ID: "s1", ProjectID: projID, Tool: "claude-code", Model: "claude-opus-4-8", StartedAt: ts}); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	if _, err := st.InsertAPITurn(ctx, models.APITurn{
		SessionID: "s1", ProjectID: projID, Timestamp: ts, Provider: "anthropic",
		Model: "claude-opus-4-8", RequestID: "r1", InputTokens: 5000, OutputTokens: 2000, CostUSD: 3.0,
	}); err != nil {
		t.Fatalf("InsertAPITurn: %v", err)
	}

	engine := cost.NewEngine(config.IntelligenceConfig{})
	builder := func(bctx context.Context, month, id string) (aggregate.Submission, error) {
		return aggregatesource.BuildSubmission(bctx, database, engine, aggregate.Meta{ObserverVersion: "1.20", SubmissionID: id, Month: month})
	}

	// Disabled rail: dry-run submit must produce nothing and write no ledger row.
	cOff, err := New(Config{Store: st, Build: builder, Live: func() aggregate.LiveState { l := validLive(); l.Enabled = false; return l }, Now: fixedNow, NewID: func() string { return "id" }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := cOff.SubmitMonth(ctx, wantMonth, true); err == nil {
		t.Error("disabled rail must refuse even a dry-run submit")
	}
	if rows, _ := st.ListAggregateStates(ctx); len(rows) != 0 {
		t.Errorf("disabled rail wrote a ledger row: %+v", rows)
	}

	// Consent present + valid: dry-run yields the expected payload.
	if err := st.SaveConsentReceipt(ctx, *validReceipt()); err != nil {
		t.Fatalf("SaveConsentReceipt: %v", err)
	}
	cOn, err := New(Config{Store: st, Build: builder, Live: validLive, Now: fixedNow, NewID: func() string { return "id" }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := cOn.SubmitMonth(ctx, wantMonth, true)
	if err != nil {
		t.Fatalf("dry-run submit: %v", err)
	}
	var found *aggregate.Cell
	for i := range res.Sub.Cells {
		if res.Sub.Cells[i].ModelFamily == "claude-opus" && res.Sub.Cells[i].Tool == "claude-code" {
			found = &res.Sub.Cells[i]
		}
	}
	if found == nil {
		t.Fatalf("expected a (claude-opus, claude-code) cell; got %+v", res.Sub.Cells)
	}
	if found.TurnsAcc != 1 || found.InputTokensAcc != 5000 || found.OutputTokensAcc != 2000 {
		t.Errorf("fixture payload sums wrong: %+v", found)
	}
	if found.CostUSDAcc != 3.0 {
		t.Errorf("fixture cost = %v, want 3.0", found.CostUSDAcc)
	}
	// Dry-run must still not persist.
	if rows, _ := st.ListAggregateStates(ctx); len(rows) != 0 {
		t.Errorf("dry-run wrote a ledger row: %+v", rows)
	}
}
