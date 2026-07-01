//go:build !no_obs

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// chatCompletionsJudge calls an OpenAI-compatible /chat/completions endpoint
// (OpenRouter by default) for a single LLM-as-judge turn and returns the reply
// text. It is the host's implementation detail behind the obs eval.JudgeClient
// (wired in obs_wire.go) — kept in its own file with NO internal/obs import so
// it stays generic and the reverse-import boundary holds (only obs_wire.go may
// import obs). This is the SINGLE outbound network call in the whole eval
// subsystem; it runs only for an explicitly-invoked `observer eval run`.
//
// The request is tagged for downstream attribution: the X-SBO-Eval header and
// the OpenRouter X-Title carry "superbased-observer eval" so the judge spend
// can be told apart from app traffic (plan §15 Q4 — full proxy-routed cost
// capture + sbo.eval spend-exclusion is the documented follow-up).
//
// hv is the Authorization header value's credential portion, sourced from an
// env var by the caller (never written to config or disk).
type chatCompletionsJudge struct {
	baseURL string
	hv      string
	httpc   *http.Client
}

func newChatCompletionsJudge(baseURL, hv string) chatCompletionsJudge {
	return chatCompletionsJudge{
		baseURL: strings.TrimRight(baseURL, "/"),
		hv:      hv,
		httpc:   &http.Client{Timeout: 60 * time.Second},
	}
}

func (j chatCompletionsJudge) complete(ctx context.Context, model, prompt string) (string, error) {
	if j.hv == "" {
		return "", fmt.Errorf("judge credential is empty — set the env var named by [observability.eval] judge_api_key_env (default OPENROUTER_API_KEY)")
	}
	if model == "" {
		return "", fmt.Errorf("judge model is empty — set [observability.eval] judge_model or the scorer's model= param")
	}
	reqBody := map[string]any{
		"model":       model,
		"temperature": 0,
		"messages": []map[string]string{
			{"role": "system", "content": "You are a strict evaluation judge. Reply with a JSON object {\"score\": <0..1>, \"rationale\": <short>} and nothing else."},
			{"role": "user", "content": prompt},
		},
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("judge: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.baseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("judge: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+j.hv)
	req.Header.Set("X-SBO-Eval", "true")                  // attribution marker (plan §15 Q4)
	req.Header.Set("X-Title", "superbased-observer eval") // OpenRouter etiquette

	resp, err := j.httpc.Do(req)
	if err != nil {
		return "", fmt.Errorf("judge: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("judge: upstream %d: %s", resp.StatusCode, truncateJudge(string(body), 300))
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("judge: decode response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("judge: empty choices in response")
	}
	return parsed.Choices[0].Message.Content, nil
}

func truncateJudge(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
