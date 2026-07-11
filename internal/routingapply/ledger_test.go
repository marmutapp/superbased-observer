package routingapply

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLedger_RoundTrip pins the audit trail: write + revert events
// append as JSONL and read back oldest-first; a missing file is an
// empty ledger; a corrupted line is skipped and counted, never fatal.
func TestLedger_RoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "data", "routing-apply-ledger.jsonl")
	l := NewLedger(path)
	base := time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC)
	tick := 0
	l.now = func() time.Time { tick++; return base.Add(time.Duration(tick) * time.Second) }

	// Missing file = empty, no error.
	events, skipped, err := l.Read()
	if err != nil || len(events) != 0 || skipped != 0 {
		t.Fatalf("empty read = (%v, %d, %v)", events, skipped, err)
	}

	c := Change{
		Path: "/agents/code-reviewer.md", Agent: "code-reviewer",
		FromModel: "claude-opus-4-8", ToModel: "claude-haiku-4-5", Rationale: "x",
	}
	if err := l.RecordWrite(c, "20260612T100000Z", "dashboard"); err != nil {
		t.Fatal(err)
	}
	if err := l.RecordRevert(Restored{Path: c.Path, Backup: c.Path + BackupPrefix + "20260612T100000Z"}, "cli"); err != nil {
		t.Fatal(err)
	}

	events, skipped, err = l.Read()
	if err != nil || skipped != 0 || len(events) != 2 {
		t.Fatalf("read = (%d events, %d skipped, %v)", len(events), skipped, err)
	}
	w, r := events[0], events[1]
	if w.Kind != "write" || w.Source != "dashboard" || w.Tool != "claude-code" ||
		w.Path != c.Path || w.FromModel != "claude-opus-4-8" || w.ToModel != "claude-haiku-4-5" ||
		w.Stamp != "20260612T100000Z" || w.Backup != c.Path+BackupPrefix+"20260612T100000Z" {
		t.Errorf("write event = %+v", w)
	}
	if r.Kind != "revert" || r.Source != "cli" || r.Backup == "" {
		t.Errorf("revert event = %+v", r)
	}
	if !r.Time.After(w.Time) {
		t.Errorf("events not in append order: %v then %v", w.Time, r.Time)
	}

	// A corrupted line (crashed process mid-append) is skipped, the
	// rest of the history stays readable.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"kind":"wri`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	events, skipped, err = l.Read()
	if err != nil || len(events) != 2 || skipped != 1 {
		t.Fatalf("corrupt-tolerant read = (%d events, %d skipped, %v)", len(events), skipped, err)
	}
}

// TestDefaultLedgerPath pins the node-local placement next to the DB.
func TestDefaultLedgerPath(t *testing.T) {
	t.Parallel()
	got := DefaultLedgerPath(filepath.Join("home", ".observer", "observer.db"))
	want := filepath.Join("home", ".observer", "routing-apply-ledger.jsonl")
	if got != want {
		t.Errorf("DefaultLedgerPath = %q, want %q", got, want)
	}
}
