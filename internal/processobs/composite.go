package processobs

import (
	"context"
	"errors"
	"strings"
	"sync"
)

// Composite is a Backend that runs several child backends at once and fans
// their event streams into a single channel — so ONE daemon can capture
// processes from more than one OS source simultaneously. On the canonical WSL
// topology that means the Linux /proc poll backend (WSL-native AI tools) AND
// the cross-OS bridge (Windows AI tools) together: neither alone sees the
// other OS's processes, and their ProcessKeys never collide (distinct boot_id
// namespaces), so the merged stream needs no reconciliation.
//
// It reports RequiresUnattributedCapture (UnattributedCapturer) if ANY child
// does — the bridge does (its events arrive AttrNone), so both Windows AND
// Linux rows persist unattributed for the deferred CorrelateCrossOS pass.
// Branching on the capability, not the child's name, keeps the wiring rule-
// compliant (CLAUDE.md: branch on capabilities, never source identity).
type Composite struct {
	backends     []Backend
	onChildError func(name string, err error)
}

// NewComposite builds a Composite over the given child backends. onChildError
// (optional, nil-safe) is called when a child fails to Start — capture
// continues with the children that did start (fail-open per child). Start
// errors only when NO child starts.
func NewComposite(backends []Backend, onChildError func(name string, err error)) *Composite {
	return &Composite{backends: backends, onChildError: onChildError}
}

// Name implements Backend, e.g. "composite[poll+bridge]".
func (c *Composite) Name() string {
	names := make([]string, 0, len(c.backends))
	for _, b := range c.backends {
		names = append(names, b.Name())
	}
	return "composite[" + strings.Join(names, "+") + "]"
}

// RequiresUnattributedCapture implements UnattributedCapturer with OR
// semantics: true if any child requires unattributed capture.
func (c *Composite) RequiresUnattributedCapture() bool {
	for _, b := range c.backends {
		if uc, ok := b.(UnattributedCapturer); ok && uc.RequiresUnattributedCapture() {
			return true
		}
	}
	return false
}

// Start starts every child, fans their channels into one merged channel, and
// returns it. A child that fails to Start is skipped (onChildError + continue);
// only when NO child starts does Start return an error. The merged channel
// closes once every started child's channel has closed.
func (c *Composite) Start(ctx context.Context) (<-chan RawEvent, error) {
	var chans []<-chan RawEvent
	for _, b := range c.backends {
		ch, err := b.Start(ctx)
		if err != nil {
			if c.onChildError != nil {
				c.onChildError(b.Name(), err)
			}
			continue
		}
		chans = append(chans, ch)
	}
	if len(chans) == 0 {
		return nil, errors.New("processobs.Composite: no child backend started")
	}

	merged := make(chan RawEvent)
	var wg sync.WaitGroup
	wg.Add(len(chans))
	for _, ch := range chans {
		go func(in <-chan RawEvent) {
			defer wg.Done()
			for ev := range in {
				select {
				case merged <- ev:
				case <-ctx.Done():
					return // child honors ctx and closes `in`; no leak
				}
			}
		}(ch)
	}
	go func() {
		wg.Wait()
		close(merged)
	}()
	return merged, nil
}

// Close closes every child backend (Backend.Close is documented safe even on a
// child that never started), joining any errors.
func (c *Composite) Close() error {
	var errs []error
	for _, b := range c.backends {
		if err := b.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
