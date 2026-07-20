package termfeed

import (
	"sync"
	"testing"
	"time"
)

func ev(kind string) Event {
	return Event{Kind: kind, Trust: TrustTrusted, At: time.Now().UTC()}
}

func drain(s *Sub) []Event {
	var out []Event
	for {
		select {
		case e, ok := <-s.C():
			if !ok {
				return out
			}
			out = append(out, e)
		default:
			return out
		}
	}
}

func TestPublishFanOut(t *testing.T) {
	t.Parallel()
	f := New(Options{ReplayCap: 8, QueueCap: 8})
	a := f.Subscribe()
	b := f.Subscribe()
	if f.SubscriberCount() != 2 {
		t.Fatalf("subscriber count = %d, want 2", f.SubscriberCount())
	}
	f.Publish(ev("hook:PreToolUse"))
	f.Publish(ev("transcript:turn"))

	for name, s := range map[string]*Sub{"a": a, "b": b} {
		got := drain(s)
		if len(got) != 2 || got[0].Kind != "hook:PreToolUse" || got[1].Kind != "transcript:turn" {
			t.Errorf("sub %s got %d events %+v", name, len(got), got)
		}
	}
}

func TestReplaySeedsLateSubscriber(t *testing.T) {
	t.Parallel()
	f := New(Options{ReplayCap: 8, QueueCap: 8})
	f.Publish(ev("e1"))
	f.Publish(ev("e2"))
	f.Publish(ev("e3"))

	late := f.Subscribe()
	got := drain(late)
	if len(got) != 3 {
		t.Fatalf("late subscriber replayed %d events, want 3", len(got))
	}
	if got[0].Kind != "e1" || got[2].Kind != "e3" {
		t.Errorf("replay order wrong: %+v", got)
	}
	if late.Lost() != 0 {
		t.Errorf("no loss expected, got %d", late.Lost())
	}
}

func TestReplayRingBounded(t *testing.T) {
	t.Parallel()
	f := New(Options{ReplayCap: 3, QueueCap: 8})
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		f.Publish(ev(k))
	}
	// Only the last 3 survive the ring.
	late := f.Subscribe()
	got := drain(late)
	if len(got) != 3 || got[0].Kind != "c" || got[2].Kind != "e" {
		t.Fatalf("bounded replay = %+v", got)
	}
}

func TestSlowSubscriberDropsOldestNotBlock(t *testing.T) {
	t.Parallel()
	// Queue depth 2; publish 5 without draining. The publisher must not block,
	// the subscriber keeps the NEWEST, and Lost accounts for the drops.
	f := New(Options{ReplayCap: 16, QueueCap: 2})
	s := f.Subscribe()
	for _, k := range []string{"1", "2", "3", "4", "5"} {
		f.Publish(ev(k)) // never blocks
	}
	got := drain(s)
	if len(got) != 2 {
		t.Fatalf("queue should hold 2 newest, got %d: %+v", len(got), got)
	}
	if got[0].Kind != "4" || got[1].Kind != "5" {
		t.Errorf("expected newest-kept [4 5], got %+v", got)
	}
	if s.Lost() != 3 {
		t.Errorf("Lost = %d, want 3", s.Lost())
	}
}

func TestReplayLargerThanQueueMarksLoss(t *testing.T) {
	t.Parallel()
	// Replay ring holds 6, but a new subscriber's queue is only 2 → it seeds the
	// 2 newest and records 4 lost.
	f := New(Options{ReplayCap: 6, QueueCap: 2})
	for _, k := range []string{"a", "b", "c", "d", "e", "f"} {
		f.Publish(ev(k))
	}
	s := f.Subscribe()
	got := drain(s)
	if len(got) != 2 || got[0].Kind != "e" || got[1].Kind != "f" {
		t.Fatalf("seed newest = %+v", got)
	}
	if s.Lost() != 4 {
		t.Errorf("Lost = %d, want 4", s.Lost())
	}
}

func TestUnsubscribeClosesChannel(t *testing.T) {
	t.Parallel()
	f := New(Options{ReplayCap: 4, QueueCap: 4})
	s := f.Subscribe()
	f.Unsubscribe(s)
	if f.SubscriberCount() != 0 {
		t.Fatalf("subscriber count = %d, want 0", f.SubscriberCount())
	}
	// Channel closed → receive returns ok=false.
	if _, ok := <-s.C(); ok {
		t.Fatal("expected closed channel after Unsubscribe")
	}
	// Publishing after unsubscribe must not panic (no closed-channel send).
	f.Publish(ev("after"))
	// Double unsubscribe is a no-op.
	f.Unsubscribe(s)
}

// TestConcurrentPublishSubscribe is the race-detector exercise: many producers
// and consumers churning while subscribers come and go.
func TestConcurrentPublishSubscribe(t *testing.T) {
	t.Parallel()
	f := New(Options{ReplayCap: 32, QueueCap: 16})
	var wg sync.WaitGroup

	// Producers.
	for p := 0; p < 4; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				f.Publish(ev("k"))
			}
		}()
	}
	// Consumers that subscribe, drain a bit, and leave.
	for c := 0; c < 4; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := f.Subscribe()
			for i := 0; i < 50; i++ {
				select {
				case <-s.C():
				default:
				}
			}
			f.Unsubscribe(s)
		}()
	}
	wg.Wait()
	// After everyone leaves, the transient subscribers are gone.
	if f.SubscriberCount() != 0 {
		t.Errorf("leaked subscribers: %d", f.SubscriberCount())
	}
}
