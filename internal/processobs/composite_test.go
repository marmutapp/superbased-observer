package processobs

import (
	"context"
	"errors"
	"testing"
)

// unattributedFake is a FakeBackend that also implements UnattributedCapturer
// (like the real cross-OS bridge), for the OR-semantics test.
type unattributedFake struct{ *FakeBackend }

func (unattributedFake) RequiresUnattributedCapture() bool { return true }

func TestCompositeFanIn(t *testing.T) {
	a := &FakeBackend{BackendName: "poll", Events: []RawEvent{
		{Type: EventExec, BootID: "linux-1", PID: 10},
		{Type: EventExec, BootID: "linux-1", PID: 11},
	}}
	b := &FakeBackend{BackendName: "bridge", Events: []RawEvent{
		{Type: EventExec, BootID: "win-boot-1", PID: 20},
	}}
	c := NewComposite([]Backend{a, b}, nil)

	if got := c.Name(); got != "composite[poll+bridge]" {
		t.Errorf("Name = %q", got)
	}

	ch, err := c.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	got := map[int]string{}
	for ev := range ch {
		got[ev.PID] = ev.BootID
	}
	if len(got) != 3 || got[10] != "linux-1" || got[11] != "linux-1" || got[20] != "win-boot-1" {
		t.Fatalf("merged events wrong: %v", got)
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if !a.Closed() || !b.Closed() {
		t.Error("Close must close all children")
	}
}

func TestCompositeRequiresUnattributedCaptureOR(t *testing.T) {
	plain := &FakeBackend{BackendName: "poll"}
	if NewComposite([]Backend{plain}, nil).RequiresUnattributedCapture() {
		t.Error("no child requires unattributed capture → want false")
	}
	bridgeLike := unattributedFake{&FakeBackend{BackendName: "bridge"}}
	if !NewComposite([]Backend{plain, bridgeLike}, nil).RequiresUnattributedCapture() {
		t.Error("one child requires it → want true (OR semantics)")
	}
}

func TestCompositePartialStartFailOpen(t *testing.T) {
	ok := &FakeBackend{BackendName: "poll", Events: []RawEvent{{Type: EventExec, PID: 1}}}
	bad := &FakeBackend{BackendName: "bridge", StartErr: errors.New("no windows binary")}
	var failed string
	c := NewComposite([]Backend{ok, bad}, func(name string, _ error) { failed = name })

	ch, err := c.Start(context.Background())
	if err != nil {
		t.Fatalf("partial start should succeed with one good child: %v", err)
	}
	if failed != "bridge" {
		t.Errorf("onChildError should report the failed child, got %q", failed)
	}
	n := 0
	for range ch {
		n++
	}
	if n != 1 {
		t.Errorf("should still receive the good child's 1 event, got %d", n)
	}
}

func TestCompositeAllChildrenFail(t *testing.T) {
	bad1 := &FakeBackend{StartErr: errors.New("x")}
	bad2 := &FakeBackend{StartErr: errors.New("y")}
	if _, err := NewComposite([]Backend{bad1, bad2}, nil).Start(context.Background()); err == nil {
		t.Error("Start must error when no child starts")
	}
}
