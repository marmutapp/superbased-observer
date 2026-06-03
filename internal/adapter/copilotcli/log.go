package copilotcli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/adapter"
	"github.com/marmutapp/superbased-observer/internal/git"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
)

// process-*.log line shapes (verified empirically on both WSL and
// Windows-side log captures, 2026-05-17):
//
//	2026-05-17T09:28:27.900Z [INFO] Workspace initialized: <uuid> (checkpoints: 0)
//	2026-05-17T09:28:29.893Z [INFO] Using default model: gpt-5-mini
//	2026-05-17T09:28:38.632Z [INFO] CompactionProcessor: Utilization 14.9% (19052/128000 tokens) below threshold 80%
//	2026-05-17T10:08:24.316Z [DEBUG] response (Request-ID 00000-…):
//	2026-05-17T10:08:24.316Z [DEBUG] data:
//	2026-05-17T10:08:24.316Z [DEBUG] {
//	  "id": "...",
//	  "choices": [...],
//	  "usage": {
//	    "completion_tokens": 565,
//	    "prompt_tokens": 15474,
//	    "total_tokens": 16039,
//	    "prompt_tokens_details": {
//	      "cached_tokens": 2560
//	    },
//	    "completion_tokens_details": {
//	      "reasoning_tokens": 448
//	    }
//	  }
//	}
//
// Continuation lines (inside the multi-line JSON dump) DO NOT carry
// the timestamp/level prefix — they're raw JSON formatting from
// `JSON.stringify(obj, null, 2)`. Brace-counting / line-prefix-match
// is the simplest way to delimit response blocks.

var (
	reWorkspaceInit = regexp.MustCompile(`\[INFO\] Workspace initialized: ([0-9a-f-]{36})`)
	reDefaultModel  = regexp.MustCompile(`\[INFO\] Using default model: (\S+)`)
	reUtilization   = regexp.MustCompile(`\[INFO\] CompactionProcessor: Utilization [\d.]+% \((\d+)/(\d+) tokens\)`)
	// reResponseHdr matches the `[DEBUG] response (Request-ID …):`
	// header that opens each Tier-1 usage block. Empirically Copilot
	// CLI emits Request-IDs in TWO distinct formats interleaved in the
	// same session (operator sample, 2026-05-18):
	//   - `00000-<uuid>` (lowercase-hex-uuid; 8.1% of asst.message rids)
	//   - `<HEX>:<HEX>:<HEX>:<HEX>:<HEX>` (uppercase-hex with colons;
	//     91.9% of asst.message rids — the dominant production format)
	// The original v1.6.6 regex `[0-9a-f-]+` only captured the uuid
	// shape, silently dropping Tier-1 coverage for the hex-colon format.
	// The permissive `[^\s)]+` captures anything up to whitespace or
	// the closing paren — the format-agnostic shape. See
	// docs/copilot-cli-audit-2026-05-18.md §B3.
	reResponseHdr   = regexp.MustCompile(`\[DEBUG\] response \(Request-ID ([^\s)]+)\):`)
	rePromptTokens  = regexp.MustCompile(`^\s*"prompt_tokens":\s*(\d+)`)
	reCompletionTok = regexp.MustCompile(`^\s*"completion_tokens":\s*(\d+)`)
	reTotalTokens   = regexp.MustCompile(`^\s*"total_tokens":\s*(\d+)`)
	reCachedTokens  = regexp.MustCompile(`^\s*"cached_tokens":\s*(\d+)`)
	reReasoningTok  = regexp.MustCompile(`^\s*"reasoning_tokens":\s*(\d+)`)
	reTimestampLine = regexp.MustCompile(`^20\d{2}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}`)
)

// logParserState carries log-level context across lines.
type logParserState struct {
	sessionID          string
	model              string
	pendingUtilization int64 // last seen prompt-side utilization (Tier 2 estimate)
	currentRequestID   string
	currentUsage       usageAccum
	inResponseBlock    bool
	// Pre-block tracking: when we encounter `response (Request-ID …):`
	// we may not see the usage immediately — the JSON dump can be very
	// large (~10 KB of encrypted blob). Brace-counting tells us when
	// we're inside the response JSON.
	braceDepth int
	// requestSeq counts response blocks per Request-ID so SourceEventID
	// stays unique across the N response blocks that share one
	// Request-ID (one WebSocket connection can carry multiple
	// response.create calls before closing).
	requestSeq map[string]int
	// seenDebugResponseHeader is set the first time a
	// `[DEBUG] response (Request-ID …):` line fires. When true at
	// end-of-parse, debug logging is on and Tier 1 (full usage)
	// covers every request — pendingTier2 is dropped to avoid
	// double-counting. When false (INFO-only logging), Tier 1 can't
	// fire and pendingTier2 is appended to the result so InputTokens
	// is captured via the utilization-snapshot estimate.
	seenDebugResponseHeader bool
	// pendingTier2 buffers utilization-derived TokenEvents that will
	// only be flushed if !seenDebugResponseHeader at end-of-parse.
	pendingTier2 []models.TokenEvent
}

// usageAccum holds in-flight usage extraction for the current response.
type usageAccum struct {
	prompt     int64
	completion int64
	total      int64
	cached     int64
	reasoning  int64
	ts         time.Time
}

func (u usageAccum) hasAny() bool {
	return u.prompt > 0 || u.completion > 0 || u.cached > 0 || u.reasoning > 0
}

// parseProcessLog scans a Copilot CLI per-process log file and emits
// per-Request-ID TokenEvents. Always re-scans from byte 0 to rebuild
// session-id + model context; only emits events whose source position
// is at or past fromOffset (downstream dedups by SourceEventID).
//
// Tier 1: parse `[DEBUG] response (Request-ID …):` blocks → full
// usage breakdown.
// Tier 2: derive input-token estimates from
// `CompactionProcessor: Utilization X% (CTX/128000 tokens)` lines when
// no debug-level usage block is present for a given Request-ID. (Tier 2
// is best-effort; deferred to a follow-up if accuracy turns out
// problematic — Tier 1 is the load-bearing case.)
func (a *Adapter) parseProcessLog(_ context.Context, path string, fromOffset int64) (adapter.ParseResult, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return adapter.ParseResult{NewOffset: fromOffset}, nil
		}
		return adapter.ParseResult{}, fmt.Errorf("copilotcli.parseProcessLog open %q: %w", path, err)
	}
	defer f.Close()

	var (
		out    adapter.ParseResult
		offset int64
	)
	st := &logParserState{requestSeq: map[string]int{}}
	br := bufio.NewReader(f)
	for {
		line, readErr := br.ReadString('\n')
		hasTerminator := readErr == nil
		lineStart := offset
		offset += int64(len(line))
		clean := strings.TrimRight(line, "\r\n")
		if clean != "" {
			processLogLine(st, clean, parseLineTimestamp(clean), path, lineStart, fromOffset, &out)
		}

		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if !hasTerminator && len(line) > 0 {
					offset = lineStart
				}
				break
			}
			return adapter.ParseResult{}, fmt.Errorf("copilotcli.parseProcessLog read %q: %w", path, readErr)
		}
	}

	// If we ended in-block with a pending usage, flush it.
	if st.inResponseBlock && st.currentUsage.hasAny() && st.currentRequestID != "" {
		emitTokenEvent(st, path, &out)
	}

	// Tier 2 flush: only when --log-level is NOT debug (no [DEBUG]
	// response (Request-ID …) line fired in this file). Otherwise
	// Tier 1 covered every request via the upstream usage block and
	// the buffered Tier 2 estimates would double-count. The Utilization
	// line fires once per outgoing request and its raw value is the
	// gross prompt size — emit one TokenEvent per sample as the input
	// estimate. Cache split is unknown (estimate over-charges the
	// cached portion) → reliability='approximate' at ~5%. The
	// matching Tier 3 (events.jsonl) row provides OutputTokens with
	// its own MessageID; the two rows are joined at the rollup layer
	// rather than at the TokenEvent level.
	if !st.seenDebugResponseHeader && len(st.pendingTier2) > 0 {
		out.TokenEvents = append(out.TokenEvents, st.pendingTier2...)
	}

	// Derive ProjectRoot + GitBranch by reading the sibling
	// workspace.yaml. The log file lives at
	// `<home>/.copilot/logs/process-*.log`; the corresponding session
	// state is at `<home>/.copilot/session-state/<sessionID>/workspace.yaml`.
	// The store.Ingest layer requires a non-empty ProjectRoot to insert
	// TokenEvents for a session it hasn't seen in the same batch, so
	// without this the log-parser-only path silently drops every
	// upserted token row (Tier 1 then never lands in DB).
	projectRoot, branch := resolveProjectFromSibling(path, st.sessionID)

	// Set SessionID / Model / ProjectRoot / GitBranch on emitted events.
	for i := range out.TokenEvents {
		if out.TokenEvents[i].SessionID == "" {
			out.TokenEvents[i].SessionID = st.sessionID
		}
		if out.TokenEvents[i].Model == "" {
			out.TokenEvents[i].Model = st.model
		}
		if out.TokenEvents[i].ProjectRoot == "" {
			out.TokenEvents[i].ProjectRoot = projectRoot
		}
		if out.TokenEvents[i].GitBranch == "" {
			out.TokenEvents[i].GitBranch = branch
		}
	}

	out.NewOffset = offset
	return out, nil
}

// resolveProjectFromSibling reads the sibling workspace.yaml for a
// log file and returns (projectRoot, branch).
//
// The log file lives at `<home>/.copilot/logs/process-*.log`; the
// session-state dir is at `<home>/.copilot/session-state/<sessionID>/`.
func resolveProjectFromSibling(logPath, sessionID string) (string, string) {
	if sessionID == "" {
		return "", ""
	}
	// <home>/.copilot/logs/...log → <home>/.copilot
	copilotRoot := filepath.Dir(filepath.Dir(logPath))
	yamlPath := filepath.Join(copilotRoot, "session-state", sessionID, "workspace.yaml")
	return resolveProjectFromWorkspaceYAML(yamlPath)
}

// resolveProjectFromWorkspaceYAML reads a Copilot CLI workspace.yaml
// at the given path and returns (projectRoot, branch). Returns ("", "")
// when the file is missing or carries no usable git_root/cwd. Path
// candidates are translated through crossmount.TranslateForeignPath
// for WSL2 ↔ Windows session capture, then resolved through
// git.Resolve to find the actual repo root.
//
// workspace.yaml carries `cwd`, `git_root`, `branch` as simple
// `key: value` lines — flat enough to parse without a YAML lib.
//
// Used by both parseProcessLog and parseEventsJSONL: the events.jsonl
// path needs this because some Copilot CLI sessions log a drive-root
// cwd (e.g. "E:\\") in session.start.context, while workspace.yaml
// carries the actual repo root.
func resolveProjectFromWorkspaceYAML(yamlPath string) (string, string) {
	f, err := os.Open(yamlPath)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	var cwd, gitRoot, branch string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		k, v, ok := splitYAMLLine(line)
		if !ok {
			continue
		}
		switch k {
		case "cwd":
			cwd = v
		case "git_root":
			gitRoot = v
		case "branch":
			branch = v
		}
	}
	candidate := gitRoot
	if candidate == "" {
		candidate = cwd
	}
	if candidate == "" {
		return "", branch
	}
	translated := crossmount.TranslateForeignPath(candidate)
	if translated == "" {
		translated = candidate
	}
	info, err := git.Resolve(translated)
	if err == nil && info.Root != "" {
		if info.Branch != "" && branch == "" {
			branch = info.Branch
		}
		return info.Root, branch
	}
	return translated, branch
}

func splitYAMLLine(line string) (key, value string, ok bool) {
	idx := strings.Index(line, ":")
	if idx <= 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:idx])
	value = strings.TrimSpace(line[idx+1:])
	value = strings.Trim(value, `'"`)
	return key, value, true
}

func processLogLine(st *logParserState, line string, ts time.Time, path string, lineStart, fromOffset int64, out *adapter.ParseResult) {
	// Session identity / model — always update state regardless of
	// fromOffset so subsequent emits have correct context.
	if m := reWorkspaceInit.FindStringSubmatch(line); m != nil {
		st.sessionID = m[1]
	}
	if m := reDefaultModel.FindStringSubmatch(line); m != nil && st.model == "" {
		st.model = m[1]
	}
	if m := reUtilization.FindStringSubmatch(line); m != nil {
		v, _ := strconv.ParseInt(m[1], 10, 64)
		st.pendingUtilization = v
		// Buffer a Tier 2 candidate. Whether it lands depends on
		// st.seenDebugResponseHeader at end-of-parse — see the
		// pendingTier2 flush in parseProcessLog.
		if v > 0 && lineStart >= fromOffset {
			st.pendingTier2 = append(st.pendingTier2, models.TokenEvent{
				SourceFile:    path,
				SourceEventID: fmt.Sprintf("logdelta:%d", lineStart),
				Timestamp:     ts,
				Tool:          models.ToolCopilotCLI,
				Model:         st.model,
				InputTokens:   v,
				Source:        models.TokenSourceLogDelta,
				Reliability:   models.ReliabilityApproximate,
			})
		}
	}

	// Response-block detection.
	if m := reResponseHdr.FindStringSubmatch(line); m != nil {
		st.seenDebugResponseHeader = true
		// New response — close any prior unfinished one (defensive).
		if st.inResponseBlock && st.currentUsage.hasAny() && st.currentRequestID != "" && lineStart >= fromOffset {
			emitTokenEvent(st, path, out)
		}
		st.currentRequestID = m[1]
		st.currentUsage = usageAccum{ts: ts}
		st.inResponseBlock = true
		st.braceDepth = 0
		return
	}

	// In-block tracking — brace count + extract usage fields.
	if !st.inResponseBlock {
		return
	}

	// New timestamped log line that's NOT inside the JSON dump signals
	// end of the response block. (DEBUG-level continuation lines are
	// the raw JSON without the timestamp prefix.)
	if reTimestampLine.MatchString(line) && !strings.Contains(line, "[DEBUG]") {
		// Block ended.
		if st.currentUsage.hasAny() && st.currentRequestID != "" && lineStart >= fromOffset {
			emitTokenEvent(st, path, out)
		}
		st.inResponseBlock = false
		st.currentRequestID = ""
		st.currentUsage = usageAccum{}
		return
	}

	// Track brace depth for end-of-block detection on closing }.
	for _, r := range line {
		switch r {
		case '{':
			st.braceDepth++
		case '}':
			st.braceDepth--
		}
	}

	// Extract usage fields.
	if m := rePromptTokens.FindStringSubmatch(line); m != nil {
		v, _ := strconv.ParseInt(m[1], 10, 64)
		st.currentUsage.prompt = v
	}
	if m := reCompletionTok.FindStringSubmatch(line); m != nil {
		v, _ := strconv.ParseInt(m[1], 10, 64)
		st.currentUsage.completion = v
	}
	if m := reTotalTokens.FindStringSubmatch(line); m != nil {
		v, _ := strconv.ParseInt(m[1], 10, 64)
		st.currentUsage.total = v
	}
	if m := reCachedTokens.FindStringSubmatch(line); m != nil {
		v, _ := strconv.ParseInt(m[1], 10, 64)
		st.currentUsage.cached = v
	}
	if m := reReasoningTok.FindStringSubmatch(line); m != nil {
		v, _ := strconv.ParseInt(m[1], 10, 64)
		st.currentUsage.reasoning = v
	}

	// braceDepth back to 0 after we've seen content — block done.
	if st.braceDepth <= 0 && st.currentUsage.hasAny() {
		if st.currentRequestID != "" && lineStart >= fromOffset {
			emitTokenEvent(st, path, out)
		}
		st.inResponseBlock = false
		st.currentRequestID = ""
		st.currentUsage = usageAccum{}
		st.braceDepth = 0
	}
}

func emitTokenEvent(st *logParserState, path string, out *adapter.ParseResult) {
	// One Request-ID can produce N response blocks (WebSocket
	// connection serves multiple `response.create` calls). The seq
	// disambiguates so all N rows land instead of upserting onto each
	// other.
	st.requestSeq[st.currentRequestID]++
	seq := st.requestSeq[st.currentRequestID]
	// Net non-cached input. Copilot CLI's debug log captures the
	// upstream API's `prompt_tokens` and `prompt_tokens_details.
	// cached_tokens` (OpenAI convention) — `prompt_tokens` is the
	// TOTAL prompt INCLUDING cached, `cached_tokens` is the subset.
	// The cost engine's TokenBundle.Input contract is NET non-cached
	// (Anthropic shape — see internal/intelligence/cost/engine.go),
	// so we net at emit time. Without this, the cached portion is
	// billed at BOTH the full input rate AND the cache_read rate
	// (~3-4× over-bill on cached sessions; cost-engine audit
	// 2026-05-24). Clamp at 0 against an upstream anomaly where
	// `cached` exceeds `prompt` (would indicate a parser regression).
	netInput := st.currentUsage.prompt - st.currentUsage.cached
	if netInput < 0 {
		netInput = 0
	}
	out.TokenEvents = append(out.TokenEvents, models.TokenEvent{
		SourceFile:      path,
		SourceEventID:   fmt.Sprintf("log:%s:%d", st.currentRequestID, seq),
		Timestamp:       st.currentUsage.ts,
		Tool:            models.ToolCopilotCLI,
		Model:           st.model,
		InputTokens:     netInput,
		OutputTokens:    st.currentUsage.completion,
		CacheReadTokens: st.currentUsage.cached,
		ReasoningTokens: st.currentUsage.reasoning,
		Source:          models.TokenSourceOTel, // not strictly OTel — it's the upstream API's own usage block, captured via debug log. Reuse OTel until a more specific source constant lands.
		Reliability:     models.ReliabilityApproximate,
		MessageID:       st.currentRequestID,
	})
}

// parseLineTimestamp pulls the ISO timestamp prefix from a log line.
// Empty time.Time when the line is a JSON continuation (no prefix).
func parseLineTimestamp(line string) time.Time {
	if len(line) < 20 || !reTimestampLine.MatchString(line) {
		return time.Time{}
	}
	// Format is "2026-05-17T10:08:24.316Z [LEVEL] …"
	end := strings.IndexByte(line, ' ')
	if end < 0 {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, line[:end])
	if err != nil {
		return time.Time{}
	}
	return t
}
