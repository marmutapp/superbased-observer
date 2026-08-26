package aider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/contentcap"
	"github.com/marmutapp/superbased-observer/internal/models"
)

// sessionHeaderRe matches an Aider session delimiter, e.g.
// `# aider chat started at 2026-07-09 04:37:13`. The trailing capture is
// the local-time start timestamp.
var sessionHeaderRe = regexp.MustCompile(`^# aider chat started at (.+?)\s*$`)

// tokenLineRe matches Aider's prose usage line (blockquote prefix already
// stripped), e.g.
//
//	Tokens: 11k sent, 21 received. Cost: $0.03 message, $0.03 session.
//	Tokens: 9.4k sent, 1.2k cache write, 512 cache hit, 328 received.
//
// Groups: 1=sent, 2=cache write (opt), 3=cache hit (opt), 4=received,
// 5=message cost (opt), 6=session cost (opt). The Cost clause is absent
// when the model carries no pricing (base_coder.py returns early).
var tokenLineRe = regexp.MustCompile(
	`^Tokens:\s+([0-9.]+k?)\s+sent` +
		`(?:,\s+([0-9.]+k?)\s+cache write)?` +
		`(?:,\s+([0-9.]+k?)\s+cache hit)?` +
		`,\s+([0-9.]+k?)\s+received\.` +
		`(?:\s+Cost:\s+\$([0-9.]+)\s+message,\s+\$([0-9.]+)\s+session\.)?`,
)

// mainModelRe / usingModelRe extract the session model from the startup
// tool-output banner.
var (
	mainModelRe  = regexp.MustCompile(`^Main model:\s+(\S+)`)
	usingModelRe = regexp.MustCompile(`^Using\s+(\S+)\s+model`)
)

// aiderTimeLayout is the layout of the session-header timestamp. Aider
// writes local time.
const aiderTimeLayout = "2006-01-02 15:04:05"

// session holds the running state while walking one aider session inside a
// transcript file.
type session struct {
	key   string
	start time.Time
	model string
	seq   int // monotonic per-session event sequence (deterministic)
}

// parser accumulates events while scanning a transcript top to bottom.
type parser struct {
	a           *Adapter
	sourceFile  string
	projectRoot string
	gitRemote   string

	sess *session
	ord  int // session ordinal within the file (0-based)

	userLines []string // pending `#### ` prompt lines
	asstLines []string // pending assistant prose lines

	tools  []models.ToolEvent
	tokens []models.TokenEvent
}

// parseTranscript is the entry point: it walks the whole Markdown file and
// returns the normalized events. ctx is honored for cancellation between
// lines.
func (a *Adapter) parseTranscript(ctx context.Context, data []byte, sourceFile, projectRoot, gitRemote string) ([]models.ToolEvent, []models.TokenEvent) {
	p := &parser{a: a, sourceFile: sourceFile, projectRoot: projectRoot, gitRemote: gitRemote}

	// Normalize CRLF; split into lines without a trailing-empty artifact.
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		select {
		case <-ctx.Done():
			p.flushAssistant()
			p.flushUser()
			return p.tools, p.tokens
		default:
		}
		p.handleLine(line)
	}
	p.flushAssistant()
	p.flushUser()
	return p.tools, p.tokens
}

// handleLine classifies one transcript line and updates parser state.
func (p *parser) handleLine(line string) {
	if m := sessionHeaderRe.FindStringSubmatch(line); m != nil {
		p.flushAssistant()
		p.flushUser()
		p.startSession(m[1])
		return
	}
	if strings.HasPrefix(line, "#### ") || line == "####" {
		// A new (or continuing) user prompt line ends any assistant prose.
		p.flushAssistant()
		p.userLines = append(p.userLines, strings.TrimPrefix(strings.TrimPrefix(line, "####"), " "))
		return
	}
	if strings.HasPrefix(line, ">") {
		// A tool-output/meta line ends a pending user prompt.
		p.flushUser()
		p.handleQuoteLine(strings.TrimSpace(strings.TrimPrefix(line, ">")))
		return
	}
	// Plain prose → assistant text. Blank lines are kept so internal
	// paragraph breaks survive; leading/trailing blanks are trimmed at
	// flush time.
	p.asstLines = append(p.asstLines, line)
}

// handleQuoteLine processes the body of a blockquote (`> `) line — the
// content after the `> ` prefix has been stripped.
func (p *parser) handleQuoteLine(body string) {
	if body == "" {
		return
	}
	if p.sess == nil {
		// A stray tool-output line before any session header — start an
		// implicit session so nothing is dropped.
		p.startSession("")
	}

	if m := tokenLineRe.FindStringSubmatch(body); m != nil {
		// The usage line closes a turn: assistant prose precedes it.
		p.flushAssistant()
		p.emitToken(m)
		return
	}
	if rest, ok := trimPrefixFold(body, "Applied edit to "); ok {
		p.flushAssistant()
		p.emitFileOp(models.ActionEditFile, "aider.apply_edit", rest)
		return
	}
	if rest, ok := trimPrefixFold(body, "Running "); ok {
		p.flushAssistant()
		p.emitFileOp(models.ActionRunCommand, "aider.run_command", rest)
		return
	}
	// Model / version banner lines: capture the model, emit nothing.
	if m := mainModelRe.FindStringSubmatch(body); m != nil {
		p.sess.model = m[1]
		return
	}
	if p.sess.model == "" {
		if m := usingModelRe.FindStringSubmatch(body); m != nil {
			p.sess.model = m[1]
		}
	}
	// Everything else (repo-map notices, argv echo, gitignore prompt, the
	// "Add file to the chat?" prompt, commit lines, …) is informational.
}

// startSession finalizes any in-flight buffers and begins a new session.
func (p *parser) startSession(rawTS string) {
	start := time.Time{}
	if rawTS != "" {
		if t, err := time.ParseInLocation(aiderTimeLayout, strings.TrimSpace(rawTS), time.Local); err == nil {
			start = t
		}
	}
	key := fmt.Sprintf("aider-%s-%02d", shortHash(p.sourceFile), p.ord)
	p.ord++
	p.sess = &session{key: key, start: start}
}

// flushUser emits a user_prompt event for the pending `#### ` lines.
func (p *parser) flushUser() {
	if len(p.userLines) == 0 {
		return
	}
	body := strings.TrimSpace(strings.Join(p.userLines, "\n"))
	p.userLines = p.userLines[:0]
	if body == "" || p.sess == nil {
		return
	}
	preview := p.a.scrub(truncate(body, 500))
	p.tools = append(p.tools, models.ToolEvent{
		SourceFile:    p.sourceFile,
		SourceEventID: p.nextID("prompt"),
		SessionID:     p.sess.key,
		ProjectRoot:   p.projectRoot,
		GitRemote:     p.gitRemote,
		Timestamp:     p.sess.start,
		Tool:          models.ToolAider,
		Model:         p.sess.model,
		ActionType:    models.ActionUserPrompt,
		Target:        truncate(preview, 200),
		Success:       true,
		RawToolName:   "aider.user_prompt",
	})
}

// flushAssistant emits an assistant-text (task_complete) event for the
// pending prose lines.
func (p *parser) flushAssistant() {
	if len(p.asstLines) == 0 {
		return
	}
	body := strings.TrimSpace(strings.Join(p.asstLines, "\n"))
	p.asstLines = p.asstLines[:0]
	if body == "" || p.sess == nil {
		return
	}
	preview := p.a.scrub(truncate(body, 200))
	output := p.a.scrub(contentcap.Cap(body, contentcap.DefaultMaxBytes))
	p.tools = append(p.tools, models.ToolEvent{
		SourceFile:    p.sourceFile,
		SourceEventID: p.nextID("text"),
		SessionID:     p.sess.key,
		ProjectRoot:   p.projectRoot,
		GitRemote:     p.gitRemote,
		Timestamp:     p.sess.start,
		Tool:          models.ToolAider,
		Model:         p.sess.model,
		// flushAssistant fires at seven distinct prose boundaries, not
		// only at end-of-turn, so every row here is per-message
		// assistant text rather than a turn terminus.
		ActionType:  models.ActionAssistantMessage,
		Target:      preview,
		Success:     true,
		RawToolName: "aider.assistant_text",
		ToolOutput:  output,
	})
}

// emitFileOp emits an edit_file / run_command event derived from an aider
// tool-output marker line.
func (p *parser) emitFileOp(actionType, rawName, target string) {
	if p.sess == nil {
		return
	}
	target = strings.TrimSpace(target)
	p.tools = append(p.tools, models.ToolEvent{
		SourceFile:    p.sourceFile,
		SourceEventID: p.nextID(actionType),
		SessionID:     p.sess.key,
		ProjectRoot:   p.projectRoot,
		GitRemote:     p.gitRemote,
		Timestamp:     p.sess.start,
		Tool:          models.ToolAider,
		Model:         p.sess.model,
		ActionType:    actionType,
		Target:        p.a.scrub(truncate(target, 200)),
		Success:       true,
		RawToolName:   rawName,
		RawToolInput:  p.a.scrub(truncate(target, 500)),
	})
}

// emitToken emits a per-message TokenEvent from a matched usage line.
// Aider's `sent` count is GROSS (includes the cache hit), so InputTokens
// is netted. All counts are aider's format_tokens-rounded prose values →
// reliability=unreliable.
func (p *parser) emitToken(m []string) {
	if p.sess == nil {
		return
	}
	sent := parseTokenCount(m[1])
	cacheWrite := parseTokenCount(m[2])
	cacheHit := parseTokenCount(m[3])
	received := parseTokenCount(m[4])

	netInput := sent - cacheHit
	if netInput < 0 {
		netInput = 0
	}

	var cost float64
	if m[5] != "" {
		cost, _ = strconv.ParseFloat(m[5], 64)
	}

	p.tokens = append(p.tokens, models.TokenEvent{
		SourceFile:          p.sourceFile,
		SourceEventID:       p.nextID("tokens"),
		SessionID:           p.sess.key,
		ProjectRoot:         p.projectRoot,
		GitRemote:           p.gitRemote,
		Timestamp:           p.sess.start,
		Tool:                models.ToolAider,
		Model:               p.sess.model,
		InputTokens:         netInput,
		OutputTokens:        received,
		CacheReadTokens:     cacheHit,
		CacheCreationTokens: cacheWrite,
		EstimatedCostUSD:    cost,
		Source:              models.TokenSourceJSONL,
		Reliability:         models.ReliabilityUnreliable,
	})
}

// nextID returns a deterministic per-session event id of the form
// `<kind>:<sessionKey>:<seq>`. The sequence is stable across re-parses
// because parsing always starts from the top of the file.
func (p *parser) nextID(kind string) string {
	id := fmt.Sprintf("%s:%s:%d", kind, p.sess.key, p.sess.seq)
	p.sess.seq++
	return id
}

// parseTokenCount reverses aider's format_tokens: a plain integer stays as
// is; a `k` suffix multiplies by 1000 (with any decimal). The result is an
// approximation — format_tokens rounds, so the original count is lost.
func parseTokenCount(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if strings.HasSuffix(s, "k") {
		v, err := strconv.ParseFloat(strings.TrimSuffix(s, "k"), 64)
		if err != nil {
			return 0
		}
		return int64(math.Round(v * 1000))
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		// Tolerate a decimal without the k suffix, just in case.
		f, ferr := strconv.ParseFloat(s, 64)
		if ferr != nil {
			return 0
		}
		return int64(math.Round(f))
	}
	return v
}

// trimPrefixFold reports whether body starts with prefix
// (case-insensitively) and returns the remainder.
func trimPrefixFold(body, prefix string) (string, bool) {
	if len(body) < len(prefix) {
		return "", false
	}
	if !strings.EqualFold(body[:len(prefix)], prefix) {
		return "", false
	}
	return body[len(prefix):], true
}

// shortHash returns the first 12 hex chars of the sha256 of s — a stable,
// path-derived session-key seed.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:6])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
