package main

// backfill_toolinput.go — `observer backfill --codex-tool-input`.
//
// THE PREMISE THIS PASS CORRECTS. The deferred item that motivated it
// read "codex-family historical backfill (~7k rows' raw_tool_input
// re-derivation)". That count conflates EMPTY with MISSING. Measured on
// the live corpus (2026-08-01), rows with an empty raw_tool_input split:
//
//	assistant_message  4621   codex/adapter.go:buildAgentMessageEvent
//	edit_file          2112   codex/adapter.go:buildPatchApplyStandaloneEvent
//	task_complete      1155   codex/adapter.go:buildTaskCompleteEvent
//	session_start       302   codex/hook.go:BuildHookEvent
//	turn_aborted         46   codex/adapter.go:buildTurnAbortedEvent
//	web_search             5   codex/adapter.go:buildWebSearchEvent
//
// Four of those six emit sites never assign RawToolInput at all — an
// assistant message, a completed turn, an aborted turn and a session
// start HAVE no tool input, so empty is the correct and only honest
// value. Writing anything there would be fabricating a field the
// producer never recorded. That is why this pass is table-driven off a
// GROUNDING table (toolInputSpecs) that names the emit site behind every
// verdict, and why the report carries "legitimately has no input" as its
// own visible bucket rather than folding it into a total.
//
// WHAT IS ACTUALLY RECOVERABLE. Only the two emit sites that CAN carry
// an input:
//
//   - edit_file / patch_apply_end. Codex's patch_apply_end event_msg
//     carries `changes` — a map of path → {type, unified_diff} — i.e.
//     the patch the executor applied, in the same record that produced
//     the row. buildPatchApplyStandaloneEvent reads that map for Target
//     and ContentBytes and then discards it, so the row lands with an
//     empty RawToolInput. Re-reading the record recovers it verbatim.
//
//   - web_search / web_search_end. buildWebSearchEvent DOES populate
//     RawToolInput, so an empty one means the source record's query was
//     empty at parse time. Re-reading either recovers a query or proves
//     there was none. (On the live corpus all five are
//     action.type=open_page / other with query:"" — genuinely
//     query-less. They resolve to UNRESOLVED, not to an invented value.)
//
// WHY THE ADAPTER IS REUSED RATHER THAN RE-IMPLEMENTED. The primary
// derivation is codex.Adapter.ParseSessionFile itself: it owns session
// context, project-root resolution, action mapping, scrubbing and
// SourceEventID construction, and it is the only thing that decides what
// a row's input IS. Re-implementing any of that would be the second,
// subtly-different parser the one-owner rule exists to prevent.
//
// ParseSessionFile cannot supply the patch_apply_end case, because the
// adapter provably drops that one field on the standalone path
// (buildPatchApplyStandaloneEvent assigns no RawToolInput). Recovering
// it needs exactly one thing the adapter does not return: the raw
// `changes` object keyed by the call_id the adapter already writes as
// the row's SourceEventID. scanPatchApplyChanges does that and ONLY
// that — it decides no action type, resolves no root, builds no rows,
// and never overrides a value ParseSessionFile produced. It is a field
// reader, not a parser.
//
// THE JOIN KEY. actions.source_event_id, matched against the same
// call_id the source record carries — for patch rows that is
// buildPatchApplyStandaloneEvent's firstNonEmpty(pa.CallID, …), for
// web-search rows buildWebSearchEvent's firstNonEmpty(ws.CallID, …).
// call_id is Codex's own per-tool-call identifier: unique within a
// rollout and independent of line position and timestamp, so a file
// that has since been appended to, resumed into or forked still joins
// correctly. Scoped by source_file the pair is globally unique. Ordinal
// position and timestamp proximity are never used — they mis-attribute
// silently the moment a rollout is resumed.
//
// Rows whose source_event_id is one of the adapter's SYNTHESIZED
// fallbacks (`patch:<file>:L<n>`, `web:<file>:L<n>:<hash>`) are left
// unresolved on purpose: those keys encode a line number, and
// reproducing the adapter's line accounting here would be exactly the
// second implementation this file avoids. Zero live rows carry that
// shape (all 2112 patch rows join on a real `exec-<uuid>` call_id), so
// failing closed costs nothing and cannot mis-attribute.
//
// SAFETY. Never overwrites: candidates are selected empty AND the UPDATE
// re-asserts emptiness in its WHERE, so a row populated between the read
// and the write is still not clobbered. Never invents: a missing file,
// an absent source record or an empty derived value all end as
// UNRESOLVED. Dry-run by default; --apply mutates inside ONE
// transaction. Not part of --all. Idempotent — an updated row is no
// longer empty, so a re-run does not select it.

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/adapter/codex"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/scrub"
)

// inputExpectation says whether a row of a given (tool, action_type) can
// have a raw_tool_input at all.
type inputExpectation string

const (
	// inputRecoverable — the emit site assigns RawToolInput, or the
	// source record demonstrably carries the value the emit site
	// dropped. Rows of this shape are candidates.
	inputRecoverable inputExpectation = "recoverable"
	// inputNeverExists — the emit site assigns no RawToolInput because
	// the event HAS no tool input. Empty is the correct value and this
	// pass must never write one.
	inputNeverExists inputExpectation = "no_input_for_action_type"
)

// toolInputSpec is one row of the grounding table: the verdict for a
// (tool, action_type) pair, plus the emit site that grounds it so a
// reader can check the claim instead of trusting the table.
type toolInputSpec struct {
	// Tool is the actions.tool value, matched with =.
	Tool string
	// ActionType is the actions.action_type value, matched with =.
	ActionType string
	// Expect is the verdict for the pair.
	Expect inputExpectation
	// Site names the emit site the verdict is grounded in.
	Site string
}

// toolInputSpecs is the ONE owner of "which codex-family action types
// can carry a raw_tool_input". Exact (tool, action_type) pairs only —
// never a LIKE or a prefix match, which would reach rows this pass has
// no grounding for.
//
// A pair absent from this table is NEVER touched (see groundingFor):
// unclassified rows are reported, not guessed at. That is the fail-
// closed direction — a row this table does not name is a row this pass
// leaves exactly as it found it.
//
// open-interpreter rows are produced by the SAME builders:
// codex.NewOpenInterpreter retags the codex parser at the §2.1 boundary
// seam, so every verdict below transfers unchanged.
var toolInputSpecs = []toolInputSpec{
	// Recoverable.
	{models.ToolCodex, models.ActionEditFile, inputRecoverable, "codex/adapter.go:buildPatchApplyStandaloneEvent (drops patch_apply_end.changes)"},
	{models.ToolCodex, models.ActionWebSearch, inputRecoverable, "codex/adapter.go:buildWebSearchEvent (assigns RawToolInput)"},
	{models.ToolOpenInterpreter, models.ActionEditFile, inputRecoverable, "codex/adapter.go:buildPatchApplyStandaloneEvent (NewOpenInterpreter retag)"},
	{models.ToolOpenInterpreter, models.ActionWebSearch, inputRecoverable, "codex/adapter.go:buildWebSearchEvent (NewOpenInterpreter retag)"},

	// Legitimately input-free. Each builder below assigns no
	// RawToolInput because the event it describes has no tool input.
	// Corroborated on the live corpus: zero codex rows of these four
	// action types carry a non-empty raw_tool_input, in either
	// direction, across the whole table.
	{models.ToolCodex, models.ActionAssistantMessage, inputNeverExists, "codex/adapter.go:buildAgentMessageEvent (assistant text, not a tool call)"},
	{models.ToolCodex, models.ActionTaskComplete, inputNeverExists, "codex/adapter.go:buildTaskCompleteEvent (turn terminus)"},
	{models.ToolCodex, models.ActionTurnAborted, inputNeverExists, "codex/adapter.go:buildTurnAbortedEvent (interruption marker)"},
	{models.ToolCodex, models.ActionSessionStart, inputNeverExists, "codex/hook.go:BuildHookEvent HookEventSessionStart (source_file=codex:hook, no rollout record)"},
	{models.ToolOpenInterpreter, models.ActionAssistantMessage, inputNeverExists, "codex/adapter.go:buildAgentMessageEvent (NewOpenInterpreter retag)"},
	{models.ToolOpenInterpreter, models.ActionTaskComplete, inputNeverExists, "codex/adapter.go:buildTaskCompleteEvent (NewOpenInterpreter retag)"},
	{models.ToolOpenInterpreter, models.ActionTurnAborted, inputNeverExists, "codex/adapter.go:buildTurnAbortedEvent (NewOpenInterpreter retag)"},
	{models.ToolOpenInterpreter, models.ActionSessionStart, inputNeverExists, "codex/hook.go:BuildHookEvent (NewOpenInterpreter retag)"},
}

// groundingFor returns the spec for a (tool, action_type) pair. The
// second result is false for a pair the table does not name — the fail-
// closed case, which this pass reports and never touches.
func groundingFor(tool, actionType string) (toolInputSpec, bool) {
	for _, s := range toolInputSpecs {
		if s.Tool == tool && s.ActionType == actionType {
			return s, true
		}
	}
	return toolInputSpec{}, false
}

// toolInputTools lists the distinct tools the grounding table names, in
// stable order. It scopes every query this pass runs.
func toolInputTools() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range toolInputSpecs {
		if !seen[s.Tool] {
			seen[s.Tool] = true
			out = append(out, s.Tool)
		}
	}
	sort.Strings(out)
	return out
}

// ToolInputBucket is one (tool, action_type) line of the report. Every
// examined row lands in exactly one of the outcome counters, so
// Examined equals their sum.
type ToolInputBucket struct {
	Tool        string `json:"tool"`
	ActionType  string `json:"action_type"`
	Expectation string `json:"expectation"`
	// Examined counts rows with an EMPTY raw_tool_input.
	Examined int `json:"examined"`
	// Updated counts rows whose input was re-derived (or, in a dry run,
	// would be).
	Updated int `json:"updated"`
	// UnresolvedFileMissing counts rows whose source file is gone.
	UnresolvedFileMissing int `json:"unresolved_source_file_missing"`
	// UnresolvedNoSourceRecord counts rows whose source file was read
	// but held no record for the row's source_event_id, or held one
	// carrying no input.
	UnresolvedNoSourceRecord int `json:"unresolved_source_record_absent"`
	// SkippedNoInputForActionType counts rows whose action type
	// legitimately has no input. Never written to.
	SkippedNoInputForActionType int `json:"skipped_no_input_for_action_type"`
	// SkippedUnclassified counts rows of a (tool, action_type) pair the
	// grounding table does not name. Never written to.
	SkippedUnclassified int `json:"skipped_unclassified"`
	// AlreadyPopulated counts rows of this pair that already carry a
	// non-empty raw_tool_input. They are not examined and never
	// rewritten; the count is the honest denominator.
	AlreadyPopulated int `json:"skipped_already_populated"`
}

// ToolInputBackfill summarises the --codex-tool-input pass.
type ToolInputBackfill struct {
	// Applied is false for a dry run.
	Applied bool `json:"applied"`
	// FilesScanned counts distinct source files re-read.
	FilesScanned int `json:"files_scanned"`
	// FilesMissing counts distinct source files no longer on disk.
	FilesMissing int `json:"files_missing"`
	// FilesUnreadable counts distinct source files present but whose
	// re-read failed. Their rows count as unresolved.
	FilesUnreadable int `json:"files_unreadable"`

	Examined                    int `json:"examined"`
	Updated                     int `json:"updated"`
	UnresolvedFileMissing       int `json:"unresolved_source_file_missing"`
	UnresolvedNoSourceRecord    int `json:"unresolved_source_record_absent"`
	SkippedNoInputForActionType int `json:"skipped_no_input_for_action_type"`
	SkippedUnclassified         int `json:"skipped_unclassified"`
	AlreadyPopulated            int `json:"skipped_already_populated"`

	PerBucket []ToolInputBucket `json:"per_bucket,omitempty"`
}

// toolInputUpdateSQL is the ONLY statement this pass writes with.
//
// The `AND (raw_tool_input IS NULL OR raw_tool_input = ”)` clause is not
// redundant with the candidate SELECT's identical predicate, and removing
// it is not a tidy-up. Candidate selection happens OUTSIDE the
// transaction, so the SELECT can only prove a row was empty when it was
// read; this clause is what proves it is still empty when it is written.
// Measured directly (see TestToolInputUpdateSQLRefusesPopulatedRow):
// with the SELECT predicate mutated away but this clause intact the
// stored value survives, and with both mutated away it is clobbered —
// i.e. THIS is the guard that protects the value, and the SELECT
// predicate merely stops the pass from doing pointless work.
//
// It is a named constant so a test can bind to the exact statement that
// ships, rather than to a copy that could drift away from it.
const toolInputUpdateSQL = `UPDATE actions SET raw_tool_input = ?
	  WHERE id = ? AND (raw_tool_input IS NULL OR raw_tool_input = '')`

// toolInputRow is one candidate row as the planner sees it.
type toolInputRow struct {
	ID           int64
	Tool         string
	ActionType   string
	SourceFile   string
	SourceEventI string
}

// backfillToolInput re-derives raw_tool_input for the codex-family rows
// that are genuinely missing one, by re-reading the rollout files the
// rows came from. Dry-run unless apply is true; a run that applies does
// every UPDATE inside one transaction, so a mid-run failure can never
// half-apply. out may be nil to suppress the report.
func backfillToolInput(ctx context.Context, db *sql.DB, apply bool, out io.Writer) (ToolInputBackfill, error) {
	res := ToolInputBackfill{Applied: apply}

	buckets := map[string]*ToolInputBucket{}
	bucket := func(tool, actionType string) *ToolInputBucket {
		key := tool + "\x00" + actionType
		b, ok := buckets[key]
		if !ok {
			exp := "unclassified"
			if s, found := groundingFor(tool, actionType); found {
				exp = string(s.Expect)
			}
			b = &ToolInputBucket{Tool: tool, ActionType: actionType, Expectation: exp}
			buckets[key] = b
		}
		return b
	}

	populated, err := loadToolInputPopulatedCounts(ctx, db)
	if err != nil {
		return res, err
	}
	for key, n := range populated {
		tool, actionType, _ := strings.Cut(key, "\x00")
		bucket(tool, actionType).AlreadyPopulated = n
		res.AlreadyPopulated += n
	}

	rows, err := loadToolInputCandidates(ctx, db)
	if err != nil {
		return res, err
	}

	// Partition. Only recoverable rows reach a file at all — the
	// no-input and unclassified verdicts are terminal, which is what
	// makes "never invent" structural rather than a later check.
	byFile := map[string][]toolInputRow{}
	for _, r := range rows {
		b := bucket(r.Tool, r.ActionType)
		b.Examined++
		res.Examined++
		spec, known := groundingFor(r.Tool, r.ActionType)
		switch {
		case !known:
			b.SkippedUnclassified++
			res.SkippedUnclassified++
		case spec.Expect == inputNeverExists:
			b.SkippedNoInputForActionType++
			res.SkippedNoInputForActionType++
		case r.SourceFile == "" || r.SourceEventI == "":
			b.UnresolvedNoSourceRecord++
			res.UnresolvedNoSourceRecord++
		default:
			byFile[r.SourceFile] = append(byFile[r.SourceFile], r)
		}
	}

	files := make([]string, 0, len(byFile))
	for f := range byFile {
		files = append(files, f)
	}
	sort.Strings(files)

	updates, err := resolveToolInputUpdates(ctx, files, byFile, &res, bucket)
	if err != nil {
		return res, err
	}

	if apply {
		if err := applyToolInputUpdates(ctx, db, updates); err != nil {
			return res, err
		}
	}

	for _, b := range buckets {
		res.PerBucket = append(res.PerBucket, *b)
	}
	sort.Slice(res.PerBucket, func(i, j int) bool {
		a, c := res.PerBucket[i], res.PerBucket[j]
		if a.Tool != c.Tool {
			return a.Tool < c.Tool
		}
		return a.ActionType < c.ActionType
	})

	if out != nil {
		writeToolInputReport(out, res)
	}
	return res, nil
}

// plannedToolInputUpdate is one grounded write: a row id and the input
// re-derived for it. Nothing reaches this type without a source record.
type plannedToolInputUpdate struct {
	ID    int64
	Input string
}

// resolveToolInputUpdates re-reads each source file once and turns the
// rows grouped under it into planned writes, tallying every row that
// does NOT become one into its own unresolved bucket.
//
// bucket is the caller's per-(tool, action_type) accumulator; res is
// updated in place so the totals and the buckets can never disagree.
func resolveToolInputUpdates(
	ctx context.Context,
	files []string,
	byFile map[string][]toolInputRow,
	res *ToolInputBackfill,
	bucket func(tool, actionType string) *ToolInputBucket,
) ([]plannedToolInputUpdate, error) {
	var updates []plannedToolInputUpdate
	// unresolveAll tallies every row under a file that could not be read
	// at all. Nothing is written for them — an unreadable source is an
	// unresolved source, not a licence to invent a value.
	unresolveAll := func(rows []toolInputRow, missing bool) {
		for _, r := range rows {
			b := bucket(r.Tool, r.ActionType)
			if missing {
				b.UnresolvedFileMissing++
				res.UnresolvedFileMissing++
				continue
			}
			b.UnresolvedNoSourceRecord++
			res.UnresolvedNoSourceRecord++
		}
	}

	sc := scrub.New()
	for _, path := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fileRows := byFile[path]
		if st, statErr := os.Stat(path); statErr != nil || st.IsDir() {
			res.FilesMissing++
			unresolveAll(fileRows, true)
			continue
		}
		derived, derr := deriveCodexToolInputs(ctx, sc, fileRows[0].Tool, path)
		if derr != nil {
			if errors.Is(derr, context.Canceled) || errors.Is(derr, context.DeadlineExceeded) {
				return nil, derr
			}
			res.FilesUnreadable++
			unresolveAll(fileRows, false)
			continue
		}
		res.FilesScanned++
		for _, r := range fileRows {
			b := bucket(r.Tool, r.ActionType)
			input := derived[r.SourceEventI]
			if input == "" {
				b.UnresolvedNoSourceRecord++
				res.UnresolvedNoSourceRecord++
				continue
			}
			updates = append(updates, plannedToolInputUpdate{ID: r.ID, Input: input})
			b.Updated++
			res.Updated++
		}
	}
	return updates, nil
}

// applyToolInputUpdates writes every planned update inside ONE
// transaction, so a mid-run failure can never leave the corpus half
// re-derived. Each write goes through toolInputUpdateSQL, whose WHERE
// re-asserts that the row is still empty.
func applyToolInputUpdates(ctx context.Context, db *sql.DB, updates []plannedToolInputUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("backfill codex-tool-input: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, toolInputUpdateSQL)
	if err != nil {
		return fmt.Errorf("backfill codex-tool-input: prepare update: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	for _, u := range updates {
		if _, err := stmt.ExecContext(ctx, u.Input, u.ID); err != nil {
			return fmt.Errorf("backfill codex-tool-input: update action %d: %w", u.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("backfill codex-tool-input: commit: %w", err)
	}
	return nil
}

// loadToolInputPopulatedCounts counts, per (tool, action_type), the rows
// that ALREADY carry a non-empty raw_tool_input. Those rows are never
// selected or rewritten; the count is reported so "would update: N"
// reads against an honest denominator.
func loadToolInputPopulatedCounts(ctx context.Context, db *sql.DB) (map[string]int, error) {
	tools := toolInputTools()
	//nolint:gosec // G202: the only concatenated fragment is a `?` placeholder
	// run generated from len(); every tool value binds via args.
	query := `SELECT tool, action_type, COUNT(*)
	            FROM actions
	           WHERE tool IN (` + placeholders(len(tools)) + `)
	             AND raw_tool_input IS NOT NULL AND raw_tool_input <> ''
	           GROUP BY tool, action_type`
	args := make([]any, 0, len(tools))
	for _, t := range tools {
		args = append(args, t)
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("backfill codex-tool-input: count populated: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var tool, actionType string
		var n int
		if err := rows.Scan(&tool, &actionType, &n); err != nil {
			return nil, fmt.Errorf("backfill codex-tool-input: scan populated: %w", err)
		}
		out[tool+"\x00"+actionType] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("backfill codex-tool-input: count populated: %w", err)
	}
	return out, nil
}

// loadToolInputCandidates selects every row of a table-named tool whose
// raw_tool_input is empty, in stable id order. Emptiness is decided HERE
// and re-asserted in the UPDATE — a populated row is never a candidate.
func loadToolInputCandidates(ctx context.Context, db *sql.DB) ([]toolInputRow, error) {
	tools := toolInputTools()
	//nolint:gosec // G202: the only concatenated fragment is a `?` placeholder
	// run generated from len(); every tool value binds via args.
	query := `SELECT id, tool, action_type,
	                 COALESCE(source_file, ''), COALESCE(source_event_id, '')
	            FROM actions
	           WHERE tool IN (` + placeholders(len(tools)) + `)
	             AND (raw_tool_input IS NULL OR raw_tool_input = '')
	           ORDER BY id`
	args := make([]any, 0, len(tools))
	for _, t := range tools {
		args = append(args, t)
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("backfill codex-tool-input: select candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []toolInputRow
	for rows.Next() {
		var r toolInputRow
		if err := rows.Scan(&r.ID, &r.Tool, &r.ActionType, &r.SourceFile, &r.SourceEventI); err != nil {
			return nil, fmt.Errorf("backfill codex-tool-input: scan candidate: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("backfill codex-tool-input: select candidates: %w", err)
	}
	return out, nil
}

// deriveCodexToolInputs re-reads one rollout file and returns the tool
// inputs it can ground, keyed by the SAME source_event_id the adapter
// writes onto the row.
//
// The adapter is the primary source: ParseSessionFile owns every
// decision about what a row's input is, and whatever it produces wins.
// scanPatchApplyChanges supplements it with the single field the
// standalone patch path provably discards, and can only ADD keys the
// adapter left absent — it can never override one.
func deriveCodexToolInputs(ctx context.Context, sc *scrub.Scrubber, tool, path string) (map[string]string, error) {
	ad := codex.NewWithOptions(sc, "")
	if tool == models.ToolOpenInterpreter {
		ad = codex.NewOpenInterpreterWithOptions(sc, "")
	}
	parsed, err := ad.ParseSessionFile(ctx, path, 0)
	if err != nil {
		return nil, fmt.Errorf("backfill codex-tool-input: parse %s: %w", path, err)
	}
	out := make(map[string]string, len(parsed.ToolEvents))
	for _, ev := range parsed.ToolEvents {
		if ev.SourceEventID != "" && ev.RawToolInput != "" {
			out[ev.SourceEventID] = ev.RawToolInput
		}
	}
	changes, err := scanPatchApplyChanges(ctx, sc, path)
	if err != nil {
		return nil, err
	}
	for id, v := range changes {
		if _, seen := out[id]; !seen {
			out[id] = v
		}
	}
	return out, nil
}

// rolloutEnvelope is the outer record of a codex rollout JSONL line.
type rolloutEnvelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// patchApplyChangesPayload is the ONLY thing this file reads out of a
// patch_apply_end record: the call_id that joins it to the row, and the
// `changes` object verbatim. Nothing here classifies, maps or shapes —
// that all stays in the adapter.
type patchApplyChangesPayload struct {
	Type    string          `json:"type"`
	CallID  string          `json:"call_id"`
	Changes json.RawMessage `json:"changes"`
}

// scanPatchApplyChanges returns the raw `changes` object of every
// patch_apply_end record in a rollout, keyed by its call_id and scrubbed
// as JSON.
//
// This exists because codex/adapter.go's buildPatchApplyStandaloneEvent
// reads patch_apply_end.changes for Target and ContentBytes and then
// drops it, so ParseSessionFile cannot return it. It is a field reader,
// not a rollout parser: it makes no decision the adapter also makes, and
// its output is only ever consulted for keys the adapter left empty.
//
// Records with an absent, null or empty `changes` yield nothing — the
// row stays unresolved rather than gaining a fabricated value.
//
// Scrubbing goes through Scrubber.RawJSON, never Scrubber.String: the
// value is compact JSON, and String's line regexes devour compact JSON
// across structural delimiters (see CLAUDE.md's scrubbing Don't).
func scanPatchApplyChanges(ctx context.Context, sc *scrub.Scrubber, path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("backfill codex-tool-input: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	out := map[string]string{}
	// A bufio.Reader, not a Scanner: rollout lines routinely exceed
	// Scanner's 64KB token cap and a truncated line would silently drop
	// records.
	br := bufio.NewReaderSize(f, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line, readErr := br.ReadString('\n')
		if s := strings.TrimSpace(line); s != "" {
			var env rolloutEnvelope
			if json.Unmarshal([]byte(s), &env) == nil && env.Type == "event_msg" && len(env.Payload) > 0 {
				var p patchApplyChangesPayload
				if json.Unmarshal(env.Payload, &p) == nil &&
					p.Type == "patch_apply_end" && p.CallID != "" {
					if v := scrubbedChanges(sc, p.Changes); v != "" {
						out[p.CallID] = v
					}
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, fmt.Errorf("backfill codex-tool-input: read %s: %w", path, readErr)
		}
	}
	return out, nil
}

// scrubbedChanges renders a patch_apply_end `changes` object for storage,
// or "" when the record carries nothing. The rendering itself is owned by
// the adapter (codex.RenderPatchChangesInput) because the live parse path
// writes the same column from the same bytes — a recovered row and a
// live-captured row must be byte-identical.
func scrubbedChanges(sc *scrub.Scrubber, raw json.RawMessage) string {
	return codex.RenderPatchChangesInput(sc, raw)
}

// writeToolInputReport prints the per-bucket table. The
// legitimately-input-free bucket is a NAMED column, not a residual: the
// whole point of the pass is that those rows are correct as they stand.
func writeToolInputReport(out io.Writer, res ToolInputBackfill) {
	mode := "dry run"
	if res.Applied {
		mode = "applied"
	}
	fmt.Fprintf(out, "codex-tool-input (%s): %d row(s) with an empty raw_tool_input across the codex family\n",
		mode, res.Examined)
	if res.Examined == 0 && res.AlreadyPopulated == 0 {
		fmt.Fprintf(out, "  no codex-family rows in this database.\n")
		return
	}
	verb := "would update"
	if res.Applied {
		verb = "updated"
	}
	fmt.Fprintf(out, "  files re-read: %d (missing on disk: %d, unreadable: %d)\n",
		res.FilesScanned, res.FilesMissing, res.FilesUnreadable)
	fmt.Fprintf(out, "  %s: %d\n", verb, res.Updated)
	fmt.Fprintf(out, "  unresolved — source file no longer on disk: %d\n", res.UnresolvedFileMissing)
	fmt.Fprintf(out, "  unresolved — source record absent or carries no input: %d\n", res.UnresolvedNoSourceRecord)
	fmt.Fprintf(out, "  skipped — this action_type legitimately has no input: %d\n", res.SkippedNoInputForActionType)
	fmt.Fprintf(out, "  skipped — (tool, action_type) not in the grounding table: %d\n", res.SkippedUnclassified)
	fmt.Fprintf(out, "  skipped — raw_tool_input already populated (never rewritten): %d\n", res.AlreadyPopulated)

	if len(res.PerBucket) > 0 {
		fmt.Fprintf(out, "  per tool × action_type:\n")
		fmt.Fprintf(out, "    %-17s %-19s %-24s %8s %8s %8s %8s %8s %8s %10s\n",
			"TOOL", "ACTION_TYPE", "VERDICT", "EMPTY", verbHead(res.Applied), "NOFILE", "NOREC", "NOINPUT", "UNCLASS", "POPULATED")
		for _, b := range res.PerBucket {
			fmt.Fprintf(out, "    %-17s %-19s %-24s %8d %8d %8d %8d %8d %8d %10d\n",
				b.Tool, b.ActionType, b.Expectation,
				b.Examined, b.Updated, b.UnresolvedFileMissing, b.UnresolvedNoSourceRecord,
				b.SkippedNoInputForActionType, b.SkippedUnclassified, b.AlreadyPopulated)
		}
	}
	if !res.Applied && res.Updated > 0 {
		fmt.Fprintf(out, "  nothing written (dry run) — re-run with --apply:\n    observer backfill --codex-tool-input --apply\n")
	}
}

// verbHead is the per-bucket column header for the update count.
func verbHead(applied bool) string {
	if applied {
		return "UPDATED"
	}
	return "WOULD"
}
