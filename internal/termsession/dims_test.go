package termsession

import (
	"sync"
	"testing"
	"time"
)

// sizedSpec is validSpec plus an initial PTY geometry.
func sizedSpec(rows, cols uint16) Spec {
	s := validSpec()
	s.Rows, s.Cols = rows, cols
	return s
}

// TestSizeSeededFromSpec verifies the launch-Spec geometry seeds BOTH initial and
// current at Create, and that a later resize advances current while initial stays
// pinned (Feature 2).
func TestSizeSeededFromSpec(t *testing.T) {
	sp := &fakeSpawner{}
	m := newTestManager(t, sp, time.Now)

	tok, err := m.Create(sizedSpec(24, 80))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	ir, ic, cr, cc, ok := m.SessionSize(tok)
	if !ok || ir != 24 || ic != 80 || cr != 24 || cc != 80 {
		t.Fatalf("SessionSize = (%d,%d,%d,%d,%v), want initial 24×80, current 24×80", ir, ic, cr, cc, ok)
	}

	client := attachLocal(t, m, tok)
	t.Cleanup(func() { client.detach(m) })
	if err := client.Resize(30, 100); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	ir, ic, cr, cc, _ = m.SessionSize(tok)
	if ir != 24 || ic != 80 {
		t.Errorf("initial after resize = %d×%d, want it PINNED at 24×80", ir, ic)
	}
	if cr != 30 || cc != 100 {
		t.Errorf("current after resize = %d×%d, want 30×100", cr, cc)
	}
}

// TestInitialFromFirstResizeWhenSpecZero verifies a 0×0 Spec defers the initial
// size to the FIRST successful resize, and a subsequent resize moves current but
// leaves that captured initial stable (Feature 2).
func TestInitialFromFirstResizeWhenSpecZero(t *testing.T) {
	sp := &fakeSpawner{}
	m := newTestManager(t, sp, time.Now)

	tok, err := m.Create(validSpec()) // Rows/Cols == 0
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if ir, ic, cr, cc, _ := m.SessionSize(tok); ir != 0 || ic != 0 || cr != 0 || cc != 0 {
		t.Fatalf("SessionSize before any resize = (%d,%d,%d,%d), want all zero", ir, ic, cr, cc)
	}

	client := attachLocal(t, m, tok)
	t.Cleanup(func() { client.detach(m) })

	if err := client.Resize(40, 120); err != nil {
		t.Fatalf("first Resize: %v", err)
	}
	ir, ic, cr, cc, _ := m.SessionSize(tok)
	if ir != 40 || ic != 120 {
		t.Errorf("initial after first resize = %d×%d, want it ADOPTED as 40×120", ir, ic)
	}
	if cr != 40 || cc != 120 {
		t.Errorf("current after first resize = %d×%d, want 40×120", cr, cc)
	}

	if err := client.Resize(50, 130); err != nil {
		t.Fatalf("second Resize: %v", err)
	}
	ir, ic, cr, cc, _ = m.SessionSize(tok)
	if ir != 40 || ic != 120 {
		t.Errorf("initial after second resize = %d×%d, want it STILL 40×120", ir, ic)
	}
	if cr != 50 || cc != 130 {
		t.Errorf("current after second resize = %d×%d, want 50×130", cr, cc)
	}
}

// TestSizeAccessorRaceSafe exercises Size() concurrently with resizes so the
// dims lock is proven safe (meaningful under -race; harmless otherwise).
func TestSizeAccessorRaceSafe(t *testing.T) {
	sp := &fakeSpawner{}
	m := newTestManager(t, sp, time.Now)
	tok, err := m.Create(sizedSpec(10, 10))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	client := attachLocal(t, m, tok)
	t.Cleanup(func() { client.detach(m) })

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_, _, _, _, _ = m.SessionSize(tok)
			}
		}()
		go func() {
			defer wg.Done()
			for j := uint16(1); j <= 200; j++ {
				_ = client.Resize(j, j)
			}
		}()
	}
	wg.Wait()

	// Initial must remain the seed (10×10) through all the concurrent resizes.
	if ir, ic, _, _, _ := m.SessionSize(tok); ir != 10 || ic != 10 {
		t.Fatalf("initial = %d×%d after concurrent resizes, want a stable 10×10", ir, ic)
	}
}

// TestSessionSizeUnknownHandle pins the not-found contract.
func TestSessionSizeUnknownHandle(t *testing.T) {
	sp := &fakeSpawner{}
	m := newTestManager(t, sp, time.Now)
	if _, _, _, _, ok := m.SessionSize("nope"); ok {
		t.Fatal("SessionSize(unknown) ok = true, want false")
	}
}
