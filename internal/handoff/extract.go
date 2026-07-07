package handoff

import (
	"fmt"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// CarryMode names how much content crosses in a handoff (plan §2).
type CarryMode string

const (
	// CarryMetadata carries action-derived facts only (files, commands,
	// errors) — the universal floor for adapters without a readable
	// transcript.
	CarryMetadata CarryMode = "metadata"
	// CarryDistilled adds the mission quote from the transcript but no
	// verbatim tail.
	CarryDistilled CarryMode = "distilled"
	// CarryDistilledTail is the default: distilled sections plus a verbatim
	// tail of the last K messages before the fork.
	CarryDistilledTail CarryMode = "distilled_tail"
	// CarryFull carries the whole flattened transcript through the fork,
	// with tool results EXCERPTED (the ResultCap snippet) — the target can
	// pull any message in full on demand via the get_session_message MCP
	// tool (references, not bodies).
	CarryFull CarryMode = "full"
	// CarryFullCache carries the whole flattened transcript with the actual
	// read content UN-EXCERPTED (full tool_result bodies inlined) — the
	// "full incl cache" mode. It replicates a provider prompt cache: the
	// full warm context travels with the handover from the first prompt and
	// is resent each turn until the target's own cache warms. Sourced from
	// the tool's own transcript files (and stash), NOT cachetrack (which
	// stores only prefix hashes + token counts). Requires a
	// FullTranscriptReader; degrades to CarryFull otherwise.
	CarryFullCache CarryMode = "full_cache"
)

// ValidCarry reports whether m is a known carry mode.
func ValidCarry(m CarryMode) bool {
	switch m {
	case CarryMetadata, CarryDistilled, CarryDistilledTail, CarryFull, CarryFullCache:
		return true
	}
	return false
}

// FileFact is one touched file, aggregated from actions at the store seam.
type FileFact struct {
	Path   string
	Edits  int
	Reads  int
	LastAt time.Time
}

// CommandFact is one command line, aggregated from actions at the store
// seam.
type CommandFact struct {
	Command   string
	Runs      int
	LastOK    bool
	LastAt    time.Time
	LastError string
}

// ErrorFact is an unresolved failure: an action that failed with no later
// success on the same target.
type ErrorFact struct {
	Target  string
	Message string
	At      time.Time
}

// Extract is the pure-logic input bundle: session metadata, action-derived
// facts, and the optional normalized transcript, all pre-loaded by the
// boundary (internal/handoffsvc) before any handoff logic runs. A nil
// Transcript degrades the handoff to CarryMetadata.
type Extract struct {
	SessionID   string
	Tool        string
	Model       string
	ProjectRoot string
	GitBranch   string
	StartedAt   time.Time
	EndedAt     time.Time

	// Transcript is the normalized message stream from the source tool's
	// own files (nil ⇒ metadata-only handoff).
	Transcript []models.TranscriptMessage

	Files    []FileFact
	Commands []CommandFact
	Errors   []ErrorFact

	// ContextTokens is the session's cumulative cached-prefix weight at its
	// end (from token_usage via the store seam); 0 = unknown. Scales the
	// full-carry estimate row.
	ContextTokens int64
}

// Validate reports whether the extract can drive a handoff at all.
func (e Extract) Validate() error {
	if e.SessionID == "" {
		return fmt.Errorf("handoff: extract has no session id")
	}
	if e.Tool == "" {
		return fmt.Errorf("handoff: extract has no source tool")
	}
	return nil
}

// TokenEstimate is the shared chars→tokens heuristic (≈4 chars/token,
// rounded up) used for doc budgets and estimate rows.
func TokenEstimate(s string) int64 {
	if len(s) == 0 {
		return 0
	}
	return int64((len(s) + 3) / 4)
}
