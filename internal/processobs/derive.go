package processobs

import "strconv"

// DeriveCommandRuns synthesizes process_runs rows from the AI tool's OWN exec
// record (the run_command actions) for the commands the OS-capture backend
// never saw. It closes a structural gap in the poll-based capture path: a fast
// read-only command (sed/rg/git/date) is born and dies between poll ticks, so
// no OS process is ever observed and the message↔command association has no row
// to attach to — even though the tool logged the command, with its message id,
// the whole time.
//
// Each derived run carries the action's DETERMINISTIC message link
// (ActionID/TurnIndex → actions.message_id) by construction — not by the
// heuristic time-window correlation the OS path uses (CorrelateActions). The
// tool never exposes the spawned command's OS pid (verified: Codex's
// exec_command / exec_command_end events carry command/cwd/duration/exit but no
// pid), so an OS-observed process can only be linked to its action by argv+time
// guesswork; a derived run sidesteps that entirely because we ARE the action.
//
// osLinked is the set of action ids that already have a captured OS process
// anchored to them (a non-derived process_runs row carrying that action_id).
// Those actions are skipped: the real process — with its pid, resource metrics,
// and subtree — is authoritative, and re-deriving would double-count the
// command. The store deletes any stale derived row for an action that later
// gains an OS anchor, so the two feed paths stay mutually exclusive: exactly
// one anchor per command, OS-or-derived, never both.
//
// Pure: the store loads the actions + the osLinked set and persists the result
// through the single PersistRuns owner. Returns a nil slice when every action
// is OS-anchored (nothing to synthesize).
func DeriveCommandRuns(sessionID, tool string, projectID int64, actions []ActionRef, osLinked map[int64]bool) []ProcessRun {
	if sessionID == "" || len(actions) == 0 {
		return nil
	}
	var out []ProcessRun
	for i := range actions {
		a := actions[i]
		if osLinked[a.ActionID] {
			continue
		}
		out = append(out, derivedRun(sessionID, tool, projectID, a))
	}
	return out
}

// DerivedProcessKeyPrefix tags the process_key of every synthesized run so the
// store, stats, and UI can tell a derived row from an OS-captured one without a
// schema column (the operator's no-migration constraint). It is paired with
// attribution_source == AttrActionCorrelation and pid == 0.
const DerivedProcessKeyPrefix = "derived:action:"

// derivedRun builds one synthetic ProcessRun from a run_command action. The
// row is deliberately metric-free (pid 0, no cpu/rss/io): it asserts "this
// command ran, issued by this message", not "here is the OS process". The
// command finished by definition (it is a completed run_command with output),
// so Exited is always true; ExitCode is coarse (0 on success, 1 on failure)
// because actions carries only a success boolean, not a numeric exit code.
func derivedRun(sessionID, tool string, projectID int64, a ActionRef) ProcessRun {
	id := a.ActionID
	exitCode := 0
	if !a.Success {
		exitCode = 1
	}
	exitedAt := a.Timestamp
	if a.Duration > 0 {
		exitedAt = a.Timestamp.Add(a.Duration)
	}
	return ProcessRun{
		ProcessKey: DerivedProcessKeyPrefix + strconv.FormatInt(id, 10),
		PID:        0,
		Attribution: Attribution{
			SessionID:  sessionID,
			Tool:       tool,
			ProjectID:  projectID,
			Source:     AttrActionCorrelation,
			Confidence: ConfHigh,
			ActionID:   &id,
			TurnIndex:  a.TurnIndex,
		},
		ExeBasename: leadingBinary(a.Command),
		ArgvPreview: a.Command,
		StartedAt:   a.Timestamp,
		LastSeenAt:  exitedAt,
		ExitedAt:    exitedAt,
		Exited:      true,
		ExitCode:    exitCode,
		DurationMs:  a.Duration.Milliseconds(),
	}
}
