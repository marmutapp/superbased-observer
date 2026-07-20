package retention

import (
	"context"
	"database/sql"
	"testing"
)

// autoVacuumMode reads the DB's current auto_vacuum mode.
func autoVacuumMode(ctx context.Context, t *testing.T, d *sql.DB) int {
	t.Helper()
	var mode int
	if err := d.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		t.Fatalf("read auto_vacuum: %v", err)
	}
	return mode
}

// TestIncrementalVacuum_RegimeGating pins the two vacuum regimes (Ticket B
// of the 2026-07-12 hook-stall + DB-prune plan): on an unconverted DB
// (auto_vacuum=NONE, the historical default) Run never issues an
// incremental_vacuum; after ConvertToIncrementalVacuum it does, and a
// negative page bound opts out. Structural assertions only — the
// IncrementalVacuumRun flag, never wall-clock or file sizes.
func TestIncrementalVacuum_RegimeGating(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name    string
		convert bool
		pages   int
		wantRun bool
	}{
		{name: "unconverted DB never incremental-vacuums", convert: false, pages: 0, wantRun: false},
		{name: "converted DB vacuums with default bound", convert: true, pages: 0, wantRun: true},
		{name: "converted DB vacuums with explicit bound", convert: true, pages: 128, wantRun: true},
		{name: "negative bound disables even when converted", convert: true, pages: -1, wantRun: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dbPath, _ := seed(t)
			d := openExisting(t, dbPath)

			if tc.convert {
				mode, err := ConvertToIncrementalVacuum(ctx, d)
				if err != nil {
					t.Fatalf("ConvertToIncrementalVacuum: %v", err)
				}
				if mode != autoVacuumIncremental {
					t.Fatalf("auto_vacuum mode after convert = %d, want %d", mode, autoVacuumIncremental)
				}
			} else if got := autoVacuumMode(ctx, t, d); got == autoVacuumIncremental {
				t.Fatalf("precondition: fresh DB unexpectedly in incremental mode")
			}

			res, err := New(d).Run(ctx, Options{
				DBPath:                 dbPath,
				IncrementalVacuumPages: tc.pages,
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.IncrementalVacuumRun != tc.wantRun {
				t.Errorf("IncrementalVacuumRun = %v, want %v", res.IncrementalVacuumRun, tc.wantRun)
			}
		})
	}
}

// TestConvertToIncrementalVacuum_Idempotent pins that re-running the
// conversion on an already-converted DB stays in incremental mode (it is
// just a compacting VACUUM the second time).
func TestConvertToIncrementalVacuum_Idempotent(t *testing.T) {
	ctx := context.Background()
	dbPath, _ := seed(t)
	d := openExisting(t, dbPath)

	for i := 0; i < 2; i++ {
		mode, err := ConvertToIncrementalVacuum(ctx, d)
		if err != nil {
			t.Fatalf("ConvertToIncrementalVacuum #%d: %v", i+1, err)
		}
		if mode != autoVacuumIncremental {
			t.Fatalf("pass %d: auto_vacuum mode = %d, want %d", i+1, mode, autoVacuumIncremental)
		}
	}
}
