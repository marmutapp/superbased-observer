package config_test

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/config"
)

// TestConfigLockSeamsAreOneMutex proves the two public serialization seams —
// WithConfigLock (the closure form the cmd/observer admission persister uses)
// and WriteLock() (the *sync.Mutex the dashboard config handlers hold
// defer-style) — contend on ONE mutex. If they did not, the two goroutines
// below would race the shared counter (flagged by -race) and lose increments.
// This is the mechanism both real callers rely on to serialize against each
// other in the daemon process.
func TestConfigLockSeamsAreOneMutex(t *testing.T) {
	const rounds = 2000
	counter := 0
	var wg sync.WaitGroup
	for i := 0; i < rounds; i++ {
		wg.Add(2)
		// Persister-style: whole span inside WithConfigLock.
		go func() {
			defer wg.Done()
			_ = config.WithConfigLock(func() error {
				counter++
				return nil
			})
		}()
		// Section-save-style: hold WriteLock() across the span.
		go func() {
			defer wg.Done()
			mu := config.WriteLock()
			mu.Lock()
			counter++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if counter != rounds*2 {
		t.Fatalf("counter = %d, want %d — the two seams are not the same mutex (lost increments)", counter, rounds*2)
	}
}

// TestConfigLockPreventsLostUpdate faithfully models the real hazard: an
// admission-persister-style read-modify-write (via WithConfigLock) racing a
// dashboard-section-save-style read-modify-write (via WriteLock()) against the
// SAME config.toml, each editing an INDEPENDENT field. Serialized, whichever
// writes second loads the other's write first, so BOTH edits survive. Without
// the shared lock this is a classic lost update. Repeated across fresh files to
// widen the window a regression would have to survive.
func TestConfigLockPreventsLostUpdate(t *testing.T) {
	for round := 0; round < 200; round++ {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.toml")
		base := config.Default()
		base.Observer.Process.Enabled = false
		base.Observer.Process.Backend = "auto" // valid seed; goroutine B flips it to "poll"
		if err := config.WriteToml(path, base); err != nil {
			t.Fatalf("seed: %v", err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		// Persister-style RMW: flips Process.Enabled.
		go func() {
			defer wg.Done()
			_ = config.WithConfigLock(func() error {
				cfg, err := config.Load(config.LoadOptions{GlobalPath: path})
				if err != nil {
					return err
				}
				cfg.Observer.Process.Enabled = true
				return config.WriteToml(path, cfg)
			})
		}()
		// Section-save-style RMW: sets Process.Backend, holding WriteLock().
		go func() {
			defer wg.Done()
			mu := config.WriteLock()
			mu.Lock()
			defer mu.Unlock()
			cfg, err := config.Load(config.LoadOptions{GlobalPath: path})
			if err != nil {
				t.Errorf("load: %v", err)
				return
			}
			cfg.Observer.Process.Backend = "poll"
			if err := config.WriteToml(path, cfg); err != nil {
				t.Errorf("write: %v", err)
			}
		}()
		wg.Wait()

		got, err := config.Load(config.LoadOptions{GlobalPath: path})
		if err != nil {
			t.Fatalf("final load: %v", err)
		}
		if !got.Observer.Process.Enabled || got.Observer.Process.Backend != "poll" {
			t.Fatalf("round %d: lost update — enabled=%v backend=%q, want both edits present",
				round, got.Observer.Process.Enabled, got.Observer.Process.Backend)
		}
	}
}
