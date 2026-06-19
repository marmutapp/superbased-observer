package routingapply

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Apply audit trail (R2.5, operator checkpoint Q6): an append-only
// JSONL ledger of every Channel A write and revert — the answer to
// "what did observer change in my tool config?". One event per line;
// both frontends (CLI and dashboard) record through this type, so the
// trail is complete regardless of which surface performed the write.
//
// Scope is claude-code by construction, not by decision: Channel A
// never writes any other tool's files (§R10.2 — snippet tools get
// paste blocks, cursor/copilot get advice), so there is nothing else
// to audit.
//
// The ledger is a NODE-LOCAL file next to the observer DB. It never
// enters the database and therefore can never reach an org push —
// the same posture as every routing surface that carries paths.

// LedgerEvent is one recorded write or revert.
type LedgerEvent struct {
	Time   time.Time `json:"time"`
	Kind   string    `json:"kind"`   // "write" | "revert"
	Source string    `json:"source"` // "cli" | "dashboard"
	Tool   string    `json:"tool"`   // always "claude-code" today (§R10.2)
	Path   string    `json:"path"`
	Agent  string    `json:"agent,omitempty"`
	// FromModel/ToModel describe a write's edit; empty on reverts.
	FromModel string `json:"from_model,omitempty"`
	ToModel   string `json:"to_model,omitempty"`
	// Stamp is the write batch's backup stamp; Backup is the backup
	// file a write created or a revert restored from.
	Stamp  string `json:"stamp,omitempty"`
	Backup string `json:"backup,omitempty"`
}

// Ledger appends and reads the audit file. The zero value is unusable;
// construct with NewLedger.
type Ledger struct {
	path string
	now  func() time.Time
}

// NewLedger returns a ledger at path. The file (and its directory) are
// created on first append.
func NewLedger(path string) *Ledger {
	return &Ledger{path: path, now: time.Now}
}

// DefaultLedgerPath places the ledger next to the observer DB — the
// node-local data directory both frontends already resolve.
func DefaultLedgerPath(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), "routing-apply-ledger.jsonl")
}

// Path returns the ledger's file path (for display surfaces).
func (l *Ledger) Path() string { return l.path }

// RecordWrite appends a write event for an applied change.
func (l *Ledger) RecordWrite(c Change, stamp, source string) error {
	return l.append(LedgerEvent{
		Kind: "write", Source: source, Tool: "claude-code",
		Path: c.Path, Agent: c.Agent,
		FromModel: c.FromModel, ToModel: c.ToModel,
		Stamp: stamp, Backup: c.Path + BackupPrefix + stamp,
	})
}

// RecordRevert appends a revert event for a restored file.
func (l *Ledger) RecordRevert(r Restored, source string) error {
	return l.append(LedgerEvent{
		Kind: "revert", Source: source, Tool: "claude-code",
		Path: r.Path, Backup: r.Backup,
	})
}

func (l *Ledger) append(ev LedgerEvent) error {
	ev.Time = l.now().UTC()
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return fmt.Errorf("routingapply: ensure ledger dir: %w", err)
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("routingapply: open ledger: %w", err)
	}
	defer f.Close()
	line, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("routingapply: marshal ledger event: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("routingapply: append ledger: %w", err)
	}
	return nil
}

// Read returns the ledger's events oldest-first. A missing file is an
// empty ledger, not an error. Unparseable lines are skipped and
// counted — a half-written line from a crashed process must not make
// the whole history unreadable.
func (l *Ledger) Read() (events []LedgerEvent, skipped int, err error) {
	f, err := os.Open(l.path)
	if os.IsNotExist(err) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("routingapply: open ledger: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev LedgerEvent
		if jsonErr := json.Unmarshal(line, &ev); jsonErr != nil {
			skipped++
			continue
		}
		events = append(events, ev)
	}
	if scanErr := sc.Err(); scanErr != nil {
		return events, skipped, fmt.Errorf("routingapply: read ledger: %w", scanErr)
	}
	return events, skipped, nil
}
