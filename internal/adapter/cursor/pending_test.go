package cursor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMain redirects the pending-reasoning stash into a throwaway temp
// directory for the WHOLE package. Without it, any test that builds an
// afterAgentThought event would write into the operator's real
// ~/.observer/cursor-reasoning — a test must never touch live user
// state.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "cursor-reasoning-test-")
	if err != nil {
		panic(err)
	}
	pendingReasoningDir = func() string { return dir }
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// participants returns n INDEPENDENT stash participants over one shared
// directory. Each stands in for a separate `observer hook cursor`
// PROCESS: they share only the filesystem, never a mutex or a memo —
// which is the whole point, since the races these tests chase happen
// between processes, where no process-local state exists to help.
func participants(t *testing.T, n int) (string, []*reasoningStash) {
	t.Helper()
	dir := t.TempDir()
	out := make([]*reasoningStash, n)
	for i := range out {
		out[i] = &reasoningStash{dir: dir}
	}
	return dir, out
}

// TestStashProtocol_ExactlyOneConsumerWins pins CONSUMED-ONCE across
// processes: N participants race for one thought and exactly one may
// come away with it. Pre-fix (read-then-remove) 190/200 iterations
// threaded the same thought onto several actions.
func TestStashProtocol_ExactlyOneConsumerWins(t *testing.T) {
	const (
		n     = 16
		iters = 200
	)
	_, procs := participants(t, n)
	for iter := 0; iter < iters; iter++ {
		procs[0].stash("conv-once", "the one thought")

		var mu sync.Mutex
		var winners []string
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range procs {
			wg.Add(1)
			go func(p *reasoningStash, i int) {
				defer wg.Done()
				<-start
				// Distinct event ids: the per-process memo must not be
				// what makes this pass.
				if v := p.take("conv-once", fmt.Sprintf("ev-%d-%d", iter, i)); v != "" {
					mu.Lock()
					winners = append(winners, v)
					mu.Unlock()
				}
			}(procs[i], i)
		}
		close(start)
		wg.Wait()

		if len(winners) != 1 {
			t.Fatalf("iteration %d: %d consumers claimed the thought, want exactly 1 (%v)", iter, len(winners), winners)
		}
		if winners[0] != "the one thought" {
			t.Fatalf("iteration %d: winner read %q", iter, winners[0])
		}
	}
}

// TestStashProtocol_TargetIsNeverPartiallyVisible pins the WRITE half at
// the PROTOCOL level: an observer that reads the stash file directly —
// which is what any other participant's read is, once it has claimed the
// name — must never see a partial body. A bare in-place write truncates
// first and fills afterwards, so the file is legitimately half-written
// for a window; write-to-temp-then-rename removes that state from
// existence, because the target name only ever points at a finished
// inode.
//
// This is the test that KILLS the in-place-write mutant. The consume-side
// test below cannot: the claiming rename already narrows the window so
// far that tearing stops being observable through take().
func TestStashProtocol_TargetIsNeverPartiallyVisible(t *testing.T) {
	_, procs := participants(t, 2)
	writer, observer := procs[0], procs[1]
	older := strings.Repeat("o", 200000)
	newer := strings.Repeat("n", 200000)
	path := observer.path("conv-visible")

	const iters = 400
	var wg sync.WaitGroup
	wg.Add(2)
	done := make(chan struct{})
	go func() {
		defer wg.Done()
		defer close(done)
		for i := 0; i < iters; i++ {
			writer.stash("conv-visible", older)
			writer.stash("conv-visible", newer)
		}
	}()
	var torn int
	var sample string
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			body, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			if v := string(body); v != older && v != newer {
				torn++
				if sample == "" {
					sample = v
				}
			}
		}
	}()
	wg.Wait()
	if torn > 0 {
		t.Fatalf("observed %d PARTIAL stash bodies at the target path (first was %d bytes)", torn, len(sample))
	}
}

// TestStashProtocol_ConsumerSeesWholeFileOrNothing is the API-level half:
// take() racing a writer returns the complete old value, the complete new
// value, or nothing.
func TestStashProtocol_ConsumerSeesWholeFileOrNothing(t *testing.T) {
	_, procs := participants(t, 2)
	writer, consumer := procs[0], procs[1]
	const iters = 200
	older := strings.Repeat("o", 200000)
	newer := strings.Repeat("n", 200000)

	for iter := 0; iter < iters; iter++ {
		writer.stash("conv-torn", older)

		var got string
		var wg sync.WaitGroup
		wg.Add(2)
		start := make(chan struct{})
		go func() { defer wg.Done(); <-start; writer.stash("conv-torn", newer) }()
		go func() { defer wg.Done(); <-start; got = consumer.take("conv-torn", fmt.Sprintf("ev-torn-%d", iter)) }()
		close(start)
		wg.Wait()

		if got != "" && got != older && got != newer {
			cut := len(got)
			if cut > 24 {
				cut = 24
			}
			t.Fatalf("iteration %d: consumer read a TORN body of %d bytes (prefix %q)", iter, len(got), got[:cut])
		}
		consumer.clear("conv-torn")
	}
}

// TestStashProtocol_SweepNeverDestroysAFreshThought pins the sweeper:
// it removes only what it has CLAIMED and re-checks staleness on the
// claimed inode, so a thought written for the SAME conversation while
// the sweep is running survives. That is the exact window the previous
// list-then-remove-by-name sweeper lost: it decided "stale" from the
// listing and then deleted whatever the name pointed at, including a
// thought written a microsecond earlier.
func TestStashProtocol_SweepNeverDestroysAFreshThought(t *testing.T) {
	dir, procs := participants(t, 2)
	writer, sweeper := procs[0], procs[1]
	stale := time.Now().Add(-2 * pendingReasoningTTL)

	const iters = 400
	for iter := 0; iter < iters; iter++ {
		// An abandoned thought for THIS conversation, backdated so the
		// sweeper is genuinely entitled to collect it.
		writer.stash("conv-swept", "an abandoned thought")
		path := writer.path("conv-swept")
		if err := os.Chtimes(path, stale, stale); err != nil {
			t.Fatalf("chtimes: %v", err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		start := make(chan struct{})
		go func() { defer wg.Done(); <-start; writer.stash("conv-swept", "the fresh thought") }()
		go func() { defer wg.Done(); <-start; sweeper.sweepStale(dir) }()
		close(start)
		wg.Wait()

		if got := writer.take("conv-swept", fmt.Sprintf("ev-swept-%d", iter)); got != "the fresh thought" {
			t.Fatalf("iteration %d: the sweep destroyed a freshly written thought (got %q)", iter, got)
		}
	}
}

// TestStashProtocol_ClearDoesNotEatALaterThought pins the turn-boundary
// discard: a prompt clearing the previous turn must not remove a thought
// that landed after its claim, and must never resurrect the stale one.
func TestStashProtocol_ClearDoesNotEatALaterThought(t *testing.T) {
	_, procs := participants(t, 2)
	writer, prompt := procs[0], procs[1]
	const iters = 300
	lost := 0
	for iter := 0; iter < iters; iter++ {
		writer.stash("conv-turn", "stale thought")

		var wg sync.WaitGroup
		wg.Add(2)
		start := make(chan struct{})
		go func() { defer wg.Done(); <-start; prompt.clear("conv-turn") }()
		go func() { defer wg.Done(); <-start; writer.stash("conv-turn", "next turn thought") }()
		close(start)
		wg.Wait()

		got := writer.take("conv-turn", fmt.Sprintf("ev-turn-%d", iter))
		switch got {
		case "next turn thought":
			// The new turn's thought survived.
		case "":
			// The clear won the claim after the replace: the newer
			// thought was the one discarded. Legal — the two events are
			// genuinely concurrent — but the STALE one must never come
			// back, which the default branch enforces.
			lost++
		default:
			t.Fatalf("iteration %d: a discarded turn's thought leaked forward: %q", iter, got)
		}
	}
	if lost == iters {
		t.Fatalf("every iteration lost the new thought — clear is still deleting by name")
	}
}

// TestPendingReasoning_TTLExpiry pins that a thought abandoned by a
// conversation that never produced a successor is not claimed by an
// unrelated event much later — it is debris, not this turn's reasoning.
func TestPendingReasoning_TTLExpiry(t *testing.T) {
	stashReasoning("conv-ttl", "an old thought")
	path := defaultStash.path("conv-ttl")
	old := time.Now().Add(-2 * pendingReasoningTTL)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if got := takeReasoning("conv-ttl", "ev-1"); got != "" {
		t.Errorf("expired stash returned %q", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expired stash file survived take: %v", err)
	}
}

// TestPendingReasoning_SameEventIDReplays pins the in-process memo the
// GUARDED hook path depends on: HandleCursorEventGuarded calls
// BuildEvent twice for ONE payload (once via BuildCursorEvent for the
// policy event, once to build the row), and the first call must not
// swallow the stash before the row that needs it exists.
func TestPendingReasoning_SameEventIDReplays(t *testing.T) {
	stashReasoning("conv-memo", "the thought")
	if got := takeReasoning("conv-memo", "ev-memo"); got != "the thought" {
		t.Fatalf("first take = %q", got)
	}
	if got := takeReasoning("conv-memo", "ev-memo"); got != "the thought" {
		t.Errorf("replay of the same event id = %q, want the memoized value", got)
	}
	if got := takeReasoning("conv-memo", "ev-other"); got != "" {
		t.Errorf("a DIFFERENT event id re-claimed the consumed stash: %q", got)
	}
}

// TestPendingReasoning_ConversationIDCannotSteerPath pins that a hostile
// conversation id from the host tool's payload lands inside the stash
// dir, never above it.
func TestPendingReasoning_ConversationIDCannotSteerPath(t *testing.T) {
	path := defaultStash.path("../../etc/passwd")
	if got := filepath.Dir(path); got != pendingReasoningDir() {
		t.Errorf("path escaped the stash dir: %q", path)
	}
}

// TestStashProtocol_LeavesNoDebris pins that the transient tmp/claim
// files never accumulate: after a write + a consume the directory is
// empty again.
func TestStashProtocol_LeavesNoDebris(t *testing.T) {
	dir, procs := participants(t, 1)
	p := procs[0]
	p.stash("conv-debris", "a thought")
	if got := p.take("conv-debris", "ev-debris"); got != "a thought" {
		t.Fatalf("take = %q", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("stash dir still holds %v", names)
	}
}
