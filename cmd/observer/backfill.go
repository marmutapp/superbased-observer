package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/adapter/claudecode"
	"github.com/marmutapp/superbased-observer/internal/adapter/cursor"
	"github.com/marmutapp/superbased-observer/internal/adapter/opencode"
	"github.com/marmutapp/superbased-observer/internal/compression/indexing"
	"github.com/marmutapp/superbased-observer/internal/git"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
	"github.com/marmutapp/superbased-observer/internal/store"
	"github.com/marmutapp/superbased-observer/internal/watcher"
	"github.com/spf13/cobra"
)

// newBackfillCmd implements `observer backfill` — re-process historical
// JSONL data to populate columns added by later migrations on rows that
// were ingested before the migration shipped.
//
// Supported dimensions:
//
//	--is-sidechain    Migration 010 added is_sidechain to actions; the
//	                  JSONL adapter now copies isSidechain per line for
//	                  new ingests, but pre-migration rows default to 0.
//	                  Walks Claude Code session logs and UPDATEs the
//	                  matching action rows by (source_file,
//	                  source_event_id) where the stored value still
//	                  reads 0.
//
//	--cache-tier      Migration 008 added cache_creation_1h_tokens to
//	                  token_usage and api_turns. Pre-migration rows have
//	                  NULL in the new column, which the cost engine
//	                  treats as 0 → "all 5m tier" → silently under-bills
//	                  1h cache writes (Anthropic's 2× tier vs 1.25× for
//	                  5m). This pass extracts
//	                  usage.cache_creation.ephemeral_1h_input_tokens
//	                  per Anthropic message id and UPDATEs both tables
//	                  by (session_id, source_event_id / request_id)
//	                  where cache_creation_1h_tokens IS NULL.
//
//	--message-id      Migration 012 added message_id to actions and
//	                  token_usage. The new column is the natural unit
//	                  for grouping tool calls and token usage by the
//	                  upstream Anthropic message that produced them
//	                  (msg_xxx). Pre-migration rows have NULL; this
//	                  pass extracts line.message.id per JSONL line and
//	                  UPDATEs both tables by (session_id, source_event_id)
//	                  trying both message.id and per-line uuid as keys.
//
//	--all             Convenience: equivalent to --is-sidechain
//	                  --cache-tier --message-id.
//
// All flags can run in the same invocation. Idempotent — safe to run
// multiple times; UPDATEs are no-ops when the stored value already
// matches the JSONL.
func newBackfillCmd() *cobra.Command {
	var (
		configPath            string
		isSidechain           bool
		cacheTier             bool
		messageID             bool
		opencodeMessageID     bool
		opencodeParts         bool
		opencodeTokens        bool
		openclawActionTypes   bool
		openclawModel         bool
		openclawReasoning     bool
		codexReasoning        bool
		codexProjectRoot      bool
		cursorModel           bool
		copilotMessageID      bool
		piMessageID           bool
		claudecodeUserPrompts bool
		claudecodeAPIErrors   bool
		cursorUserPrompts     bool
		cursorSubagents       bool
		all                   bool
		jsonOut               bool
		limit                 int
	)
	cmd := &cobra.Command{
		Use:   "backfill",
		Short: "Re-populate columns added by later migrations / parity passes on pre-existing rows",
		Long: `Re-walks platform session files (Claude Code JSONL, Codex rollouts,
OpenCode SQLite, OpenClaw mixed formats, Cursor hook logs) and updates
the actions / token_usage / api_turns tables for rows that were
ingested before a later migration or adapter parity fix.

After upgrading the binary, run ` + "`observer backfill --all`" + ` once to bring
historical data up to current. The command is idempotent — re-running
is safe; matched rows that already carry the new value are no-ops.

Supported dimensions (each can be passed alone or together):

  Existing migration backfills:
    --is-sidechain    actions.is_sidechain (migration 010)
    --cache-tier      token_usage / api_turns.cache_creation_1h_tokens
                      (migration 008)
    --message-id      actions / token_usage.message_id (migration 012);
                      bundles claudecode + codex + cursor + opencode
                      (the latter is also exposed as --opencode-message-id).

  Adapter parity backfills (added 2026-04-30):
    --opencode-message-id    Strip prefix from source_event_id and write
                             to message_id for opencode user-prompt /
                             completion / token rows.
    --opencode-parts         Re-read opencode.db parts to populate
                             tool_output, duration_ms, and message_id
                             on tool / subtask action rows.
    --openclaw-action-types  Retag historical openclaw 'sessions_spawn'
                             rows from mcp_call to spawn_subagent.
    --openclaw-model         Lift model from sessions.json aliases onto
                             openclaw sqlite-path action rows.
    --openclaw-reasoning     Re-walk openclaw jsonl to populate
                             preceding_reasoning on tool calls from
                             preceding text/thinking content.
    --codex-reasoning        Re-walk codex rollouts to populate
                             preceding_reasoning from agent_message.
    --cursor-model           Copy model from matching token_usage row
                             onto cursor action rows.
    --cursor-user-prompts    Walk cursor agent-transcripts JSONL and
                             insert user_prompt action rows (with the
                             <user_query> wrapper stripped) for sessions
                             that pre-date the beforeSubmitPrompt hook
                             installation.
    --cursor-subagents       Walk cursor agent-transcripts/<session>/
                             subagents/<sub>.jsonl files and ingest their
                             contents as sidechain rows under the parent
                             session (IsSidechain=true).

  Convenience:
    --all             Run every supported backfill in one invocation.

Run ` + "`observer backfill --all`" + ` after each binary upgrade to keep
historical rows in sync with the latest schema and adapter behaviour.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if all {
				isSidechain = true
				cacheTier = true
				messageID = true
				opencodeMessageID = true
				opencodeParts = true
				opencodeTokens = true
				openclawActionTypes = true
				openclawModel = true
				openclawReasoning = true
				codexReasoning = true
				codexProjectRoot = true
				cursorModel = true
				copilotMessageID = true
				piMessageID = true
				claudecodeUserPrompts = true
				claudecodeAPIErrors = true
				cursorUserPrompts = true
				cursorSubagents = true
			}
			if !isSidechain && !cacheTier && !messageID &&
				!opencodeMessageID && !opencodeParts && !opencodeTokens &&
				!openclawActionTypes && !openclawModel && !openclawReasoning &&
				!codexReasoning && !codexProjectRoot && !cursorModel &&
				!copilotMessageID && !piMessageID &&
				!claudecodeUserPrompts && !claudecodeAPIErrors &&
				!cursorUserPrompts && !cursorSubagents {
				return fmt.Errorf("nothing to backfill — pass one of the dimension flags or --all")
			}
			// --message-id is the umbrella for adapter message-id work; it
			// drives the opencode + copilot + pi passes too so users only
			// need the one flag for routine use. The granular flags stay
			// for targeted re-runs.
			if messageID {
				opencodeMessageID = true
				copilotMessageID = true
				piMessageID = true
			}

			summary := struct {
				Rescan                *watcher.ScanResult            `json:"rescan,omitempty"`
				IsSidechain           *BackfillResult                `json:"is_sidechain,omitempty"`
				CacheTier             *CacheTierBackfill             `json:"cache_tier,omitempty"`
				MessageID             *MessageIDBackfill             `json:"message_id,omitempty"`
				OpenCodeMessageID     *MessageIDBackfill             `json:"opencode_message_id,omitempty"`
				OpenCodeParts         *OpenCodePartsBackfill         `json:"opencode_parts,omitempty"`
				OpenCodeTokens        *OpenCodeTokensBackfill        `json:"opencode_tokens,omitempty"`
				OpenClawActionTypes   *OpenClawActionsBackfill       `json:"openclaw_action_types,omitempty"`
				OpenClawModel         *OpenClawModelBackfill         `json:"openclaw_model,omitempty"`
				OpenClawReasoning     *OpenClawReasoningBackfill     `json:"openclaw_reasoning,omitempty"`
				CodexReasoning        *CodexReasoningBackfill        `json:"codex_reasoning,omitempty"`
				CodexProjectRoot      *CodexProjectRootBackfill      `json:"codex_project_root,omitempty"`
				CursorModel           *CursorModelBackfill           `json:"cursor_model,omitempty"`
				CopilotMessageID      *MessageIDBackfill             `json:"copilot_message_id,omitempty"`
				PiMessageID           *MessageIDBackfill             `json:"pi_message_id,omitempty"`
				ClaudeCodeUserPrompts *ClaudeCodeUserPromptsBackfill `json:"claudecode_user_prompts,omitempty"`
				ClaudeCodeAPIErrors   *ClaudeCodeAPIErrorsBackfill   `json:"claudecode_api_errors,omitempty"`
				CursorUserPrompts     *CursorUserPromptsBackfill     `json:"cursor_user_prompts,omitempty"`
				CursorSubagents       *CursorSubagentsBackfill       `json:"cursor_subagents,omitempty"`
			}{}

			// --all kicks a full rescan from offset 0 BEFORE the surgical
			// backfills. This is the recovery path for when the live
			// watcher fell behind silently (fsnotify event drops, daemon
			// restart with stale parse_cursors, etc.) — without this,
			// missing assistant tool-call rows would never re-ingest.
			// Surgical backfills only update specific columns on rows
			// that already exist; they don't insert anything new.
			if all {
				w, wCleanup, err := buildWatcher(cmd.Context(), configPath)
				if err != nil {
					return fmt.Errorf("--all rescan: %w", err)
				}
				rescanRes, rescanErr := w.Rescan(cmd.Context())
				wCleanup()
				if rescanErr != nil {
					return fmt.Errorf("--all rescan: %w", rescanErr)
				}
				summary.Rescan = &rescanRes
				if !jsonOut {
					fmt.Fprintf(cmd.OutOrStdout(),
						"rescan complete: files_processed=%d errors=%d (re-walked every JSONL from offset 0; (source_file, source_event_id) UNIQUE keeps it idempotent)\n",
						rescanRes.FilesProcessed, rescanRes.Errors,
					)
				}
			}

			_, database, cleanup, err := loadConfigAndDB(cmd.Context(), configPath)
			if err != nil {
				return err
			}
			defer cleanup()

			if isSidechain {
				res, err := backfillIsSidechain(cmd.Context(), database, claudeProjectsDir(), limit)
				if err != nil {
					return err
				}
				summary.IsSidechain = &res
				if !jsonOut {
					fmt.Fprintf(cmd.OutOrStdout(),
						"is_sidechain backfill complete: scanned %d files, %d sidechain lines, updated %d rows (skipped %d unmatched)\n",
						res.FilesScanned, res.SidechainLines, res.RowsUpdated, res.UnmatchedLines,
					)
				}
			}
			if cacheTier {
				res, err := backfillCacheTier(cmd.Context(), database, claudeProjectsDir(), limit)
				if err != nil {
					return err
				}
				summary.CacheTier = &res
				if !jsonOut {
					fmt.Fprintf(cmd.OutOrStdout(),
						"cache-tier backfill complete: scanned %d files, %d msg-id rows examined, updated %d token_usage rows + %d api_turns rows (1h tokens recovered: %d)\n",
						res.FilesScanned, res.MsgIDsExamined, res.TokenUsageUpdated, res.APITurnsUpdated, res.TokensRecovered,
					)
				}
			}
			if messageID {
				res, err := backfillMessageID(cmd.Context(), database, claudeProjectsDir(), limit)
				if err != nil {
					return err
				}
				codexRes, err := backfillCodexMessageID(cmd.Context(), database, codexSessionsDir(), limit)
				if err != nil {
					return err
				}
				cursorRes, err := backfillCursorMessageID(cmd.Context(), database)
				if err != nil {
					return err
				}
				cursorUsageRes, err := backfillCursorHookUsage(cmd.Context(), database, cursorLogsDir(), limit)
				if err != nil {
					return err
				}
				res.FilesScanned += codexRes.FilesScanned
				res.LinesExamined += codexRes.LinesExamined
				res.ActionsUpdated += codexRes.ActionsUpdated
				res.TokenUsageUpdated += codexRes.TokenUsageUpdated
				res.ActionsUpdated += cursorRes.ActionsUpdated
				res.FilesScanned += cursorUsageRes.FilesScanned
				res.LinesExamined += cursorUsageRes.LinesExamined
				res.TokenUsageUpdated += cursorUsageRes.TokenUsageUpdated
				cursorTranscriptRes, err := backfillCursorTranscriptActions(cmd.Context(), database, cursorProjectsDir(), limit)
				if err != nil {
					return err
				}
				res.FilesScanned += cursorTranscriptRes.FilesScanned
				res.LinesExamined += cursorTranscriptRes.LinesExamined
				res.ActionsUpdated += cursorTranscriptRes.ActionsUpdated
				summary.MessageID = &res
				if !jsonOut {
					fmt.Fprintf(cmd.OutOrStdout(),
						"message-id backfill complete: scanned %d files, %d lines examined, updated %d action rows + %d token_usage rows\n",
						res.FilesScanned, res.LinesExamined, res.ActionsUpdated, res.TokenUsageUpdated,
					)
				}
			}

			if opencodeMessageID {
				res, err := backfillOpenCodeMessageID(cmd.Context(), database)
				if err != nil {
					return err
				}
				summary.OpenCodeMessageID = &res
				if !jsonOut && (res.ActionsUpdated > 0 || res.TokenUsageUpdated > 0) {
					fmt.Fprintf(cmd.OutOrStdout(),
						"opencode message-id backfill complete: updated %d action rows + %d token_usage rows\n",
						res.ActionsUpdated, res.TokenUsageUpdated,
					)
				}
			}
			if opencodeParts {
				res, err := backfillOpenCodeParts(cmd.Context(), database)
				if err != nil {
					return err
				}
				summary.OpenCodeParts = &res
				if !jsonOut {
					fmt.Fprintf(cmd.OutOrStdout(),
						"opencode-parts backfill complete: scanned %d DB(s), examined %d parts; updated tool_output=%d, duration=%d, message_id=%d\n",
						res.DBsScanned, res.PartsExamined,
						res.ToolOutputUpdated, res.DurationUpdated, res.MessageIDUpdated,
					)
				}
			}
			if opencodeTokens {
				res, err := backfillOpenCodeTokens(cmd.Context(), database)
				if err != nil {
					return err
				}
				summary.OpenCodeTokens = &res
				if !jsonOut {
					fmt.Fprintf(cmd.OutOrStdout(),
						"opencode-tokens backfill complete: scanned %d DB(s), extracted %d token events; inserted %d new token_usage rows\n",
						res.DBsScanned, res.TokenRowsExtracted, res.TokenRowsInserted,
					)
				}
			}
			if openclawActionTypes {
				res, err := backfillOpenClawActionTypes(cmd.Context(), database)
				if err != nil {
					return err
				}
				summary.OpenClawActionTypes = &res
				if !jsonOut {
					fmt.Fprintf(cmd.OutOrStdout(),
						"openclaw action-types backfill complete: %d sessions_spawn rows retagged to spawn_subagent\n",
						res.ActionsUpdated,
					)
				}
			}
			if openclawModel {
				res, err := backfillOpenClawModel(cmd.Context(), database)
				if err != nil {
					return err
				}
				summary.OpenClawModel = &res
				if !jsonOut {
					fmt.Fprintf(cmd.OutOrStdout(),
						"openclaw-model backfill complete: scanned %d alias file(s), %d aliases loaded; lifted model onto %d session row(s)\n",
						res.AliasFilesScanned, res.AliasesLoaded, res.SessionsUpdated,
					)
				}
			}
			if openclawReasoning {
				res, err := backfillOpenClawReasoning(cmd.Context(), database, openclawAgentsDir(), limit)
				if err != nil {
					return err
				}
				summary.OpenClawReasoning = &res
				if !jsonOut {
					fmt.Fprintf(cmd.OutOrStdout(),
						"openclaw-reasoning backfill complete: scanned %d files, examined %d lines; updated %d action rows\n",
						res.FilesScanned, res.LinesExamined, res.ActionsUpdated,
					)
				}
			}
			if codexReasoning {
				res, err := backfillCodexReasoning(cmd.Context(), database, codexSessionsDir(), limit)
				if err != nil {
					return err
				}
				summary.CodexReasoning = &res
				if !jsonOut {
					fmt.Fprintf(cmd.OutOrStdout(),
						"codex-reasoning backfill complete: scanned %d files, captured %d turn preambles; updated %d action rows\n",
						res.FilesScanned, res.TurnsCaptured, res.ActionsUpdated,
					)
				}
			}
			if codexProjectRoot {
				res, err := backfillCodexProjectRoot(cmd.Context(), database, codexProjectRootBackfillDirs(), limit)
				if err != nil {
					return err
				}
				summary.CodexProjectRoot = &res
				if !jsonOut {
					fmt.Fprintf(cmd.OutOrStdout(),
						"codex-project-root backfill complete: scanned %d files, %d sessions reattributed; %d action rows updated\n",
						res.FilesScanned, res.SessionsReattributed, res.ActionsUpdated,
					)
				}
			}
			if cursorModel {
				res, err := backfillCursorModel(cmd.Context(), database)
				if err != nil {
					return err
				}
				summary.CursorModel = &res
				if !jsonOut {
					fmt.Fprintf(cmd.OutOrStdout(),
						"cursor-model backfill complete: %d cursor session(s) had model lifted from matching token_usage\n",
						res.SessionsUpdated,
					)
				}
			}
			if copilotMessageID {
				res, err := backfillCopilotMessageID(cmd.Context(), database)
				if err != nil {
					return err
				}
				summary.CopilotMessageID = &res
				if !jsonOut {
					fmt.Fprintf(cmd.OutOrStdout(),
						"copilot message-id backfill complete: scanned %d files, examined %d lines; updated %d action rows + %d token_usage rows\n",
						res.FilesScanned, res.LinesExamined, res.ActionsUpdated, res.TokenUsageUpdated,
					)
				}
			}
			if piMessageID {
				res, err := backfillPiMessageID(cmd.Context(), database)
				if err != nil {
					return err
				}
				summary.PiMessageID = &res
				if !jsonOut {
					fmt.Fprintf(cmd.OutOrStdout(),
						"pi message-id backfill complete: scanned %d files, examined %d lines; updated %d action rows + %d token_usage rows\n",
						res.FilesScanned, res.LinesExamined, res.ActionsUpdated, res.TokenUsageUpdated,
					)
				}
			}
			if claudecodeUserPrompts {
				res, err := backfillClaudeCodeUserPrompts(cmd.Context(), database, claudeProjectsDir(), limit)
				if err != nil {
					return err
				}
				summary.ClaudeCodeUserPrompts = &res
				if !jsonOut {
					fmt.Fprintf(cmd.OutOrStdout(),
						"claudecode user-prompts backfill complete: scanned %d files, found %d user_prompt events; inserted %d new action rows\n",
						res.FilesScanned, res.UserEventsFound, res.ActionsInserted,
					)
				}
			}
			if claudecodeAPIErrors {
				res, err := backfillClaudeCodeAPIErrors(cmd.Context(), database, claudeProjectsDir(), limit)
				if err != nil {
					return err
				}
				summary.ClaudeCodeAPIErrors = &res
				if !jsonOut {
					fmt.Fprintf(cmd.OutOrStdout(),
						"claudecode api-errors backfill complete: scanned %d files, found %d api_error events; inserted %d new action rows\n",
						res.FilesScanned, res.APIErrorsFound, res.ActionsInserted,
					)
				}
			}
			if cursorUserPrompts {
				res, err := backfillCursorUserPrompts(cmd.Context(), database, cursorProjectsDir(), limit)
				if err != nil {
					return err
				}
				summary.CursorUserPrompts = &res
				if !jsonOut {
					fmt.Fprintf(cmd.OutOrStdout(),
						"cursor user-prompts backfill complete: scanned %d files, found %d user_prompt events; inserted %d new action rows\n",
						res.FilesScanned, res.UserEventsFound, res.ActionsInserted,
					)
				}
			}
			if cursorSubagents {
				res, err := backfillCursorSubagents(cmd.Context(), database, cursorProjectsDir(), limit)
				if err != nil {
					return err
				}
				summary.CursorSubagents = &res
				if !jsonOut {
					fmt.Fprintf(cmd.OutOrStdout(),
						"cursor subagents backfill complete: scanned %d files, built %d sidechain events; inserted %d new action rows\n",
						res.FilesScanned, res.EventsBuilt, res.ActionsInserted,
					)
				}
			}

			if jsonOut {
				body, _ := json.MarshalIndent(summary, "", "  ")
				fmt.Fprintln(cmd.OutOrStdout(), string(body))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml")
	cmd.Flags().BoolVar(&isSidechain, "is-sidechain", false, "Backfill actions.is_sidechain from JSONL")
	cmd.Flags().BoolVar(&cacheTier, "cache-tier", false, "Backfill cache_creation_1h_tokens from JSONL")
	cmd.Flags().BoolVar(&messageID, "message-id", false, "Backfill message_id columns from JSONL (umbrella covering claudecode + codex + cursor + opencode)")
	cmd.Flags().BoolVar(&opencodeMessageID, "opencode-message-id", false, "Backfill message_id on opencode rows from source_event_id prefix")
	cmd.Flags().BoolVar(&opencodeParts, "opencode-parts", false, "Re-read opencode.db parts to populate tool_output / duration_ms / message_id")
	cmd.Flags().BoolVar(&opencodeTokens, "opencode-tokens", false, "Re-run the opencode adapter to insert any missing token_usage rows from data.tokens")
	cmd.Flags().BoolVar(&openclawActionTypes, "openclaw-action-types", false, "Retag historical openclaw sessions_spawn rows from mcp_call to spawn_subagent")
	cmd.Flags().BoolVar(&openclawModel, "openclaw-model", false, "Lift model from sessions.json aliases onto openclaw sqlite-path action rows")
	cmd.Flags().BoolVar(&openclawReasoning, "openclaw-reasoning", false, "Re-walk openclaw jsonl to populate preceding_reasoning on tool calls")
	cmd.Flags().BoolVar(&codexReasoning, "codex-reasoning", false, "Re-walk codex rollouts to populate preceding_reasoning from agent_message")
	cmd.Flags().BoolVar(&codexProjectRoot, "codex-project-root", false, "Re-attribute codex action / token / session rows to the correct project when their cwd was a Windows-style path that previously misresolved to observer's own repo (v1.4.28)")
	cmd.Flags().BoolVar(&cursorModel, "cursor-model", false, "Lift model from matching token_usage row onto cursor session rows whose model is empty")
	cmd.Flags().BoolVar(&copilotMessageID, "copilot-message-id", false, "Backfill message_id on copilot rows by walking debug-log JSONL")
	cmd.Flags().BoolVar(&piMessageID, "pi-message-id", false, "Backfill message_id on pi rows by walking session JSONL")
	cmd.Flags().BoolVar(&claudecodeUserPrompts, "claudecode-user-prompts", false, "Insert missing user_prompt action rows for Claude Code sessions ingested before the adapter started emitting them")
	cmd.Flags().BoolVar(&claudecodeAPIErrors, "claudecode-api-errors", false, "Insert api_error action rows for Claude Code system/api_error JSONL records (content-policy blocks, rate limits, etc.) ingested before v1.4.20 added capture")
	cmd.Flags().BoolVar(&cursorUserPrompts, "cursor-user-prompts", false, "Insert user_prompt action rows for Cursor sessions by walking agent-transcripts JSONL — fills the gap for sessions before the beforeSubmitPrompt hook was installed, with the <user_query> wrapper stripped")
	cmd.Flags().BoolVar(&cursorSubagents, "cursor-subagents", false, "Walk Cursor agent-transcripts/<session>/subagents/<sub>.jsonl files and ingest as sidechain rows under the parent session (IsSidechain=true)")
	cmd.Flags().BoolVar(&all, "all", false, "Run every supported backfill in one invocation (recommended after upgrading)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON")
	cmd.Flags().IntVar(&limit, "limit", 0, "Stop after N source files per JSONL-walking pass (0 = all)")
	return cmd
}

// BackfillResult is the per-run summary returned to the caller.
type BackfillResult struct {
	FilesScanned   int `json:"files_scanned"`
	SidechainLines int `json:"sidechain_lines"`
	RowsUpdated    int `json:"rows_updated"`
	UnmatchedLines int `json:"unmatched_lines"`
}

// CacheTierBackfill summarises the --cache-tier pass.
type CacheTierBackfill struct {
	FilesScanned      int   `json:"files_scanned"`
	MsgIDsExamined    int   `json:"msg_ids_examined"`
	TokenUsageUpdated int   `json:"token_usage_updated"`
	APITurnsUpdated   int   `json:"api_turns_updated"`
	TokensRecovered   int64 `json:"tokens_recovered"`
}

// MessageIDBackfill summarises the --message-id pass.
type MessageIDBackfill struct {
	FilesScanned      int `json:"files_scanned"`
	LinesExamined     int `json:"lines_examined"`
	ActionsUpdated    int `json:"actions_updated"`
	TokenUsageUpdated int `json:"token_usage_updated"`
}

// claudeProjectsDir returns the location Claude Code writes its session
// JSONL files. Honors $CLAUDE_HOME for tests + non-default installs;
// falls back to ~/.claude/projects/.
func claudeProjectsDir() string {
	if v := os.Getenv("CLAUDE_HOME"); v != "" {
		return filepath.Join(v, "projects")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "projects")
}

// codexSessionsDir returns the location Codex Desktop / CLI writes rollout
// JSONL files. Honors $CODEX_HOME for tests + non-default installs; falls
// back to ~/.codex/sessions/.
func codexSessionsDir() string {
	if v := os.Getenv("CODEX_HOME"); v != "" {
		return filepath.Join(v, "sessions")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex", "sessions")
}

func cursorLogsDir() string {
	if v := os.Getenv("APPDATA"); v != "" {
		return filepath.Join(v, "Cursor", "logs")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "AppData", "Roaming", "Cursor", "logs")
}

func cursorProjectsDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cursor", "projects")
}

// backfillIsSidechain walks every *.jsonl under projectsDir, extracts
// `(uuid, isSidechain)` pairs from each line where isSidechain is true,
// and UPDATEs the matching actions row by source_event_id (= the line's
// content[].id for tool_use blocks, which is what the adapter writes).
//
// UUID matching: the adapter uses the tool_use block's id as
// source_event_id, NOT the line uuid. So we scan content[] looking for
// tool_use blocks. Lines without tool_use blocks contribute zero to
// the update count even if isSidechain=true (text-only assistant
// messages don't produce action rows; only tool calls do).
func backfillIsSidechain(ctx context.Context, db *sql.DB, projectsDir string, fileLimit int) (BackfillResult, error) {
	res := BackfillResult{}

	updateStmt, err := db.PrepareContext(ctx,
		`UPDATE actions SET is_sidechain = 1
		 WHERE source_file = ? AND source_event_id = ? AND is_sidechain = 0`)
	if err != nil {
		return res, fmt.Errorf("backfill: prepare: %w", err)
	}
	defer updateStmt.Close()

	walkErr := filepath.WalkDir(projectsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable subtrees rather than fail the whole pass
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".jsonl" {
			return nil
		}
		if fileLimit > 0 && res.FilesScanned >= fileLimit {
			return filepath.SkipAll
		}
		res.FilesScanned++
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		// Increase the bufio scanner's max line size — Claude Code's
		// JSONL lines can carry large tool_results that exceed the
		// default 64 KB.
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
		for scanner.Scan() {
			line := scanner.Bytes()
			var rl struct {
				IsSidechain bool `json:"isSidechain"`
				Message     struct {
					Content json.RawMessage `json:"content"`
				} `json:"message"`
			}
			if err := json.Unmarshal(line, &rl); err != nil {
				continue
			}
			if !rl.IsSidechain {
				continue
			}
			res.SidechainLines++
			// Decode content[] looking for tool_use blocks.
			var blocks []struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			}
			if err := json.Unmarshal(rl.Message.Content, &blocks); err != nil {
				continue
			}
			for _, b := range blocks {
				if b.Type != "tool_use" || b.ID == "" {
					continue
				}
				rs, err := updateStmt.ExecContext(ctx, path, b.ID)
				if err != nil {
					continue
				}
				n, _ := rs.RowsAffected()
				if n > 0 {
					res.RowsUpdated += int(n)
				} else {
					res.UnmatchedLines++
				}
			}
		}
		return nil
	})
	if walkErr != nil && walkErr != filepath.SkipAll {
		return res, fmt.Errorf("backfill: walk %s: %w", projectsDir, walkErr)
	}
	return res, nil
}

// backfillCacheTier walks every *.jsonl under projectsDir, extracts
// (session_id, message.id, ephemeral_1h_input_tokens) tuples from each
// assistant-message line that carries a usage block, and UPDATEs:
//
//   - token_usage SET cache_creation_1h_tokens = ? WHERE session_id = ?
//     AND source_event_id = ? AND cache_creation_1h_tokens IS NULL
//   - api_turns SET cache_creation_1h_tokens = ? WHERE session_id = ?
//     AND request_id = ? AND cache_creation_1h_tokens IS NULL
//
// The IS NULL guard preserves any explicit value already written by
// post-migration ingests; the backfill only fills in the NULLs left
// behind when migration 008 ran on existing data.
//
// Note that we only update if the JSONL row HAS the
// `cache_creation.ephemeral_1h_input_tokens` field — older Claude Code
// JSONL didn't emit the per-tier breakdown at all, in which case the
// historical row was 100% 5m and the NULL → 0 default is correct. We
// only correct rows where Anthropic actually returned a 1h subset.
func backfillCacheTier(ctx context.Context, db *sql.DB, projectsDir string, fileLimit int) (CacheTierBackfill, error) {
	res := CacheTierBackfill{}

	updateTokenUsage, err := db.PrepareContext(ctx,
		`UPDATE token_usage SET cache_creation_1h_tokens = ?
		 WHERE session_id = ? AND source_event_id = ?
		   AND cache_creation_1h_tokens IS NULL`)
	if err != nil {
		return res, fmt.Errorf("backfill cache-tier: prepare token_usage: %w", err)
	}
	defer updateTokenUsage.Close()

	updateAPITurns, err := db.PrepareContext(ctx,
		`UPDATE api_turns SET cache_creation_1h_tokens = ?
		 WHERE session_id = ? AND request_id = ?
		   AND cache_creation_1h_tokens IS NULL`)
	if err != nil {
		return res, fmt.Errorf("backfill cache-tier: prepare api_turns: %w", err)
	}
	defer updateAPITurns.Close()

	walkErr := filepath.WalkDir(projectsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".jsonl" {
			return nil
		}
		if fileLimit > 0 && res.FilesScanned >= fileLimit {
			return filepath.SkipAll
		}
		res.FilesScanned++

		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)

		// We iterate every line and try BOTH the message.id and the
		// line.uuid as source_event_id keys, because historical
		// claudecode adapter ingests used the line uuid (one per
		// content block) while later versions use the message.id.
		// The IS NULL guard makes each UPDATE a no-op when the row
		// has already been corrected (or was never NULL to begin
		// with), so trying both keys is safe and idempotent.
		//
		// We DON'T dedup by message.id within the file because the
		// per-line uuid changes line-to-line even when the message.id
		// is the same — and the token_usage rows we need to update
		// were keyed by line.uuid in the older adapter version.

		// Track which message.ids we've already credited toward
		// MsgIDsExamined / TokensRecovered so the summary numbers
		// reflect the true count of corrected upstream turns, not
		// the multi-line content-block fan-out.
		seenMsgs := map[string]bool{}

		for scanner.Scan() {
			line := scanner.Bytes()
			var rl struct {
				UUID      string `json:"uuid"`
				SessionID string `json:"sessionId"`
				Message   struct {
					ID    string `json:"id"`
					Usage struct {
						CacheCreation struct {
							Ephemeral1hInputTokens int64 `json:"ephemeral_1h_input_tokens"`
						} `json:"cache_creation"`
					} `json:"usage"`
				} `json:"message"`
			}
			if err := json.Unmarshal(line, &rl); err != nil {
				continue
			}
			cw1h := rl.Message.Usage.CacheCreation.Ephemeral1hInputTokens
			if cw1h == 0 || rl.SessionID == "" {
				continue
			}
			if rl.Message.ID != "" && !seenMsgs[rl.Message.ID] {
				seenMsgs[rl.Message.ID] = true
				res.MsgIDsExamined++
			}

			// Try both message.id and line.uuid as the source_event_id
			// key. Historical (pre-2025-Q3) claudecode ingest used
			// line.uuid; current uses message.id. Both forms exist in
			// the wild and the IS-NULL guard makes the second attempt
			// a no-op when the first already wrote.
			tryUpdateTokenUsage := func(eid string) {
				if eid == "" {
					return
				}
				r, err := updateTokenUsage.ExecContext(ctx, cw1h, rl.SessionID, eid)
				if err != nil {
					return
				}
				n, _ := r.RowsAffected()
				if n > 0 {
					res.TokenUsageUpdated += int(n)
					res.TokensRecovered += cw1h * n
				}
			}
			tryUpdateAPITurns := func(eid string) {
				if eid == "" {
					return
				}
				r, err := updateAPITurns.ExecContext(ctx, cw1h, rl.SessionID, eid)
				if err != nil {
					return
				}
				n, _ := r.RowsAffected()
				if n > 0 {
					res.APITurnsUpdated += int(n)
					res.TokensRecovered += cw1h * n
				}
			}
			tryUpdateTokenUsage(rl.Message.ID)
			tryUpdateTokenUsage(rl.UUID)
			tryUpdateAPITurns(rl.Message.ID)
			tryUpdateAPITurns(rl.UUID)
		}
		return nil
	})
	if walkErr != nil && walkErr != filepath.SkipAll {
		return res, fmt.Errorf("backfill cache-tier: walk %s: %w", projectsDir, walkErr)
	}
	return res, nil
}

// backfillMessageID walks every *.jsonl under projectsDir, extracts
// (sessionId, line.uuid, message.id, content[].id) tuples, and UPDATEs
// the new message_id column on actions and token_usage where it's
// currently NULL.
//
// Two key shapes for the source_event_id match:
//   - actions.source_event_id stores the tool_use block's id (toolu_xxx)
//     for tool calls, or line.uuid for non-tool actions.
//   - token_usage.source_event_id stores message.id (msg_xxx) for newer
//     ingests, line.uuid for older.
//
// We try both message.id and line.uuid as the key for each row, and
// for actions also walk content[] to capture every tool_use block id
// that belongs to this message. The IS-NULL guard makes redundant
// UPDATEs no-ops.
func backfillMessageID(ctx context.Context, db *sql.DB, projectsDir string, fileLimit int) (MessageIDBackfill, error) {
	res := MessageIDBackfill{}

	updateActions, err := db.PrepareContext(ctx,
		`UPDATE actions
		    SET message_id = COALESCE(NULLIF(message_id, ''), ?)
		 WHERE session_id = ? AND source_event_id = ?
		   AND (message_id IS NULL OR message_id = '')`)
	if err != nil {
		return res, fmt.Errorf("backfill message-id: prepare actions: %w", err)
	}
	defer updateActions.Close()

	updateTokenUsage, err := db.PrepareContext(ctx,
		`UPDATE token_usage
		    SET message_id = COALESCE(NULLIF(message_id, ''), ?),
		        model = COALESCE(NULLIF(model, ''), ?)
		 WHERE session_id = ? AND source_event_id = ?
		   AND ((message_id IS NULL OR message_id = '')
		        OR (model IS NULL OR model = ''))`)
	if err != nil {
		return res, fmt.Errorf("backfill message-id: prepare token_usage: %w", err)
	}
	defer updateTokenUsage.Close()

	walkErr := filepath.WalkDir(projectsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".jsonl" {
			return nil
		}
		if fileLimit > 0 && res.FilesScanned >= fileLimit {
			return filepath.SkipAll
		}
		res.FilesScanned++

		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)

		for scanner.Scan() {
			line := scanner.Bytes()
			var rl struct {
				UUID      string `json:"uuid"`
				SessionID string `json:"sessionId"`
				Message   struct {
					ID      string          `json:"id"`
					Content json.RawMessage `json:"content"`
				} `json:"message"`
			}
			if err := json.Unmarshal(line, &rl); err != nil {
				continue
			}
			if rl.SessionID == "" || rl.Message.ID == "" {
				continue
			}
			res.LinesExamined++
			msgID := rl.Message.ID

			// token_usage: source_event_id can be msg.id (newer
			// adapter) or line.uuid (legacy). Try both.
			for _, eid := range [2]string{msgID, rl.UUID} {
				if eid == "" {
					continue
				}
				if r, err := updateTokenUsage.ExecContext(ctx, msgID, "", rl.SessionID, eid); err == nil {
					if n, _ := r.RowsAffected(); n > 0 {
						res.TokenUsageUpdated += int(n)
					}
				}
			}

			// actions: source_event_id is the tool_use block's id
			// (toolu_xxx) for tool calls, or line.uuid otherwise.
			// Walk content[] for tool_use ids and try them all.
			if r, err := updateActions.ExecContext(ctx, msgID, rl.SessionID, rl.UUID); err == nil {
				if n, _ := r.RowsAffected(); n > 0 {
					res.ActionsUpdated += int(n)
				}
			}
			var blocks []struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			}
			if err := json.Unmarshal(rl.Message.Content, &blocks); err == nil {
				for _, b := range blocks {
					if b.Type != "tool_use" || b.ID == "" {
						continue
					}
					if r, err := updateActions.ExecContext(ctx, msgID, rl.SessionID, b.ID); err == nil {
						if n, _ := r.RowsAffected(); n > 0 {
							res.ActionsUpdated += int(n)
						}
					}
				}
			}
		}
		return nil
	})
	if walkErr != nil && walkErr != filepath.SkipAll {
		return res, fmt.Errorf("backfill message-id: walk %s: %w", projectsDir, walkErr)
	}
	return res, nil
}

// backfillCodexMessageID walks Codex rollout JSONL files and populates the
// message_id column using Codex turn ids as the assistant-message key, plus a
// synthetic "user:<turn_id>" key for user prompts. It also backfills the model
// onto actions/token rows that were ingested before the adapter started
// carrying it through consistently.
func backfillCodexMessageID(ctx context.Context, db *sql.DB, sessionsDir string, fileLimit int) (MessageIDBackfill, error) {
	res := MessageIDBackfill{}

	updateActions, err := db.PrepareContext(ctx,
		`UPDATE actions
		    SET message_id = COALESCE(NULLIF(message_id, ''), ?)
		 WHERE session_id = ? AND source_event_id = ?
		   AND (message_id IS NULL OR message_id = '')`)
	if err != nil {
		return res, fmt.Errorf("backfill codex message-id: prepare actions: %w", err)
	}
	defer updateActions.Close()

	updateTokenUsage, err := db.PrepareContext(ctx,
		`UPDATE token_usage
		    SET message_id = COALESCE(NULLIF(message_id, ''), ?),
		        model = COALESCE(NULLIF(model, ''), ?)
		 WHERE session_id = ? AND source_event_id = ?
		   AND ((message_id IS NULL OR message_id = '')
		        OR (model IS NULL OR model = ''))`)
	if err != nil {
		return res, fmt.Errorf("backfill codex message-id: prepare token_usage: %w", err)
	}
	defer updateTokenUsage.Close()

	updateTokenUsageByMessage, err := db.PrepareContext(ctx,
		`UPDATE token_usage
		    SET model = COALESCE(NULLIF(model, ''), ?)
		 WHERE session_id = ? AND message_id = ?
		   AND (model IS NULL OR model = '')`)
	if err != nil {
		return res, fmt.Errorf("backfill codex message-id: prepare token_usage by message: %w", err)
	}
	defer updateTokenUsageByMessage.Close()

	walkErr := filepath.WalkDir(sessionsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if filepath.Ext(path) != ".jsonl" || !strings.HasPrefix(base, "rollout-") {
			return nil
		}
		if fileLimit > 0 && res.FilesScanned >= fileLimit {
			return filepath.SkipAll
		}
		res.FilesScanned++

		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()

		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)

		sessionIDs := []string{strings.TrimPrefix(strings.TrimSuffix(base, ".jsonl"), "rollout-")}
		currentTurnID := ""
		currentModel := ""
		lineNum := 0
		addSessionID := func(id string) {
			if id == "" {
				return
			}
			for _, existing := range sessionIDs {
				if existing == id {
					return
				}
			}
			sessionIDs = append(sessionIDs, id)
		}
		updateActionsForSessions := func(messageID, sourceEventID string) int {
			updated := 0
			for _, sessionID := range sessionIDs {
				if r, err := updateActions.ExecContext(ctx, messageID, sessionID, sourceEventID); err == nil {
					if n, _ := r.RowsAffected(); n > 0 {
						updated += int(n)
					}
				}
			}
			return updated
		}
		updateTokenUsageForSessions := func(messageID, model, sourceEventID string) int {
			updated := 0
			for _, sessionID := range sessionIDs {
				if r, err := updateTokenUsage.ExecContext(ctx, messageID, model, sessionID, sourceEventID); err == nil {
					if n, _ := r.RowsAffected(); n > 0 {
						updated += int(n)
					}
				}
			}
			return updated
		}
		updateTokenUsageByMessageForSessions := func(model, messageID string) int {
			updated := 0
			for _, sessionID := range sessionIDs {
				if r, err := updateTokenUsageByMessage.ExecContext(ctx, model, sessionID, messageID); err == nil {
					if n, _ := r.RowsAffected(); n > 0 {
						updated += int(n)
					}
				}
			}
			return updated
		}
		pendingTurnlessTokenSourceIDs := []string{}

		for scanner.Scan() {
			lineNum++
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			res.LinesExamined++

			var rl struct {
				ID      string          `json:"id"`
				Type    string          `json:"type"`
				Payload json.RawMessage `json:"payload"`
			}
			if err := json.Unmarshal(line, &rl); err != nil {
				continue
			}

			switch rl.Type {
			case "session_meta", "session_configured", "session_start", "turn_context":
				var payload struct {
					ID        string `json:"id"`
					SessionID string `json:"session_id"`
					TurnID    string `json:"turn_id"`
					Model     string `json:"model"`
				}
				if err := json.Unmarshal(rl.Payload, &payload); err != nil {
					continue
				}
				if payload.ID != "" {
					addSessionID(payload.ID)
				}
				if payload.SessionID != "" {
					addSessionID(payload.SessionID)
				}
				if payload.TurnID != "" {
					currentTurnID = payload.TurnID
				}
				if payload.Model != "" {
					currentModel = payload.Model
					if currentTurnID != "" {
						res.TokenUsageUpdated += updateTokenUsageByMessageForSessions(currentModel, currentTurnID)
					}
				}
				if currentTurnID != "" && len(pendingTurnlessTokenSourceIDs) > 0 {
					for _, sourceID := range pendingTurnlessTokenSourceIDs {
						res.TokenUsageUpdated += updateTokenUsageForSessions(currentTurnID, currentModel, sourceID)
					}
					pendingTurnlessTokenSourceIDs = nil
				}
			case "event_msg":
				var env struct {
					Type string `json:"type"`
				}
				if err := json.Unmarshal(rl.Payload, &env); err != nil {
					continue
				}
				switch env.Type {
				case "task_started":
					var payload struct {
						TurnID string `json:"turn_id"`
					}
					if err := json.Unmarshal(rl.Payload, &payload); err == nil && payload.TurnID != "" {
						currentTurnID = payload.TurnID
						if len(pendingTurnlessTokenSourceIDs) > 0 {
							for _, sourceID := range pendingTurnlessTokenSourceIDs {
								res.TokenUsageUpdated += updateTokenUsageForSessions(currentTurnID, currentModel, sourceID)
							}
							pendingTurnlessTokenSourceIDs = nil
						}
					}
				case "user_message":
					var payload struct {
						Message string `json:"message"`
					}
					if err := json.Unmarshal(rl.Payload, &payload); err != nil {
						continue
					}
					msgID := ""
					if currentTurnID != "" {
						msgID = "user:" + currentTurnID
					} else {
						msgID = fmt.Sprintf("user:%s:L%d:%s", base, lineNum, shortHash(strings.TrimSpace(payload.Message)))
					}
					sourceID := fmt.Sprintf("user:%s:L%d:%s", base, lineNum, shortHash(strings.TrimSpace(payload.Message)))
					res.ActionsUpdated += updateActionsForSessions(msgID, sourceID)
				case "exec_command_end":
					var payload struct {
						CallID string `json:"call_id"`
						TurnID string `json:"turn_id"`
					}
					if err := json.Unmarshal(rl.Payload, &payload); err != nil {
						continue
					}
					if payload.TurnID != "" {
						currentTurnID = payload.TurnID
						if len(pendingTurnlessTokenSourceIDs) > 0 {
							for _, sourceID := range pendingTurnlessTokenSourceIDs {
								res.TokenUsageUpdated += updateTokenUsageForSessions(currentTurnID, currentModel, sourceID)
							}
							pendingTurnlessTokenSourceIDs = nil
						}
					}
					msgID := firstNonEmpty(payload.TurnID, currentTurnID)
					if msgID == "" || payload.CallID == "" {
						continue
					}
					res.ActionsUpdated += updateActionsForSessions(msgID, payload.CallID)
				case "web_search_end":
					var payload struct {
						CallID string `json:"call_id"`
						TurnID string `json:"turn_id"`
						Query  string `json:"query"`
						Action struct {
							Query   string   `json:"query"`
							Queries []string `json:"queries"`
						} `json:"action"`
					}
					if err := json.Unmarshal(rl.Payload, &payload); err != nil {
						continue
					}
					if payload.TurnID != "" {
						currentTurnID = payload.TurnID
						if len(pendingTurnlessTokenSourceIDs) > 0 {
							for _, sourceID := range pendingTurnlessTokenSourceIDs {
								res.TokenUsageUpdated += updateTokenUsageForSessions(currentTurnID, currentModel, sourceID)
							}
							pendingTurnlessTokenSourceIDs = nil
						}
					}
					msgID := firstNonEmpty(payload.TurnID, currentTurnID)
					if msgID == "" {
						continue
					}
					sourceID := payload.CallID
					if sourceID == "" {
						query := firstNonEmpty(payload.Query, payload.Action.Query, strings.Join(payload.Action.Queries, "; "))
						sourceID = fmt.Sprintf("web:%s:L%d:%s", base, lineNum, shortHash(query))
					}
					res.ActionsUpdated += updateActionsForSessions(msgID, sourceID)
				case "task_complete":
					var payload struct {
						TurnID string `json:"turn_id"`
					}
					if err := json.Unmarshal(rl.Payload, &payload); err != nil {
						continue
					}
					if payload.TurnID != "" {
						currentTurnID = payload.TurnID
						if len(pendingTurnlessTokenSourceIDs) > 0 {
							for _, sourceID := range pendingTurnlessTokenSourceIDs {
								res.TokenUsageUpdated += updateTokenUsageForSessions(currentTurnID, currentModel, sourceID)
							}
							pendingTurnlessTokenSourceIDs = nil
						}
					}
					msgID := firstNonEmpty(payload.TurnID, currentTurnID)
					if msgID == "" {
						continue
					}
					sourceID := fmt.Sprintf("complete:%s:%d", firstNonEmpty(payload.TurnID, sessionIDs[0], base), lineNum)
					res.ActionsUpdated += updateActionsForSessions(msgID, sourceID)
				case "token_count":
					msgID := currentTurnID
					sourceID := fmt.Sprintf("tk:%s:L%d", base, lineNum)
					if msgID == "" {
						pendingTurnlessTokenSourceIDs = append(pendingTurnlessTokenSourceIDs, sourceID)
						continue
					}
					res.TokenUsageUpdated += updateTokenUsageForSessions(msgID, currentModel, sourceID)
				}
			case "token_count", "usage":
				sourceID := fmt.Sprintf("tk:%s:L%d", base, lineNum)
				if currentTurnID == "" {
					pendingTurnlessTokenSourceIDs = append(pendingTurnlessTokenSourceIDs, sourceID)
					continue
				}
				res.TokenUsageUpdated += updateTokenUsageForSessions(currentTurnID, currentModel, sourceID)
			}
		}
		return nil
	})
	if walkErr != nil && walkErr != filepath.SkipAll {
		return res, fmt.Errorf("backfill codex message-id: walk %s: %w", sessionsDir, walkErr)
	}
	return res, nil
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// backfillOpenCodeMessageID populates message_id on actions and token_usage
// rows for the opencode adapter. Pre-parity-pass ingests stored the
// upstream message id in source_event_id (with a `message:` / `part:` /
// `tokens:` / `complete:` / `subtask:` prefix) but never wrote it to
// the dedicated message_id column, so the dashboard's per-message
// timeline / per-turn dedup couldn't see those rows.
//
// The fix is pure SQL — strip the prefix from source_event_id and
// write what's left to message_id. For tool-part rows, source_event_id
// is `part:<id>` while the parent message id is what we want; we get
// it from the actions row's link to its parent message_id (already
// populated by the post-parity adapter). The IS NULL/empty guard
// preserves any explicit value already written.
func backfillOpenCodeMessageID(ctx context.Context, db *sql.DB) (MessageIDBackfill, error) {
	res := MessageIDBackfill{}

	// Actions: prefix mapping per source_event_id shape.
	//   message:<id>   → user prompt, message_id = "user:<id>"
	//   complete:<id>  → assistant completion, message_id = <id>
	//   subtask:<id>   → subagent spawn (parent's part), message_id = <part-id-derived>
	//                    parts don't carry message_id directly in source_event_id,
	//                    so subtask rows can't be cleanly backfilled here — they need
	//                    the source DB re-read. Skip in this SQL-only pass.
	//   part:<id>      → tool call; message_id is the parent message, which the part
	//                    row's parent_message_id would carry but we don't store that.
	//                    Skip in SQL-only; backfillOpenCodeParts handles it.
	//   todo:<sess>:<pos>:<tu> → todos aren't message-attached; leave message_id empty.
	//
	// So this pass handles the message-keyed rows: user prompts and
	// completions. The remaining shapes need a source DB re-read,
	// which lives in backfillOpenCodeParts.
	r, err := db.ExecContext(ctx, `
		UPDATE actions
		   SET message_id = 'user:' || substr(source_event_id, length('message:') + 1)
		 WHERE tool = 'opencode'
		   AND action_type = 'user_prompt'
		   AND source_event_id LIKE 'message:%'
		   AND (message_id IS NULL OR message_id = '')`)
	if err != nil {
		return res, fmt.Errorf("backfill opencode message-id (user prompts): %w", err)
	}
	if n, _ := r.RowsAffected(); n > 0 {
		res.ActionsUpdated += int(n)
	}

	r, err = db.ExecContext(ctx, `
		UPDATE actions
		   SET message_id = substr(source_event_id, length('complete:') + 1)
		 WHERE tool = 'opencode'
		   AND action_type = 'task_complete'
		   AND source_event_id LIKE 'complete:%'
		   AND (message_id IS NULL OR message_id = '')`)
	if err != nil {
		return res, fmt.Errorf("backfill opencode message-id (completions): %w", err)
	}
	if n, _ := r.RowsAffected(); n > 0 {
		res.ActionsUpdated += int(n)
	}

	// Token usage: source_event_id is `tokens:<message_id>`. Pure prefix strip.
	r, err = db.ExecContext(ctx, `
		UPDATE token_usage
		   SET message_id = substr(source_event_id, length('tokens:') + 1)
		 WHERE tool = 'opencode'
		   AND source_event_id LIKE 'tokens:%'
		   AND (message_id IS NULL OR message_id = '')`)
	if err != nil {
		return res, fmt.Errorf("backfill opencode message-id (token rows): %w", err)
	}
	if n, _ := r.RowsAffected(); n > 0 {
		res.TokenUsageUpdated += int(n)
	}

	return res, nil
}

// OpenClawActionsBackfill summarises the --openclaw-action-types pass.
type OpenClawActionsBackfill struct {
	ActionsUpdated int `json:"actions_updated"`
}

// backfillOpenClawActionTypes corrects historical openclaw rows where
// `sessions_spawn` was bucketed with the rest of `sessions_*` /
// `agents_*` / `gateway` calls under `mcp_call`. Post-parity-pass the
// adapter maps it to `spawn_subagent`; this brings old rows in line.
func backfillOpenClawActionTypes(ctx context.Context, db *sql.DB) (OpenClawActionsBackfill, error) {
	res := OpenClawActionsBackfill{}
	r, err := db.ExecContext(ctx, `
		UPDATE actions
		   SET action_type = 'spawn_subagent'
		 WHERE tool = 'openclaw'
		   AND raw_tool_name = 'sessions_spawn'
		   AND action_type = 'mcp_call'`)
	if err != nil {
		return res, fmt.Errorf("backfill openclaw action-types: %w", err)
	}
	if n, _ := r.RowsAffected(); n > 0 {
		res.ActionsUpdated = int(n)
	}
	return res, nil
}

// CursorModelBackfill summarises the --cursor-model pass.
type CursorModelBackfill struct {
	SessionsUpdated int `json:"sessions_updated"`
}

// backfillCursorModel populates the model column on cursor session
// rows from the matching token_usage row. Pre-parity-pass the cursor
// hook decoded rawHookPayload.Model into the struct but never assigned
// it to the ToolEvent — so for sessions where only tool events landed
// before the stop token row, the session row's model stayed empty.
// (The actions table itself has no model column; per-action model is
// always read off the joining token_usage row, so there's nothing to
// backfill there.)
//
// The post-parity hook does set Model on every ToolEvent so
// UpsertSession lifts it correctly going forward; this catches up
// historical sessions whose first ingest was a tool event without a
// model and whose session row therefore stayed empty.
func backfillCursorModel(ctx context.Context, db *sql.DB) (CursorModelBackfill, error) {
	res := CursorModelBackfill{}
	r, err := db.ExecContext(ctx, `
		UPDATE sessions
		   SET model = (
		         SELECT t.model
		           FROM token_usage t
		          WHERE t.tool = 'cursor'
		            AND t.session_id = sessions.id
		            AND t.model IS NOT NULL AND t.model != ''
		          ORDER BY t.id DESC
		          LIMIT 1
		       )
		 WHERE tool = 'cursor'
		   AND (model IS NULL OR model = '')
		   AND EXISTS (
		         SELECT 1 FROM token_usage t
		          WHERE t.tool = 'cursor'
		            AND t.session_id = sessions.id
		            AND t.model IS NOT NULL AND t.model != ''
		       )`)
	if err != nil {
		return res, fmt.Errorf("backfill cursor model: %w", err)
	}
	if n, _ := r.RowsAffected(); n > 0 {
		res.SessionsUpdated = int(n)
	}
	return res, nil
}

func backfillCursorMessageID(ctx context.Context, db *sql.DB) (MessageIDBackfill, error) {
	res := MessageIDBackfill{}

	result, err := db.ExecContext(ctx, `
		UPDATE actions
		   SET message_id = substr(source_event_id, 1, instr(source_event_id, ':') - 1)
		 WHERE tool = 'cursor'
		   AND action_type != 'user_prompt'
		   AND (message_id IS NULL OR message_id = '')
		   AND instr(source_event_id, ':') > 0`)
	if err != nil {
		return res, fmt.Errorf("backfill cursor message-id: %w", err)
	}
	if n, _ := result.RowsAffected(); n > 0 {
		res.ActionsUpdated = int(n)
	}
	result, err = db.ExecContext(ctx, `
		UPDATE actions
		   SET message_id = 'user:' || substr(source_event_id, 1, instr(source_event_id, ':') - 1)
		 WHERE tool = 'cursor'
		   AND action_type = 'user_prompt'
		   AND (message_id IS NULL OR message_id = '' OR message_id = substr(source_event_id, 1, instr(source_event_id, ':') - 1))
		   AND instr(source_event_id, ':') > 0`)
	if err != nil {
		return res, fmt.Errorf("backfill cursor user-prompt message-id: %w", err)
	}
	if n, _ := result.RowsAffected(); n > 0 {
		res.ActionsUpdated += int(n)
	}
	return res, nil
}

func backfillCursorHookUsage(ctx context.Context, db *sql.DB, logsDir string, fileLimit int) (MessageIDBackfill, error) {
	res := MessageIDBackfill{}
	st := store.New(db)

	walkErr := filepath.WalkDir(logsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasPrefix(filepath.Base(path), "cursor.hooks.") || filepath.Ext(path) != ".log" {
			return nil
		}
		if fileLimit > 0 && res.FilesScanned >= fileLimit {
			return fs.SkipAll
		}
		res.FilesScanned++

		body, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, block := range extractCursorHookInputs(string(body)) {
			res.LinesExamined++
			tk, ok, err := cursor.BuildStopTokenEvent([]byte(block))
			if err != nil || !ok {
				continue
			}
			if tk.ProjectRoot == "" || tk.SessionID == "" {
				continue
			}
			n, err := st.InsertTokenEvents(ctx, []models.TokenEvent{tk})
			if err != nil {
				return err
			}
			res.TokenUsageUpdated += n
			if tk.Model != "" {
				if _, err := db.ExecContext(ctx,
					`UPDATE sessions SET model = COALESCE(NULLIF(model, ''), ?) WHERE id = ?`,
					tk.Model, tk.SessionID); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if walkErr != nil && walkErr != fs.SkipAll {
		return res, fmt.Errorf("backfill cursor hook usage: walk: %w", walkErr)
	}
	return res, nil
}

// OpenCodePartsBackfill summarises the --opencode-parts pass.
type OpenCodePartsBackfill struct {
	DBsScanned        int `json:"dbs_scanned"`
	PartsExamined     int `json:"parts_examined"`
	ToolOutputUpdated int `json:"tool_output_updated"`
	DurationUpdated   int `json:"duration_updated"`
	MessageIDUpdated  int `json:"message_id_updated"`
}

// backfillOpenCodeParts re-reads each opencode.db referenced by
// historical action rows and populates the post-parity-pass values
// that pure SQL can't recover:
//
//   - duration_ms on actions (State.Time.End − Start)
//   - message_id on actions for tool / subtask rows where the parent
//     message id lives in the part row itself rather than encoded in
//     source_event_id
//   - tool_output excerpts in the FTS5 action_excerpts table (where
//     ToolOutput actually lives — the actions table has no
//     tool_output column; the indexer.Indexer writes excerpts keyed
//     by action_id so search_past_outputs can retrieve them).
//
// Idempotent: actions UPDATE has IS NULL/zero guards; the indexer
// re-indexes by deleting the prior excerpt for the same action_id
// before inserting. Each opencode.db is opened read-only.
func backfillOpenCodeParts(ctx context.Context, db *sql.DB) (OpenCodePartsBackfill, error) {
	res := OpenCodePartsBackfill{}

	srcRows, err := db.QueryContext(ctx, `
		SELECT DISTINCT source_file FROM actions
		 WHERE tool = 'opencode'
		   AND source_file IS NOT NULL
		   AND source_file != ''`)
	if err != nil {
		return res, fmt.Errorf("backfill opencode-parts: list source_files: %w", err)
	}
	var dbPaths []string
	for srcRows.Next() {
		var p string
		if err := srcRows.Scan(&p); err != nil {
			srcRows.Close()
			return res, err
		}
		dbPaths = append(dbPaths, p)
	}
	srcRows.Close()

	// Two prepared statements: one to update the persistent action row
	// (duration + message_id), one to look up the action's id +
	// metadata so we can write its excerpt to action_excerpts.
	updateAction, err := db.PrepareContext(ctx, `
		UPDATE actions
		   SET duration_ms = CASE
		           WHEN duration_ms IS NULL OR duration_ms = 0 THEN ?
		           ELSE duration_ms
		       END,
		       message_id = COALESCE(NULLIF(message_id, ''), ?)
		 WHERE tool = 'opencode'
		   AND source_file = ?
		   AND source_event_id = ?
		   AND ((duration_ms IS NULL OR duration_ms = 0)
		        OR (message_id IS NULL OR message_id = ''))`)
	if err != nil {
		return res, fmt.Errorf("backfill opencode-parts: prepare action update: %w", err)
	}
	defer updateAction.Close()

	lookupAction, err := db.PrepareContext(ctx, `
		SELECT id, COALESCE(raw_tool_name, ''), COALESCE(target, ''), COALESCE(error_message, '')
		  FROM actions
		 WHERE tool = 'opencode'
		   AND source_file = ?
		   AND source_event_id = ?`)
	if err != nil {
		return res, fmt.Errorf("backfill opencode-parts: prepare action lookup: %w", err)
	}
	defer lookupAction.Close()

	excerptExists, err := db.PrepareContext(ctx, `
		SELECT 1 FROM action_excerpts WHERE action_id = ? LIMIT 1`)
	if err != nil {
		return res, fmt.Errorf("backfill opencode-parts: prepare excerpt check: %w", err)
	}
	defer excerptExists.Close()

	indexer := indexing.New(db, 0) // 0 → DefaultMaxExcerptBytes (2KB)

	for _, dbPath := range dbPaths {
		if _, err := os.Stat(dbPath); err != nil {
			continue // db gone — skip silently
		}
		ocDB, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(2000)", filepath.ToSlash(dbPath)))
		if err != nil {
			continue
		}
		res.DBsScanned++

		partRows, err := ocDB.QueryContext(ctx, `
			SELECT p.id, COALESCE(p.message_id, ''), p.data
			  FROM part p
			 WHERE json_extract(p.data, '$.type') IN ('tool', 'subtask')`)
		if err != nil {
			ocDB.Close()
			continue
		}
		for partRows.Next() {
			var (
				partID    string
				messageID string
				data      string
			)
			if err := partRows.Scan(&partID, &messageID, &data); err != nil {
				partRows.Close()
				ocDB.Close()
				return res, err
			}
			res.PartsExamined++

			var part struct {
				Type  string `json:"type"`
				State struct {
					Output   string `json:"output"`
					Metadata struct {
						Output string `json:"output"`
					} `json:"metadata"`
					Time struct {
						Start int64 `json:"start"`
						End   int64 `json:"end"`
					} `json:"time"`
				} `json:"state"`
			}
			if err := json.Unmarshal([]byte(data), &part); err != nil {
				continue
			}
			output := part.State.Output
			if output == "" {
				output = part.State.Metadata.Output
			}
			var durationMs int64
			if part.State.Time.Start > 0 && part.State.Time.End > part.State.Time.Start {
				durationMs = part.State.Time.End - part.State.Time.Start
			}
			if output == "" && durationMs == 0 && messageID == "" {
				continue
			}

			// SourceEventID the adapter writes for tool parts is `part:<id>`,
			// for subtask parts it's `subtask:<id>`. Try both shapes; the
			// IS-NULL/zero guards make the wrong one a no-op.
			for _, prefix := range []string{"part:", "subtask:"} {
				sourceID := prefix + partID
				r, err := updateAction.ExecContext(ctx, durationMs, messageID, dbPath, sourceID)
				if err != nil {
					continue
				}
				if n, _ := r.RowsAffected(); n > 0 {
					if durationMs > 0 {
						res.DurationUpdated += int(n)
					}
					if messageID != "" {
						res.MessageIDUpdated += int(n)
					}
				}

				// Index the tool output excerpt against this action's id.
				// Skip when the action row didn't exist (sourceID didn't
				// match) or when an excerpt is already indexed.
				if output == "" {
					continue
				}
				var (
					actionID int64
					rawTool  string
					target   string
					errMsg   string
				)
				if err := lookupAction.QueryRowContext(ctx, dbPath, sourceID).Scan(&actionID, &rawTool, &target, &errMsg); err != nil {
					continue
				}
				var present int
				if err := excerptExists.QueryRowContext(ctx, actionID).Scan(&present); err == nil && present == 1 {
					continue // already indexed
				}
				if err := indexer.Index(ctx, actionID, rawTool, target, output, errMsg); err != nil {
					continue
				}
				res.ToolOutputUpdated++
				break
			}
		}
		partRows.Close()
		ocDB.Close()
	}
	return res, nil
}

// OpenClawModelBackfill summarises the --openclaw-model pass.
type OpenClawModelBackfill struct {
	AliasFilesScanned int `json:"alias_files_scanned"`
	AliasesLoaded     int `json:"aliases_loaded"`
	SessionsUpdated   int `json:"sessions_updated"`
}

// backfillOpenClawModel populates the model column on openclaw
// SESSION rows whose model is empty. Pre-parity-pass the sqlite-path
// taskPromptEvent / taskCompleteEvent set Model="" on the ToolEvent,
// which became sessions.model="" via UpsertSession. The parity adapter
// looks the model up via sessions.json aliases; this backfill catches
// up historical session rows.
//
// (Note: the actions table has no model column — per-action model is
// always derived from the joining token_usage row. There's nothing to
// backfill on actions for OpenClaw model; the gap is on sessions.)
//
// Strategy: for each runs.sqlite file referenced by openclaw actions,
// (1) load every sibling sessions.json under .../agents/*/sessions/,
// (2) walk task_runs and resolve each row's session_id via the same
// key-priority chain the adapter uses, (3) UPDATE sessions SET model
// where the resolved alias has provider/model and the session row's
// model is empty.
func backfillOpenClawModel(ctx context.Context, db *sql.DB) (OpenClawModelBackfill, error) {
	res := OpenClawModelBackfill{}

	srcRows, err := db.QueryContext(ctx, `
		SELECT DISTINCT source_file FROM actions
		 WHERE tool = 'openclaw'
		   AND source_file LIKE '%runs.sqlite'`)
	if err != nil {
		return res, fmt.Errorf("backfill openclaw-model: list runs.sqlite paths: %w", err)
	}
	var runsPaths []string
	for srcRows.Next() {
		var p string
		if err := srcRows.Scan(&p); err != nil {
			srcRows.Close()
			return res, err
		}
		runsPaths = append(runsPaths, p)
	}
	srcRows.Close()

	updateSession, err := db.PrepareContext(ctx, `
		UPDATE sessions
		   SET model = ?
		 WHERE id = ?
		   AND tool = 'openclaw'
		   AND (model IS NULL OR model = '')`)
	if err != nil {
		return res, fmt.Errorf("backfill openclaw-model: prepare session update: %w", err)
	}
	defer updateSession.Close()

	for _, runsPath := range runsPaths {
		if _, err := os.Stat(runsPath); err != nil {
			continue
		}
		// Sibling agents/ tree lives at .openclaw/agents/ (parent of tasks/).
		agentsRoot := filepath.Join(filepath.Dir(filepath.Dir(runsPath)), "agents")

		// Load aliases.
		aliases := map[string]struct {
			Provider string
			Model    string
		}{}
		matches, _ := filepath.Glob(filepath.Join(agentsRoot, "*", "sessions", "sessions.json"))
		for _, indexPath := range matches {
			body, err := os.ReadFile(indexPath)
			if err != nil {
				continue
			}
			res.AliasFilesScanned++
			var idx map[string]struct {
				SessionID     string `json:"sessionId"`
				ModelProvider string `json:"modelProvider"`
				Model         string `json:"model"`
				SystemPrompt  struct {
					Provider string `json:"provider"`
					Model    string `json:"model"`
				} `json:"systemPromptReport"`
			}
			if err := json.Unmarshal(body, &idx); err != nil {
				continue
			}
			for key, entry := range idx {
				provider := entry.ModelProvider
				if provider == "" {
					provider = entry.SystemPrompt.Provider
				}
				model := entry.Model
				if model == "" {
					model = entry.SystemPrompt.Model
				}
				if provider == "" && model == "" {
					continue
				}
				aliases[key] = struct {
					Provider string
					Model    string
				}{provider, model}
				if entry.SessionID != "" {
					aliases[entry.SessionID] = struct {
						Provider string
						Model    string
					}{provider, model}
				}
				res.AliasesLoaded++
			}
		}

		// Walk task_runs and resolve each row's session_id + model via the
		// same priority chain the adapter uses (child_session_key →
		// owner_key → requester_session_key → run_id → source_id → task_id).
		ocDB, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(2000)", filepath.ToSlash(runsPath)))
		if err != nil {
			continue
		}
		taskRows, err := ocDB.QueryContext(ctx, `
			SELECT task_id, COALESCE(child_session_key, ''), owner_key,
			       COALESCE(requester_session_key, ''), COALESCE(run_id, ''),
			       COALESCE(source_id, '')
			  FROM task_runs`)
		if err != nil {
			ocDB.Close()
			continue
		}
		for taskRows.Next() {
			var taskID, child, owner, requester, run, source string
			if err := taskRows.Scan(&taskID, &child, &owner, &requester, &run, &source); err != nil {
				taskRows.Close()
				ocDB.Close()
				return res, err
			}
			// Resolve the session_id: same priority chain as
			// openclaw/adapter.go::sessionID().
			sessionID := ""
			for _, key := range []string{child, owner, requester, run, source, taskID} {
				if key != "" {
					sessionID = key
					break
				}
			}
			if sessionID == "" {
				continue
			}
			// Resolve the alias by trying the same keys.
			var alias struct {
				Provider string
				Model    string
			}
			for _, key := range []string{child, owner, requester, run, source} {
				if a, ok := aliases[key]; ok && (a.Provider != "" || a.Model != "") {
					alias = a
					break
				}
			}
			if alias.Provider == "" && alias.Model == "" {
				continue
			}
			model := alias.Model
			if alias.Provider != "" && alias.Model != "" {
				model = alias.Provider + "/" + alias.Model
			}
			r, err := updateSession.ExecContext(ctx, model, sessionID)
			if err != nil {
				continue
			}
			if n, _ := r.RowsAffected(); n > 0 {
				res.SessionsUpdated += int(n)
			}
		}
		taskRows.Close()
		ocDB.Close()
	}
	return res, nil
}

// CodexReasoningBackfill summarises the --codex-reasoning pass.
type CodexReasoningBackfill struct {
	FilesScanned   int `json:"files_scanned"`
	LinesExamined  int `json:"lines_examined"`
	TurnsCaptured  int `json:"turns_captured"`
	ActionsUpdated int `json:"actions_updated"`
}

// CodexProjectRootBackfill summarises the --codex-project-root pass.
// SessionsReattributed counts sessions whose project_id changed;
// ActionsUpdated counts the cascaded action rows. token_usage has no
// project_id column (the cost engine joins to sessions for project
// context), so it's not surfaced here.
type CodexProjectRootBackfill struct {
	FilesScanned         int `json:"files_scanned"`
	SessionsReattributed int `json:"sessions_reattributed"`
	ActionsUpdated       int `json:"actions_updated"`
}

// codexProjectRootBackfillDirs returns the set of codex sessions
// directories to scan: every crossmount-resolved home's .codex/sessions,
// or just the CODEX_HOME override when set. Exposed so the cmd
// dispatcher and tests can compute / inject the same set.
func codexProjectRootBackfillDirs() []string {
	if v := os.Getenv("CODEX_HOME"); v != "" {
		return []string{filepath.Join(v, "sessions")}
	}
	var dirs []string
	for _, h := range crossmount.AllHomes() {
		dirs = append(dirs, filepath.Join(h.Path, ".codex", "sessions"))
	}
	return dirs
}

// backfillCodexProjectRoot re-attributes codex action / token /
// session rows to the correct project when their cwd was a
// Windows-style path. Pre-v1.4.28 the codex adapter passed cwd
// directly to git.Resolve, which on a non-Windows host treats
// "c:\foo\bar" as a relative path, prepends the observer's CWD, and
// then walks UP that bogus path looking for .git — landing on
// observer's own repo in the worst case. v1.4.28's adapter fix
// translates the cwd via crossmount.TranslateForeignPath; this
// backfill applies the same translation to rows ingested before the
// fix shipped, so existing data converges to the correct project.
//
// Walks codex rollout JSONL across every supplied directory — the
// dispatcher passes crossmount-resolved homes so /mnt/c/Users/*/.codex
// is included when observer runs on WSL2. Each file's first
// session_meta line provides the cwd. We translate, run git.Resolve,
// upsert the project, and UPDATE the session, all of its actions, and
// all of its token_usage rows to point to the new project_id when it
// differs.
func backfillCodexProjectRoot(ctx context.Context, db *sql.DB, sessionsDirs []string, fileLimit int) (CodexProjectRootBackfill, error) {
	res := CodexProjectRootBackfill{}

	st := store.New(db)

	updateSession, err := db.PrepareContext(ctx,
		`UPDATE sessions SET project_id = ? WHERE id = ? AND project_id != ?`)
	if err != nil {
		return res, fmt.Errorf("backfill codex-project-root: prepare session: %w", err)
	}
	defer updateSession.Close()

	updateActions, err := db.PrepareContext(ctx,
		`UPDATE actions SET project_id = ? WHERE session_id = ? AND project_id != ?`)
	if err != nil {
		return res, fmt.Errorf("backfill codex-project-root: prepare actions: %w", err)
	}
	defer updateActions.Close()

	// token_usage has no project_id column — the cost engine resolves
	// project context by joining token_usage → sessions, so updating
	// the session row is enough to fix the per-project rollup.

	for _, sessionsDir := range sessionsDirs {
		walkErr := filepath.WalkDir(sessionsDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				return nil
			}
			base := filepath.Base(path)
			if filepath.Ext(path) != ".jsonl" || !strings.HasPrefix(base, "rollout-") {
				return nil
			}
			if fileLimit > 0 && res.FilesScanned >= fileLimit {
				return filepath.SkipAll
			}
			res.FilesScanned++

			sessionID, cwd, ok := readCodexSessionMeta(path)
			if !ok || sessionID == "" || cwd == "" {
				return nil
			}
			translated := crossmount.TranslateForeignPath(cwd)
			info, gerr := git.Resolve(translated)
			if gerr != nil {
				return nil
			}
			newRoot := info.Root
			if newRoot == "" {
				return nil
			}
			pid, err := st.UpsertProject(ctx, newRoot, "")
			if err != nil {
				return nil
			}

			if r, err := updateSession.ExecContext(ctx, pid, sessionID, pid); err == nil {
				if n, _ := r.RowsAffected(); n > 0 {
					res.SessionsReattributed += int(n)
				}
			}
			if r, err := updateActions.ExecContext(ctx, pid, sessionID, pid); err == nil {
				if n, _ := r.RowsAffected(); n > 0 {
					res.ActionsUpdated += int(n)
				}
			}
			return nil
		})
		if walkErr != nil && walkErr != filepath.SkipAll {
			// Per-tree errors are non-fatal — the watcher tolerates the
			// same failure modes (ENOENT on a stale crossmount home,
			// EACCES on a Windows-side directory).
			continue
		}
	}
	return res, nil
}

// readCodexSessionMeta pulls the (session_id, cwd) pair from a codex
// rollout's first session_meta record. Returns ok=false if the file
// is unreadable or doesn't contain a session_meta line in its first
// few records.
func readCodexSessionMeta(path string) (sessionID string, cwd string, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for i := 0; i < 32 && scanner.Scan(); i++ {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rl struct {
			Type    string `json:"type"`
			Payload struct {
				ID  string `json:"id"`
				Cwd string `json:"cwd"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(line, &rl); err != nil {
			continue
		}
		if rl.Type != "session_meta" {
			continue
		}
		return rl.Payload.ID, rl.Payload.Cwd, true
	}
	return "", "", false
}

// backfillCodexReasoning re-walks codex rollout JSONL to capture
// `event_msg`/`agent_message` text per turn and writes it to
// `actions.preceding_reasoning` for tool / exec / web / task_complete
// rows in that turn. The post-parity adapter does this on ingest;
// this catches up historical rows.
//
// One pass per file. Within each file we track currentTurnID across
// session_meta / turn_context / task_started / event_msg payloads
// that carry it, and bind every agent_message to that turn. After the
// file is fully scanned we issue one UPDATE per (session_id, turn_id)
// pair with the captured preamble.
func backfillCodexReasoning(ctx context.Context, db *sql.DB, sessionsDir string, fileLimit int) (CodexReasoningBackfill, error) {
	res := CodexReasoningBackfill{}

	updateAction, err := db.PrepareContext(ctx, `
		UPDATE actions
		   SET preceding_reasoning = ?
		 WHERE tool = 'codex'
		   AND session_id = ?
		   AND message_id = ?
		   AND (preceding_reasoning IS NULL OR preceding_reasoning = '')`)
	if err != nil {
		return res, fmt.Errorf("backfill codex-reasoning: prepare update: %w", err)
	}
	defer updateAction.Close()

	walkErr := filepath.WalkDir(sessionsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if filepath.Ext(path) != ".jsonl" || !strings.HasPrefix(base, "rollout-") {
			return nil
		}
		if fileLimit > 0 && res.FilesScanned >= fileLimit {
			return filepath.SkipAll
		}
		res.FilesScanned++

		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)

		// (session_id, turn_id) → preamble text. Multiple session ids
		// can apply per file (filename stem fallback + explicit session_id).
		sessionIDs := []string{strings.TrimPrefix(strings.TrimSuffix(base, ".jsonl"), "rollout-")}
		addSession := func(id string) {
			if id == "" {
				return
			}
			for _, e := range sessionIDs {
				if e == id {
					return
				}
			}
			sessionIDs = append(sessionIDs, id)
		}
		preambles := map[string]string{} // turn_id → text
		currentTurnID := ""

		for scanner.Scan() {
			res.LinesExamined++
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			var rl struct {
				Type    string          `json:"type"`
				Payload json.RawMessage `json:"payload"`
			}
			if err := json.Unmarshal(line, &rl); err != nil {
				continue
			}
			switch rl.Type {
			case "session_meta", "session_configured", "session_start", "turn_context":
				var p struct {
					ID        string `json:"id"`
					SessionID string `json:"session_id"`
					TurnID    string `json:"turn_id"`
				}
				if err := json.Unmarshal(rl.Payload, &p); err == nil {
					addSession(p.ID)
					addSession(p.SessionID)
					if p.TurnID != "" {
						currentTurnID = p.TurnID
					}
				}
			case "event_msg":
				var env struct {
					Type string `json:"type"`
				}
				if err := json.Unmarshal(rl.Payload, &env); err != nil {
					continue
				}
				switch env.Type {
				case "task_started":
					var p struct {
						TurnID string `json:"turn_id"`
					}
					if err := json.Unmarshal(rl.Payload, &p); err == nil && p.TurnID != "" {
						currentTurnID = p.TurnID
					}
				case "agent_message":
					var p struct {
						TurnID  string `json:"turn_id"`
						Message string `json:"message"`
					}
					if err := json.Unmarshal(rl.Payload, &p); err != nil {
						continue
					}
					turnID := p.TurnID
					if turnID == "" {
						turnID = currentTurnID
					}
					if turnID == "" {
						continue
					}
					msg := strings.TrimSpace(p.Message)
					if msg == "" {
						continue
					}
					preambles[turnID] = msg
				case "exec_command_end", "web_search_end", "task_complete":
					var p struct {
						TurnID string `json:"turn_id"`
					}
					if err := json.Unmarshal(rl.Payload, &p); err == nil && p.TurnID != "" {
						currentTurnID = p.TurnID
					}
				}
			}
		}

		// Apply: one UPDATE per (sessionID, turnID, preamble).
		for turnID, text := range preambles {
			truncated := text
			if len(truncated) > 500 {
				truncated = truncated[:500]
			}
			res.TurnsCaptured++
			for _, sessID := range sessionIDs {
				r, err := updateAction.ExecContext(ctx, truncated, sessID, turnID)
				if err != nil {
					continue
				}
				if n, _ := r.RowsAffected(); n > 0 {
					res.ActionsUpdated += int(n)
				}
			}
		}
		return nil
	})
	if walkErr != nil && walkErr != filepath.SkipAll {
		return res, fmt.Errorf("backfill codex-reasoning: walk %s: %w", sessionsDir, walkErr)
	}
	return res, nil
}

// OpenClawReasoningBackfill summarises the --openclaw-reasoning pass.
type OpenClawReasoningBackfill struct {
	FilesScanned   int `json:"files_scanned"`
	LinesExamined  int `json:"lines_examined"`
	ActionsUpdated int `json:"actions_updated"`
}

// backfillOpenClawReasoning re-walks openclaw session JSONL files and
// populates `actions.preceding_reasoning` from text/thinking content
// blocks that precede each toolCall in an assistant message. Mirrors
// the post-parity adapter's per-toolCall capture: as content iterates,
// each text/thinking block updates the running preamble, and each
// toolCall block captures its current value.
//
// The action row is identified by (source_file, source_event_id),
// where source_event_id is `firstNonEmpty(content.ID, "tool:<name>:L<n>")`
// matching what the adapter wrote.
func backfillOpenClawReasoning(ctx context.Context, db *sql.DB, sessionsDir string, fileLimit int) (OpenClawReasoningBackfill, error) {
	res := OpenClawReasoningBackfill{}

	updateAction, err := db.PrepareContext(ctx, `
		UPDATE actions
		   SET preceding_reasoning = ?
		 WHERE tool = 'openclaw'
		   AND source_file = ?
		   AND source_event_id = ?
		   AND (preceding_reasoning IS NULL OR preceding_reasoning = '')`)
	if err != nil {
		return res, fmt.Errorf("backfill openclaw-reasoning: prepare update: %w", err)
	}
	defer updateAction.Close()

	walkErr := filepath.WalkDir(sessionsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".jsonl" {
			return nil
		}
		if fileLimit > 0 && res.FilesScanned >= fileLimit {
			return filepath.SkipAll
		}
		res.FilesScanned++

		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
		lineNum := 0
		for scanner.Scan() {
			lineNum++
			res.LinesExamined++
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			var rl struct {
				Type    string `json:"type"`
				Message struct {
					Role    string `json:"role"`
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"content"`
				} `json:"message"`
			}
			if err := json.Unmarshal(line, &rl); err != nil {
				continue
			}
			if rl.Type != "message" || rl.Message.Role != "assistant" {
				continue
			}
			var preceding string
			for _, c := range rl.Message.Content {
				switch c.Type {
				case "text", "thinking":
					if t := strings.TrimSpace(c.Text); t != "" {
						preceding = t
					}
				case "toolCall":
					if preceding == "" {
						continue
					}
					sourceEventID := c.ID
					if sourceEventID == "" {
						sourceEventID = fmt.Sprintf("tool:%s:L%d", c.Name, lineNum)
					}
					truncated := preceding
					if len(truncated) > 500 {
						truncated = truncated[:500]
					}
					r, err := updateAction.ExecContext(ctx, truncated, path, sourceEventID)
					if err != nil {
						continue
					}
					if n, _ := r.RowsAffected(); n > 0 {
						res.ActionsUpdated += int(n)
					}
				}
			}
		}
		return nil
	})
	if walkErr != nil && walkErr != filepath.SkipAll {
		return res, fmt.Errorf("backfill openclaw-reasoning: walk %s: %w", sessionsDir, walkErr)
	}
	return res, nil
}

// openclawAgentsDir returns the openclaw agents tree (where session
// jsonl files live). Defaults to ~/.openclaw/agents.
func openclawAgentsDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".openclaw", "agents")
}

// ClaudeCodeUserPromptsBackfill summarises the --claudecode-user-prompts pass.
type ClaudeCodeUserPromptsBackfill struct {
	FilesScanned    int `json:"files_scanned"`
	UserEventsFound int `json:"user_events_found"`
	ActionsInserted int `json:"actions_inserted"`
}

// backfillClaudeCodeUserPrompts walks every Claude Code JSONL file
// under the projects tree, re-runs the adapter, and ingests only the
// user_prompt events. Catches sessions ingested before the adapter
// started emitting user_prompt actions for user-role lines with text
// content. Idempotent via the (source_file, source_event_id) UNIQUE
// index — already-present rows are no-ops.
func backfillClaudeCodeUserPrompts(ctx context.Context, db *sql.DB, projectsDir string, fileLimit int) (ClaudeCodeUserPromptsBackfill, error) {
	res := ClaudeCodeUserPromptsBackfill{}
	a := claudecode.New()
	st := store.New(db)

	walkErr := filepath.WalkDir(projectsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".jsonl" {
			return nil
		}
		if fileLimit > 0 && res.FilesScanned >= fileLimit {
			return filepath.SkipAll
		}
		res.FilesScanned++

		parseRes, err := a.ParseSessionFile(ctx, path, 0)
		if err != nil {
			return nil // skip unreadable files rather than fail the whole pass
		}
		var userPrompts []models.ToolEvent
		for _, ev := range parseRes.ToolEvents {
			if ev.ActionType == models.ActionUserPrompt {
				userPrompts = append(userPrompts, ev)
			}
		}
		res.UserEventsFound += len(userPrompts)
		if len(userPrompts) == 0 {
			return nil
		}
		ingestRes, err := st.Ingest(ctx, userPrompts, nil, store.IngestOptions{
			IsNativeTool: claudecode.IsNativeTool,
		})
		if err != nil {
			return fmt.Errorf("backfill claudecode-user-prompts: ingest: %w", err)
		}
		res.ActionsInserted += ingestRes.ActionsInserted
		return nil
	})
	if walkErr != nil && walkErr != filepath.SkipAll {
		return res, fmt.Errorf("backfill claudecode-user-prompts: walk %s: %w", projectsDir, walkErr)
	}
	return res, nil
}

// ClaudeCodeAPIErrorsBackfill summarises the --claudecode-api-errors pass.
type ClaudeCodeAPIErrorsBackfill struct {
	FilesScanned    int `json:"files_scanned"`
	APIErrorsFound  int `json:"api_errors_found"`
	ActionsInserted int `json:"actions_inserted"`
}

// backfillClaudeCodeAPIErrors walks every Claude Code JSONL file under
// projectsDir, re-runs the adapter, and ingests only the api_error
// events. Catches sessions ingested before v1.4.20 — pre-fix the
// adapter dropped type=system records (where api_error subtype lives)
// because of the `len(line.Message) == 0` short-circuit. Idempotent
// via the (source_file, source_event_id) UNIQUE index — already-
// present rows are no-ops.
func backfillClaudeCodeAPIErrors(ctx context.Context, db *sql.DB, projectsDir string, fileLimit int) (ClaudeCodeAPIErrorsBackfill, error) {
	res := ClaudeCodeAPIErrorsBackfill{}
	a := claudecode.New()
	st := store.New(db)

	walkErr := filepath.WalkDir(projectsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".jsonl" {
			return nil
		}
		if fileLimit > 0 && res.FilesScanned >= fileLimit {
			return filepath.SkipAll
		}
		res.FilesScanned++

		parseRes, err := a.ParseSessionFile(ctx, path, 0)
		if err != nil {
			return nil // skip unreadable files rather than fail the whole pass
		}
		var apiErrors []models.ToolEvent
		for _, ev := range parseRes.ToolEvents {
			if ev.ActionType == models.ActionAPIError {
				apiErrors = append(apiErrors, ev)
			}
		}
		res.APIErrorsFound += len(apiErrors)
		if len(apiErrors) == 0 {
			return nil
		}
		ingestRes, err := st.Ingest(ctx, apiErrors, nil, store.IngestOptions{
			IsNativeTool: claudecode.IsNativeTool,
		})
		if err != nil {
			return fmt.Errorf("backfill claudecode-api-errors: ingest: %w", err)
		}
		res.ActionsInserted += ingestRes.ActionsInserted
		return nil
	})
	if walkErr != nil && walkErr != filepath.SkipAll {
		return res, fmt.Errorf("backfill claudecode-api-errors: walk %s: %w", projectsDir, walkErr)
	}
	return res, nil
}

// OpenCodeTokensBackfill summarises the --opencode-tokens pass.
type OpenCodeTokensBackfill struct {
	DBsScanned         int `json:"dbs_scanned"`
	TokenRowsExtracted int `json:"token_rows_extracted"`
	TokenRowsInserted  int `json:"token_rows_inserted"`
}

// backfillOpenCodeTokens re-runs the opencode adapter against every
// opencode.db referenced by historical action rows and ingests any
// token_usage rows that aren't already present. Catches the case
// where actions were ingested by an older adapter version that
// didn't yet read data.tokens, or where the parse_cursors watermark
// advanced past the assistant message before token extraction was
// added.
//
// Each opencode.db is parsed with fromOffset=0 so the full message
// table is re-scanned. ToolEvents from the parse result are
// discarded (actions are already in observer's DB); only TokenEvents
// are passed to store.Ingest, which is idempotent on
// (source_file, source_event_id) — duplicate token rows are
// rejected by the unique index.
func backfillOpenCodeTokens(ctx context.Context, db *sql.DB) (OpenCodeTokensBackfill, error) {
	res := OpenCodeTokensBackfill{}

	srcRows, err := db.QueryContext(ctx, `
		SELECT DISTINCT source_file FROM actions
		 WHERE tool = 'opencode'
		   AND source_file IS NOT NULL
		   AND source_file != ''`)
	if err != nil {
		return res, fmt.Errorf("backfill opencode-tokens: list source_files: %w", err)
	}
	var dbPaths []string
	for srcRows.Next() {
		var p string
		if err := srcRows.Scan(&p); err != nil {
			srcRows.Close()
			return res, err
		}
		dbPaths = append(dbPaths, p)
	}
	srcRows.Close()

	a := opencode.New()
	st := store.New(db)

	for _, dbPath := range dbPaths {
		if _, err := os.Stat(dbPath); err != nil {
			continue
		}
		parseRes, err := a.ParseSessionFile(ctx, dbPath, 0)
		if err != nil {
			continue
		}
		res.DBsScanned++
		res.TokenRowsExtracted += len(parseRes.TokenEvents)
		if len(parseRes.TokenEvents) == 0 {
			continue
		}
		// Ingest only the token events. ToolEvents are already in the
		// DB from the original ingest pass; re-ingesting them would be
		// safe (idempotent on source_event_id) but wasteful.
		ingestRes, err := st.Ingest(ctx, nil, parseRes.TokenEvents, store.IngestOptions{})
		if err != nil {
			return res, fmt.Errorf("backfill opencode-tokens: ingest: %w", err)
		}
		res.TokenRowsInserted += ingestRes.TokensInserted
	}
	return res, nil
}

// backfillCopilotMessageID walks the Copilot debug log paths
// referenced by historical action rows and populates message_id where
// it's still NULL. Mirrors the post-parity adapter's grouping:
//
//   - user_message lines: message_id = "user:" + spanId
//   - tool_call / agent_response / llm_request lines: message_id =
//     "assistant:" + (parentSpanId | spanId)
//
// The action's source_event_id is the line's spanId verbatim (with a
// synthesized fallback when spanId is empty), so we match by
// (source_file, source_event_id).
func backfillCopilotMessageID(ctx context.Context, db *sql.DB) (MessageIDBackfill, error) {
	res := MessageIDBackfill{}

	srcRows, err := db.QueryContext(ctx, `
		SELECT DISTINCT source_file FROM actions
		 WHERE tool = 'copilot'
		   AND source_file IS NOT NULL
		   AND source_file != ''`)
	if err != nil {
		return res, fmt.Errorf("backfill copilot message-id: list source_files: %w", err)
	}
	var paths []string
	for srcRows.Next() {
		var p string
		if err := srcRows.Scan(&p); err != nil {
			srcRows.Close()
			return res, err
		}
		paths = append(paths, p)
	}
	srcRows.Close()

	updateAction, err := db.PrepareContext(ctx, `
		UPDATE actions
		   SET message_id = COALESCE(NULLIF(message_id, ''), ?)
		 WHERE tool = 'copilot'
		   AND source_file = ?
		   AND source_event_id = ?
		   AND (message_id IS NULL OR message_id = '')`)
	if err != nil {
		return res, fmt.Errorf("backfill copilot message-id: prepare actions: %w", err)
	}
	defer updateAction.Close()

	updateTokenUsage, err := db.PrepareContext(ctx, `
		UPDATE token_usage
		   SET message_id = COALESCE(NULLIF(message_id, ''), ?)
		 WHERE tool = 'copilot'
		   AND source_file = ?
		   AND source_event_id = ?
		   AND (message_id IS NULL OR message_id = '')`)
	if err != nil {
		return res, fmt.Errorf("backfill copilot message-id: prepare token_usage: %w", err)
	}
	defer updateTokenUsage.Close()

	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		res.FilesScanned++
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
		for scanner.Scan() {
			line := scanner.Bytes()
			res.LinesExamined++
			var rl struct {
				Type         string `json:"type"`
				SpanID       string `json:"spanId"`
				ParentSpanID string `json:"parentSpanId"`
			}
			if err := json.Unmarshal(line, &rl); err != nil {
				continue
			}
			if rl.SpanID == "" {
				continue
			}
			var msgID string
			switch rl.Type {
			case "user_message":
				msgID = "user:" + rl.SpanID
			default:
				root := rl.ParentSpanID
				if root == "" {
					root = rl.SpanID
				}
				msgID = "assistant:" + root
			}
			if r, err := updateAction.ExecContext(ctx, msgID, path, rl.SpanID); err == nil {
				if n, _ := r.RowsAffected(); n > 0 {
					res.ActionsUpdated += int(n)
				}
			}
			// llm_request lines drive token_usage rows; same source_event_id key.
			if rl.Type == "llm_request" {
				if r, err := updateTokenUsage.ExecContext(ctx, msgID, path, rl.SpanID); err == nil {
					if n, _ := r.RowsAffected(); n > 0 {
						res.TokenUsageUpdated += int(n)
					}
				}
			}
		}
		f.Close()
	}
	return res, nil
}

// backfillPiMessageID walks Pi session JSONL files referenced by
// historical action rows and populates message_id where NULL.
// Mirrors the post-parity adapter's grouping:
//
//   - user role messages: message_id = "user:" + id (or
//     "user:L<n>" fallback when id is empty)
//   - assistant role messages: message_id = id (or
//     "assistant:L<n>" fallback)
//
// The source_event_id for tool calls within an assistant message is
// the inner content.id (synthesized as `tool:<name>:L<n>` when that's
// missing), so we ALSO sweep content[] entries when assistant rows
// produce multiple actions per message.
func backfillPiMessageID(ctx context.Context, db *sql.DB) (MessageIDBackfill, error) {
	res := MessageIDBackfill{}

	srcRows, err := db.QueryContext(ctx, `
		SELECT DISTINCT source_file FROM actions
		 WHERE tool = 'pi'
		   AND source_file IS NOT NULL
		   AND source_file != ''`)
	if err != nil {
		return res, fmt.Errorf("backfill pi message-id: list source_files: %w", err)
	}
	var paths []string
	for srcRows.Next() {
		var p string
		if err := srcRows.Scan(&p); err != nil {
			srcRows.Close()
			return res, err
		}
		paths = append(paths, p)
	}
	srcRows.Close()

	updateAction, err := db.PrepareContext(ctx, `
		UPDATE actions
		   SET message_id = COALESCE(NULLIF(message_id, ''), ?)
		 WHERE tool = 'pi'
		   AND source_file = ?
		   AND source_event_id = ?
		   AND (message_id IS NULL OR message_id = '')`)
	if err != nil {
		return res, fmt.Errorf("backfill pi message-id: prepare actions: %w", err)
	}
	defer updateAction.Close()

	updateTokenUsage, err := db.PrepareContext(ctx, `
		UPDATE token_usage
		   SET message_id = COALESCE(NULLIF(message_id, ''), ?)
		 WHERE tool = 'pi'
		   AND source_file = ?
		   AND source_event_id = ?
		   AND (message_id IS NULL OR message_id = '')`)
	if err != nil {
		return res, fmt.Errorf("backfill pi message-id: prepare token_usage: %w", err)
	}
	defer updateTokenUsage.Close()

	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		res.FilesScanned++
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
		lineNum := 0
		base := filepath.Base(path)
		_ = base
		for scanner.Scan() {
			lineNum++
			res.LinesExamined++
			line := scanner.Bytes()
			var rl struct {
				ID      string `json:"id"`
				Message struct {
					Role    string `json:"role"`
					Content []struct {
						Type string `json:"type"`
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"content"`
				} `json:"message"`
			}
			if err := json.Unmarshal(line, &rl); err != nil {
				continue
			}
			role := rl.Message.Role
			if role == "" {
				continue
			}
			// Compute the per-line message_id the adapter would have
			// written — assistantMessageID(id, lineNum) for assistant
			// rows, "user:" + (id|L<n>) for user rows.
			var msgID string
			if role == "user" {
				if rl.ID != "" {
					msgID = "user:" + rl.ID
				} else {
					msgID = fmt.Sprintf("user:L%d", lineNum)
				}
			} else {
				if rl.ID != "" {
					msgID = rl.ID
				} else {
					msgID = fmt.Sprintf("assistant:L%d", lineNum)
				}
			}

			// Match the source_event_id the adapter wrote for this row's
			// outer envelope (user prompt / task_complete / usage).
			outerSourceID := rl.ID
			if outerSourceID == "" {
				switch role {
				case "user":
					outerSourceID = fmt.Sprintf("user:L%d", lineNum)
				default:
					outerSourceID = fmt.Sprintf("complete:L%d", lineNum)
				}
			}
			if r, err := updateAction.ExecContext(ctx, msgID, path, outerSourceID); err == nil {
				if n, _ := r.RowsAffected(); n > 0 {
					res.ActionsUpdated += int(n)
				}
			}

			// For assistant rows, sweep content[] for tool_use blocks —
			// each gets its own action keyed by content.id (or the
			// `tool:<name>:L<n>` synthesis).
			if role == "assistant" {
				for _, c := range rl.Message.Content {
					eid := c.ID
					if eid == "" {
						eid = fmt.Sprintf("tool:%s:L%d", c.Name, lineNum)
					}
					if r, err := updateAction.ExecContext(ctx, msgID, path, eid); err == nil {
						if n, _ := r.RowsAffected(); n > 0 {
							res.ActionsUpdated += int(n)
						}
					}
				}
				// Token rows on assistant messages — source_event_id
				// is `usage:<id>` or `usage:L<n>`.
				usageID := "usage:" + rl.ID
				if rl.ID == "" {
					usageID = fmt.Sprintf("usage:L%d", lineNum)
				}
				if r, err := updateTokenUsage.ExecContext(ctx, msgID, path, usageID); err == nil {
					if n, _ := r.RowsAffected(); n > 0 {
						res.TokenUsageUpdated += int(n)
					}
				}
			}
		}
		f.Close()
	}
	return res, nil
}

func extractCursorHookInputs(body string) []string {
	lines := strings.Split(body, "\n")
	var blocks []string
	var cur []string
	inInput := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "INPUT:":
			inInput = true
			cur = cur[:0]
		case trimmed == "OUTPUT:":
			if inInput && len(cur) > 0 {
				blocks = append(blocks, strings.Join(cur, "\n"))
			}
			inInput = false
		case inInput:
			cur = append(cur, line)
		}
	}
	return blocks
}

// CursorUserPromptsBackfill summarises the --cursor-user-prompts pass.
type CursorUserPromptsBackfill struct {
	FilesScanned    int `json:"files_scanned"`
	UserEventsFound int `json:"user_events_found"`
	ActionsInserted int `json:"actions_inserted"`
}

// backfillCursorUserPrompts walks every cursor agent-transcripts JSONL
// file under projectsDir and ingests user_prompt action rows for the
// turn-opening user lines. The cursor adapter's live path emits user
// prompts via the beforeSubmitPrompt hook; this catches sessions that
// pre-date the hook installation (or sessions where the hook fires for
// some prompts but not others).
//
// Each user line's text is unwrapped via stripUserQueryWrapper so the
// row carries the user-typed prompt rather than the
// `<user_query>...</user_query>` envelope cursor's runtime injects.
//
// Idempotent via the (source_file, source_event_id) UNIQUE index —
// re-running over a session that already has user_prompt rows is a
// no-op. Skips subagents/*.jsonl explicitly; those are handled in
// the parallel --cursor-subagents pass.
func backfillCursorUserPrompts(ctx context.Context, db *sql.DB, projectsDir string, fileLimit int) (CursorUserPromptsBackfill, error) {
	res := CursorUserPromptsBackfill{}
	st := store.New(db)

	type turnRef struct {
		MessageID string
		Timestamp string
	}

	walkErr := filepath.WalkDir(projectsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".jsonl" || strings.Contains(path, string(filepath.Separator)+"subagents"+string(filepath.Separator)) {
			return nil
		}
		sessionID := filepath.Base(filepath.Dir(path))
		if strings.TrimSuffix(filepath.Base(path), ".jsonl") != sessionID {
			return nil
		}
		if !strings.Contains(path, string(filepath.Separator)+"agent-transcripts"+string(filepath.Separator)) {
			return nil
		}
		if fileLimit > 0 && res.FilesScanned >= fileLimit {
			return fs.SkipAll
		}
		res.FilesScanned++

		var projectRoot string
		_ = db.QueryRowContext(ctx, `
			SELECT COALESCE(p.root_path, '')
			  FROM sessions s
			  LEFT JOIN projects p ON p.id = s.project_id
			 WHERE s.id = ?`, sessionID).Scan(&projectRoot)
		if projectRoot == "" {
			return nil
		}

		rows, err := db.QueryContext(ctx, `
			SELECT message_id, timestamp
			  FROM token_usage
			 WHERE tool = 'cursor' AND session_id = ? AND message_id IS NOT NULL AND message_id != ''
			 ORDER BY timestamp ASC, id ASC`, sessionID)
		if err != nil {
			return err
		}
		var refs []turnRef
		for rows.Next() {
			var ref turnRef
			if err := rows.Scan(&ref.MessageID, &ref.Timestamp); err != nil {
				rows.Close()
				return err
			}
			refs = append(refs, ref)
		}
		rows.Close()
		if len(refs) == 0 {
			return nil
		}

		turns, err := cursor.ParseTranscriptTurns(path)
		if err != nil {
			return nil
		}
		n := len(turns)
		if len(refs) < n {
			n = len(refs)
		}
		var events []models.ToolEvent
		for i := 0; i < n; i++ {
			ts, _ := time.Parse(time.RFC3339Nano, refs[i].Timestamp)
			ev, ok := cursor.BuildTranscriptUserPromptEvent(turns[i], sessionID, projectRoot, refs[i].MessageID, path, ts, nil)
			if !ok {
				continue
			}
			events = append(events, ev)
		}
		res.UserEventsFound += len(events)
		if len(events) == 0 {
			return nil
		}
		ingestRes, err := st.Ingest(ctx, events, nil, store.IngestOptions{})
		if err != nil {
			return fmt.Errorf("backfill cursor-user-prompts: ingest: %w", err)
		}
		res.ActionsInserted += ingestRes.ActionsInserted
		return nil
	})
	if walkErr != nil && walkErr != fs.SkipAll {
		return res, fmt.Errorf("backfill cursor-user-prompts: walk %s: %w", projectsDir, walkErr)
	}
	return res, nil
}

// CursorSubagentsBackfill summarises the --cursor-subagents pass.
type CursorSubagentsBackfill struct {
	FilesScanned    int `json:"files_scanned"`
	EventsBuilt     int `json:"events_built"`
	ActionsInserted int `json:"actions_inserted"`
}

// backfillCursorSubagents walks the
// agent-transcripts/<parent_uuid>/subagents/<sub_uuid>.jsonl files
// Cursor writes when the parent agent spawns a sub-agent. The current
// cursor backfill explicitly skips these (the parent transcript only
// records a `Subagent` tool_use; the sub-agent's actual work is
// in the sub-file). This pass ingests those nested transcripts as
// sidechain rows under the parent session (IsSidechain=true,
// SessionID = parent_uuid), mirroring claudecode's isSidechain
// semantics.
//
// Each sub-agent line is timestamped from the file's mtime (the
// sub transcript carries no per-line timestamps). MessageID is
// synthesized as "sub:<sub_uuid>:turn<N>" so rows from the same
// sub-agent thread group together; SourceEventID prefixes ensure
// idempotency on re-runs.
func backfillCursorSubagents(ctx context.Context, db *sql.DB, projectsDir string, fileLimit int) (CursorSubagentsBackfill, error) {
	res := CursorSubagentsBackfill{}
	st := store.New(db)

	walkErr := filepath.WalkDir(projectsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".jsonl" {
			return nil
		}
		sep := string(filepath.Separator)
		if !strings.Contains(path, sep+"subagents"+sep) || !strings.Contains(path, sep+"agent-transcripts"+sep) {
			return nil
		}
		if fileLimit > 0 && res.FilesScanned >= fileLimit {
			return fs.SkipAll
		}
		res.FilesScanned++

		subUUID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		// path = .../agent-transcripts/<parent_uuid>/subagents/<sub>.jsonl
		// dir(path) = .../subagents
		// dir(dir(path)) = .../<parent_uuid>
		parentUUID := filepath.Base(filepath.Dir(filepath.Dir(path)))

		var projectRoot string
		_ = db.QueryRowContext(ctx, `
			SELECT COALESCE(p.root_path, '')
			  FROM sessions s
			  LEFT JOIN projects p ON p.id = s.project_id
			 WHERE s.id = ?`, parentUUID).Scan(&projectRoot)
		if projectRoot == "" {
			return nil
		}

		info, statErr := os.Stat(path)
		if statErr != nil {
			return nil
		}
		ts := info.ModTime().UTC()

		turns, err := cursor.ParseTranscriptTurns(path)
		if err != nil {
			return nil
		}

		var events []models.ToolEvent
		for i, turn := range turns {
			generationID := fmt.Sprintf("sub:%s:turn%d", subUUID, i)
			if ev, ok := cursor.BuildTranscriptUserPromptEvent(turn, parentUUID, projectRoot, generationID, path, ts, nil); ok {
				ev.IsSidechain = true
				events = append(events, ev)
			}
			toolEvs := cursor.BuildTranscriptToolEvents(turn, parentUUID, projectRoot, generationID, path, ts, nil)
			for _, te := range toolEvs {
				te.IsSidechain = true
				events = append(events, te)
			}
		}
		res.EventsBuilt += len(events)
		if len(events) == 0 {
			return nil
		}
		ingestRes, err := st.Ingest(ctx, events, nil, store.IngestOptions{})
		if err != nil {
			return fmt.Errorf("backfill cursor-subagents: ingest: %w", err)
		}
		res.ActionsInserted += ingestRes.ActionsInserted
		return nil
	})
	if walkErr != nil && walkErr != fs.SkipAll {
		return res, fmt.Errorf("backfill cursor-subagents: walk %s: %w", projectsDir, walkErr)
	}
	return res, nil
}

func backfillCursorTranscriptActions(ctx context.Context, db *sql.DB, projectsDir string, fileLimit int) (MessageIDBackfill, error) {
	res := MessageIDBackfill{}
	st := store.New(db)

	type turnRef struct {
		MessageID string
		Timestamp string
	}

	walkErr := filepath.WalkDir(projectsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".jsonl" || strings.Contains(path, string(filepath.Separator)+"subagents"+string(filepath.Separator)) {
			return nil
		}
		sessionID := filepath.Base(filepath.Dir(path))
		if strings.TrimSuffix(filepath.Base(path), ".jsonl") != sessionID {
			return nil
		}
		if !strings.Contains(path, string(filepath.Separator)+"agent-transcripts"+string(filepath.Separator)) {
			return nil
		}
		if fileLimit > 0 && res.FilesScanned >= fileLimit {
			return fs.SkipAll
		}
		res.FilesScanned++

		var projectRoot string
		_ = db.QueryRowContext(ctx, `
			SELECT COALESCE(p.root_path, '')
			  FROM sessions s
			  LEFT JOIN projects p ON p.id = s.project_id
			 WHERE s.id = ?`, sessionID).Scan(&projectRoot)
		if projectRoot == "" {
			return nil
		}

		rows, err := db.QueryContext(ctx, `
			SELECT message_id, timestamp
			  FROM token_usage
			 WHERE tool = 'cursor' AND session_id = ? AND message_id IS NOT NULL AND message_id != ''
			 ORDER BY timestamp ASC, id ASC`, sessionID)
		if err != nil {
			return err
		}
		var refs []turnRef
		for rows.Next() {
			var ref turnRef
			if err := rows.Scan(&ref.MessageID, &ref.Timestamp); err != nil {
				rows.Close()
				return err
			}
			refs = append(refs, ref)
		}
		rows.Close()
		if len(refs) == 0 {
			return nil
		}

		turns, err := cursor.ParseTranscriptTurns(path)
		if err != nil {
			return nil
		}
		n := len(turns)
		if len(refs) < n {
			n = len(refs)
		}
		for i := 0; i < n; i++ {
			ts, _ := time.Parse(time.RFC3339Nano, refs[i].Timestamp)
			events := cursor.BuildTranscriptToolEvents(turns[i], sessionID, projectRoot, refs[i].MessageID, path, ts, nil)
			res.LinesExamined += len(events)
			ingestRes, err := st.Ingest(ctx, events, nil, store.IngestOptions{})
			if err != nil {
				return err
			}
			res.ActionsUpdated += ingestRes.ActionsInserted
		}
		return nil
	})
	if walkErr != nil && walkErr != fs.SkipAll {
		return res, fmt.Errorf("backfill cursor transcript actions: walk: %w", walkErr)
	}
	return res, nil
}
