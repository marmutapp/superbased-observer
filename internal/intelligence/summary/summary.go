package summary

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/scrub"
)

// Options configures a Summarizer.
type Options struct {
	// DB is the observer database. Required.
	DB *sql.DB
	// APIKey is the Anthropic API key. If empty, os.Getenv("ANTHROPIC_API_KEY")
	// is used. If still empty, Summarize returns an error.
	APIKey string
	// APIKeyEnv overrides the environment variable name for the API key.
	// Defaults to "ANTHROPIC_API_KEY".
	APIKeyEnv string
	// Model is the Claude model to use. Defaults to "claude-haiku-4-5-20251001".
	Model string
	// BaseURL is the Anthropic Messages API base URL. Defaults to
	// "https://api.anthropic.com".
	BaseURL string
	// HTTPClient overrides the default HTTP client (for tests).
	HTTPClient *http.Client
	// Scrubber removes secrets before storage. Defaults to scrub.New().
	Scrubber *scrub.Scrubber
	// MaxTokens caps the summary response length. Defaults to 512.
	MaxTokens int
}

// Summarizer generates and stores session summaries.
type Summarizer struct {
	opts Options
}

// New returns a Summarizer. DB is required.
func New(opts Options) (*Summarizer, error) {
	if opts.DB == nil {
		return nil, errors.New("summary.New: DB is required")
	}
	if opts.Model == "" {
		opts.Model = "claude-haiku-4-5-20251001"
	}
	if opts.BaseURL == "" {
		opts.BaseURL = "https://api.anthropic.com"
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = http.DefaultClient
	}
	if opts.Scrubber == nil {
		opts.Scrubber = scrub.New()
	}
	if opts.MaxTokens <= 0 {
		opts.MaxTokens = 512
	}
	return &Summarizer{opts: opts}, nil
}

// Result summarizes one run.
type Result struct {
	SessionsProcessed int   `json:"sessions_processed"`
	SessionsSkipped   int   `json:"sessions_skipped"`
	Errors            int   `json:"errors"`
	DurationMs        int64 `json:"duration_ms"`
}

// SummarizeAll generates summaries for all sessions that lack one.
func (s *Summarizer) SummarizeAll(ctx context.Context) (Result, error) {
	start := time.Now()
	apiKey := s.resolveAPIKey()
	if apiKey == "" {
		return Result{}, errors.New("summary: no API key (set ANTHROPIC_API_KEY or configure intelligence.api_key_env)")
	}

	rows, err := s.opts.DB.QueryContext(ctx,
		`SELECT s.id, s.tool, s.started_at,
		        (SELECT COUNT(*) FROM actions a WHERE a.session_id = s.id) AS action_count,
		        COALESCE(p.root_path, '')
		 FROM sessions s
		 LEFT JOIN projects p ON p.id = s.project_id
		 WHERE s.summary_md IS NULL
		   AND (SELECT COUNT(*) FROM actions a WHERE a.session_id = s.id) > 0
		 ORDER BY s.started_at DESC
		 LIMIT 50`)
	if err != nil {
		return Result{}, fmt.Errorf("summary: list sessions: %w", err)
	}
	defer rows.Close()

	type sessionMeta struct {
		id, tool, startedAt, project string
		totalActions                 int
	}
	var sessions []sessionMeta
	for rows.Next() {
		var m sessionMeta
		if err := rows.Scan(&m.id, &m.tool, &m.startedAt, &m.totalActions, &m.project); err != nil {
			return Result{}, fmt.Errorf("summary: scan session: %w", err)
		}
		sessions = append(sessions, m)
	}
	if err := rows.Err(); err != nil {
		return Result{}, fmt.Errorf("summary: list rows: %w", err)
	}

	res := Result{}
	for _, m := range sessions {
		if err := ctx.Err(); err != nil {
			break
		}
		digest, err := s.buildDigest(ctx, m.id, m.tool, m.startedAt, m.project, m.totalActions)
		if err != nil {
			res.Errors++
			continue
		}
		if digest == "" {
			res.SessionsSkipped++
			continue
		}
		md, err := s.callClaude(ctx, apiKey, digest)
		if err != nil {
			res.Errors++
			continue
		}
		md = s.opts.Scrubber.String(md)
		if _, err := s.opts.DB.ExecContext(ctx,
			`UPDATE sessions SET summary_md = ? WHERE id = ?`,
			md, m.id,
		); err != nil {
			res.Errors++
			continue
		}
		res.SessionsProcessed++
	}
	res.DurationMs = time.Since(start).Milliseconds()
	return res, nil
}

func (s *Summarizer) resolveAPIKey() string {
	if s.opts.APIKey != "" {
		return s.opts.APIKey
	}
	envName := s.opts.APIKeyEnv
	if envName == "" {
		envName = "ANTHROPIC_API_KEY"
	}
	return os.Getenv(envName)
}

func (s *Summarizer) buildDigest(ctx context.Context, sessionID, tool, startedAt, project string, totalActions int) (string, error) {
	rows, err := s.opts.DB.QueryContext(ctx,
		`SELECT action_type, target, success, COALESCE(error_message, '')
		 FROM actions WHERE session_id = ?
		 ORDER BY timestamp LIMIT 100`, sessionID)
	if err != nil {
		return "", fmt.Errorf("summary: actions query: %w", err)
	}
	defer rows.Close()

	var b strings.Builder
	fmt.Fprintf(&b, "Session %s (tool: %s, started: %s, project: %s, total_actions: %d)\n\nActions:\n",
		sessionID, tool, startedAt, project, totalActions)
	count := 0
	for rows.Next() {
		var actionType, target string
		var success int
		var errMsg string
		if err := rows.Scan(&actionType, &target, &success, &errMsg); err != nil {
			return "", fmt.Errorf("summary: action scan: %w", err)
		}
		status := "ok"
		if success == 0 {
			status = "FAIL"
			if errMsg != "" {
				status += ": " + truncateStr(errMsg, 80)
			}
		}
		fmt.Fprintf(&b, "- %s %s [%s]\n", actionType, truncateStr(target, 60), status)
		count++
	}
	if count == 0 {
		return "", nil
	}
	return b.String(), rows.Err()
}

type claudeRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	Messages  []claudeMessage `json:"messages"`
}

type claudeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type claudeResponse struct {
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
}

func (s *Summarizer) callClaude(ctx context.Context, apiKey, digest string) (string, error) {
	prompt := "Summarize this AI coding session in 2-4 sentences. Focus on what was accomplished, which files were touched, and whether there were failures. Be concise.\n\n" + digest
	reqBody := claudeRequest{
		Model:     s.opts.Model,
		MaxTokens: s.opts.MaxTokens,
		Messages:  []claudeMessage{{Role: "user", Content: prompt}},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("summary: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.opts.BaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("summary: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)
	req.Header.Set("Anthropic-Version", "2023-06-01")

	resp, err := s.opts.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("summary: API call: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("summary: API %d: %s", resp.StatusCode, respBody)
	}
	var cr claudeResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&cr); err != nil {
		return "", fmt.Errorf("summary: parse response: %w", err)
	}
	if len(cr.Content) == 0 {
		return "", errors.New("summary: empty response content")
	}
	return cr.Content[0].Text, nil
}

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
